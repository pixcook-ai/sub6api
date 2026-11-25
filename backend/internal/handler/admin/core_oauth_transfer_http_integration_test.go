package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
	"github.com/MACOS-DO/sub4api/internal/pkg/tlsfingerprint"
	"github.com/MACOS-DO/sub4api/internal/repository"
	"github.com/MACOS-DO/sub4api/internal/server/middleware"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type transferHTTPSealedRepo struct {
	mu   sync.Mutex
	rows map[string]service.CoreOAuthSealedProfile
}

func (r *transferHTTPSealedRepo) ReadProfile(_ context.Context, ref string) (*service.CoreOAuthSealedProfile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.rows[ref]
	if !ok {
		return nil, service.ErrCoreOAuthProfileNotFound
	}
	return &row, nil
}
func (r *transferHTTPSealedRepo) ListProfiles(context.Context) ([]service.CoreOAuthSealedProfile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows := make([]service.CoreOAuthSealedProfile, 0, len(r.rows))
	for _, row := range r.rows {
		rows = append(rows, row)
	}
	return rows, nil
}
func (r *transferHTTPSealedRepo) CreateProfile(_ context.Context, row service.CoreOAuthSealedProfile) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.rows[row.ProfileRef]; ok {
		return service.ErrCoreOAuthConflict
	}
	r.rows[row.ProfileRef] = row
	return nil
}
func (r *transferHTTPSealedRepo) CompareAndSwapProfile(_ context.Context, ref string, old int64, row service.CoreOAuthSealedProfile) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rows[ref].Version != old {
		return false, nil
	}
	r.rows[ref] = row
	return true, nil
}

type transferHTTPProtocol struct {
	grant     service.CoreOAuthGrant
	refreshes atomic.Int64
}

