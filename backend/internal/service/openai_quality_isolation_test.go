package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/stretchr/testify/require"
)

type tiboQualityAdmissionRepo struct {
	AccountRepository
	accounts     map[int64]*Account
	err          error
	fullReads    int
	qualityReads int
}

var _ openAITiboQualityIsolationReader = (*tiboQualityAdmissionRepo)(nil)

func (r *tiboQualityAdmissionRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	r.fullReads++
	if r.err != nil {
		return nil, r.err
	}
	return r.accounts[id], nil
}

func (r *tiboQualityAdmissionRepo) GetOpenAITiboQualityIsolation(_ context.Context, id int64) (bool, error) {
	r.qualityReads++
	if r.err != nil {
		return false, r.err
	}
	a := r.accounts[id]
	if a == nil {
		return false, ErrAccountNotFound
	}
	if a.IsOpenAITiboQualityIsolated() {
		return true, nil
	}
	if a.IsShadow() {
		parent := r.accounts[*a.ParentAccountID]
		if parent == nil {
			return false, ErrAccountNotFound
		}
		return parent.IsOpenAITiboQualityIsolated(), nil
	}
	return false, nil
}

func TestTiboQualityIsolationPreservesManualSchedulingState(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
		Extra: map[string]any{"openai_tibo_quality_isolated": true}}
	require.False(t, account.IsSchedulable())
	require.False(t, account.IsCredentialUsableForShadow())
	require.True(t, account.Schedulable, "quality isolation must not overwrite the administrator switch")
	account.Schedulable = false
	account.Extra["openai_tibo_quality_isolated"] = false
	require.False(t, account.IsSchedulable(), "recovery cannot resume a manual pause")
	account.Schedulable = true
	require.True(t, account.IsSchedulable())
}

func TestTiboQualityIsolationForwardEntriesRejectMarkedSnapshot(t *testing.T) {
	stale := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
	fresh := *stale
	fresh.Extra = map[string]any{"openai_tibo_quality_isolated": true}
	svc := &OpenAIGatewayService{accountRepo: &tiboQualityAdmissionRepo{accounts: map[int64]*Account{1: &fresh}}}
	body := []byte("{}")
	for name, forward := range map[string]func() (*OpenAIForwardResult, error){
		"responses": func() (*OpenAIForwardResult, error) { return svc.Forward(context.Background(), nil, &fresh, body) },
		"chat": func() (*OpenAIForwardResult, error) {
			return svc.ForwardAsChatCompletions(context.Background(), nil, &fresh, body, "", "")
		},
		"messages": func() (*OpenAIForwardResult, error) {
			return svc.ForwardAsAnthropic(context.Background(), nil, &fresh, body, "", "")
		},
		"images": func() (*OpenAIForwardResult, error) {
			return svc.ForwardImages(context.Background(), nil, &fresh, body, nil, "")
		},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := forward()
			require.Nil(t, result)
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.Equal(t, http.StatusServiceUnavailable, failover.ClientStatusCode)
			require.Equal(t, GatewayFailureReason("openai_tibo_quality_isolated"), failover.Reason)
			require.True(t, failover.ShouldRetryNextAccount())
		})
	}
}

func TestTiboQualityIsolationContinuationStopsAccountFailover(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Extra: map[string]any{"openai_tibo_quality_isolated": true}}
	svc := &OpenAIGatewayService{}
	result, err := svc.Forward(context.Background(), nil, account, []byte("{\"previous_response_id\":\"resp_bound\"}"))
	require.Nil(t, result)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.False(t, failover.ShouldRetryNextAccount())
}

func TestTiboQualityIsolationRejectsShadowOfIsolatedParent(t *testing.T) {
	parentID := int64(1)
	parent := &Account{ID: parentID, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
		Extra: map[string]any{"openai_tibo_quality_isolated": true}}
	shadow := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, ParentAccountID: &parentID}
	svc := &OpenAIGatewayService{accountRepo: &tiboQualityAdmissionRepo{accounts: map[int64]*Account{1: parent, 2: shadow}}}
	err := svc.checkOpenAITiboQualityAdmission(context.Background(), shadow, []byte("{}"))
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
}

func TestTiboQualityIsolationCurrentRowReadFailureDoesNotSend(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
	svc := &OpenAIGatewayService{accountRepo: &tiboQualityAdmissionRepo{err: errors.New("database unavailable")}}
	err := svc.checkOpenAITiboQualityAdmission(context.Background(), account, []byte("{}"))
	require.ErrorContains(t, err, "verify OpenAI quality isolation")
}

func TestTiboQualityIsolationSchedulerSkipsIsolatedStickyForBothModes(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		name := "legacy"
		if advanced {
			name = "advanced"
		}
		t.Run(name, func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			groupID := int64(98101)
			accounts := []Account{
				{ID: 98111, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
					Concurrency: 1, GroupIDs: []int64{groupID}, Extra: map[string]any{"openai_tibo_quality_isolated": true}},
				{ID: 98112, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
					Concurrency: 1, GroupIDs: []int64{groupID}, Priority: 10},
			}
			cfg := &config.Config{}
			cfg.Gateway.OpenAIWS.LBTopK = 1
			svc := &OpenAIGatewayService{
				accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}, cfg: cfg,
				cache:              &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:quality-session": 98111}},
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
			}
			if advanced {
				svc.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService("true", "false")
			}
			selection, _, err := svc.SelectAccountWithScheduler(context.Background(), &groupID, "", "quality-session", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.Equal(t, int64(98112), selection.Account.ID)
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
		})
	}
}

