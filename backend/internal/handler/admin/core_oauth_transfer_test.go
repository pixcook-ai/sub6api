package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
	"github.com/MACOS-DO/sub4api/internal/pkg/tlsfingerprint"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type coreOAuthTransferSpy struct {
	coreOAuthAdminService
	actor      int64
	calls      int
	invitation service.CoreOAuthHandoffInvitationInput
}

func (s *coreOAuthTransferSpy) CreateHandoffInvitation(_ context.Context, _ string, in service.CoreOAuthHandoffInvitationInput) (*service.CoreOAuthHandoffInvitation, error) {
	s.actor, s.invitation = in.ActorID, in
	s.calls++
	return &service.CoreOAuthHandoffInvitation{}, nil
}
func (s *coreOAuthTransferSpy) AcceptProtectedHandoff(ctx context.Context, _ string, actor int64, _ service.CoreOAuthHandoffEnvelope, verifier service.CoreOAuthHandoffVerifier) (*service.CoreOAuthHandoffReceipt, error) {
	s.actor = actor
	s.calls++
	if err := verifier(ctx, service.CoreOAuthHandoffVerification{}); err != nil {
		return nil, err
	}
	return &service.CoreOAuthHandoffReceipt{Status: "accepted"}, nil
}
func (s *coreOAuthTransferSpy) GetHandoff(_ context.Context, _, _ string, actor int64) (*service.CoreOAuthHandoffReceipt, error) {
	s.actor = actor
	s.calls++
	return &service.CoreOAuthHandoffReceipt{Status: "pending"}, nil
}
func (s *coreOAuthTransferSpy) AbandonHandoff(_ context.Context, _, _ string, actor int64) (*service.CoreOAuthHandoffReceipt, error) {
	s.actor = actor
	s.calls++
	return &service.CoreOAuthHandoffReceipt{Status: "abandoned"}, nil
}
func coreOAuthTransferJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return raw
}

func TestCoreOAuthProtectedTransferHandlerRequiresTrustedActorAndVerifier(t *testing.T) {
	spy := &coreOAuthTransferSpy{}
	h := &CoreOAuthHandler{core: spy}
	for _, handle := range []func(*gin.Context){h.CreateHandoffInvitation, h.AcceptProtectedHandoff, h.GetHandoff, h.AbandonHandoff} {
		require.Equal(t, http.StatusUnauthorized, coreOAuthHandlerRequest(t, 0, "admin", `{}`, handle).Code)
		require.Equal(t, http.StatusForbidden, coreOAuthHandlerRequest(t, 7, "user", `{}`, handle).Code)
	}
	envelope := `{"operation_id":"operation","wrapped_key":"private-wrapped-key","nonce":"nonce","ciphertext":"private-ciphertext"}`
	w := coreOAuthHandlerRequest(t, 7, "admin", envelope, h.AcceptProtectedHandoff)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Zero(t, spy.calls)
	h.SetHandoffVerifier(func(context.Context, service.CoreOAuthHandoffVerification) error { return nil })
	w = coreOAuthHandlerRequest(t, 7, "admin", envelope, h.AcceptProtectedHandoff)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, int64(7), spy.actor)
	require.NotContains(t, w.Body.String(), "private-")
	for _, body := range []string{
		strings.TrimSuffix(envelope, `}`) + `,"actor_id":999}`,
		strings.Replace(envelope, `"nonce":"nonce"`, `"Nonce":"nonce"`, 1),
		strings.Replace(envelope, `"nonce":"nonce"`, `"nonce":null`, 1),
		strings.TrimSuffix(envelope, `}`) + `,"nonce":"duplicate"}`,
		strings.Replace(envelope, "private-ciphertext", strings.Repeat("x", 256<<10), 1),
	} {
		before := spy.calls
		w = coreOAuthHandlerRequest(t, 7, "admin", body, h.AcceptProtectedHandoff)
		require.Equal(t, http.StatusBadRequest, w.Code)
		require.Equal(t, before, spy.calls)
		require.NotContains(t, w.Body.String(), "private-")
	}
	invitation := `{"operation_id":"op","profile_ref":"example","source_profile_ref":"source","provider":"codex","subject":"subject","workspace_id":"workspace","identity_epoch":"epoch","token_revision":3,"expires_at":"2030-01-01T00:00:00Z"}`
	w = coreOAuthHandlerRequest(t, 17, "admin", invitation, h.CreateHandoffInvitation)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, int64(17), spy.invitation.ActorID)
	before := spy.calls
	w = coreOAuthHandlerRequest(t, 17, "admin", strings.TrimSuffix(invitation, `}`)+`,"actor_id":999}`, h.CreateHandoffInvitation)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Equal(t, before, spy.calls)
	for _, handle := range []func(*gin.Context){h.GetHandoff, h.AbandonHandoff} {
		require.Equal(t, http.StatusOK, coreOAuthHandlerRequest(t, 17, "admin", `{}`, handle).Code)
		require.Equal(t, int64(17), spy.actor)
	}
}

