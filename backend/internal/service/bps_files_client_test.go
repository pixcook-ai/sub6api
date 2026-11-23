package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
	"github.com/MACOS-DO/sub4api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type bpsFilesTestTransport struct {
	client *http.Client
	calls  int
}

func (t *bpsFilesTestTransport) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	t.calls++
	if req.GetBody != nil || !HTTPUpstreamRedirectsDisabled(req.Context()) || req.Header.Get("Idempotency-Key") != "" {
		return nil, fmt.Errorf("unsafe private request")
	}
	client := *t.client
	if HTTPUpstreamRedirectsDisabled(req.Context()) {
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	return client.Do(req)
}
func (t *bpsFilesTestTransport) DoWithTLS(req *http.Request, p string, a int64, c int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return t.Do(req, p, a, c)
}
func bpsFilesTestIdentity() managedfiles.Identity {
	return managedfiles.Identity{BackendID: "test-backend", SubjectFingerprint: "sha256:" + strings.Repeat("a", 64), CredentialGeneration: "epoch-1"}
}
func bpsFilesTestDescription() managedfiles.DescribeResult {
	return managedfiles.DescribeResult{Contract: managedfiles.Contract, JSONProfile: managedfiles.JSONProfile, Identity: bpsFilesTestIdentity(), Capabilities: managedfiles.Capabilities{BatchAdmit: true, EnsureUpload: true}, Limits: managedfiles.Limits{FileBytes: managedfiles.MaxFileBytes, MessageBytes: managedfiles.MaxMessageBytes, MessagePositions: 20, StagingBytes: 2 << 30, AdmissionBytes: 1 << 30, ConcurrentAdmissions: 4, LiveAdmissions: 32, AdmissionTTLSeconds: 600, ManifestBytes: 1 << 20, UniqueFiles: 128, FreeSpaceReserveBytes: 1, AdmitTimeoutSeconds: 60, EnsureTimeoutSeconds: 300}}
}
func bpsFilesTestBackend(t *testing.T, url string, transport *bpsFilesTestTransport) *BPSFilesBackend {
	t.Helper()
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}
	backend, err := NewBPSFilesBackend(BPSFilesBackendConfig{ControlURL: url, ServiceKey: "private-test-key", Expected: bpsFilesTestIdentity(), AccountID: 7, Concurrency: 1}, transport, cfg)
	require.NoError(t, err)
	return backend
}
func TestBPSFilesBackendDescribeAuthenticatesAndChecksIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/internal/bps-files/v1/describe", r.URL.Path)
		require.Equal(t, "private-test-key", r.Header.Get(managedfiles.ServiceKeyHeader))
		require.Empty(t, r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("Idempotency-Key"))
		require.NoError(t, json.NewEncoder(w).Encode(bpsFilesTestDescription()))
	}))
	defer server.Close()
	transport := &bpsFilesTestTransport{client: server.Client()}
	got, err := bpsFilesTestBackend(t, server.URL, transport).Describe(context.Background())
	require.NoError(t, err)
	require.Equal(t, bpsFilesTestIdentity(), got.Identity)
	require.Equal(t, 1, transport.calls)
}

