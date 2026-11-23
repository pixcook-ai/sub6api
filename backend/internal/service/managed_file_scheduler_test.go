package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/openai_compat"
	"github.com/stretchr/testify/require"
)

func managedSchedulerTestAccount(id, groupID int64) Account {
	return Account{
		ID: id, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{groupID},
		Credentials: map[string]any{
			"api_key": "sk-test", "base_url": "https://bps.example",
			"openai_capabilities": []string{"chat_completions", "bps_files"},
			"bps_files_contract":  "bps-files/1", "bps_files_control_url": "https://bps.example",
			"bps_files_service_key": "service-test", "bps_files_backend_id": "backend-test",
			"bps_files_subject_fingerprint":   "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			"bps_files_credential_generation": "generation-test",
		},
		Extra: map[string]any{openai_compat.ExtraKeyResponsesMode: string(openai_compat.ResponsesSupportModeForceResponses)},
	}
}

func TestManagedFileSchedulerPinsRequiredAccountWithoutChangingSticky(t *testing.T) {
	for _, advanced := range []string{"false", "true"} {
		t.Run(advanced, func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			groupID := int64(81001)
			accounts := []Account{managedSchedulerTestAccount(81002, groupID), managedSchedulerTestAccount(81003, groupID)}
			cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:old-sticky": 81002}}
			svc := &OpenAIGatewayService{
				accountRepo: managedSchedulerAccountRepo{accounts: accounts}, cfg: &config.Config{}, cache: cache,
				rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService(advanced),
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
			}
			selection, _, err := svc.SelectAccountWithSchedulerForManagedFiles(context.Background(), &groupID, "old-sticky", "gpt-5.5", 81003)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.Equal(t, int64(81003), selection.Account.ID)
			require.True(t, selection.Acquired)
			require.Nil(t, selection.WaitPlan)
			selection.ReleaseFunc()
			require.Equal(t, int64(81002), cache.sessionBindings["openai:old-sticky"])
			require.Empty(t, cache.deletedSessions)
		})
	}
}

func TestManagedFileSchedulerRequiresConfirmedNativeResponsesRoute(t *testing.T) {
	groupID := int64(82001)
	a := managedSchedulerTestAccount(82002, groupID)
	a.Extra = nil
	svc := &OpenAIGatewayService{accountRepo: managedSchedulerAccountRepo{accounts: []Account{a}}, cfg: &config.Config{}}
	selection, _, err := svc.SelectAccountWithSchedulerForManagedFiles(context.Background(), &groupID, "", "gpt-5.5", a.ID)
	require.Error(t, err)
	require.Nil(t, selection)
}

func TestManagedFileSchedulerRejectsUnavailableOwnerWithoutFallback(t *testing.T) {
	mutations := map[string]func(*Account){
		"paused":              func(a *Account) { a.Schedulable = false },
		"moved_group":         func(a *Account) { a.GroupIDs = []int64{999} },
		"expired":             func(a *Account) { at := time.Now().Add(-time.Minute); a.ExpiresAt = &at },
		"missing_capability":  func(a *Account) { delete(a.Credentials, "openai_capabilities") },
		"empty_capability":    func(a *Account) { a.Credentials["openai_capabilities"] = []string{} },
		"revoked_capability":  func(a *Account) { a.Credentials["openai_capabilities"] = []string{"chat_completions"} },
		"unsupported_model":   func(a *Account) { a.Credentials["model_mapping"] = map[string]any{"gpt-4o": "gpt-4o"} },
		"missing_contract":    func(a *Account) { delete(a.Credentials, "bps_files_contract") },
		"missing_service_key": func(a *Account) { delete(a.Credentials, "bps_files_service_key") },
		"invalid_identity":    func(a *Account) { a.Credentials["bps_files_subject_fingerprint"] = "other" },
		"raw_relay":           func(a *Account) { a.Extra["openai_raw_relay"] = true },
		"passthrough":         func(a *Account) { a.Extra["openai_passthrough"] = true },
		"websocket":           func(a *Account) { a.Extra["openai_apikey_responses_websockets_v2_enabled"] = true },
		"quality_guard":       func(a *Account) { a.Extra["openai_quality_guard_enabled"] = true },
	}
	for _, advanced := range []string{"false", "true"} {
		for name, mutate := range mutations {
			t.Run(advanced+"/"+name, func(t *testing.T) {
				resetOpenAIAdvancedSchedulerSettingCacheForTest()
				groupID := int64(83001)
				owner := managedSchedulerTestAccount(83002, groupID)
				mutate(&owner)
				var acquired []int64
				svc := &OpenAIGatewayService{
					accountRepo: managedSchedulerAccountRepo{accounts: []Account{owner, managedSchedulerTestAccount(83003, groupID)}},
					cfg:         &config.Config{RunMode: config.RunModeSimple}, rateLimitService: newOpenAIAdvancedSchedulerRateLimitService(advanced),
					concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquiredIDs: &acquired}),
				}
				selection, _, err := svc.SelectAccountWithSchedulerForManagedFiles(context.Background(), &groupID, "", "gpt-5.5", owner.ID)
				require.Error(t, err)
				require.Nil(t, selection)
				require.Empty(t, acquired)
			})
		}
	}
}

