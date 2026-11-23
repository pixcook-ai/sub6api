package service

import "context"

// SelectStoredResponseNativeAccount15 consumes only the owning account obtained
// from a freshly authorized local response. There is no alternative candidate,
// sticky lookup, or inference retry on a different account.
func (s *OpenAIGatewayService) SelectStoredResponseNativeAccount15(ctx context.Context, groupID *int64, requiredAccountID int64, forwardModel string) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	decision := OpenAIAccountScheduleDecision{Layer: "stored_response_native"}
	if s == nil || groupID == nil || *groupID <= 0 || requiredAccountID <= 0 || forwardModel == "" {
		return nil, decision, ErrBPSStoredResponseRoute
	}
	if err := ctx.Err(); err != nil {
		return nil, decision, err
	}
	ctx = s.withOpenAIQuotaAutoPauseContext(ctx)
	ctx = s.withOpenAIGroupPrivacyRequirement(ctx, groupID)
	ctx = s.withOpenAIProfitControlGate(ctx, groupID)
	ctx = context.WithValue(ctx, openAIProxyStreamQuarantineBypassKey{}, false)
	selected, err := s.RecheckBPSStoredResponseAccount(ctx, groupID, requiredAccountID, forwardModel)
	if err != nil {
		return nil, decision, err
	}
	before, err := BPSStoredResponsesConfigForAccount(selected)
	if err != nil {
		return nil, decision, err
	}
	apiKey, baseURL := selected.GetCredential("api_key"), selected.GetCredential("base_url")
	resolved := ResolveOpenAIAccountUpstreamModelForRequest(selected, forwardModel, false)
	slot, err := s.tryAcquireAccountSlot(ctx, requiredAccountID, selected.Concurrency)
	if err != nil {
		return nil, decision, err
	}
	release := func() {
		if slot != nil && slot.ReleaseFunc != nil {
			slot.ReleaseFunc()
		}
	}
	latest, err := s.RecheckBPSStoredResponseAccount(ctx, groupID, requiredAccountID, forwardModel)
	var after BPSStoredResponsesBackendConfig
	if err == nil {
		after, err = BPSStoredResponsesConfigForAccount(latest)
	}
	if err != nil || before != after || latest.GetCredential("api_key") != apiKey || latest.GetCredential("base_url") != baseURL || ResolveOpenAIAccountUpstreamModelForRequest(latest, forwardModel, false) != resolved {
		release()
		return nil, decision, ErrBPSRequestAuthorizationRevoked
	}
	if err := ctx.Err(); err != nil {
		release()
		return nil, decision, err
	}
	decision.SelectedAccountID, decision.SelectedAccountType = latest.ID, latest.Type
	decision.CandidateCount, decision.TopK = 1, 1
	if slot != nil && slot.Acquired {
		return attachSelectionProfitGate(ctx, &AccountSelectionResult{Account: latest, Acquired: true, ReleaseFunc: slot.ReleaseFunc}), decision, nil
	}
	// Reuse inference's existing wait contract for the same account. The handler
	// must reacquire exactly this ID and repeat parent authorization after wait.
	if s.concurrencyService != nil {
		cfg := s.schedulingConfig()
		return attachSelectionProfitGate(ctx, &AccountSelectionResult{Account: latest, WaitPlan: &AccountWaitPlan{AccountID: requiredAccountID, MaxConcurrency: latest.Concurrency, Timeout: cfg.StickySessionWaitTimeout, MaxWaiting: cfg.StickySessionMaxWaiting}}), decision, nil
	}
	return nil, decision, ErrNoAvailableAccounts
}