type coreOAuthProofAdmin struct {
	accounts map[int64]*service.Account
	fences   map[int64]*service.ManagedOAuthPublicationFence
	readErr  error
	reads    int
}

func (a *coreOAuthProofAdmin) GetAccount(_ context.Context, id int64) (*service.Account, error) {
	return a.accounts[id], a.readErr
}
func (a *coreOAuthProofAdmin) GetManagedOAuthPublicationFence(_ context.Context, id int64) (*service.ManagedOAuthPublicationFence, error) {
	a.reads++
	return a.fences[id], a.readErr
}
func (a *coreOAuthProofAdmin) FenceManagedOAuthPublication(context.Context, int64, *service.ManagedOAuthPublicationFenceInput) (*service.ManagedOAuthPublicationFence, error) {
	panic("proof verifier must never write a source fence")
}

func coreOAuthCodexProofFixture(t *testing.T) (*coreOAuthProofAdmin, service.CoreOAuthHandoffVerification, service.ManagedOAuthPublicationFence) {
	t.Helper()
	invitation := service.CoreOAuthHandoffInvitation{}
	require.NoError(t, json.Unmarshal([]byte(`{"operation_id":"`+uuid.NewString()+`","profile_ref":"`+uuid.NewString()+`","source_profile_ref":"oauth_`+uuid.NewString()+`","provider":"codex","subject":"subject","workspace_id":"workspace","identity_epoch":"`+uuid.NewString()+`","token_revision":4}`), &invitation))
	identity := service.ManagedOAuthIdentity{Contract: service.ManagedOAuthContract, BackendID: "source-backend", BindingRef: "bind_" + uuid.NewString(),
		ProfileRef: invitation.SourceProfileRef, Provider: invitation.Provider, Subject: invitation.Subject, WorkspaceID: invitation.WorkspaceID, IdentityEpoch: invitation.IdentityEpoch}
	fence := service.ManagedOAuthPublicationFence{ManagedOAuthPublicationFenceInput: service.ManagedOAuthPublicationFenceInput{OperationID: invitation.OperationID, Identity: identity, TokenRevision: 4}, AccountID: 41, Fenced: true}
	account := &service.Account{ID: 41, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Extra: map[string]any{service.ManagedOAuthExtraKey: service.ManagedOAuthState{ManagedOAuthIdentity: identity, TokenRevision: 4, PublicationFenceOperationID: invitation.OperationID}}}
	a := &coreOAuthProofAdmin{accounts: map[int64]*service.Account{41: account}, fences: map[int64]*service.ManagedOAuthPublicationFence{41: &fence}}
	verification := service.CoreOAuthHandoffVerification{Invitation: invitation, SourceQuiesced: true, PublicationFences: []json.RawMessage{coreOAuthTransferJSON(t, coreOAuthHandoffPublicationProof{Kind: "codex_controller", AccountID: 41, Receipt: coreOAuthTransferJSON(t, fence)})}}
	return a, verification, fence
}