func TestManagedFileSchedulerBusyDoesNotWaitOrFallback(t *testing.T) {
	groupID := int64(84001)
	var acquired []int64
	svc := &OpenAIGatewayService{
		accountRepo:        managedSchedulerAccountRepo{accounts: []Account{managedSchedulerTestAccount(84002, groupID), managedSchedulerTestAccount(84003, groupID)}},
		cfg:                &config.Config{},
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquiredIDs: &acquired, acquireResults: map[int64]bool{84002: false}}),
	}
	selection, _, err := svc.SelectAccountWithSchedulerForManagedFiles(context.Background(), &groupID, "", "gpt-5.5", 84002)
	require.Error(t, err)
	require.Nil(t, selection)
	require.Equal(t, []int64{84002}, acquired)
}

func TestManagedFileSchedulerFirstUseOnlySelectsExplicitBPSBackend(t *testing.T) {
	for _, advanced := range []string{"false", "true"} {
		t.Run(advanced, func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			groupID := int64(85001)
			ordinary := managedSchedulerTestAccount(85002, groupID)
			ordinary.Credentials["openai_capabilities"] = []string{"chat_completions"}
			ordinary.Priority = -100
			var acquired, released []int64
			cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:old-sticky": ordinary.ID}}
			svc := &OpenAIGatewayService{
				accountRepo: managedSchedulerAccountRepo{accounts: []Account{ordinary, managedSchedulerTestAccount(85003, groupID)}},
				cfg:         &config.Config{}, cache: cache, rateLimitService: newOpenAIAdvancedSchedulerRateLimitService(advanced),
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquiredIDs: &acquired, releasedIDs: &released}),
			}
			selection, _, err := svc.SelectAccountWithSchedulerForManagedFiles(context.Background(), &groupID, "old-sticky", "gpt-5.5", 0)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.Equal(t, int64(85003), selection.Account.ID)
			require.NotEmpty(t, acquired)
			for _, id := range acquired {
				require.Equal(t, int64(85003), id)
			}
			require.Len(t, released, len(acquired)-1)
			require.Equal(t, int64(85002), cache.sessionBindings["openai:old-sticky"])
			require.Empty(t, cache.deletedSessions)
			selection.ReleaseFunc()
		})
	}
}

type managedSchedulerMutatingConcurrencyCache struct {
	schedulerTestConcurrencyCache
	afterAcquire func()
}

func (c managedSchedulerMutatingConcurrencyCache) AcquireAccountSlot(ctx context.Context, accountID int64, maxConcurrency int, requestID string) (bool, error) {
	acquired, err := c.schedulerTestConcurrencyCache.AcquireAccountSlot(ctx, accountID, maxConcurrency, requestID)
	if acquired && err == nil && c.afterAcquire != nil {
		c.afterAcquire()
	}
	return acquired, err
}

