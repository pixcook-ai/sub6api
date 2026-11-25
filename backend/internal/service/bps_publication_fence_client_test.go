package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestBPSFilesPublicationFenceAuthenticatesAndStrictlyVerifiesProof(t *testing.T) {
	id := BPSPublicationIdentity{ManagedOAuthIdentity: ManagedOAuthIdentity{Contract: ManagedOAuthContract, BackendID: "backend", BindingRef: "bind_" + uuid.NewString(), ProfileRef: "oauth_" + uuid.NewString(), Provider: CoreOAuthProviderExcel, Subject: "subject", WorkspaceID: "workspace", IdentityEpoch: uuid.NewString()}, TokenRevision: 3}
	proof := BPSPublicationFenceProof{Contract: BPSPublicationFenceProofContract, Identity: id, Fenced: true, Receipt: &BPSPublicationFenceReceipt{BPSPublicationIdentity: id, OperationID: uuid.NewString()}}
	var reply string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "GET", r.Method)
		require.Equal(t, "/internal/bps-files/v1/publication-fence", r.URL.Path)
		require.Equal(t, "private-proof-key", r.Header.Get(managedfiles.ServiceKeyHeader))
		require.Empty(t, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	defer server.Close()
	up := &bpsFilesTestTransport{client: server.Client()}
	backend, err := NewBPSFilesBackend(BPSFilesBackendConfig{ControlURL: server.URL, ServiceKey: "private-proof-key", Expected: managedfiles.Identity{BackendID: id.BackendID, CredentialGeneration: id.IdentityEpoch, SubjectFingerprint: "sha256:" + strings.Repeat("a", 64)}, AccountID: 7, Concurrency: 1}, up, &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}})
	require.NoError(t, err)
	raw, _ := json.Marshal(proof)
	reply = string(raw)
	got, err := backend.PublicationFence(context.Background())
	require.NoError(t, err)
	require.Equal(t, proof, *got)
	proof.Fenced, proof.Receipt = false, nil
	raw, _ = json.Marshal(proof)
	reply = string(raw)
	got, err = backend.PublicationFence(context.Background())
	require.NoError(t, err)
	require.Equal(t, proof, *got)
	proof.Fenced, proof.Receipt = true, &BPSPublicationFenceReceipt{BPSPublicationIdentity: id, OperationID: uuid.NewString()}
	raw, _ = json.Marshal(proof)
	reply = string(raw)
	for _, invalid := range []string{strings.Replace(reply, "backend", "other", 1), strings.Replace(reply, "bps-publication-fence-proof/v1", "bad", 1), strings.Replace(reply, `"fenced":true`, `"fenced":false`, 1), strings.TrimSuffix(reply, "}") + `,"refresh_token":"secret"}`, strings.Replace(reply, `"receipt":{`, `"receipt":null,"extra":{`, 1)} {
		reply = invalid
		_, err = backend.PublicationFence(context.Background())
		require.Error(t, err)
		require.NotContains(t, err.Error(), "secret")
		require.NotContains(t, err.Error(), "private-proof-key")
	}
}
