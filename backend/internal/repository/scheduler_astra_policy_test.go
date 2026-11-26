//go:build unit

package repository

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
)

func tiboSchedulerAccount(kind string, isolated bool, sampledAt, changedAt time.Time) service.Account {
	return service.Account{ID: 8819, Platform: service.PlatformOpenAI, Type: kind, Schedulable: true,
		Extra: map[string]any{
			service.OpenAIAstraPolicyExtraKey: map[string]any{
				"model":          "gpt-6-astra",
				"recent_samples": []map[string]any{{"verdict": "healthy", "at": sampledAt.UTC().Format(time.RFC3339Nano)}},
			},
			service.OpenAITiboQualityIsolatedExtraKey:    isolated,
			service.OpenAITiboQualityIsolationAtExtraKey: changedAt.UTC().Format(time.RFC3339Nano),
		},
	}
}

func TestSchedulerTiboCacheRejectsOlderIsolationStateInBothProjections(t *testing.T) {
	for _, kind := range []string{service.AccountTypeOAuth, service.AccountTypeSetupToken, service.AccountTypeAPIKey} {
		for _, wantIsolated := range []bool{true, false} {
			t.Run(kind+strconv.FormatBool(wantIsolated), func(t *testing.T) {
				cache := newSchedulerCacheUnit(t)
				ctx := context.Background()
				oldAt := time.Now().Add(-time.Minute)
				newAt := oldAt.Add(time.Second)
				stale := tiboSchedulerAccount(kind, !wantIsolated, oldAt, oldAt)
				current := tiboSchedulerAccount(kind, wantIsolated, oldAt, newAt)
				require.NoError(t, cache.SetAccount(ctx, &current))
				require.NoError(t, cache.SetAccount(ctx, &stale))
				loaded, err := cache.GetAccount(ctx, current.ID)
				require.NoError(t, err)
				require.NotNil(t, loaded)
				require.Equal(t, wantIsolated, loaded.Extra[service.OpenAITiboQualityIsolatedExtraKey])
				raw, err := cache.rdb.Get(ctx, schedulerAccountMetaKey(strconv.FormatInt(current.ID, 10))).Bytes()
				require.NoError(t, err)
				var metadata service.Account
				require.NoError(t, json.Unmarshal(raw, &metadata))
				require.Equal(t, wantIsolated, metadata.Extra[service.OpenAITiboQualityIsolatedExtraKey])
			})
		}
	}
}

func TestSchedulerTiboMetadataRetainsProbeAndIsolation(t *testing.T) {
	at := time.Now().Add(-time.Minute)
	account := tiboSchedulerAccount(service.AccountTypeOAuth, true, at, at)
	account.Extra["private_unrelated_value"] = "secret"
	projected := buildSchedulerMetadataAccount(account)
	require.Contains(t, projected.Extra, service.OpenAIAstraPolicyExtraKey)
	require.Equal(t, true, projected.Extra[service.OpenAITiboQualityIsolatedExtraKey])
	require.Equal(t, at.UTC().Format(time.RFC3339Nano), projected.Extra[service.OpenAITiboQualityIsolationAtExtraKey])
	require.NotContains(t, projected.Extra, "private_unrelated_value")
}
