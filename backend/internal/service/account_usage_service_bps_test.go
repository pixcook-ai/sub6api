package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

// bpsWindowStatsRepoStub returns different stats for the 5h and 7d windows and
// records every window start it was asked for.
type bpsWindowStatsRepoStub struct {
	usageBatchLogRepoStub
	starts []time.Time
}

func (r *bpsWindowStatsRepoStub) GetAccountWindowStats(_ context.Context, _ int64, start time.Time) (*usagestats.AccountStats, error) {
	r.starts = append(r.starts, start)
	if time.Since(start) < 6*time.Hour {
		return &usagestats.AccountStats{Requests: 5, Tokens: 500, Cost: 1.5, UserCost: 1.0}, nil
	}
	return &usagestats.AccountStats{Requests: 70, Tokens: 7000, Cost: 21, UserCost: 14}, nil
}

func bpsUsageAccount() *Account {
	return &Account{ID: 2, Platform: PlatformOpenAIBPS, Type: AccountTypeOAuth}
}

func TestGetOpenAIBPSUsage_ThirtyDayQuota(t *testing.T) {
	now := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"rate_limit": map[string]any{
			"primary_window": map[string]any{"used_percent": 0, "limit_window_seconds": 2592000, "reset_at": now.Add(30 * 24 * time.Hour).Unix()},
		}})
	}))
	defer srv.Close()
	account := bpsUsageAccount()
	account.Credentials = map[string]any{"access_token": "token", "chatgpt_account_id": "workspace"}
	svc := &AccountUsageService{openAIQuotaService: &OpenAIQuotaService{privacyClientFactory: newQuotaRedirectingFactory(srv)}}
	usage, err := svc.GetUsageForAccount(context.Background(), account, true)
	require.NoError(t, err)
	require.NotNil(t, usage.ThirtyDay)
	require.False(t, usage.ThirtyDay.QuotaUnknown)
	require.Zero(t, usage.ThirtyDay.Utilization)
	require.Empty(t, usage.ErrorCode)
	require.WithinDuration(t, now.Add(30*24*time.Hour), *usage.ThirtyDay.ResetsAt, time.Second)

	var cached bpsUsageSnapshot
	data, err := json.Marshal(account.Extra[bpsUsageSnapshotKey])
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &cached))
	cached.FetchedAt = now.Add(-openAIProbeCacheTTL - time.Minute)
	account.Extra[bpsUsageSnapshotKey] = cached
	svc.openAIQuotaService = nil
	usage, err = svc.GetUsageForAccount(context.Background(), account, true)
	require.NoError(t, err)
	require.NotNil(t, usage.ThirtyDay)
	require.True(t, usage.ThirtyDay.QuotaUnknown)
	require.Equal(t, "quota_unavailable", usage.ErrorCode)
}

func TestGetOpenAIBPSUsage_ReportsLocalWindowsWhenQuotaUnavailable(t *testing.T) {
	repo := &bpsWindowStatsRepoStub{}
	svc := &AccountUsageService{usageLogRepo: repo}

	usage, err := svc.getOpenAIBPSUsage(context.Background(), bpsUsageAccount())
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.Equal(t, "quota_unavailable", usage.ErrorCode)
	require.True(t, usage.FiveHour.QuotaUnknown)
	require.True(t, usage.SevenDay.QuotaUnknown)

	require.NotNil(t, usage.FiveHour)
	require.NotNil(t, usage.FiveHour.WindowStats)
	require.Equal(t, int64(5), usage.FiveHour.WindowStats.Requests)
	require.Equal(t, int64(500), usage.FiveHour.WindowStats.Tokens)

	require.NotNil(t, usage.SevenDay)
	require.NotNil(t, usage.SevenDay.WindowStats)
	require.Equal(t, int64(70), usage.SevenDay.WindowStats.Requests)
	require.Equal(t, 14.0, usage.SevenDay.WindowStats.UserCost)

	require.Len(t, repo.starts, 2)
	require.WithinDuration(t, time.Now().Add(-5*time.Hour), repo.starts[0], time.Minute)
	require.WithinDuration(t, time.Now().Add(-7*24*time.Hour), repo.starts[1], time.Minute)
}

func TestGetOpenAIBPSUsage_WithoutUsageLogReturnsUnknownQuota(t *testing.T) {
	svc := &AccountUsageService{}

	usage, err := svc.getOpenAIBPSUsage(context.Background(), bpsUsageAccount())
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.True(t, usage.FiveHour.QuotaUnknown)
	require.True(t, usage.SevenDay.QuotaUnknown)
}