func TestBPSFilesBackendAdmitStreamsAndValidatesTemplate(t *testing.T) {
	identity := bpsFilesTestIdentity()
	payload := "hello managed file"
	hash := sha256.Sum256([]byte(payload))
	manifest := managedfiles.AdmitManifest{Contract: managedfiles.Contract, JSONProfile: managedfiles.JSONProfile, RequestID: "request-1", Expected: identity, OwnerRef: "owner-1", GroupRef: "group-1", RequestTemplate: json.RawMessage(`{"model":"gpt-5.5","input":[{"role":"user","content":[{"type":"input_file","file_id":"file-public"}]}]}`), Files: []managedfiles.File{{FileRef: "file-public", Mode: "upload", Part: "file_0", Filename: "test.txt", MIME: "text/plain", Bytes: int64(len(payload)), SHA256: hex.EncodeToString(hash[:]), Positions: []managedfiles.Position{{Input: 0, Part: 0}}}}}
	templateHash, err := managedfiles.TemplateHash(manifest.RequestTemplate)
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/internal/bps-files/v1/admit", r.URL.Path)
		require.Positive(t, r.ContentLength)
		multipart, err := r.MultipartReader()
		require.NoError(t, err)
		first, err := multipart.NextPart()
		require.NoError(t, err)
		require.Equal(t, "manifest", first.FormName())
		raw, err := io.ReadAll(first)
		require.NoError(t, err)
		decoded, err := managedfiles.DecodeManifest(raw)
		require.NoError(t, err)
		require.Equal(t, manifest.Files, decoded.Files)
		file, err := multipart.NextPart()
		require.NoError(t, err)
		require.Equal(t, "file_0", file.FormName())
		got, err := io.ReadAll(file)
		require.NoError(t, err)
		require.Equal(t, payload, string(got))
		_, err = multipart.NextPart()
		require.ErrorIs(t, err, io.EOF)
		json.NewEncoder(w).Encode(managedfiles.AdmitResult{Contract: managedfiles.Contract, JSONProfile: managedfiles.JSONProfile, Identity: identity, AdmissionID: "admission-1", ExpiresAt: time.Now().Add(time.Minute).Unix(), TemplateSHA256: templateHash, Files: []managedfiles.AdmittedFile{{FileRef: "file-public", State: "staged"}}})
	}))
	defer server.Close()
	transport := &bpsFilesTestTransport{client: server.Client()}
	result, err := bpsFilesTestBackend(t, server.URL, transport).Admit(context.Background(), manifest, map[string]io.Reader{"file-public": strings.NewReader(payload)})
	require.NoError(t, err)
	require.Equal(t, "admission-1", result.AdmissionID)
	require.Equal(t, 1, transport.calls)
}

func TestBPSFilesBackendEnsureChecksUploadedCopy(t *testing.T) {
	file := managedfiles.File{FileRef: "file-public", Mode: "upload", Filename: "test.txt", MIME: "text/plain", Bytes: 5, SHA256: strings.Repeat("b", 64)}
	request := managedfiles.EnsureRequest{Contract: managedfiles.Contract, Expected: bpsFilesTestIdentity(), AdmissionID: "admission-1", FileRef: file.FileRef, OperationID: "operation-1"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/internal/bps-files/v1/ensure-uploaded", r.URL.Path)
		require.Equal(t, "private-test-key", r.Header.Get(managedfiles.ServiceKeyHeader))
		var got managedfiles.EnsureRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		require.Equal(t, request, got)
		json.NewEncoder(w).Encode(managedfiles.EnsureResult{Contract: managedfiles.Contract, Identity: bpsFilesTestIdentity(), OperationID: request.OperationID, FileRef: file.FileRef, RemoteID: "file-remote", Bytes: file.Bytes, SHA256: file.SHA256, Filename: file.Filename, MIME: file.MIME, InputTokens: 17})
	}))
	defer server.Close()
	transport := &bpsFilesTestTransport{client: server.Client()}
	result, err := bpsFilesTestBackend(t, server.URL, transport).EnsureUploaded(context.Background(), request, file)
	require.NoError(t, err)
	require.Equal(t, int64(17), result.InputTokens)
	require.Equal(t, 1, transport.calls)
}