func transferHTTPJWT(claims map[string]any) string {
	payload, err := json.Marshal(claims)
	if err != nil {
		panic("synthetic JWT claims must marshal")
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func (*transferHTTPProtocol) Begin(context.Context, service.CoreOAuthTransport) (*service.CoreOAuthProtocolAttempt, error) {
	return nil, errors.New("unexpected authorization")
}
func (*transferHTTPProtocol) ExchangeCallback(context.Context, *service.CoreOAuthProtocolAttempt, string, service.CoreOAuthIdentity, service.CoreOAuthTransport) (*service.CoreOAuthGrant, error) {
	return nil, errors.New("unexpected code exchange")
}
func (p *transferHTTPProtocol) Refresh(_ context.Context, old service.CoreOAuthGrant, _ service.CoreOAuthTransport) (*service.CoreOAuthGrant, error) {
	if old.RefreshToken != p.grant.RefreshToken || old.ClientID != p.grant.ClientID || old.Subject != p.grant.Subject || old.WorkspaceID != p.grant.WorkspaceID {
		return nil, errors.New("imported grant mismatch")
	}
	p.refreshes.Add(1)
	next := p.grant
	next.AccessToken, next.RefreshToken = "synthetic-http-access-rotated", "synthetic-http-refresh-rotated"
	return &next, nil
}

type transferHTTPAccounts struct {
	account *service.Account
	fence   *service.ManagedOAuthPublicationFence
	reads   atomic.Int64
}

func (a *transferHTTPAccounts) GetAccount(_ context.Context, id int64) (*service.Account, error) {
	if a.account.ID != id {
		return nil, errors.New("unknown synthetic account")
	}
	a.reads.Add(1)
	return a.account, nil
}
func (a *transferHTTPAccounts) GetManagedOAuthPublicationFence(_ context.Context, id int64) (*service.ManagedOAuthPublicationFence, error) {
	if a.fence == nil || a.fence.AccountID != id {
		return nil, service.ErrManagedOAuthFenceMissing
	}
	return a.fence, nil
}
func (*transferHTTPAccounts) FenceManagedOAuthPublication(context.Context, int64, *service.ManagedOAuthPublicationFenceInput) (*service.ManagedOAuthPublicationFence, error) {
	return nil, errors.New("receiver verification must be read only")
}

type transferHTTPUpstream struct{ client *http.Client }

func (u transferHTTPUpstream) Do(r *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
	if proxy != "" || id != 41 || concurrency != 1 || !service.HTTPUpstreamRedirectsDisabled(r.Context()) {
		return nil, errors.New("untrusted synthetic proof transport")
	}
	return u.client.Do(r)
}
func (u transferHTTPUpstream) DoWithTLS(r *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(r, proxy, id, concurrency)
}

// This is actual loopback HTTP and production source/receiver OAuth services.
// The source vault is synthetic AES-GCM storage and the receiver repository is
// encrypted in-memory; no real OAuth, database, Keychain, or upstream is used.
func TestCoreOAuthTransferProductionClientHTTPIntegration(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for cross HTTP interoperability")
	}
	runner := transferHTTPNodeRunner(t)
	for _, provider := range []string{service.CoreOAuthProviderCodex, service.CoreOAuthProviderExcel} {
		t.Run(provider, func(t *testing.T) {
			cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}
			cfg.Totp.EncryptionKey = strings.Repeat("ab", 32)
			encryptor, err := repository.NewAESEncryptor(cfg)
			require.NoError(t, err)
			store := &transferHTTPSealedRepo{rows: map[string]service.CoreOAuthSealedProfile{}}
			expiresAt := time.Now().UTC().Add(time.Hour)
			protocol := &transferHTTPProtocol{grant: service.CoreOAuthGrant{RefreshToken: "synthetic-http-refresh-original",
				ClientID: "app_EMoamEEZ73f0CkXaXp7hrann", Subject: "http-subject", WorkspaceID: "http-workspace", Email: "synthetic-http@example.invalid", ExpiresAt: expiresAt}}
			if provider == service.CoreOAuthProviderExcel {
				protocol.grant.ClientID = "app_fnr0pYvVwwFDocDumLG3H2Bp"
			}
			protocol.grant.AccessToken = transferHTTPJWT(map[string]any{"sub": protocol.grant.Subject,
				"exp": expiresAt.Unix(), "client_id": protocol.grant.ClientID,
				"https://api.openai.com/auth":    map[string]string{"chatgpt_account_id": protocol.grant.WorkspaceID},
				"https://api.openai.com/profile": map[string]string{"email": protocol.grant.Email}})
			protocol.grant.IDToken = transferHTTPJWT(map[string]any{"sub": protocol.grant.Subject, "aud": protocol.grant.ClientID,
				"client_id": protocol.grant.ClientID, "exp": expiresAt.Unix(), "email": protocol.grant.Email,
				"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": protocol.grant.WorkspaceID}})
			newCore := func() *service.CoreOAuthService {
				core, err := service.NewCoreOAuthService(store, encryptor, map[string]service.CoreOAuthProtocol{provider: protocol}, nil)
				require.NoError(t, err)
				return core
			}
			core := newCore()
			profile, err := core.CreateProfile(context.Background(), service.CoreOAuthCreateInput{Provider: provider, Name: "HTTP transfer receiver"})
			if err != nil {
				t.Fatal(err)
			}
			const operation = "11111111-1111-4111-8111-111111111111"
			id := service.ManagedOAuthIdentity{Contract: service.ManagedOAuthContract, BackendID: "http-source-backend", BindingRef: "bind_44444444-4444-4444-8444-444444444444",
				ProfileRef: "oauth_33333333-3333-4333-8333-333333333333", Provider: provider, Subject: protocol.grant.Subject, WorkspaceID: protocol.grant.WorkspaceID, IdentityEpoch: "22222222-2222-4222-8222-222222222222"}
			account := &service.Account{ID: 41, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Concurrency: 1,
				Extra: map[string]any{service.ManagedOAuthExtraKey: service.ManagedOAuthState{ManagedOAuthIdentity: id, TokenRevision: 7, PublicationFenceOperationID: operation}}}
			fence := service.ManagedOAuthPublicationFence{ManagedOAuthPublicationFenceInput: service.ManagedOAuthPublicationFenceInput{OperationID: operation, Identity: id, TokenRevision: 7}, AccountID: 41, Fenced: true}
			accounts := &transferHTTPAccounts{account: account, fence: &fence}
			var receipt any = fence
			var upstream service.HTTPUpstream
			var proofReads atomic.Int64
			if provider == service.CoreOAuthProviderExcel {
				bpsReceipt := service.BPSPublicationFenceReceipt{BPSPublicationIdentity: service.BPSPublicationIdentity{ManagedOAuthIdentity: id, TokenRevision: 7}, OperationID: operation}
				receipt = bpsReceipt
				proofServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/internal/bps-files/v1/publication-fence" || r.Method != http.MethodGet || r.Header.Get(managedfiles.ServiceKeyHeader) != "synthetic-http-files-service-key-00000000" || r.Header.Get("Authorization") != "" {
						http.Error(w, "invalid proof request", http.StatusUnauthorized)
						return
					}
					proofReads.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(service.BPSPublicationFenceProof{Contract: service.BPSPublicationFenceProofContract, Identity: bpsReceipt.BPSPublicationIdentity, Fenced: true, Receipt: &bpsReceipt})
				}))
				t.Cleanup(proofServer.Close)
				httpClient := proofServer.Client()
				httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
				upstream = transferHTTPUpstream{client: httpClient}
				account.Type = service.AccountTypeAPIKey
				account.Extra = map[string]any{"openai_responses_mode": "force_responses", service.ManagedDeploymentAccountExtraKey: service.ManagedDeploymentBinding{
					Provider: provider, AccountID: 41, BackendID: id.BackendID, BindingRef: id.BindingRef, ProfileRef: id.ProfileRef, IdentityEpoch: id.IdentityEpoch, Subject: id.Subject, WorkspaceID: id.WorkspaceID}}
				account.Credentials = map[string]any{"base_url": proofServer.URL + "/v1", "api_key": "synthetic-http-inference-key-000000000",
					"openai_capabilities": []string{"chat_completions", "bps_files"}, "bps_files_contract": managedfiles.Contract, "bps_files_control_url": proofServer.URL,
					"bps_files_service_key": "synthetic-http-files-service-key-00000000", "bps_files_backend_id": id.BackendID,
					"bps_files_subject_fingerprint": "sha256:" + strings.Repeat("a", 64), "bps_files_credential_generation": id.IdentityEpoch}
			}
			h := NewCoreOAuthHandler(core)
			h.SetHandoffVerifier(newCoreOAuthHandoffVerifier(accounts, upstream, cfg))
			gin.SetMode(gin.TestMode)
			router := gin.New()
			const adminKey = "synthetic-http-admin-key-000000000000"
			router.Use(func(c *gin.Context) {
				if c.GetHeader("Authorization") != "Bearer "+adminKey {
					c.AbortWithStatus(http.StatusUnauthorized)
					return
				}
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 42})
				c.Set(string(middleware.ContextKeyUserRole), "admin")
				c.Next()
			})
			base := "/api/v1/admin/core-oauth/profiles/:ref"
			router.POST(base+"/handoff-invitations", h.CreateHandoffInvitation)
			router.POST(base+"/handoffs/accept", h.AcceptProtectedHandoff)
			router.GET(base+"/handoffs/:operation_id", h.GetHandoff)
			receiver := httptest.NewServer(router)
			t.Cleanup(receiver.Close)
			g := protocol.grant
			fixture := map[string]any{"provider": provider, "config": map[string]any{"baseUrl": receiver.URL, "adminApiKey": adminKey},
				"profileRef": id.ProfileRef, "destinationRef": profile.ProfileRef, "operationId": operation, "bindingRef": id.BindingRef, "remoteAccountId": 41, "receipt": receipt,
				"identity": map[string]any{"identityEpoch": id.IdentityEpoch, "tokenRevision": 7, "subject": id.Subject, "workspaceId": id.WorkspaceID},
				"grant": map[string]any{"access_token": g.AccessToken, "refresh_token": g.RefreshToken, "id_token": g.IDToken, "client_id": g.ClientID,
					"subject": g.Subject, "workspace_id": g.WorkspaceID, "email": g.Email, "expires_at": g.ExpiresAt.Format(time.RFC3339Nano)}}
			raw, err := json.Marshal(fixture)
			require.NoError(t, err)
			defer clear(raw)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, node, runner)
			cmd.Stdin = bytes.NewReader(raw)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			output, err := cmd.Output()
			if err != nil {
				var safe struct {
					Stage   string `json:"stage"`
					Failure string `json:"failure"`
				}
				allowedStages := map[string]bool{"read_fixture": true, "prepare_storage": true, "seal_source": true, "release_source": true, "unexpected_abort": true, "revoke_source": true, "http_invitation": true, "http_accept": true, "http_receipt": true, "open_client": true, "transfer": true, "assert_accepted": true, "replay": true, "assert_replay": true, "assert_journal": true}
				allowedFailures := map[string]bool{"ERR_ASSERTION": true, "oauth_transfer_invalid": true, "oauth_transfer_conflict": true, "oauth_transfer_storage_failed": true, "oauth_transfer_unconfirmed": true, "oauth_handoff_invitation_invalid": true, "oauth_handoff_identity_conflict": true, "oauth_handoff_publication_unverified": true, "unclassified": true}
				if stderr.Len() < 512 && json.Unmarshal(stderr.Bytes(), &safe) == nil && allowedStages[safe.Stage] && allowedFailures[safe.Failure] {
					t.Fatalf("production Node HTTP transfer failed: stage=%s failure=%s; sensitive output withheld", safe.Stage, safe.Failure)
				}
				t.Fatal("production Node HTTP transfer failed; credential-bearing fixture and subprocess output withheld")
			}
			require.True(t, cmd.ProcessState.Exited())
			require.True(t, cmd.ProcessState.Success())
			var result struct {
				Provider                                                      string                          `json:"provider"`
				Status                                                        string                          `json:"status"`
				Receipt                                                       service.CoreOAuthHandoffReceipt `json:"receipt"`
				SourceCredentialAbsent                                        bool                            `json:"sourceCredentialAbsent"`
				SourceRefreshRejectedAfterRestart                             bool                            `json:"sourceRefreshRejectedAfterRestart"`
				SourcePrepareRejectedAfterRestart                             bool                            `json:"sourcePrepareRejectedAfterRestart"`
				Preparations, Revocations, Seals, Accepts, Released, Requests int
			}
			if json.Unmarshal(output, &result) != nil {
				t.Fatal("invalid safe Node result")
			}
			require.Equal(t, provider, result.Provider)
			require.Equal(t, "accepted", result.Status)
			require.Equal(t, 1, result.Preparations)
			require.Equal(t, 1, result.Revocations)
			require.Equal(t, 1, result.Seals)
			require.Equal(t, 1, result.Accepts)
			require.Equal(t, 1, result.Released)
			require.True(t, result.SourceCredentialAbsent)
			require.True(t, result.SourceRefreshRejectedAfterRestart)
			require.True(t, result.SourcePrepareRejectedAfterRestart)
			require.Equal(t, int64(1), accounts.reads.Load(), "accepted receipt replay must not reverify old authority")
			if provider == service.CoreOAuthProviderExcel {
				require.Equal(t, int64(1), proofReads.Load(), "receiver must verify the saved worker over HTTP")
			}
			require.Zero(t, protocol.refreshes.Load())
			core = newCore()
			snapshot, err := core.CredentialSnapshot(context.Background(), profile.ProfileRef)
			require.NoError(t, err)
			require.True(t, g.AccessToken == snapshot.AccessToken, "received credential mismatch; token contents withheld")
			refreshed, err := core.Refresh(context.Background(), profile.ProfileRef, 7)
			require.NoError(t, err)
			require.Equal(t, int64(8), refreshed.TokenRevision)
			require.Equal(t, int64(1), protocol.refreshes.Load())
			core = newCore()
			snapshot, err = core.CredentialSnapshot(context.Background(), profile.ProfileRef)
			require.NoError(t, err)
			require.True(t, snapshot.AccessToken == "synthetic-http-access-rotated", "refreshed credential mismatch; token contents withheld")
			accepted, err := core.GetHandoff(context.Background(), profile.ProfileRef, operation, 42)
			require.NoError(t, err)
			require.Equal(t, result.Receipt, *accepted)
			for _, secret := range []string{g.AccessToken, g.RefreshToken, g.IDToken, adminKey, "synthetic-http-access-rotated", "synthetic-http-refresh-rotated", "refresh_token", "access_token"} {
				if bytes.Contains(output, []byte(secret)) {
					t.Fatal("safe Node result exposed synthetic credential material")
				}
				store.mu.Lock()
				for _, row := range store.rows {
					if strings.Contains(row.Ciphertext, secret) {
						store.mu.Unlock()
						t.Fatal("receiver persisted unsealed credential material")
					}
				}
				store.mu.Unlock()
			}
		})
	}
}

func transferHTTPNodeRunner(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		file := filepath.Join(dir, "toSub2", "test", "oauth-transfer-http.mjs")
		if info, err := os.Stat(file); err == nil && info.Mode().IsRegular() {
			return file
		} else if err != nil && !os.IsNotExist(err) {
			t.Fatal("cannot inspect sibling Node runner")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Skip("the sibling toSub2 HTTP runner is required for integration")
	return ""
}