func TestGetUsageForAccount_BPSReadsQuotaWithoutResetOrCredentialMutation(t *testing.T) {
	now := time.Now()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "Bearer bps-token", r.Header.Get("Authorization"))
		require.Equal(t, "workspace", r.Header.Get("chatgpt-account-id"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"rate_limit": map[string]any{
			"primary_window":   map[string]any{"used_percent": 37.5, "limit_window_seconds": 604800, "reset_at": now.Add(48 * time.Hour).Unix()},
			"secondary_window": map[string]any{"used_percent": 0, "limit_window_seconds": 18000, "reset_after_seconds": 3600},
		}})
	}))
	defer srv.Close()
	account := bpsUsageAccount()
	account.Credentials = map[string]any{"access_token": "bps-token", "chatgpt_account_id": "workspace"}
	account.Schedulable = false
	stats := &bpsWindowStatsRepoStub{}
	svc := &AccountUsageService{usageLogRepo: stats, openAIQuotaService: &OpenAIQuotaService{privacyClientFactory: newQuotaRedirectingFactory(srv)}, cache: NewUsageCache()}
	usage, err := svc.GetUsageForAccount(context.Background(), account, false)
	require.NoError(t, err)
	require.Empty(t, usage.Error)
	require.False(t, usage.FiveHour.QuotaUnknown)
	require.Zero(t, usage.FiveHour.Utilization, "a real zero must stay distinguishable from unknown quota")
	require.False(t, usage.SevenDay.QuotaUnknown)
	require.Equal(t, 37.5, usage.SevenDay.Utilization)
	require.WithinDuration(t, now.Add(48*time.Hour), *usage.SevenDay.ResetsAt, time.Second)
	require.NotNil(t, usage.FiveHour.WindowStats)
	require.False(t, account.Schedulable)
	require.Equal(t, "bps-token", account.GetCredential("access_token"))
	require.Equal(t, []string{"/backend-api/wham/usage"}, paths)

	_, err = svc.GetUsageForAccount(context.Background(), account, false)
	require.NoError(t, err)
	require.Len(t, paths, 1, "fresh quota is cached")
	_, err = svc.GetUsageForAccount(context.Background(), account, true)
	require.NoError(t, err)
	require.Len(t, paths, 2, "manual refresh bypasses cache")
}

func TestGetOpenAIBPSUsage_PartialAndRejectedQuota(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status        int
		body          string
		knownFiveHour bool
	}{
		{"partial", 200, `{"rate_limit":{"primary_window":{"used_percent":12,"limit_window_seconds":18000}}}`, true},
		{"missing_percentage", 200, `{"rate_limit":{"primary_window":{"limit_window_seconds":18000}}}`, false},
		{"rejected", 403, `{"error":"sensitive upstream response"}`, false},
		{"unsupported_window", 200, `{"rate_limit":{"primary_window":{"used_percent":12,"limit_window_seconds":60}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			account := bpsUsageAccount()
			account.Credentials = map[string]any{"access_token": "token", "chatgpt_account_id": "workspace"}
			svc := &AccountUsageService{openAIQuotaService: &OpenAIQuotaService{privacyClientFactory: newQuotaRedirectingFactory(srv)}}
			usage, err := svc.GetUsageForAccount(context.Background(), account, true)
			require.NoError(t, err)
			require.Equal(t, !tc.knownFiveHour, usage.FiveHour.QuotaUnknown)
			require.True(t, usage.SevenDay.QuotaUnknown)
			require.NotContains(t, usage.Error, "sensitive")
			require.Equal(t, "quota_unavailable", usage.ErrorCode, "missing windows must retain an explicit degradation signal")
		})
	}
}

func TestGetOpenAIBPSUsage_FailedRefreshHidesStaleQuotaAndKeepsDegradationWhileThrottled(t *testing.T) {
	for _, hasSnapshot := range []bool{false, true} {
		t.Run(fmt.Sprintf("cached_%t", hasSnapshot), func(t *testing.T) {
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.WriteHeader(http.StatusForbidden)
			}))
			defer srv.Close()
			account := bpsUsageAccount()
			account.Credentials = map[string]any{"access_token": "token", "chatgpt_account_id": "workspace"}
			if hasSnapshot {
				account.Extra = map[string]any{bpsUsageSnapshotKey: bpsUsageSnapshot{
					CredentialIdentity: OpenAIBPSCredentialIdentity(OpenAIBPSCredentialSnapshotFromAccount(account)),
					FetchedAt:          time.Now().Add(-openAIProbeCacheTTL - time.Minute),
					FiveHour:           &UsageProgress{Utilization: 61},
					SevenDay:           &UsageProgress{Utilization: 43},
				}}
			}
			stats := &bpsWindowStatsRepoStub{}
			svc := &AccountUsageService{usageLogRepo: stats, cache: NewUsageCache(), openAIQuotaService: &OpenAIQuotaService{privacyClientFactory: newQuotaRedirectingFactory(srv)}}
			for attempt := 0; attempt < 2; attempt++ {
				usage, err := svc.GetUsageForAccount(context.Background(), account)
				require.NoError(t, err)
				require.True(t, usage.FiveHour.QuotaUnknown)
				require.True(t, usage.SevenDay.QuotaUnknown)
				require.Equal(t, "quota_unavailable", usage.ErrorCode)
				require.NotEmpty(t, usage.Error)
				require.NotNil(t, usage.FiveHour.WindowStats)
				require.NotNil(t, usage.SevenDay.WindowStats)
			}
			require.Equal(t, 1, requests, "the second failed read is throttled while degradation stays visible")
		})
	}
}

func TestGetOpenAIBPSUsage_ManualRefreshFailureRetainsFreshCache(t *testing.T) {
	reject := false
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if reject {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":18,"limit_window_seconds":18000},"secondary_window":{"used_percent":27,"limit_window_seconds":604800}}}`))
	}))
	defer srv.Close()
	account := bpsUsageAccount()
	account.Credentials = map[string]any{"access_token": "token", "chatgpt_account_id": "workspace"}
	svc := &AccountUsageService{cache: NewUsageCache(), openAIQuotaService: &OpenAIQuotaService{privacyClientFactory: newQuotaRedirectingFactory(srv)}}
	usage, err := svc.GetUsageForAccount(context.Background(), account)
	require.NoError(t, err)
	require.Empty(t, usage.ErrorCode)
	reject = true
	usage, err = svc.GetUsageForAccount(context.Background(), account, true)
	require.NoError(t, err)
	require.Equal(t, "quota_unavailable", usage.ErrorCode)
	usage, err = svc.GetUsageForAccount(context.Background(), account)
	require.NoError(t, err)
	require.Empty(t, usage.ErrorCode, "a complete snapshot is still usable within its freshness TTL")
	require.False(t, usage.FiveHour.QuotaUnknown)
	require.False(t, usage.SevenDay.QuotaUnknown)
	require.Equal(t, 2, requests, "a failed manual read does not cause an immediate retry")

	reject = false
	usage, err = svc.GetUsageForAccount(context.Background(), account, true)
	require.NoError(t, err)
	require.Empty(t, usage.ErrorCode)
	require.False(t, usage.FiveHour.QuotaUnknown)
	require.False(t, usage.SevenDay.QuotaUnknown)
}

