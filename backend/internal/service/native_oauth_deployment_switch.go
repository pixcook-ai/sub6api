package service

import (
	"context"
	"strings"

	infraerrors "github.com/MACOS-DO/sub4api/internal/pkg/errors"
)

var ErrManagedDeploymentNotFound = infraerrors.New(404, "MANAGED_DEPLOYMENT_NOT_FOUND", "managed deployment not found")

type ManagedDeploymentSwitchRequest struct {
	ExpectedRouteRevision int64                      `json:"expected_route_revision"`
	SelectedProvider      string                     `json:"selected_provider"`
	RequestedModel        string                     `json:"requested_model"`
	RequiredCapabilities  []OpenAIEndpointCapability `json:"required_capabilities"`
	ScheduleNewSessions   bool                       `json:"schedule_new_sessions"`
}

type ManagedDeploymentSwitchOption struct {
	Provider   string `json:"provider"`
	AccountID  int64  `json:"account_id"`
	ProfileRef string `json:"profile_ref"`
	Available  bool   `json:"available"`
	Reason     string `json:"reason"`
}

type ManagedDeploymentSwitchState struct {
	DeploymentID         string                          `json:"deployment_id"`
	GroupID              int64                           `json:"group_id"`
	RouteRevision        int64                           `json:"route_revision"`
	SelectedProvider     string                          `json:"selected_provider"`
	AppliesTo            string                          `json:"applies_to"`
	ContextRequired      bool                            `json:"context_required"`
	RequestedModel       string                          `json:"requested_model"`
	RequiredCapabilities []OpenAIEndpointCapability      `json:"required_capabilities"`
	Options              []ManagedDeploymentSwitchOption `json:"options"`
}

func managedDeploymentSwitchContext(model string, capabilities []OpenAIEndpointCapability) (bool, error) {
	if len(model) > 256 || strings.TrimSpace(model) != model || strings.ContainsAny(model, "\r\n\x00") || len(capabilities) > 16 {
		return false, ErrManagedDeploymentInvalid
	}
	seen := map[OpenAIEndpointCapability]bool{}
	for _, capability := range capabilities {
		switch capability {
		case OpenAIEndpointCapabilityResponses, OpenAIEndpointCapabilityChatCompletions, OpenAIEndpointCapabilityEmbeddings, OpenAIEndpointCapabilityAlphaSearch, OpenAIEndpointCapabilityLive, OpenAIEndpointCapabilityBPSFiles, OpenAIEndpointCapabilityBPSLocalResponses:
		default:
			return false, ErrManagedDeploymentInvalid
		}
		if seen[capability] {
			return false, ErrManagedDeploymentInvalid
		}
		seen[capability] = true
	}
	return model != "" && len(capabilities) > 0, nil
}

func (s *OpenAIGatewayService) managedDeploymentForSwitch(ctx context.Context, id string) (*ManagedDeployment, error) {
	if s == nil {
		return nil, ErrManagedDeploymentUnavailable
	}
	r, err := managedDeploymentRepo(s.accountRepo)
	if err != nil {
		return nil, err
	}
	d, err := r.GetManagedDeployment(ctx, id)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, ErrManagedDeploymentNotFound
	}
	if d.DeploymentID != id || d.Validate() != nil {
		return nil, ErrManagedDeploymentConflict
	}
	return d, nil
}

