//go:build unit

package service

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"strings"
	"sync"
	"testing"
)

type deploymentTestRepo struct {
	AccountRepository
	mu         sync.Mutex
	deployment ManagedDeployment
	sessions   map[string]*ManagedDeploymentSession
	resources  map[string]*ManagedDeploymentSession
	accounts   map[int64]*Account
}

func deploymentFixture() (*OpenAIGatewayService, *deploymentTestRepo) {
	b := ManagedDeploymentBinding{Provider: "bps_excel", AccountID: 11, BackendID: "local-bps", BindingRef: "bind_11111111-1111-4111-8111-111111111111", ProfileRef: "oauth_22222222-2222-4222-8222-222222222222", IdentityEpoch: "33333333-3333-4333-8333-333333333333", WorkspaceID: "ws-bps", Subject: "subject-bps"}
	c := b
	c.Provider = "codex"
	c.AccountID = 12
	c.BackendID = "sub6"
	c.BindingRef = "bind_44444444-4444-4444-8444-444444444444"
	c.ProfileRef = "oauth_55555555-5555-4555-8555-555555555555"
	c.IdentityEpoch = "66666666-6666-4666-8666-666666666666"
	c.Subject = "subject-codex"
	c.WorkspaceID = "ws-codex"
	d := ManagedDeployment{Contract: ManagedDeploymentContract, DeploymentID: "deployment_77777777-7777-4777-8777-777777777777", GroupID: 1, RouteRevision: 1, SelectedProvider: "bps_excel", BPS: b, Codex: c}
	r := &deploymentTestRepo{deployment: d, sessions: map[string]*ManagedDeploymentSession{}, resources: map[string]*ManagedDeploymentSession{}, accounts: map[int64]*Account{}}
	raw, _ := json.Marshal(b)
	var bm map[string]any
	_ = json.Unmarshal(raw, &bm)
	delete(bm, "account_id")
	r.accounts[11] = &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, GroupIDs: []int64{1}, Extra: map[string]any{ManagedDeploymentAccountExtraKey: bm}}
	r.accounts[12] = &Account{ID: 12, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{ManagedOAuthExtraKey: map[string]any{"contract": ManagedOAuthContract, "provider": c.Provider, "backend_id": c.BackendID, "binding_ref": c.BindingRef, "profile_ref": c.ProfileRef, "identity_epoch": c.IdentityEpoch, "workspace_id": c.WorkspaceID, "subject": c.Subject}}}
	return &OpenAIGatewayService{accountRepo: r}, r
}
func (r *deploymentTestRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	return r.accounts[id], nil
}
func (r *deploymentTestRepo) GetManagedDeployment(context.Context, string) (*ManagedDeployment, error) {
	d := r.deployment
	return &d, nil
}
func (r *deploymentTestRepo) GetManagedDeploymentForGroup(_ context.Context, id int64) (*ManagedDeployment, error) {
	if id != r.deployment.GroupID {
		return nil, nil
	}
	d := r.deployment
	return &d, nil
}
func (r *deploymentTestRepo) PutManagedDeployment(_ context.Context, in *ManagedDeploymentUpdate) (*ManagedDeployment, error) {
	r.deployment = in.ManagedDeployment
	return &r.deployment, nil
}
func deploymentTestScope(group, user, key int64, id string) string {
	raw, _ := json.Marshal([]any{group, user, key, id})
	return string(raw)
}
func (r *deploymentTestRepo) BindManagedDeploymentSession(_ context.Context, group, user, key int64, id string, existing bool) (*ManagedDeploymentSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := deploymentTestScope(group, user, key, id)
	if v := r.sessions[k]; v != nil {
		return v, nil
	}
	if existing || group != r.deployment.GroupID {
		return nil, ErrManagedDeploymentContext
	}
	b := r.deployment.BPS
	if r.deployment.SelectedProvider == "codex" {
		b = r.deployment.Codex
	}
	v := &ManagedDeploymentSession{DeploymentID: r.deployment.DeploymentID, GroupID: group, UserID: user, APIKeyID: key, SessionKey: id, RouteRevision: r.deployment.RouteRevision, Binding: b}
	r.sessions[k] = v
	return v, nil
}
func (r *deploymentTestRepo) BindManagedDeploymentResource(_ context.Context, s *ManagedDeploymentSession, kind, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := deploymentTestScope(s.GroupID, s.UserID, s.APIKeyID, kind+":"+id)
	if old := r.resources[k]; old != nil {
		if old.Binding != s.Binding {
			return ErrManagedDeploymentConflict
		}
		return nil
	}
	r.resources[k] = s
	return nil
}
func (r *deploymentTestRepo) ResolveManagedDeploymentResource(_ context.Context, group, user, key int64, kind, id string) (*ManagedDeploymentSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.resources[deploymentTestScope(group, user, key, kind+":"+id)]
	if s == nil {
		return nil, ErrManagedDeploymentContext
	}
	return s, nil
}
func (r *deploymentTestRepo) CheckManagedDeploymentResource(ctx context.Context, s *ManagedDeploymentSession, kind, id string) (bool, error) {
	v, e := r.ResolveManagedDeploymentResource(ctx, s.GroupID, s.UserID, s.APIKeyID, kind, id)
	return v != nil && v.Binding == s.Binding, e
}
func TestManagedDeploymentSwitchKeepsSessionsAndTrustedResources(t *testing.T) {
	s, r := deploymentFixture()
	ctx := context.Background()
	old, e := s.BindManagedDeploymentRequest(ctx, 1, 1, 1, "old", nil)
	require.NoError(t, e)
	require.Equal(t, "bps_excel", old.Binding.Provider)
	require.NoError(t, s.RecordManagedDeploymentResource(ctx, old, "response", "resp_old"))
	r.deployment.SelectedProvider = "codex"
	r.deployment.RouteRevision++
	again, e := s.BindManagedDeploymentRequest(ctx, 1, 1, 1, "old", nil)
	require.NoError(t, e)
	require.Equal(t, old, again)
	fresh, e := s.BindManagedDeploymentRequest(ctx, 1, 1, 1, "new", nil)
	require.NoError(t, e)
	require.Equal(t, "codex", fresh.Binding.Provider)
	continued, e := s.BindManagedDeploymentRequest(ctx, 1, 1, 1, "", []ManagedDeploymentResourceRef{{"response", "resp_old"}})
	require.NoError(t, e)
	require.Equal(t, old, continued)
	_, e = s.BindManagedDeploymentRequest(ctx, 1, 1, 1, "new", []ManagedDeploymentResourceRef{{"response", "resp_old"}})
	require.ErrorIs(t, e, ErrManagedDeploymentContext)
	for _, ids := range [][3]int64{{1, 2, 1}, {1, 1, 2}, {2, 1, 1}} {
		_, e = s.BindManagedDeploymentRequest(ctx, ids[0], ids[1], ids[2], "", []ManagedDeploymentResourceRef{{"response", "resp_old"}})
		require.ErrorIs(t, e, ErrManagedDeploymentContext)
	}
	r.deployment.SelectedProvider = "bps_excel"
	same, e := s.BindManagedDeploymentRequest(ctx, 1, 1, 1, "same", []ManagedDeploymentResourceRef{{"response", "resp_old"}})
	require.NoError(t, e)
	require.Equal(t, old.Binding, same.Binding)
	require.NoError(t, s.RecordManagedDeploymentResource(ctx, same, "response", "resp_old"))
	require.ErrorIs(t, s.RecordManagedDeploymentResource(ctx, fresh, "response", "resp_old"), ErrManagedDeploymentConflict)
}
func TestManagedDeploymentRejectsMissingOwnerAndOpaqueCrossEpoch(t *testing.T) {
	s, r := deploymentFixture()
	ctx := context.Background()
	_, e := s.BindManagedDeploymentRequest(ctx, 1, 1, 1, "", nil)
	require.ErrorIs(t, e, ErrManagedDeploymentContext)
	_, e = s.BindManagedDeploymentRequest(ctx, 1, 1, 1, "new", []ManagedDeploymentResourceRef{{"file", "file_unknown"}})
	require.ErrorIs(t, e, ErrManagedDeploymentContext)
	a, e := s.BindManagedDeploymentRequest(ctx, 1, 1, 1, "old", nil)
	require.NoError(t, e)
	require.NoError(t, s.RecordManagedDeploymentResource(ctx, a, "encrypted_reasoning", "ciphertext"))
	r.deployment.BPS.IdentityEpoch = "88888888-8888-4888-8888-888888888888"
	_, e = s.BindManagedDeploymentRequest(ctx, 1, 1, 1, "new-epoch", []ManagedDeploymentResourceRef{{"encrypted_reasoning", "ciphertext"}})
	require.ErrorIs(t, e, ErrManagedDeploymentContext)
}
func TestManagedDeploymentConcurrentFirstRequestOneBinding(t *testing.T) {
	s, _ := deploymentFixture()
	ctx := context.Background()
	var wg sync.WaitGroup
	got := make(chan *ManagedDeploymentSession, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, e := s.BindManagedDeploymentRequest(ctx, 1, 1, 1, "first", nil)
			if e == nil {
				got <- v
			}
		}()
	}
	wg.Wait()
	close(got)
	n := 0
	for v := range got {
		require.Equal(t, "bps_excel", v.Binding.Provider)
		require.Equal(t, ManagedDeploymentOwnerHash("first"), v.SessionKey)
		n++
	}
	require.Equal(t, 32, n)
}
func TestManagedDeploymentValidatesPublicationMarkerAndIdentity(t *testing.T) {
	_, r := deploymentFixture()
	require.NoError(t, r.deployment.Validate())
	require.NoError(t, ValidateManagedDeploymentAccount(r.accounts[11], r.deployment.BPS))
	require.NoError(t, ValidateManagedDeploymentAccount(r.accounts[12], r.deployment.Codex))
	r.accounts[11].Extra[ManagedDeploymentAccountExtraKey].(map[string]any)["identity_epoch"] = "changed"
	require.ErrorIs(t, ValidateManagedDeploymentAccount(r.accounts[11], r.deployment.BPS), ErrManagedDeploymentConflict)
}
func TestManagedDeploymentSchedulerNeverFallsBackOrEnablesStoppedAccount(t *testing.T) {
	s, r := deploymentFixture()
	ctx := context.Background()
	session, e := s.BindManagedDeploymentRequest(ctx, 1, 1, 1, "old", nil)
	require.NoError(t, e)
	group := int64(1)
	req := OpenAIAccountScheduleRequest{GroupID: &group, Platform: PlatformOpenAI, ExcludedIDs: map[int64]struct{}{11: {}}}
	_, _, e = s.selectManagedDeploymentAccount(ctx, session, req)
	require.ErrorIs(t, e, ErrManagedDeploymentUnavailable)
	req.ExcludedIDs = nil
	r.accounts[11].Schedulable = false
	_, _, e = s.selectManagedDeploymentAccount(ctx, session, req)
	require.ErrorIs(t, e, ErrManagedDeploymentUnavailable)
	require.False(t, r.accounts[11].Schedulable)
	r.accounts[11].Schedulable = true
	r.accounts[11].GroupIDs = []int64{2}
	_, _, e = s.selectManagedDeploymentAccount(ctx, session, req)
	require.ErrorIs(t, e, ErrManagedDeploymentUnavailable)
}
func TestManagedDeploymentPayloadOwnsResourceFieldsAndRejectsAmbiguity(t *testing.T) {
	refs, e := ManagedDeploymentPayloadResources([]byte(`{"previous_response_id":"resp_old","input":[{"type":"input_file","file_id":"file_a"},{"type":"reasoning","encrypted_content":"opaque"}],"tools":[{"vector_store_ids":["vs_a"]}]}`), false)
	require.NoError(t, e)
	require.Len(t, refs, 4)
	for _, raw := range []string{`{"previous_response_id":"resp_a","previous_response_id":"resp_b"}`, `{"input":[{"file_id":"file_a","file_id":"file_b"}]}`, `{} {}`, strings.Repeat("[", 70)} {
		_, e = ManagedDeploymentPayloadResources([]byte(raw), false)
		require.ErrorIs(t, e, ErrManagedDeploymentInvalid)
	}
	s, _ := deploymentFixture()
	session, e := s.BindManagedDeploymentRequest(context.Background(), 1, 1, 1, "first", nil)
	require.NoError(t, e)
	ctx := WithManagedDeploymentSession(context.Background(), session)
	require.NoError(t, s.RecordManagedDeploymentPayload(ctx, []byte(`{"response":{"id":"resp_created","output":[{"id":"rs_reasoning","encrypted_content":"opaque"},{"id":"fc_item","call_id":"call_a"}]}}`)))
	require.NoError(t, s.ValidateManagedDeploymentPayload(ctx, []byte(`{"previous_response_id":"resp_created","input":[{"type":"item_reference","id":"rs_reasoning"},{"encrypted_content":"opaque"},{"call_id":"call_a"}]}`)))
	require.ErrorIs(t, s.ValidateManagedDeploymentPayload(ctx, []byte(`{"file_id":"file_unknown"}`)), ErrManagedDeploymentContext)
	require.NoError(t, s.RecordManagedDeploymentPayload(context.Background(), []byte("not json")), "unmanaged traffic is untouched")
}

