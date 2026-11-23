package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type coreOAuthAttemptContextKey struct{}
type coreOAuthAttempt struct {
	AccountID     int64
	ProfileRef    string
	IdentityEpoch string
	TokenRevision int64
	AccessToken   string
}

func coreOAuthAttemptFromRequest(r *http.Request) *coreOAuthAttempt {
	if r == nil {
		return nil
	}
	v, _ := r.Context().Value(coreOAuthAttemptContextKey{}).(*coreOAuthAttempt)
	return v
}

func withCoreOAuthAttempt(ctx context.Context, attempt *coreOAuthAttempt) context.Context {
	return context.WithValue(ctx, coreOAuthAttemptContextKey{}, attempt)
}

func (s *OpenAIGatewayService) SetCoreOAuthService(core *CoreOAuthService) {
	if s == nil {
		return
	}
	s.coreOAuth = core
}

type CoreOAuthRuntimeAccountUpdate struct {
	Expected    *Account
	Credentials map[string]any
	Extra       map[string]any
}

type CoreOAuthRuntimeInstallRepository interface {
	InstallCoreOAuthRuntime(context.Context, CoreOAuthRuntimeAccountUpdate, CoreOAuthRuntimeAccountUpdate, *ManagedDeploymentUpdate) (*ManagedDeployment, error)
}

type coreOAuthRuntimeSnapshotRefresher interface {
	RefreshSchedulerAccount(context.Context, int64)
}

func coreOAuthRuntimeUUID(parts ...string) string {
	raw, _ := json.Marshal(parts)
	sum := sha256.Sum256(raw)
	var id uuid.UUID
	copy(id[:], sum[:16])
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return id.String()
}

func coreOAuthCopyMap(source map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range source {
		out[key] = value
	}
	return out
}

func coreOAuthDeploymentBinding(dID string, a *Account, snapshot *CoreOAuthCredentialSnapshot, backend string) ManagedDeploymentBinding {
	p := snapshot.Profile
	return ManagedDeploymentBinding{Provider: p.Provider, AccountID: a.ID, BackendID: backend, BindingRef: "bind_" + coreOAuthRuntimeUUID("core-runtime-binding/v1", dID, p.ProfileRef, strconv.FormatInt(a.ID, 10)), ProfileRef: "oauth_" + p.ProfileRef, Subject: p.Subject, WorkspaceID: p.WorkspaceID, IdentityEpoch: p.IdentityEpoch}
}

func coreOAuthBindingMatchesProfile(binding ManagedDeploymentBinding, profile CoreOAuthProfile) bool {
	return binding.Provider == profile.Provider && binding.ProfileRef == "oauth_"+profile.ProfileRef && binding.Subject == profile.Subject && binding.WorkspaceID == profile.WorkspaceID && binding.IdentityEpoch == profile.IdentityEpoch
}

func coreOAuthRuntimeBinding(a *Account) (ManagedDeploymentBinding, error) {
	var binding ManagedDeploymentBinding
	if a == nil {
		return binding, ErrCoreOAuthConflict
	}
	raw, err := json.Marshal(a.Extra[ManagedDeploymentAccountExtraKey])
	if err != nil || json.Unmarshal(raw, &binding) != nil {
		return binding, ErrCoreOAuthConflict
	}
	binding.AccountID = a.ID
	return binding, binding.Validate()
}

func validateCoreOAuthRuntimeOwnership(a *Account, binding ManagedDeploymentBinding, snapshot *CoreOAuthCredentialSnapshot) error {
	marker, err := CoreOAuthAccountBindingFromAccount(a)
	if err != nil || a.ID != binding.AccountID || !coreOAuthBindingMatchesProfile(binding, snapshot.Profile) || "oauth_"+marker.ProfileRef != binding.ProfileRef || marker.Provider != binding.Provider || marker.Subject != binding.Subject || marker.WorkspaceID != binding.WorkspaceID || marker.IdentityEpoch != binding.IdentityEpoch || marker.TokenRevision > snapshot.Profile.TokenRevision {
		return ErrCoreOAuthConflict
	}
	var stored ManagedDeploymentBinding
	raw, _ := json.Marshal(a.Extra[ManagedDeploymentAccountExtraKey])
	if json.Unmarshal(raw, &stored) != nil {
		return ErrCoreOAuthConflict
	}
	stored.AccountID = a.ID
	if stored != binding {
		return ErrCoreOAuthConflict
	}
	return nil
}