func TestCoreOAuthHandoffVerifierRequiresCompleteLiveCodexFences(t *testing.T) {
	a, verification, _ := coreOAuthCodexProofFixture(t)
	require.NoError(t, newCoreOAuthHandoffVerifier(a, nil, nil)(context.Background(), verification))
	require.Equal(t, 1, a.reads)
	for _, tc := range []string{"not quiesced", "empty", "duplicate account", "second proof unknown account", "wrong revision", "wrong provider", "wrong live operation", "saved identity changed", "live unavailable", "extra URL"} {
		t.Run(tc, func(t *testing.T) {
			a, v, fence := coreOAuthCodexProofFixture(t)
			switch tc {
			case "not quiesced":
				v.SourceQuiesced = false
			case "empty":
				v.PublicationFences = nil
			case "duplicate account":
				v.PublicationFences = append(v.PublicationFences, v.PublicationFences[0])
			case "second proof unknown account":
				v.PublicationFences = append(v.PublicationFences, coreOAuthTransferJSON(t, coreOAuthHandoffPublicationProof{Kind: "codex_controller", AccountID: 999, Receipt: coreOAuthTransferJSON(t, fence)}))
			case "wrong revision":
				v.Invitation.TokenRevision++
			case "wrong provider":
				v.Invitation.Provider = service.CoreOAuthProviderExcel
			case "wrong live operation":
				changed := fence
				changed.OperationID = uuid.NewString()
				a.fences[41] = &changed
			case "saved identity changed":
				a.accounts[41].Extra[service.ManagedOAuthExtraKey] = service.ManagedOAuthState{}
			case "live unavailable":
				a.readErr = errors.New("secret-upstream-body")
			case "extra URL":
				v.PublicationFences[0] = append(append(json.RawMessage{}, v.PublicationFences[0][:len(v.PublicationFences[0])-1]...), []byte(`,"url":"http://untrusted.invalid","key":"secret"}`)...)
			}
			err := newCoreOAuthHandoffVerifier(a, nil, nil)(context.Background(), v)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "secret")
		})
	}
	// Two locally saved accounts cannot repeat the same source binding.
	a, verification, fence := coreOAuthCodexProofFixture(t)
	copyAccount := *a.accounts[41]
	copyAccount.ID = 42
	a.accounts[42] = &copyAccount
	fence.AccountID = 42
	a.fences[42] = &fence
	verification.PublicationFences = append(verification.PublicationFences, coreOAuthTransferJSON(t, coreOAuthHandoffPublicationProof{Kind: "codex_controller", AccountID: 42, Receipt: coreOAuthTransferJSON(t, fence)}))
	require.ErrorIs(t, newCoreOAuthHandoffVerifier(a, nil, nil)(context.Background(), verification), service.ErrCoreOAuthConflict)
}

type coreOAuthProofUpstream struct {
	proof service.BPSPublicationFenceProof
	proxy string
	calls int
	t     *testing.T
}

func (u *coreOAuthProofUpstream) Do(req *http.Request, proxy string, accountID int64, concurrency int) (*http.Response, error) {
	u.calls++
	require.Equal(u.t, "https://saved-worker.example/internal/bps-files/v1/publication-fence", req.URL.String())
	require.Equal(u.t, "saved-files-service-key", req.Header.Get(managedfiles.ServiceKeyHeader))
	require.Equal(u.t, int64(41), accountID)
	require.Equal(u.t, 1, concurrency)
	require.Equal(u.t, u.proxy, proxy)
	require.True(u.t, service.HTTPUpstreamRedirectsDisabled(req.Context()))
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(coreOAuthTransferJSON(u.t, u.proof))))}, nil
}
func (u *coreOAuthProofUpstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, concurrency)
}