func TestManagedDeploymentNeverTrustsReflectedClientMetadata(t *testing.T) {
	raw := []byte(`{"id":"resp_legitimate","metadata":{"id":"file_victim","encrypted_content":"stolen"},"output":[{"type":"function_call","id":"fc_real","arguments":{"file_id":"file_victim","id":"resp_victim"}},{"type":"function_call_output","output":{"id":"file_victim"}}],"tools":[{"parameters":{"properties":{"id":"file_victim"}}}],"content_block":{"type":"tool_use","input":{"id":"file_victim","file_id":"file_victim"}}}`)
	refs, e := ManagedDeploymentPayloadResources(raw, true)
	require.NoError(t, e)
	require.ElementsMatch(t, []ManagedDeploymentResourceRef{{"response", "resp_legitimate"}, {"item", "fc_real"}}, refs)
	input, e := ManagedDeploymentPayloadResources([]byte(`{"metadata":{"file_id":"file_victim"},"tools":[{"type":"function","parameters":{"file_id":"file_victim"}}],"input":[{"type":"function_call_output","output":{"file_id":"file_victim"}}]}`), false)
	require.NoError(t, e)
	require.Empty(t, input)
}

func TestManagedDeploymentChatAndMessagesResourceAliases(t *testing.T) {
	for _, raw := range []string{`{"messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"file_victim"}}]}]}`, `{"messages":[{"role":"tool","tool_call_id":"call_victim","content":"result"}]}`, `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_victim","content":"result"}]}]}`} {
		s, _ := deploymentFixture()
		session, e := s.BindManagedDeploymentRequest(context.Background(), 1, 1, 1, "first", nil)
		require.NoError(t, e)
		ctx := WithManagedDeploymentSession(context.Background(), session)
		require.ErrorIs(t, s.ValidateManagedDeploymentPayload(ctx, []byte(raw)), ErrManagedDeploymentContext)
	}
	s, _ := deploymentFixture()
	session, e := s.BindManagedDeploymentRequest(context.Background(), 1, 1, 1, "first", nil)
	require.NoError(t, e)
	ctx := WithManagedDeploymentSession(context.Background(), session)
	require.NoError(t, s.RecordManagedDeploymentPayload(ctx, []byte(`{"choices":[{"message":{"tool_calls":[{"id":"call_created","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`)))
	require.NoError(t, s.ValidateManagedDeploymentPayload(ctx, []byte(`{"messages":[{"role":"tool","tool_call_id":"call_created","content":"result"}]}`)))
	require.NoError(t, s.RecordManagedDeploymentPayload(ctx, []byte(`{"type":"content_block_start","content_block":{"type":"tool_use","id":"toolu_created","name":"f","input":{}}}`)))
	require.NoError(t, s.ValidateManagedDeploymentPayload(ctx, []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_created","content":"result"}]}]}`)))
}

func TestManagedDeploymentThinkingSignaturesRemainOwned(t *testing.T) {
	s, r := deploymentFixture()
	ctx := context.Background()
	session, e := s.BindManagedDeploymentRequest(ctx, 1, 1, 1, "old", nil)
	require.NoError(t, e)
	ctx = WithManagedDeploymentSession(ctx, session)
	require.ErrorIs(t, s.ValidateManagedDeploymentPayload(ctx, []byte(`{"messages":[{"content":[{"type":"thinking","signature":" opaque_signature "}]}]}`)), ErrManagedDeploymentContext)
	require.NoError(t, s.RecordManagedDeploymentPayload(ctx, []byte(`{"type":"content_block_delta","delta":{"type":"signature_delta","signature":"opaque_signature"}}`)))
	require.NoError(t, s.ValidateManagedDeploymentPayload(ctx, []byte(`{"messages":[{"content":[{"type":"thinking","signature":" opaque_signature "}]}]}`)))
	r.deployment.BPS.IdentityEpoch = "99999999-9999-4999-8999-999999999999"
	next, e := s.BindManagedDeploymentRequest(context.Background(), 1, 1, 1, "new-epoch", nil)
	require.NoError(t, e)
	require.ErrorIs(t, s.ValidateManagedDeploymentPayload(WithManagedDeploymentSession(context.Background(), next), []byte(`{"messages":[{"content":[{"type":"thinking","signature":"opaque_signature"}]}]}`)), ErrManagedDeploymentContext)
	refs, e := ManagedDeploymentPayloadResources([]byte(`{"content":[{"type":"thinking","signature":"gAAAA_ignored_by_converter"},{"type":"redacted_thinking","data":"ignored_by_converter"}]}`), false)
	require.NoError(t, e)
	require.Empty(t, refs)
}

func TestManagedDeploymentSchedulerPreservesChannelRestriction(t *testing.T) {
	for _, source := range []string{BillingModelSourceRequested, BillingModelSourceChannelMapped} {
		t.Run(source, func(t *testing.T) {
			s, _ := deploymentFixture()
			s.channelService = newTestChannelService(makeStandardRepo(Channel{ID: 1, Status: StatusActive, GroupIDs: []int64{1}, RestrictModels: true, BillingModelSource: source, ModelPricing: []ChannelModelPricing{{Platform: PlatformOpenAI, Models: []string{"allowed-model"}}}, ModelMapping: map[string]map[string]string{PlatformOpenAI: {"denied-model": "denied-upstream"}}}, map[int64]string{1: PlatformOpenAI}))
			session, e := s.BindManagedDeploymentRequest(context.Background(), 1, 1, 1, "first", nil)
			require.NoError(t, e)
			group := int64(1)
			selection, decision, e := s.selectManagedDeploymentAccount(context.Background(), session, OpenAIAccountScheduleRequest{GroupID: &group, Platform: PlatformOpenAI, RequestedModel: "denied-model"})
			require.ErrorIs(t, e, ErrManagedDeploymentUnavailable)
			require.Nil(t, selection)
			require.Equal(t, "managed_deployment", decision.Layer)
		})
	}
}

func TestManagedDeploymentUpstreamAffinityScopesEveryIdentityDimension(t *testing.T) {
	s, _ := deploymentFixture()
	base, e := s.BindManagedDeploymentRequest(context.Background(), 1, 1, 1, "same", nil)
	require.NoError(t, e)
	ctx := WithManagedDeploymentSession(context.Background(), base)
	want, ok := ScopeManagedDeploymentAffinity(ctx, "same-wire-session")
	require.True(t, ok)
	again, _ := ScopeManagedDeploymentAffinity(ctx, "same-wire-session")
	require.Equal(t, want, again)
	for _, mutate := range []func(*ManagedDeploymentSession){func(s *ManagedDeploymentSession) { s.UserID++ }, func(s *ManagedDeploymentSession) { s.APIKeyID++ }, func(s *ManagedDeploymentSession) { s.GroupID++ }, func(s *ManagedDeploymentSession) { s.DeploymentID += "-new" }, func(s *ManagedDeploymentSession) { s.Binding.IdentityEpoch = "new-epoch" }, func(s *ManagedDeploymentSession) { s.Binding.Provider = "codex" }, func(s *ManagedDeploymentSession) { s.Binding.WorkspaceID = "new-workspace" }, func(s *ManagedDeploymentSession) { s.Binding.Subject = "new-subject" }, func(s *ManagedDeploymentSession) { s.Binding.ProfileRef = "new-profile" }, func(s *ManagedDeploymentSession) { s.Binding.BindingRef = "new-binding" }, func(s *ManagedDeploymentSession) { s.Binding.AccountID++ }, func(s *ManagedDeploymentSession) { s.Binding.BackendID = "new-backend" }} {
		next := *base
		mutate(&next)
		got, _ := ScopeManagedDeploymentAffinity(WithManagedDeploymentSession(context.Background(), &next), "same-wire-session")
		require.NotEqual(t, want, got)
	}
	got, managed := ScopeManagedDeploymentAffinity(context.Background(), "ordinary")
	require.False(t, managed)
	require.Equal(t, "ordinary", got)
}

func TestManagedDeploymentEmptyWireAffinityStillSeparatesSessions(t *testing.T) {
	s, _ := deploymentFixture()
	a, e := s.BindManagedDeploymentRequest(context.Background(), 1, 1, 1, "first", nil)
	require.NoError(t, e)
	b, e := s.BindManagedDeploymentRequest(context.Background(), 1, 1, 1, "second", nil)
	require.NoError(t, e)
	ka, _ := ScopeManagedDeploymentAffinity(WithManagedDeploymentSession(context.Background(), a), "")
	kb, _ := ScopeManagedDeploymentAffinity(WithManagedDeploymentSession(context.Background(), b), "")
	require.NotEqual(t, ka, kb)
}