func (s *OpenAIGatewayService) coreOAuthCurrentSnapshot(ctx context.Context, a *Account) (ManagedDeploymentBinding, *CoreOAuthCredentialSnapshot, error) {
	binding, err := coreOAuthRuntimeBinding(a)
	if err != nil || s == nil || s.coreOAuth == nil || s.accountRepo == nil {
		return binding, nil, ErrCoreOAuthUnavailable
	}
	marker, err := CoreOAuthAccountBindingFromAccount(a)
	if err != nil {
		return binding, nil, err
	}
	current, err := s.accountRepo.GetByID(ctx, a.ID)
	if err != nil || current == nil || !current.IsSchedulable() || ValidateManagedDeploymentAccount(current, binding) != nil {
		return binding, nil, ErrCoreOAuthUnavailable
	}
	if session, ok := ManagedDeploymentSessionFromContext(ctx); !ok || session.Binding != binding || !openAIStickyAccountMatchesGroup(current, &session.GroupID) {
		return binding, nil, ErrManagedDeploymentContext
	}
	snapshot, err := s.coreOAuth.CredentialSnapshot(ctx, marker.ProfileRef)
	if err != nil {
		return binding, nil, err
	}
	if err = validateCoreOAuthRuntimeOwnership(current, binding, snapshot); err != nil {
		return binding, nil, err
	}
	return binding, snapshot, nil
}

func (s *OpenAIGatewayService) prepareCoreOAuthRuntimeRequest(request *http.Request, account *Account) (*http.Request, ManagedDeploymentBinding, error) {
	var binding ManagedDeploymentBinding
	if !CoreOAuthDeclared(account) {
		return request, binding, nil
	}
	binding, snapshot, err := s.coreOAuthCurrentSnapshot(request.Context(), account)
	if err != nil {
		return nil, binding, err
	}
	request = request.Clone(WithHTTPUpstreamRedirectsDisabled(request.Context()))
	if binding.Provider == CoreOAuthProviderExcel {
		configured, err := s.configureCoreOAuthWorkerAccount(account, "", "")
		if err != nil {
			return nil, binding, err
		}
		if err = s.publishCoreOAuthWorker(request.Context(), configured, binding, snapshot); err != nil {
			return nil, binding, err
		}
	} else {
		request.Header.Set("Authorization", "Bearer "+snapshot.AccessToken)
		request = request.WithContext(withCoreOAuthAttempt(request.Context(), &coreOAuthAttempt{AccountID: account.ID, ProfileRef: snapshot.Profile.ProfileRef, IdentityEpoch: snapshot.Profile.IdentityEpoch, TokenRevision: snapshot.Profile.TokenRevision, AccessToken: snapshot.AccessToken}))
	}
	return request, binding, nil
}

func (s *OpenAIGatewayService) recoverCoreOAuthRuntimeUnauthorized(request *http.Request, account *Account, binding ManagedDeploymentBinding) {
	if s == nil || s.coreOAuth == nil || request.Context().Err() != nil {
		return
	}
	if binding.Provider == CoreOAuthProviderExcel {
		configured, err := s.configureCoreOAuthWorkerAccount(account, "", "")
		if err != nil {
			return
		}
		events, err := s.coreOAuthWorkerUnauthorized(request.Context(), configured, binding)
		if err != nil {
			return
		}
		// Core CAS ignores stale versions; use the newest verified event first.
		for i := len(events) - 1; i >= 0; i-- {
			if _, err = s.coreOAuth.RecoverUnauthorized(request.Context(), events[i]); err != nil {
				return
			}
		}
		return
	}
	attempt := coreOAuthAttemptFromRequest(request)
	if attempt == nil || attempt.AccountID != account.ID || request.Header.Get("Authorization") != "Bearer "+attempt.AccessToken {
		return
	}
	_, _ = s.coreOAuth.RecoverUnauthorized(request.Context(), CoreOAuthUnauthorizedInput{ProfileRef: attempt.ProfileRef, IdentityEpoch: attempt.IdentityEpoch, FailedRevision: attempt.TokenRevision, StatusCode: http.StatusUnauthorized, Origin: "upstream_oauth"})
}