func TestGetOpenAIBPSUsage_CachedQuotaBoundToCredentialsAndWindow(t *testing.T) {
	account := bpsUsageAccount()
	account.Credentials = map[string]any{"access_token": "token", "chatgpt_account_id": "workspace"}
	now := time.Now()
	expiredReset := now.Add(-time.Minute)
	account.Extra = map[string]any{bpsUsageSnapshotKey: bpsUsageSnapshot{
		CredentialIdentity: OpenAIBPSCredentialIdentity(OpenAIBPSCredentialSnapshotFromAccount(account)),
		FetchedAt:          now,
		FiveHour:           &UsageProgress{Utilization: 70, ResetsAt: &expiredReset},
		SevenDay:           &UsageProgress{Utilization: 12},
	}}
	svc := &AccountUsageService{}
	usage, err := svc.GetUsageForAccount(context.Background(), account)
	require.NoError(t, err)
	require.True(t, usage.FiveHour.QuotaUnknown, "expired windows must not claim a current percentage")
	require.False(t, usage.SevenDay.QuotaUnknown)
	require.Equal(t, 12.0, usage.SevenDay.Utilization)

	account.Credentials["chatgpt_account_id"] = "replacement-workspace"
	usage, err = svc.GetUsageForAccount(context.Background(), account)
	require.NoError(t, err)
	require.True(t, usage.FiveHour.QuotaUnknown)
	require.True(t, usage.SevenDay.QuotaUnknown, "another credential identity cannot reuse the old quota")
}

func TestGetUsageForAccount_RoutesOpenAIBPSToLocalWindows(t *testing.T) {
	repo := &bpsWindowStatsRepoStub{}
	svc := &AccountUsageService{usageLogRepo: repo}

	usage, err := svc.getUsageForAccount(context.Background(), bpsUsageAccount(), true)
	require.NoError(t, err)
	require.NotNil(t, usage.FiveHour)
	require.Equal(t, int64(5), usage.FiveHour.WindowStats.Requests)
	require.NotNil(t, usage.SevenDay)
	require.Equal(t, int64(70), usage.SevenDay.WindowStats.Requests)
	require.Len(t, repo.starts, 2, "openai_bps must use only the two local window queries")
}
