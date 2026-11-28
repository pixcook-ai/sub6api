package repository

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

func TestHTTPUpstreamUsesChangedTLSProfile(t *testing.T) {
	const childEnv = "SUB6API_TLS_PROFILE_REUSE_TEST_CHILD"
	if os.Getenv(childEnv) != "1" {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		executable, err := os.Executable()
		require.NoError(t, err)
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestHTTPUpstreamUsesChangedTLSProfile$", "-test.v")
		cmd.Env = []string{childEnv + "=1", "GODEBUG=x509usefallbackroots=1"}
		cmd.WaitDelay = time.Second
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return
	}

	certificate, authority := newClaudeTLSFixtureIdentity(t, 1)
	roots := x509.NewCertPool()
	roots.AddCert(authority)
	x509.SetFallbackRoots(roots)
	type connection struct {
		CipherSuite uint16
		Address     string
	}
	server := newClaudeTLSFixtureServer(t, certificate, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(connection{CipherSuite: r.TLS.CipherSuite, Address: r.RemoteAddr})
	})
	upstream := NewHTTPUpstream(nil)
	request := func(cipherSuite uint16) connection {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
		require.NoError(t, err)
		profile := &tlsfingerprint.Profile{Name: "test template", CipherSuites: []uint16{cipherSuite}}
		response, err := upstream.DoWithTLS(req, "", 1, 1, profile)
		require.NoError(t, err)
		defer func(body io.ReadCloser) { _ = body.Close() }(response.Body)
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		var observed connection
		require.NoError(t, json.Unmarshal(body, &observed))
		return observed
	}

	original := request(tls.TLS_AES_128_GCM_SHA256)
	require.Equal(t, uint16(tls.TLS_AES_128_GCM_SHA256), original.CipherSuite)
	require.Equal(t, original, request(tls.TLS_AES_128_GCM_SHA256), "identical profile contents should reuse the connection")
	changed := request(tls.TLS_AES_256_GCM_SHA384)
	require.Equal(t, uint16(tls.TLS_AES_256_GCM_SHA384), changed.CipherSuite, "changed contents must apply even when the template name stays the same")
	require.NotEqual(t, original.Address, changed.Address)
	require.Equal(t, changed, request(tls.TLS_AES_256_GCM_SHA384), "the replacement profile should also reuse its connection")
}
