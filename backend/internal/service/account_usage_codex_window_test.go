package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGetUsageForAccount_OpenAIMonthlyQuotaKeepsLocalWindows(t *testing.T) {
	now := time.Now()
	account := &Account{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
		"codex_7d_used_percent":   12.5,
		"codex_7d_window_minutes": 43200,
		"codex_7d_reset_at":       now.Add(29 * 24 * time.Hour).Format(time.RFC3339),
	}}
	repo := &bpsWindowStatsRepoStub{}
	svc := &AccountUsageService{usageLogRepo: repo}

	usage, err := svc.GetUsageForAccount(context.Background(), account, false)
	require.NoError(t, err)
	require.NotNil(t, usage.ThirtyDay, "a 30-day upstream window must not be labelled 7d")
	require.Equal(t, 12.5, usage.ThirtyDay.Utilization)
	require.False(t, usage.ThirtyDay.QuotaUnknown)
	require.True(t, usage.FiveHour.QuotaUnknown)
	require.True(t, usage.SevenDay.QuotaUnknown)
	require.Equal(t, int64(5), usage.FiveHour.WindowStats.Requests)
	require.Equal(t, int64(70), usage.SevenDay.WindowStats.Requests)
	require.Equal(t, int64(70), usage.ThirtyDay.WindowStats.Requests)
	require.Len(t, repo.starts, 3)
	require.WithinDuration(t, now.Add(-24*time.Hour), repo.starts[2], 2*time.Second)
}

func TestMonthlyCodexSnapshotDoesNotProbeForMissingShortWindows(t *testing.T) {
	now := time.Now()
	account := &Account{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
		"codex_usage_updated_at": now.Format(time.RFC3339),
	}}
	usage := &UsageInfo{ThirtyDay: &UsageProgress{Utilization: 10}}
	require.False(t, shouldRefreshOpenAICodexSnapshot(account, usage, now), "a known monthly quota is not a missing snapshot")
}