func TestTiboQualityIsolationBoundResponseDoesNotFallBackOrForgetOwner(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		name := "legacy"
		if advanced {
			name = "advanced"
		}
		t.Run(name, func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			ctx := context.Background()
			groupID := int64(98201)
			accounts := []Account{
				{ID: 98211, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1,
					GroupIDs: []int64{groupID}, Extra: map[string]any{"openai_tibo_quality_isolated": true}},
				{ID: 98212, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{groupID}},
			}
			cfg := newSchedulerTestOpenAIWSV2Config()
			svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}, cfg: cfg,
				cache: &schedulerTestGatewayCache{}, concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{})}
			if advanced {
				svc.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService("true", "false")
			}
			store := svc.getOpenAIWSStateStore()
			require.NoError(t, store.BindResponseAccount(ctx, groupID, "resp_quality_bound", 98211, time.Hour))
			selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "resp_quality_bound", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
			require.Nil(t, selection)
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.False(t, failover.ShouldRetryNextAccount())
			owner, err := store.GetResponseAccount(ctx, groupID, "resp_quality_bound")
			require.NoError(t, err)
			require.Equal(t, int64(98211), owner)
		})
	}
}

func TestTiboQualityIsolationShadowResolverPreservesResponseOwner(t *testing.T) {
	ctx := context.Background()
	groupID, parentID := int64(98301), int64(98311)
	accounts := []Account{
		{ID: parentID, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
			Extra: map[string]any{"openai_tibo_quality_isolated": true}},
		{ID: 98312, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
			Concurrency: 1, GroupIDs: []int64{groupID}, ParentAccountID: &parentID},
	}
	// Match production's narrow authority contract, including the persisted
	// credential parent's isolation even when the shadow snapshot is unmarked.
	repo := &tiboQualityAdmissionRepo{accounts: map[int64]*Account{parentID: &accounts[0], 98312: &accounts[1]}}
	svc := &OpenAIGatewayService{accountRepo: repo,
		cfg: newSchedulerTestOpenAIWSV2Config(), cache: &schedulerTestGatewayCache{}}
	store := svc.getOpenAIWSStateStore()
	require.NoError(t, store.BindResponseAccount(ctx, groupID, "resp_quality_shadow", 98312, time.Hour))
	id, account, _, _ := svc.resolveAccountByPreviousResponseIDForCapability(ctx, &groupID, "resp_quality_shadow",
		"", nil, OpenAIEndpointCapabilityResponses, false)
	require.Zero(t, id)
	require.Nil(t, account)
	owner, err := store.GetResponseAccount(ctx, groupID, "resp_quality_shadow")
	require.NoError(t, err)
	require.Equal(t, int64(98312), owner)
	require.Positive(t, repo.qualityReads, "parent isolation must be checked through final authority")
	err = svc.checkOpenAITiboPreviousResponseAdmission(ctx, &groupID, "resp_quality_shadow")
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.False(t, failover.ShouldRetryNextAccount(), "a shadow continuation cannot escape its isolated parent")
}

func TestTiboQualityIsolationDefaultSnapshotGateDoesNotLoadAccounts(t *testing.T) {
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true}
	// A legacy/mock repository can embed unused methods without implementing
	// GetByID; entry admission must not unexpectedly invoke that full-object API.
	svc := &OpenAIGatewayService{accountRepo: &struct{ AccountRepository }{}}
	require.NoError(t, svc.checkOpenAITiboQualitySnapshotAdmission(context.Background(), account, []byte("{}")))
	require.NoError(t, svc.checkOpenAITiboQualityAdmission(context.Background(), account, []byte("{}")))
	require.NoError(t, svc.checkOpenAITiboQualityAdmission(context.Background(), nil, []byte("{}")))
}

func TestTiboQualityIsolationFinalSenderUsesPersistentGateWhenMonitorDisabled(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	stale := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true}
	marked := *stale
	marked.Extra = map[string]any{OpenAITiboQualityIsolatedExtraKey: true}
	repo := &tiboQualityAdmissionRepo{accounts: map[int64]*Account{42: &marked}}
	off := false
	cfg := &config.Config{}
	cfg.Gateway.OpenAITiboRoute.Enabled = &off
	svc := &OpenAIGatewayService{cfg: cfg, accountRepo: repo, httpUpstream: &tiboLocalHTTPUpstream{client: server.Client()}}
	require.NoError(t, svc.checkOpenAITiboQualitySnapshotAdmission(context.Background(), stale, []byte("{}")))
	require.Zero(t, repo.qualityReads, "entry does not query the authority")
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, nil)
	require.NoError(t, err)
	resp, err := svc.doOpenAIUpstreamOnce(req, "", stale)
	require.Nil(t, resp)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Equal(t, GatewayFailureReason("openai_tibo_quality_isolated"), failover.Reason)
	require.Zero(t, requests.Load())
	require.Zero(t, repo.fullReads)
	require.Equal(t, 1, repo.qualityReads)

	// The same default-disabled sender admits an ordinary unmarked account
	// with one boolean query and no complete account materialization.
	repo.accounts[42] = stale
	resp, err = svc.doOpenAIUpstreamOnce(req, "", stale)
	require.NoError(t, err)
	require.NotNil(t, resp)
	_ = resp.Body.Close()
	require.EqualValues(t, 1, requests.Load())
	require.Zero(t, repo.fullReads)
	require.Equal(t, 2, repo.qualityReads)
}