func (s *OpenAIGatewayService) GetManagedDeploymentSwitchOptions(ctx context.Context, id, model string, capabilities []OpenAIEndpointCapability) (*ManagedDeploymentSwitchState, error) {
	complete, err := managedDeploymentSwitchContext(model, capabilities)
	if err != nil {
		return nil, err
	}
	d, err := s.managedDeploymentForSwitch(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.managedDeploymentSwitchState(ctx, d, model, capabilities, complete), nil
}

func (s *OpenAIGatewayService) managedDeploymentSwitchState(ctx context.Context, d *ManagedDeployment, model string, capabilities []OpenAIEndpointCapability, complete bool) *ManagedDeploymentSwitchState {
	state := &ManagedDeploymentSwitchState{DeploymentID: d.DeploymentID, GroupID: d.GroupID, RouteRevision: d.RouteRevision, SelectedProvider: d.SelectedProvider, AppliesTo: "new_sessions", ContextRequired: !complete, RequestedModel: model, RequiredCapabilities: append([]OpenAIEndpointCapability{}, capabilities...), Options: []ManagedDeploymentSwitchOption{}}
	bindings := []ManagedDeploymentBinding{d.BPS, d.Codex}
	accounts := make([]*Account, len(bindings))
	identityReason := ""
	for i, binding := range bindings {
		a, err := s.accountRepo.GetByID(ctx, binding.AccountID)
		if err != nil || ValidateManagedDeploymentAccount(a, binding) != nil {
			identityReason = "authorization_unavailable"
		}
		accounts[i] = a
	}
	if identityReason == "" && d.BPS.Subject != d.Codex.Subject {
		identityReason = "identity_mismatch"
	}
	if identityReason == "" && d.BPS.WorkspaceID != d.Codex.WorkspaceID {
		identityReason = "workspace_mismatch"
	}
	for i, binding := range bindings {
		option := ManagedDeploymentSwitchOption{Provider: binding.Provider, AccountID: binding.AccountID, ProfileRef: binding.ProfileRef}
		switch {
		case !complete:
			option.Reason = "request_context_required"
		case identityReason != "":
			option.Reason = identityReason
		case !s.managedDeploymentCoreProfileReady(ctx, accounts[i], binding):
			option.Reason = "authorization_unavailable"
		default:
			option.Reason = s.managedDeploymentSwitchTargetReason(ctx, d, accounts[i], model, capabilities)
		}
		option.Available = option.Reason == ""
		state.Options = append(state.Options, option)
	}
	return state
}

func (s *OpenAIGatewayService) managedDeploymentCoreProfileReady(ctx context.Context, a *Account, binding ManagedDeploymentBinding) bool {
	if !CoreOAuthDeclared(a) {
		return true
	}
	if s.coreOAuth == nil {
		return false
	}
	marker, err := CoreOAuthAccountBindingFromAccount(a)
	if err != nil {
		return false
	}
	profile, err := s.coreOAuth.GetProfile(ctx, marker.ProfileRef)
	return err == nil && profile != nil && profile.Authorized && !profile.NeedsAuthorization && coreOAuthBindingMatchesProfile(binding, *profile) && profile.TokenRevision >= marker.TokenRevision
}

func (s *OpenAIGatewayService) managedDeploymentSwitchTargetReason(ctx context.Context, d *ManagedDeployment, a *Account, model string, capabilities []OpenAIEndpointCapability) string {
	if a == nil || !a.IsActive() {
		return "authorization_unavailable"
	}
	if !a.Schedulable {
		return "scheduling_paused"
	}
	if !a.IsSchedulable() || a.ParentAccountID != nil {
		return "authorization_unavailable"
	}
	if !openAIStickyAccountMatchesGroup(a, &d.GroupID) {
		return "group_mismatch"
	}
	if !a.IsModelSupported(model) {
		return "model_not_supported"
	}
	scheduler := &defaultOpenAIAccountScheduler{service: s}
	for _, capability := range capabilities {
		if !a.SupportsOpenAIEndpointCapability(capability) {
			return "capability_mismatch"
		}
		request := OpenAIAccountScheduleRequest{GroupID: &d.GroupID, Platform: PlatformOpenAI, RequestedModel: model, RequiredTransport: OpenAIUpstreamTransportHTTPSSE, RequiredCapability: capability, RequirePrivacySet: s.openAIGroupRequiresPrivacySet(ctx, &d.GroupID)}
		if compatible, _ := scheduler.isAccountRequestCompatibleReason(ctx, a, request); !compatible {
			return "policy_restricted"
		}
	}
	if s.checkChannelPricingRestriction(ctx, &d.GroupID, model) || s.isOpenAIAccountBlockedBySchedulingThreshold(ctx, a) {
		return "policy_restricted"
	}
	return ""
}

func (s *OpenAIGatewayService) SwitchManagedDeployment(ctx context.Context, id string, in ManagedDeploymentSwitchRequest) (*ManagedDeploymentSwitchState, error) {
	complete, err := managedDeploymentSwitchContext(in.RequestedModel, in.RequiredCapabilities)
	if err != nil || !complete || !in.ScheduleNewSessions || in.ExpectedRouteRevision <= 0 || in.ExpectedRouteRevision >= 9007199254740991 || (in.SelectedProvider != "codex" && in.SelectedProvider != "bps_excel") {
		return nil, ErrManagedDeploymentInvalid
	}
	d, err := s.managedDeploymentForSwitch(ctx, id)
	if err != nil {
		return nil, err
	}
	if d.RouteRevision != in.ExpectedRouteRevision {
		return nil, ErrManagedDeploymentConflict
	}
	state := s.managedDeploymentSwitchState(ctx, d, in.RequestedModel, in.RequiredCapabilities, true)
	for _, option := range state.Options {
		if option.Provider == in.SelectedProvider && !option.Available {
			return nil, ErrManagedDeploymentUnavailable
		}
	}
	if d.SelectedProvider == in.SelectedProvider {
		return state, nil
	}
	next := *d
	next.SelectedProvider, next.RouteRevision = in.SelectedProvider, d.RouteRevision+1
	r, err := managedDeploymentRepo(s.accountRepo)
	if err != nil {
		return nil, err
	}
	written, err := r.PutManagedDeployment(ctx, &ManagedDeploymentUpdate{ManagedDeployment: next, ExpectedRouteRevision: in.ExpectedRouteRevision})
	if err != nil {
		return nil, err
	}
	return s.managedDeploymentSwitchState(ctx, written, in.RequestedModel, in.RequiredCapabilities, true), nil
}
