package service

import (
	"context"
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

func TestGetOpenAIBPSUsage_ReportsLocalWindowsOnly(t *testing.T) {
	repo := &bpsWindowStatsRepoStub{}
	svc := &AccountUsageService{usageLogRepo: repo}

	usage, err := svc.getOpenAIBPSUsage(context.Background(), bpsUsageAccount())
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.Empty(t, usage.Error, "local statistics must never carry a stale-quota error")

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

func TestGetOpenAIBPSUsage_WithoutUsageLogReturnsEmptyUsage(t *testing.T) {
	svc := &AccountUsageService{}

	usage, err := svc.getOpenAIBPSUsage(context.Background(), bpsUsageAccount())
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.Nil(t, usage.FiveHour)
	require.Nil(t, usage.SevenDay)
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
