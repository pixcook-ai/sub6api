package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
	"github.com/stretchr/testify/require"
)

func bpsRecoveryManifest() managedfiles.AdmitManifest {
	return managedfiles.AdmitManifest{Contract: managedfiles.Contract, JSONProfile: managedfiles.JSONProfile, RequestID: "recovery-request", Expected: bpsFilesTestIdentity(), OwnerRef: "recovery-owner", GroupRef: "recovery-group", RequestTemplate: json.RawMessage(`{"model":"fixture","input":[{"role":"user","content":[{"type":"input_file","file_id":"file-existing"},{"type":"input_file","file_id":"file-upload"}]}]}`), Files: []managedfiles.File{
		{FileRef: "file-existing", Mode: "existing", Filename: "old.txt", MIME: "text/plain", Bytes: 3, SHA256: strings.Repeat("a", 64), RemoteID: "file-native-old", OperationID: "old-operation", Positions: []managedfiles.Position{{Input: 0, Part: 0}}},
		{FileRef: "file-upload", Mode: "upload", Part: "upload_part", Filename: "new.txt", MIME: "text/plain", Bytes: 3, SHA256: "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", Positions: []managedfiles.Position{{Input: 0, Part: 1}}},
	}}
}

func bpsRecoveryPayload(t *testing.T, m managedfiles.AdmitManifest) []byte {
	t.Helper()
	hash, err := managedfiles.RecoveryManifestHash(m)
	require.NoError(t, err)
	raw, err := json.Marshal(map[string]any{"error": map[string]any{"type": "invalid_request_error", "code": "managed_metadata_missing", "param": nil, "message": "owner-scoped native metadata is unavailable", "recovery": managedfiles.RecoveryMissing{Contract: "bps-metadata-recovery/1", Identity: m.Expected, RequestID: m.RequestID, ManifestSHA256: hash, MissingRefs: []string{"file-existing"}}}})
	require.NoError(t, err)
	return raw
}

func TestBPSFilesAdmitRecoverablePreservesMultipartAndReturnsPrivateMissing(t *testing.T) {
	m := bpsRecoveryManifest()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, managedfiles.Prefix+"admit", r.URL.Path)
		require.Equal(t, "private-test-key", r.Header.Get(managedfiles.ServiceKeyHeader))
		require.Empty(t, r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("Idempotency-Key"))
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.EqualValues(t, len(raw), r.ContentLength)
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		require.NoError(t, err)
		reader := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
		part, err := reader.NextPart()
		require.NoError(t, err)
		require.Equal(t, "manifest", part.FormName())
		manifestRaw, err := io.ReadAll(part)
		require.NoError(t, err)
		got, err := managedfiles.DecodeManifest(manifestRaw)
		require.NoError(t, err)
		require.JSONEq(t, string(m.RequestTemplate), string(got.RequestTemplate))
		got.RequestTemplate = m.RequestTemplate
		require.Equal(t, m, got)
		part, err = reader.NextPart()
		require.NoError(t, err)
		require.Equal(t, "upload_part", part.FormName())
		require.Equal(t, "new.txt", part.FileName())
		payload, err := io.ReadAll(part)
		require.NoError(t, err)
		require.Equal(t, "abc", string(payload))
		_, err = reader.NextPart()
		require.ErrorIs(t, err, io.EOF)
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write(bpsRecoveryPayload(t, got))
	}))
	defer server.Close()
	transport := &bpsFilesTestTransport{client: server.Client()}
	result, missing, err := bpsFilesTestBackend(t, server.URL, transport).AdmitRecoverable(context.Background(), m, map[string]io.Reader{"file-upload": strings.NewReader("abc")})
	require.NoError(t, err)
	require.Empty(t, result.AdmissionID)
	require.NotNil(t, missing)
	require.Equal(t, []string{"file-existing"}, missing.MissingRefs)
	require.Equal(t, m.Expected, missing.Identity)
	require.Equal(t, 1, transport.calls)
}

func bpsRecoverySuccess(t *testing.T, m managedfiles.AdmitManifest) managedfiles.AdmitResult {
	t.Helper()
	hash, err := managedfiles.TemplateHash(m.RequestTemplate)
	require.NoError(t, err)
	return managedfiles.AdmitResult{Contract: managedfiles.Contract, JSONProfile: managedfiles.JSONProfile, Identity: m.Expected, AdmissionID: "admission-recovery", ExpiresAt: time.Now().Add(time.Minute).Unix(), TemplateSHA256: hash, Files: []managedfiles.AdmittedFile{{FileRef: "file-existing", State: "known"}, {FileRef: "file-upload", State: "staged"}}}
}