func prepareCoreOAuthRuntimeUpdate(a *Account, binding ManagedDeploymentBinding, snapshot *CoreOAuthCredentialSnapshot) (CoreOAuthRuntimeAccountUpdate, error) {
	if a == nil || snapshot == nil || !coreOAuthBindingMatchesProfile(binding, snapshot.Profile) || a.Platform != PlatformOpenAI || a.ParentAccountID != nil || ManagedOAuthDeclared(a) {
		return CoreOAuthRuntimeAccountUpdate{}, ErrCoreOAuthConflict
	}
	for _, key := range []string{"refresh_token", "id_token"} {
		if _, exists := a.Credentials[key]; exists {
			return CoreOAuthRuntimeAccountUpdate{}, ErrCoreOAuthConflict
		}
	}
	if (binding.Provider == "codex" && a.Type != AccountTypeOAuth) || (binding.Provider == "bps_excel" && a.Type != AccountTypeAPIKey) {
		return CoreOAuthRuntimeAccountUpdate{}, ErrCoreOAuthConflict
	}
	if CoreOAuthDeclared(a) {
		if err := validateCoreOAuthRuntimeOwnership(a, binding, snapshot); err != nil {
			return CoreOAuthRuntimeAccountUpdate{}, err
		}
	} else {
		if _, declared := a.Extra[ManagedDeploymentAccountExtraKey]; declared {
			return CoreOAuthRuntimeAccountUpdate{}, ErrCoreOAuthConflict
		}
		shell, shellErr := CoreOAuthRuntimeShellProvider(a)
		if shellErr != nil || shell != binding.Provider || a.Schedulable {
			return CoreOAuthRuntimeAccountUpdate{}, ErrCoreOAuthConflict
		}
		if strings.TrimSpace(a.GetCredential("access_token")) != "" || (binding.Provider == "codex" && strings.TrimSpace(a.GetCredential("api_key")) != "") {
			return CoreOAuthRuntimeAccountUpdate{}, ErrCoreOAuthConflict
		}
	}
	marker, err := CoreOAuthAccountBindingForSnapshot(snapshot)
	if err != nil {
		return CoreOAuthRuntimeAccountUpdate{}, err
	}
	credentials, extra := coreOAuthCopyMap(a.Credentials), coreOAuthCopyMap(a.Extra)
	extra[CoreOAuthAccountExtraKey] = *marker
	delete(extra, CoreOAuthRuntimeShellExtraKey)
	declared := binding
	declared.AccountID = 0
	encoded, _ := json.Marshal(declared)
	var bindingMap map[string]any
	_ = json.Unmarshal(encoded, &bindingMap)
	delete(bindingMap, "account_id")
	extra[ManagedDeploymentAccountExtraKey] = bindingMap
	if binding.Provider == "codex" {
		credentials["access_token"] = snapshot.AccessToken
		credentials["expires_at"] = snapshot.Profile.ExpiresAt.UTC().Format(time.RFC3339Nano)
		credentials["chatgpt_account_id"] = snapshot.Profile.WorkspaceID
		credentials["client_id"] = snapshot.Profile.ClientID
		extra["openai_oauth_responses_websockets_v2_enabled"] = false
		extra["openai_oauth_responses_websockets_v2_mode"] = "off"
	}
	return CoreOAuthRuntimeAccountUpdate{Expected: a, Credentials: credentials, Extra: extra}, nil
}

