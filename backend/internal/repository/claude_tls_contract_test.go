package repository

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/tlsfingerprint"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestClaudeTLSUpstreamContract(t *testing.T) {
	const childEnv = "SUB6API_CLAUDE_TLS_CONTRACT_CHILD"
	if os.Getenv(childEnv) != "1" {
		// Go's root cache is process-wide. Keep the fixture roots private to a
		// subprocess so other tests and the system trust store stay isolated.
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		executable, err := os.Executable()
		require.NoError(t, err)
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestClaudeTLSUpstreamContract$", "-test.v")
		cmd.Env = []string{childEnv + "=1", "GODEBUG=x509usefallbackroots=1"}
		cmd.WaitDelay = time.Second
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		t.Logf("\n%s", output)
		return
	}

	trusted, trustedCA := newClaudeTLSFixtureIdentity(t, 1)
	untrusted, _ := newClaudeTLSFixtureIdentity(t, 3)
	roots := x509.NewCertPool()
	roots.AddCert(trustedCA)
	x509.SetFallbackRoots(roots)

	profile := (&service.TLSFingerprintProfileService{}).ResolveTLSProfile(&service.Account{
		Platform: service.PlatformAnthropic,
		Type:     service.AccountTypeOAuth,
		Extra:    map[string]any{"enable_tls_fingerprint": true},
	})
	require.NotNil(t, profile, "the existing Anthropic OAuth configuration must resolve a builtin TLS profile")
	for _, mode := range []struct {
		name    string
		profile *tlsfingerprint.Profile
	}{{name: "standard"}, {name: "builtin", profile: profile}} {
		t.Run(mode.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			client := NewHTTPUpstream(nil)
			const first = "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
			const final = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
			releaseFinal := make(chan struct{})
			type negotiatedTLS struct {
				state tls.ConnectionState
				proto string
			}
			negotiated := make(chan negotiatedTLS, 1)
			trustedServer := newClaudeTLSFixtureServer(t, trusted, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" || r.TLS == nil {
					http.Error(w, "expected a TLS Messages request", http.StatusBadRequest)
					return
				}
				negotiated <- negotiatedTLS{state: *r.TLS, proto: r.Proto}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, first)
				w.(http.Flusher).Flush()
				select {
				case <-releaseFinal:
					_, _ = io.WriteString(w, final)
				case <-r.Context().Done():
				}
			})
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, trustedServer.URL+"/v1/messages", nil)
			require.NoError(t, err)
			resp, err := client.DoWithTLS(req, "", 1, 1, mode.profile)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
			firstFrame := make([]byte, len(first))
			_, err = io.ReadFull(resp.Body, firstFrame)
			require.NoError(t, err)
			require.Equal(t, first, string(firstFrame))
			close(releaseFinal)
			remaining, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, final, string(remaining))

			observed := <-negotiated
			require.True(t, observed.state.HandshakeComplete)
			require.GreaterOrEqual(t, observed.state.Version, uint16(tls.VersionTLS12))
			require.Equal(t, "HTTP/1.1", observed.proto)
			if mode.profile != nil {
				require.Equal(t, "http/1.1", observed.state.NegotiatedProtocol)
			}
			t.Logf("TLS=%s cipher=%s ALPN=%q HTTP=%s", tls.VersionName(observed.state.Version),
				tls.CipherSuiteName(observed.state.CipherSuite), observed.state.NegotiatedProtocol, observed.proto)

			var untrustedRequest atomic.Bool
			untrustedServer := newClaudeTLSFixtureServer(t, untrusted, func(w http.ResponseWriter, _ *http.Request) {
				untrustedRequest.Store(true)
				w.WriteHeader(http.StatusNoContent)
			})
			req, err = http.NewRequestWithContext(ctx, http.MethodPost, untrustedServer.URL+"/v1/messages", nil)
			require.NoError(t, err)
			resp, err = client.DoWithTLS(req, "", 1, 1, mode.profile)
			if resp != nil {
				require.NoError(t, resp.Body.Close())
			}
			var unknownCA x509.UnknownAuthorityError
			require.ErrorAs(t, err, &unknownCA)
			require.False(t, untrustedRequest.Load(), "an untrusted peer must not receive an HTTP request")
		})
	}
}

func newClaudeTLSFixtureServer(t *testing.T, certificate tls.Certificate, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(handler)
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h2", "http/1.1"},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func newClaudeTLSFixtureIdentity(t *testing.T, serial int64) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	now := time.Now()
	ca := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: fmt.Sprintf("Claude TLS test root %d", serial)},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	require.NoError(t, err)
	ca, err = x509.ParseCertificate(caDER)
	require.NoError(t, err)
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(serial + 1),
		NotBefore:    ca.NotBefore,
		NotAfter:     ca.NotAfter,
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}, ca
}