func TestBPSFilesAdmitRecoverableKeepsSuccessAndOrdinaryFailureValidation(t *testing.T) {
	m := bpsRecoveryManifest()
	good := string(bpsRecoveryPayload(t, m))
	success := bpsRecoverySuccess(t, m)
	successRaw, err := json.Marshal(success)
	require.NoError(t, err)
	for _, tc := range []struct {
		name, body string
		status     int
		success    bool
	}{
		{"success", string(successRaw), 200, true},
		{"invalid success", strings.Replace(string(successRaw), "admission-recovery", "", 1), 200, false},
		{"wrong success identity", strings.Replace(string(successRaw), "test-backend", "other-backend", 1), 200, false},
		{"wrong success template", strings.Replace(string(successRaw), success.TemplateSHA256, strings.Repeat("f", 64), 1), 200, false},
		{"wrong success state", strings.Replace(string(successRaw), "staged", "known", 1), 200, false},
		{"generic conflict", `{"error":{"code":"managed_metadata_missing"}}`, 409, false},
		{"private rejection", `secret-token private-key`, 409, false},
		{"unknown alias", strings.Replace(good, `"recovery"`, `"Recovery"`, 1), 409, false},
		{"wrong manifest hash", strings.Replace(good, `"manifest_sha256":"`, `"manifest_sha256":"f`, 1), 409, false},
		{"other status", good, 503, false},
		{"oversized padded signal", good + strings.Repeat(" ", int(bpsFilesResponseLimit)), 409, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			transport := &bpsFilesTestTransport{client: server.Client()}
			got, missing, err := bpsFilesTestBackend(t, server.URL, transport).AdmitRecoverable(context.Background(), m, map[string]io.Reader{"file-upload": strings.NewReader("abc")})
			require.Nil(t, missing)
			if tc.success {
				require.NoError(t, err)
				require.Equal(t, success, got)
			} else {
				require.Error(t, err)
				require.Empty(t, got.AdmissionID)
				require.NotContains(t, err.Error(), "secret-token")
				require.NotContains(t, err.Error(), "private-key")
				var failure *BPSFilesBackendError
				require.ErrorAs(t, err, &failure)
				require.False(t, failure.OutcomeUnknown)
				if tc.status != 200 {
					require.Equal(t, "backend_rejected", failure.Code)
					require.Equal(t, tc.status, failure.HTTPStatus)
				}
			}
			require.Equal(t, 1, transport.calls)
		})
	}
}

func TestBPSFilesAdmitRecoverableDoesNotChangeOtherCallsOrRetryRedirects(t *testing.T) {
	m := bpsRecoveryManifest()
	var redirected int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected++ }))
	defer target.Close()
	var redirect bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if redirect {
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(409)
		_, _ = w.Write(bpsRecoveryPayload(t, m))
	}))
	defer server.Close()
	transport := &bpsFilesTestTransport{client: server.Client()}
	backend := bpsFilesTestBackend(t, server.URL, transport)
	_, missing, err := backend.AdmitRecoverable(context.Background(), m, map[string]io.Reader{"file-upload": strings.NewReader("abc")})
	require.NoError(t, err)
	require.NotNil(t, missing)
	_, err = backend.Admit(context.Background(), m, map[string]io.Reader{"file-upload": strings.NewReader("abc")})
	require.Error(t, err, "ordinary Admit retains its original409 behavior")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, missing, err = backend.AdmitRecoverable(ctx, m, map[string]io.Reader{"file-upload": strings.NewReader("abc")})
	require.Error(t, err)
	require.Nil(t, missing)
	require.Equal(t, 2, transport.calls)
	redirect = true
	_, missing, err = backend.AdmitRecoverable(context.Background(), m, map[string]io.Reader{"file-upload": strings.NewReader("abc")})
	require.Error(t, err)
	require.Nil(t, missing)
	require.Zero(t, redirected)
	require.Equal(t, 3, transport.calls)
}

func TestBPSFilesAdmitRecoverableCancellationDuringResponseDoesNotRecover(t *testing.T) {
	m := bpsRecoveryManifest()
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(409)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	transport := &bpsFilesTestTransport{client: server.Client()}
	backend := bpsFilesTestBackend(t, server.URL, transport)
	type outcome struct {
		missing *managedfiles.RecoveryMissing
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		_, missing, err := backend.AdmitRecoverable(ctx, m, map[string]io.Reader{"file-upload": strings.NewReader("abc")})
		done <- outcome{missing, err}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not reach private server")
	}
	cancel()
	select {
	case got := <-done:
		require.Error(t, got.err)
		require.Nil(t, got.missing)
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled response read did not finish")
	}
	require.Equal(t, 1, transport.calls)
}