func CoreOAuthRuntimeShellProvider(account *Account) (string, error) {
	if account == nil || account.Extra == nil {
		return "", ErrCoreOAuthInvalid
	}
	raw, exists := account.Extra[CoreOAuthRuntimeShellExtraKey]
	if !exists {
		return "", ErrCoreOAuthInvalid
	}
	data, err := json.Marshal(raw)
	if err != nil || len(data) > 2048 {
		return "", ErrCoreOAuthInvalid
	}
	var shell struct {
		Provider string `json:"provider"`
		GroupID  int64  `json:"group_id"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&shell) != nil || decoder.Decode(new(any)) != io.EOF || (shell.Provider != CoreOAuthProviderCodex && shell.Provider != CoreOAuthProviderExcel) || shell.GroupID <= 0 || !openAIStickyAccountMatchesGroup(account, &shell.GroupID) {
		return "", ErrCoreOAuthInvalid
	}
	if account.Platform != PlatformOpenAI || account.ParentAccountID != nil || ManagedOAuthDeclared(account) {
		return "", ErrCoreOAuthConflict
	}
	if (shell.Provider == CoreOAuthProviderCodex && account.Type != AccountTypeOAuth) || (shell.Provider == CoreOAuthProviderExcel && account.Type != AccountTypeAPIKey) {
		return "", ErrCoreOAuthConflict
	}
	return shell.Provider, nil
}

type CoreOAuthRuntimeInstallRequest struct {
	GroupID               int64                      `json:"group_id"`
	BPSProfileRef         string                     `json:"bps_profile_ref"`
	CodexProfileRef       string                     `json:"codex_profile_ref"`
	BPSAccountID          int64                      `json:"bps_account_id"`
	CodexAccountID        int64                      `json:"codex_account_id"`
	BPSBackendID          string                     `json:"bps_backend_id"`
	BPSControlURL         string                     `json:"bps_control_url"`
	BPSControlKey         string                     `json:"bps_control_key"`
	ExpectedRouteRevision int64                      `json:"expected_route_revision"`
	SelectedProvider      string                     `json:"selected_provider"`
	RequestedModel        string                     `json:"requested_model"`
	RequiredCapabilities  []OpenAIEndpointCapability `json:"required_capabilities"`
	ScheduleNewSessions   bool                       `json:"schedule_new_sessions"`
}

func (s *OpenAIGatewayService) InstallCoreOAuthRuntime(ctx context.Context, id string, in CoreOAuthRuntimeInstallRequest) (*ManagedDeploymentSwitchState, error) {
	if s == nil || s.coreOAuth == nil {
		return nil, ErrCoreOAuthUnavailable
	}
	complete, err := managedDeploymentSwitchContext(in.RequestedModel, in.RequiredCapabilities)
	if err != nil || !complete || !in.ScheduleNewSessions || in.GroupID <= 0 || in.BPSAccountID <= 0 || in.CodexAccountID <= 0 || in.BPSAccountID == in.CodexAccountID || in.ExpectedRouteRevision < 0 || in.ExpectedRouteRevision >= 9007199254740991 || (in.SelectedProvider != "codex" && in.SelectedProvider != "bps_excel") {
		return nil, ErrManagedDeploymentInvalid
	}
	bps, err := s.coreOAuth.CredentialSnapshot(ctx, in.BPSProfileRef)
	if err != nil {
		return nil, err
	}
	codex, err := s.coreOAuth.CredentialSnapshot(ctx, in.CodexProfileRef)
	if err != nil {
		return nil, err
	}
	if bps.Profile.Provider != CoreOAuthProviderExcel || codex.Profile.Provider != CoreOAuthProviderCodex || bps.Profile.Subject != codex.Profile.Subject || bps.Profile.WorkspaceID != codex.Profile.WorkspaceID {
		return nil, ErrCoreOAuthConflict
	}
	bpsAccount, err := s.accountRepo.GetByID(ctx, in.BPSAccountID)
	if err != nil || bpsAccount == nil {
		return nil, ErrManagedDeploymentInvalid
	}
	codexAccount, err := s.accountRepo.GetByID(ctx, in.CodexAccountID)
	if err != nil || codexAccount == nil {
		return nil, ErrManagedDeploymentInvalid
	}
	d := ManagedDeployment{Contract: ManagedDeploymentContract, DeploymentID: id, GroupID: in.GroupID, RouteRevision: in.ExpectedRouteRevision + 1, SelectedProvider: in.SelectedProvider, BPS: coreOAuthDeploymentBinding(id, bpsAccount, bps, in.BPSBackendID), Codex: coreOAuthDeploymentBinding(id, codexAccount, codex, "sub6-core")}
	if d.Validate() != nil {
		return nil, ErrManagedDeploymentInvalid
	}
	repo, err := managedDeploymentRepo(s.accountRepo)
	if err != nil {
		return nil, err
	}
	existing, err := repo.GetManagedDeployment(ctx, id)
	if err != nil {
		return nil, err
	}
	if existing == nil && in.ExpectedRouteRevision != 0 {
		return nil, ErrManagedDeploymentConflict
	}
	if existing != nil && (existing.RouteRevision != in.ExpectedRouteRevision || existing.BPS != d.BPS || existing.Codex != d.Codex || existing.GroupID != in.GroupID) {
		return nil, ErrManagedDeploymentConflict
	}
	selected := codexAccount
	if in.SelectedProvider == CoreOAuthProviderExcel {
		selected = bpsAccount
	}
	selectedForValidation := selected
	if shellProvider, shellErr := CoreOAuthRuntimeShellProvider(selected); shellErr == nil {
		bindingProvider := d.Codex.Provider
		if selected.ID == d.BPS.AccountID {
			bindingProvider = d.BPS.Provider
		}
		if shellProvider != bindingProvider || selected.Schedulable {
			return nil, ErrCoreOAuthConflict
		}
		validated := *selected
		validated.Schedulable = true
		selectedForValidation = &validated
	}
	for _, pair := range []struct {
		account *Account
		binding ManagedDeploymentBinding
	}{{bpsAccount, d.BPS}, {codexAccount, d.Codex}} {
		if shellProvider, shellErr := CoreOAuthRuntimeShellProvider(pair.account); shellErr == nil {
			if shellProvider != pair.binding.Provider || pair.account.Schedulable {
				return nil, ErrCoreOAuthConflict
			}
		} else if !pair.account.Schedulable {
			return nil, ErrCoreOAuthConflict
		}
	}
	if s.managedDeploymentSwitchTargetReason(ctx, &d, selectedForValidation, in.RequestedModel, in.RequiredCapabilities) != "" || !openAIStickyAccountMatchesGroup(bpsAccount, &d.GroupID) || !openAIStickyAccountMatchesGroup(codexAccount, &d.GroupID) {
		return nil, ErrManagedDeploymentUnavailable
	}
	// Validate both before publishing either snapshot or mutating either runtime account.
	if _, err = prepareCoreOAuthRuntimeUpdate(bpsAccount, d.BPS, bps); err != nil {
		return nil, err
	}
	if _, err = prepareCoreOAuthRuntimeUpdate(codexAccount, d.Codex, codex); err != nil {
		return nil, err
	}
	originalBPS := bpsAccount
	bpsAccount, err = s.configureCoreOAuthWorkerAccount(bpsAccount, in.BPSControlURL, in.BPSControlKey)
	if err != nil {
		return nil, err
	}
	if err = s.publishCoreOAuthWorker(ctx, bpsAccount, d.BPS, bps); err != nil {
		return nil, err
	}
	bpsUpdate, err := prepareCoreOAuthRuntimeUpdate(bpsAccount, d.BPS, bps)
	if err != nil {
		return nil, err
	}
	bpsUpdate.Expected = originalBPS
	codexUpdate, err := prepareCoreOAuthRuntimeUpdate(codexAccount, d.Codex, codex)
	if err != nil {
		return nil, err
	}
	installer, ok := s.accountRepo.(CoreOAuthRuntimeInstallRepository)
	if !ok {
		return nil, ErrCoreOAuthUnavailable
	}
	written, err := installer.InstallCoreOAuthRuntime(ctx, bpsUpdate, codexUpdate, &ManagedDeploymentUpdate{ManagedDeployment: d, ExpectedRouteRevision: in.ExpectedRouteRevision})
	if err != nil {
		return nil, err
	}
	if refresher, ok := s.accountRepo.(coreOAuthRuntimeSnapshotRefresher); ok {
		refresher.RefreshSchedulerAccount(ctx, bpsAccount.ID)
		refresher.RefreshSchedulerAccount(ctx, codexAccount.ID)
	}
	return s.managedDeploymentSwitchState(ctx, written, in.RequestedModel, in.RequiredCapabilities, true), nil
}