func TestBPSFilesBackendRejectsUntrustedDescriptionAndNeverRetries(t *testing.T) {
	good, _ := json.Marshal(bpsFilesTestDescription())
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"wrong identity", strings.Replace(string(good), "test-backend", "other-backend", 1), 200},
		{"duplicate", strings.TrimSuffix(string(good), "}") + `,"contract":"bps-files/1"}`, 200},
		{"case alias", strings.Replace(string(good), `"contract"`, `"Contract"`, 1), 200},
		{"missing required", `{"contract":"bps-files/1"}`, 200},
		{"null field", strings.Replace(string(good), `"batch_admit":true`, `"batch_admit":null`, 1), 200},
		{"oversized", strings.Repeat("x", int(bpsFilesResponseLimit)+1), 200},
		{"server failure", `secret-token /private/secret`, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); io.WriteString(w, tc.body) }))
			defer server.Close()
			transport := &bpsFilesTestTransport{client: server.Client()}
			_, err := bpsFilesTestBackend(t, server.URL, transport).Describe(context.Background())
			require.Error(t, err)
			require.NotContains(t, err.Error(), "secret")
			require.Equal(t, 1, transport.calls)
		})
	}
}
func TestBPSFilesBackendRedirectAndCancelledContextDoNotSendCredentialsAgain(t *testing.T) {
	var redirected int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected++ }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	transport := &bpsFilesTestTransport{client: source.Client()}
	backend := bpsFilesTestBackend(t, source.URL, transport)
	_, err := backend.Describe(context.Background())
	require.Error(t, err)
	require.Zero(t, redirected)
	require.Equal(t, 1, transport.calls)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = backend.Describe(ctx)
	require.Error(t, err)
	require.Equal(t, 1, transport.calls)
}
func TestBPSFilesBackendEnsureInvalidResultIsUnknownAndNotRetried(t *testing.T) {
	file := managedfiles.File{FileRef: "file-public", Mode: "upload", Filename: "test.txt", MIME: "text/plain", Bytes: 5, SHA256: strings.Repeat("b", 64)}
	request := managedfiles.EnsureRequest{Contract: managedfiles.Contract, Expected: bpsFilesTestIdentity(), AdmissionID: "admission-1", FileRef: file.FileRef, OperationID: "operation-1"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(managedfiles.EnsureResult{Contract: managedfiles.Contract, Identity: bpsFilesTestIdentity(), OperationID: "wrong-operation", FileRef: file.FileRef, RemoteID: "file-remote", Bytes: 5, SHA256: file.SHA256, Filename: file.Filename, MIME: file.MIME})
	}))
	defer server.Close()
	transport := &bpsFilesTestTransport{client: server.Client()}
	_, err := bpsFilesTestBackend(t, server.URL, transport).EnsureUploaded(context.Background(), request, file)
	var typed *BPSFilesBackendError
	require.ErrorAs(t, err, &typed)
	require.True(t, typed.OutcomeUnknown)
	require.Equal(t, 1, transport.calls)
}

type bpsFilesPatternReader struct {
	remaining   int64
	largestRead int
}