func TestCoreOAuthHandoffVerifierBPSUsesOnlySavedOutboundConfiguration(t *testing.T) {
	a, v, codexFence := coreOAuthCodexProofFixture(t)
	v.Invitation.Provider = service.CoreOAuthProviderExcel
	identity := codexFence.Identity
	identity.Provider = service.CoreOAuthProviderExcel
	receipt := service.BPSPublicationFenceReceipt{BPSPublicationIdentity: service.BPSPublicationIdentity{ManagedOAuthIdentity: identity, TokenRevision: v.Invitation.TokenRevision}, OperationID: v.Invitation.OperationID}
	account := a.accounts[41]
	account.Type, account.Concurrency = service.AccountTypeAPIKey, 1
	account.Extra = map[string]any{
		"openai_responses_mode":                  "force_responses",
		service.ManagedDeploymentAccountExtraKey: service.ManagedDeploymentBinding{Provider: identity.Provider, AccountID: account.ID, BackendID: identity.BackendID, BindingRef: identity.BindingRef, ProfileRef: identity.ProfileRef, IdentityEpoch: identity.IdentityEpoch, WorkspaceID: identity.WorkspaceID, Subject: identity.Subject},
	}
	account.Credentials = map[string]any{"base_url": "https://saved-worker.example/v1", "api_key": "saved-inference-key",
		"openai_capabilities": []string{"chat_completions", "responses", "bps_files"},
		"bps_files_contract":  managedfiles.Contract, "bps_files_control_url": "https://saved-worker.example", "bps_files_service_key": "saved-files-service-key",
		"bps_files_backend_id": identity.BackendID, "bps_files_subject_fingerprint": "sha256:" + strings.Repeat("a", 64), "bps_files_credential_generation": identity.IdentityEpoch}
	v.PublicationFences = []json.RawMessage{coreOAuthTransferJSON(t, coreOAuthHandoffPublicationProof{Kind: "bps_worker", AccountID: 41, Receipt: coreOAuthTransferJSON(t, receipt)})}
	upstream := &coreOAuthProofUpstream{t: t, proof: service.BPSPublicationFenceProof{Contract: service.BPSPublicationFenceProofContract, Identity: receipt.BPSPublicationIdentity, Fenced: true, Receipt: &receipt}}
	cfg := &config.Config{}
	verify := newCoreOAuthHandoffVerifier(a, upstream, cfg)
	_, err := service.BPSFilesConfigForAccount(account)
	require.NoError(t, err, "the saved account fixture must be eligible for BPS control access")
	require.NoError(t, verify(context.Background(), v))
	require.Equal(t, 1, upstream.calls)
	proxyID := int64(13)
	for _, tc := range []struct {
		name  string
		id    *int64
		proxy *service.Proxy
	}{
		{name: "unloaded saved proxy", id: &proxyID},
		{name: "mismatched saved proxy", id: &proxyID, proxy: &service.Proxy{ID: 14}},
		{name: "unsaved proxy relation", proxy: &service.Proxy{ID: 13}},
		{name: "invalid saved proxy ID", id: new(int64), proxy: &service.Proxy{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account.ProxyID, account.Proxy = tc.id, tc.proxy
			before := upstream.calls
			require.ErrorIs(t, verify(context.Background(), v), service.ErrCoreOAuthConflict)
			require.Equal(t, before, upstream.calls, "invalid saved proxy association must stop before outbound access")
		})
	}
	account.ProxyID = &proxyID
	account.Proxy = &service.Proxy{ID: proxyID, Protocol: "http", Host: "saved-proxy.example", Port: 8080}
	upstream.proxy = "http://saved-proxy.example:8080"
	before := upstream.calls
	require.NoError(t, verify(context.Background(), v))
	require.Equal(t, before+1, upstream.calls, "the matching saved proxy must be passed to the upstream")
	account.ProxyID, account.Proxy, upstream.proxy = nil, nil, ""
	changed := receipt
	changed.OperationID = uuid.NewString()
	upstream.proof.Receipt = &changed
	require.ErrorIs(t, verify(context.Background(), v), service.ErrCoreOAuthConflict)
	upstream.proof.Receipt = &receipt
	account.Extra[service.ManagedDeploymentAccountExtraKey] = service.ManagedDeploymentBinding{}
	before = upstream.calls
	require.ErrorIs(t, verify(context.Background(), v), service.ErrCoreOAuthConflict)
	require.Equal(t, before, upstream.calls, "a changed saved binding must stop before outbound access")
	require.ErrorIs(t, newCoreOAuthHandoffVerifier(nil, upstream, cfg)(context.Background(), v), service.ErrCoreOAuthConflict)
}
