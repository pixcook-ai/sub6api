package service

import (
	"context"
	"encoding/json"
	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBPSFilesBackendDescribeNegotiatesRecoveryBoolean(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "old gateway false", true: "negotiated gateway true"}[enabled], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, []string{"bps-metadata-recovery/1"}, r.Header.Values("X-Bps-Files-Recovery"))
				require.Equal(t, "private-test-key", r.Header.Get("X-Bps-Service-Key"))
				description := bpsFilesTestDescription()
				description.Capabilities.MetadataRecovery = enabled
				require.NoError(t, json.NewEncoder(w).Encode(description))
			}))
			defer server.Close()
			transport := &bpsFilesTestTransport{client: server.Client()}
			got, err := bpsFilesTestBackend(t, server.URL, transport).Describe(context.Background())
			require.NoError(t, err)
			require.Equal(t, enabled, got.Capabilities.MetadataRecovery)
			require.Equal(t, 1, transport.calls)
		})
	}
}

func TestBPSFilesBackendLegacyFalseStillAdmitsAndUploadsWithoutFeatureHeader(t *testing.T) {
	identity := bpsFilesTestIdentity()
	manifest := managedfiles.AdmitManifest{Contract: managedfiles.Contract, JSONProfile: managedfiles.JSONProfile, RequestID: "legacy-request", Expected: identity, OwnerRef: "owner-legacy", GroupRef: "group-legacy", RequestTemplate: json.RawMessage(`{"model":"fixture","input":[{"role":"user","content":[{"type":"input_file","file_id":"file-legacy"}]}]}`), Files: []managedfiles.File{{FileRef: "file-legacy", Mode: "upload", Part: "file_0", Filename: "legacy.txt", MIME: "text/plain", Bytes: 5, SHA256: "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824", Positions: []managedfiles.Position{{Input: 0, Part: 0}}}}}
	file := manifest.Files[0]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "private-test-key", r.Header.Get(managedfiles.ServiceKeyHeader))
		if strings.HasSuffix(r.URL.Path, "describe") {
			require.Equal(t, []string{"bps-metadata-recovery/1"}, r.Header.Values("X-Bps-Files-Recovery"))
			require.NoError(t, json.NewEncoder(w).Encode(bpsFilesTestDescription()))
			return
		}
		require.Empty(t, r.Header.Values("X-Bps-Files-Recovery"), "feature negotiation belongs only to Describe")
		switch r.URL.Path {
		case managedfiles.Prefix + "admit":
			mr, err := r.MultipartReader()
			require.NoError(t, err)
			part, err := mr.NextPart()
			require.NoError(t, err)
			raw, err := io.ReadAll(part)
			require.NoError(t, err)
			decoded, err := managedfiles.DecodeManifest(raw)
			require.NoError(t, err)
			require.Equal(t, manifest.Files, decoded.Files)
			part, err = mr.NextPart()
			require.NoError(t, err)
			content, err := io.ReadAll(part)
			require.NoError(t, err)
			require.Equal(t, "hello", string(content))
			_, err = mr.NextPart()
			require.ErrorIs(t, err, io.EOF)
			hash, err := managedfiles.TemplateHash(manifest.RequestTemplate)
			require.NoError(t, err)
			require.NoError(t, json.NewEncoder(w).Encode(managedfiles.AdmitResult{Contract: managedfiles.Contract, JSONProfile: managedfiles.JSONProfile, Identity: identity, AdmissionID: "legacy-admission", ExpiresAt: time.Now().Add(time.Minute).Unix(), TemplateSHA256: hash, Files: []managedfiles.AdmittedFile{{FileRef: file.FileRef, State: "staged"}}}))
		case managedfiles.Prefix + "ensure-uploaded":
			var got managedfiles.EnsureRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
			require.Equal(t, "legacy-operation", got.OperationID)
			result := managedfiles.EnsureResult{Contract: managedfiles.Contract, Identity: identity, OperationID: got.OperationID, FileRef: file.FileRef, RemoteID: "file-legacy-remote", Bytes: file.Bytes, SHA256: file.SHA256, Filename: file.Filename, MIME: file.MIME, Ready: true}
			bound, err := managedfiles.Bind(manifest, []managedfiles.EnsureResult{result})
			require.NoError(t, err)
			result.WireSHA256 = bound.WireSHA256
			result.BindingSHA256 = bound.BindingSHA256
			require.NoError(t, json.NewEncoder(w).Encode(result))
		default:
			t.Errorf("unexpected operation %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	transport := &bpsFilesTestTransport{client: server.Client()}
	backend := bpsFilesTestBackend(t, server.URL, transport)
	description, err := backend.Describe(context.Background())
	require.NoError(t, err)
	require.False(t, description.Capabilities.MetadataRecovery)
	admission, err := backend.Admit(context.Background(), manifest, map[string]io.Reader{file.FileRef: strings.NewReader("hello")})
	require.NoError(t, err)
	uploaded, err := backend.EnsureUploaded(context.Background(), managedfiles.EnsureRequest{Contract: managedfiles.Contract, Expected: identity, AdmissionID: admission.AdmissionID, FileRef: file.FileRef, OperationID: "legacy-operation"}, file)
	require.NoError(t, err)
	require.Equal(t, "file-legacy-remote", uploaded.RemoteID)
	require.Equal(t, 3, transport.calls)
}