func (r *bpsFilesPatternReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	if len(p) > r.largestRead {
		r.largestRead = len(p)
	}
	for i := range p {
		p[i] = 'x'
	}
	r.remaining -= int64(len(p))
	return len(p), nil
}
func TestBPSFilesBackendAdmitStreamsLargeFileAndPreservesHash(t *testing.T) {
	const size int64 = 33 << 20
	expected := sha256.New()
	_, err := io.Copy(expected, &bpsFilesPatternReader{remaining: size})
	require.NoError(t, err)
	file := managedfiles.File{FileRef: "file-large", Mode: "upload", Part: "file_0", Filename: "large.txt", MIME: "text/plain", Bytes: size, SHA256: hex.EncodeToString(expected.Sum(nil)), Positions: []managedfiles.Position{{Input: 0, Part: 0}}}
	manifest := managedfiles.AdmitManifest{Contract: managedfiles.Contract, JSONProfile: managedfiles.JSONProfile, RequestID: "request-large", Expected: bpsFilesTestIdentity(), OwnerRef: "owner-1", GroupRef: "group-1", RequestTemplate: json.RawMessage(`{"model":"gpt-5.5","input":[{"role":"user","content":[{"type":"input_file","file_id":"file-large"}]}]}`), Files: []managedfiles.File{file}}
	templateHash, err := managedfiles.TemplateHash(manifest.RequestTemplate)
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		multipart, err := r.MultipartReader()
		require.NoError(t, err)
		first, err := multipart.NextPart()
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, first)
		require.NoError(t, err)
		part, err := multipart.NextPart()
		require.NoError(t, err)
		observed := sha256.New()
		n, err := io.Copy(observed, part)
		require.NoError(t, err)
		require.Equal(t, size, n)
		require.Equal(t, file.SHA256, hex.EncodeToString(observed.Sum(nil)))
		json.NewEncoder(w).Encode(managedfiles.AdmitResult{Contract: managedfiles.Contract, JSONProfile: managedfiles.JSONProfile, Identity: bpsFilesTestIdentity(), AdmissionID: "admission-large", ExpiresAt: time.Now().Add(time.Minute).Unix(), TemplateSHA256: templateHash, Files: []managedfiles.AdmittedFile{{FileRef: file.FileRef, State: "staged"}}})
	}))
	defer server.Close()
	source := &bpsFilesPatternReader{remaining: size}
	transport := &bpsFilesTestTransport{client: server.Client()}
	_, err = bpsFilesTestBackend(t, server.URL, transport).Admit(context.Background(), manifest, map[string]io.Reader{file.FileRef: source})
	require.NoError(t, err)
	require.Zero(t, source.remaining)
	require.LessOrEqual(t, source.largestRead, 1<<20)
	require.Equal(t, 1, transport.calls)
}

func TestBPSFilesBackendRejectsUnsafeConfigBeforeAnyRequest(t *testing.T) {
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}
	for _, url := range []string{"http://user:secret@example.com", "http://example.com?key=secret", "http://example.com#secret", "http://example.com/a/../b", "http://example.com/%2froot"} {
		_, err := NewBPSFilesBackend(BPSFilesBackendConfig{ControlURL: url, ServiceKey: "private-test-key", Expected: bpsFilesTestIdentity(), AccountID: 7, Concurrency: 1}, &bpsFilesTestTransport{}, cfg)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "secret")
	}
}

func TestBPSFilesBackendControlPrefixIsOperatorConfigured(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/control/internal/bps-files/v1/describe", r.URL.Path)
		json.NewEncoder(w).Encode(bpsFilesTestDescription())
	}))
	defer server.Close()
	transport := &bpsFilesTestTransport{client: server.Client()}
	_, err := bpsFilesTestBackend(t, server.URL+"/control/", transport).Describe(context.Background())
	require.NoError(t, err)
	_, err = NewBPSFilesBackend(BPSFilesBackendConfig{ControlURL: server.URL + "/", ServiceKey: "private-test-key", Expected: bpsFilesTestIdentity(), AccountID: 7, Concurrency: 1}, transport, &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}})
	require.NoError(t, err)
}

func TestBPSFilesBackendEnsureTransportFailureDoesNotLeakOrRetry(t *testing.T) {
	file := managedfiles.File{FileRef: "file-public", Mode: "upload", Filename: "test.txt", MIME: "text/plain", Bytes: 5, SHA256: strings.Repeat("b", 64)}
	request := managedfiles.EnsureRequest{Contract: managedfiles.Contract, Expected: bpsFilesTestIdentity(), AdmissionID: "admission-1", FileRef: file.FileRef, OperationID: "operation-1"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, _, err := w.(http.Hijacker).Hijack()
		require.NoError(t, err)
		connection.Close()
	}))
	defer server.Close()
	transport := &bpsFilesTestTransport{client: server.Client()}
	_, err := bpsFilesTestBackend(t, server.URL, transport).EnsureUploaded(context.Background(), request, file)
	var typed *BPSFilesBackendError
	require.ErrorAs(t, err, &typed)
	require.True(t, typed.OutcomeUnknown)
	require.Equal(t, 1, transport.calls)
	require.NotContains(t, err.Error(), server.URL)
}