func TestManagedFileSchedulerReleasesSlotWhenOwnerChanges(t *testing.T) {
	for name, change := range map[string]func(*Account){
		"paused":             func(a *Account) { a.Schedulable = false },
		"moved":              func(a *Account) { a.GroupIDs = []int64{999} },
		"capability_revoked": func(a *Account) { delete(a.Credentials, "openai_capabilities") },
		"backend_changed":    func(a *Account) { a.Credentials["bps_files_backend_id"] = "other-backend" },
		"subject_changed": func(a *Account) {
			a.Credentials["bps_files_subject_fingerprint"] = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
		},
		"generation_changed":  func(a *Account) { a.Credentials["bps_files_credential_generation"] = "generation-next" },
		"service_key_changed": func(a *Account) { a.Credentials["bps_files_service_key"] = "service-next" },
		"control_url_changed": func(a *Account) { a.Credentials["bps_files_control_url"] = "https://other.example" },
		"api_key_changed":     func(a *Account) { a.Credentials["api_key"] = "key-next" },
		"base_url_changed":    func(a *Account) { a.Credentials["base_url"] = "https://other.example" },
		"concurrency_changed": func(a *Account) { a.Concurrency = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			groupID := int64(86001)
			accounts := []Account{managedSchedulerTestAccount(86002, groupID), managedSchedulerTestAccount(86003, groupID)}
			var acquired, released []int64
			svc := &OpenAIGatewayService{
				accountRepo: managedSchedulerAccountRepo{accounts: accounts}, cfg: &config.Config{},
				concurrencyService: NewConcurrencyService(managedSchedulerMutatingConcurrencyCache{
					schedulerTestConcurrencyCache: schedulerTestConcurrencyCache{acquiredIDs: &acquired, releasedIDs: &released},
					afterAcquire:                  func() { change(&accounts[0]) },
				}),
			}
			selection, _, err := svc.SelectAccountWithSchedulerForManagedFiles(context.Background(), &groupID, "", "gpt-5.5", 86002)
			require.Error(t, err)
			require.Nil(t, selection)
			require.Equal(t, []int64{86002}, acquired)
			require.Equal(t, []int64{86002}, released)
		})
	}
}

func TestManagedFileSchedulerCancellationAndGuardianConflict(t *testing.T) {
	groupID := int64(87001)
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprint(conflict), func(t *testing.T) {
			var acquired []int64
			svc := &OpenAIGatewayService{
				accountRepo: managedSchedulerAccountRepo{accounts: []Account{managedSchedulerTestAccount(87002, groupID), managedSchedulerTestAccount(87003, groupID)}},
				cfg:         &config.Config{}, cache: &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:parent": 87003}},
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquiredIDs: &acquired}),
			}
			ctx := context.Background()
			if conflict {
				ctx = context.WithValue(ctx, openAIGuardianParentAffinityContextKey{}, openAIGuardianParentAffinity{currentSessionHash: "parent"})
			} else {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			selection, _, err := svc.SelectAccountWithSchedulerForManagedFiles(ctx, &groupID, "", "gpt-5.5", 87002)
			require.Error(t, err)
			require.Nil(t, selection)
			require.Empty(t, acquired)
		})
	}
}

func TestManagedFileSchedulerNeverBypassesProxyQuarantine(t *testing.T) {
	groupID, proxyID := int64(88001), int64(88004)
	a := managedSchedulerTestAccount(88002, groupID)
	a.ProxyID = &proxyID
	for _, owner := range []int64{0, a.ID} {
		for _, inheritedBypass := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/%t", owner, inheritedBypass), func(t *testing.T) {
				var acquired []int64
				svc := &OpenAIGatewayService{
					accountRepo: managedSchedulerAccountRepo{accounts: []Account{a}}, cfg: &config.Config{},
					concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquiredIDs: &acquired}),
				}
				svc.openaiProxyStreamCircuit = newOpenAIProxyStreamCircuit(openAIProxyStreamCircuitSettings{
					failureThreshold: 1, failureWindow: time.Minute, quarantineTTL: time.Minute, maxEntries: 16,
				})
				svc.openaiProxyStreamCircuit.recordFailure(proxyID, time.Now())
				ctx := context.Background()
				if inheritedBypass {
					ctx = withOpenAIProxyStreamQuarantineBypass(ctx)
				}
				selection, _, err := svc.SelectAccountWithSchedulerForManagedFiles(ctx, &groupID, "", "gpt-5.5", owner)
				require.Error(t, err)
				require.Nil(t, selection)
				require.Empty(t, acquired)
			})
		}
	}
}

// The migration asks the repository for a capability-independent first-use catalog.
type managedSchedulerAccountRepo struct {
 AccountRepository
 accounts []Account
}
func (r managedSchedulerAccountRepo) GetByID(ctx context.Context,id int64)(*Account,error){
 return (schedulerTestOpenAIAccountRepo{accounts:r.accounts}).GetByID(ctx,id)
}
func (r managedSchedulerAccountRepo) ListModelAvailabilityCandidates(ctx context.Context,group *int64,platforms []string,_ bool)([]Account,error){
 if err:=ctx.Err();err!=nil{return nil,err}
 return r.accounts,nil
}
