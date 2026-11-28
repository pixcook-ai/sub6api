//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestTiboBPSAutoGroupIsolationAndRecovery(t *testing.T) {
	f := newTiboGroupFixtureForPlatform(t, service.PlatformOpenAIBPS)
	f.settings(service.TiboAutomationSettings{Enabled: true, AutoGroupEnabled: true,
		BPSHealthyGroupID: f.healthy, BPSDegradedGroupID: f.welfare,
		PauseOnDegraded: true, AutoResumeOnHealthy: true})
	a := f.account(service.AccountTypeOAuth, service.PlatformOpenAIBPS, f.healthy, f.other)
	require.NoError(t, f.client.Account.UpdateOneID(a.ID).SetSchedulable(false).Exec(f.ctx))
	before := f.memberships(a.ID)
	f.sample(a.ID, "degraded")
	after := f.memberships(a.ID)
	require.NotContains(t, after, f.healthy)
	require.Equal(t, before[f.healthy].Priority, after[f.welfare].Priority)
	require.Equal(t, before[f.healthy].CreatedAt, after[f.welfare].CreatedAt)
	require.Equal(t, before[f.other], after[f.other])
	isolated, err := f.repo.GetOpenAITiboQualityIsolation(f.ctx, a.ID)
	require.NoError(t, err)
	require.True(t, isolated, "BPS isolation is read from durable authority")
	require.Equal(t, true, f.cache.accounts[a.ID].Extra[service.OpenAITiboQualityIsolatedExtraKey])
	f.sample(a.ID, "healthy")
	require.Equal(t, before, f.memberships(a.ID))
	current, err := f.repo.GetByID(f.ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, false, current.Extra[service.OpenAITiboQualityIsolatedExtraKey])
	require.False(t, current.Schedulable, "automatic recovery preserves the administrator's pause")
	require.Equal(t, a.Credentials, current.Credentials)
	isolated, err = f.repo.GetOpenAITiboQualityIsolation(f.ctx, a.ID)
	require.NoError(t, err)
	require.False(t, isolated)
}

func TestTiboBPSAutoGroupNeverUsesOpenAIPair(t *testing.T) {
	f := newTiboGroupFixture(t)
	bpsHealthy := mustCreateGroup(t, f.client, &service.Group{Name: "bps-healthy-" + t.Name(), Platform: service.PlatformOpenAIBPS})
	bpsDegraded := mustCreateGroup(t, f.client, &service.Group{Name: "bps-degraded-" + t.Name(), Platform: service.PlatformOpenAIBPS})
	for _, id := range []int64{bpsHealthy.ID, bpsDegraded.ID} {
		id := id
		t.Cleanup(func() {
			_, err := integrationDB.ExecContext(context.Background(), "DELETE FROM groups WHERE id=$1", id)
			require.NoError(t, err)
		})
	}
	// Even a stale/cross-platform membership cannot use the legacy OpenAI pair.
	bps := f.account(service.AccountTypeOAuth, service.PlatformOpenAIBPS, f.healthy, bpsHealthy.ID)
	f.sample(bps.ID, "degraded")
	require.Contains(t, f.memberships(bps.ID), f.healthy)
	require.Contains(t, f.memberships(bps.ID), bpsHealthy.ID)
	f.settings(service.TiboAutomationSettings{Enabled: true, AutoGroupEnabled: true,
		HealthyGroupID: f.healthy, DegradedGroupID: f.welfare,
		BPSHealthyGroupID: bpsHealthy.ID, BPSDegradedGroupID: bpsDegraded.ID})
	f.sample(bps.ID, "degraded")
	bpsMemberships := f.memberships(bps.ID)
	require.Contains(t, bpsMemberships, f.healthy, "unrelated legacy membership is preserved")
	require.NotContains(t, bpsMemberships, f.welfare)
	require.NotContains(t, bpsMemberships, bpsHealthy.ID)
	require.Contains(t, bpsMemberships, bpsDegraded.ID)
	openai := f.account(service.AccountTypeOAuth, service.PlatformOpenAI, f.healthy)
	f.sample(openai.ID, "degraded")
	require.Contains(t, f.memberships(openai.ID), f.welfare)
	require.NotContains(t, f.memberships(openai.ID), bpsDegraded.ID)
	// A target that later changes platform cannot accept an automatic migration.
	require.NoError(t, f.client.Group.UpdateOneID(bpsHealthy.ID).SetPlatform(service.PlatformOpenAI).Exec(f.ctx))
	before := f.memberships(bps.ID)
	f.sample(bps.ID, "healthy")
	require.Equal(t, before, f.memberships(bps.ID))
}

func TestTiboBPSQualityIsolationWithoutGroupsAndManualClearFence(t *testing.T) {
	f := newTiboGroupFixtureForPlatform(t, service.PlatformOpenAIBPS)
	f.settings(service.TiboAutomationSettings{Enabled: true, PauseOnDegraded: true})
	a := f.account(service.AccountTypeOAuth, service.PlatformOpenAIBPS, f.other)
	require.NoError(t, f.client.Account.UpdateOneID(a.ID).SetSchedulable(false).Exec(f.ctx))
	f.sample(a.ID, "degraded")
	// Disabling probes retains isolation and cannot bypass the durable read.
	f.settings(service.TiboAutomationSettings{})
	isolated, err := f.repo.GetOpenAITiboQualityIsolation(f.ctx, a.ID)
	require.NoError(t, err)
	require.True(t, isolated)
	started := time.Now()
	require.NoError(t, f.repo.ClearOpenAITiboQualityIsolation(f.ctx, a.ID))
	f.settings(service.TiboAutomationSettings{Enabled: true, PauseOnDegraded: true})
	updates, sample := tiboGroupObservation("degraded", started, time.Now())
	require.NoError(t, f.repo.PersistTiboProbeResult(f.ctx, a.ID, updates, sample))
	current, err := f.repo.GetByID(f.ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, false, current.Extra[service.OpenAITiboQualityIsolatedExtraKey], "an in-flight probe cannot undo manual recovery")
	require.False(t, current.Schedulable)
	f.sample(a.ID, "degraded")
	isolated, err = f.repo.GetOpenAITiboQualityIsolation(f.ctx, a.ID)
	require.NoError(t, err)
	require.True(t, isolated, "a fresh probe still retains isolation authority")
	require.Contains(t, f.memberships(a.ID), f.other)
}

func TestTiboBPSQualityIsolationRejectsNonOAuth(t *testing.T) {
	f := newTiboGroupFixtureForPlatform(t, service.PlatformOpenAIBPS)
	f.settings(service.TiboAutomationSettings{Enabled: true, AutoGroupEnabled: true,
		BPSHealthyGroupID: f.healthy, BPSDegradedGroupID: f.welfare, PauseOnDegraded: true})
	for _, kind := range []string{service.AccountTypeAPIKey, service.AccountTypeSetupToken} {
		a := f.account(kind, service.PlatformOpenAIBPS, f.healthy)
		f.sample(a.ID, "degraded")
		current, err := f.repo.GetByID(f.ctx, a.ID)
		require.NoError(t, err)
		require.NotEqual(t, true, current.Extra[service.OpenAITiboQualityIsolatedExtraKey])
		require.Contains(t, f.memberships(a.ID), f.healthy)
		require.ErrorIs(t, f.repo.ClearOpenAITiboQualityIsolation(f.ctx, a.ID), service.ErrAccountNotFound)
	}
}

func TestTiboBPSProbeRejectsReplacedWorkspace(t *testing.T) {
	f := newTiboGroupFixtureForPlatform(t, service.PlatformOpenAIBPS)
	f.settings(service.TiboAutomationSettings{Enabled: true, AutoGroupEnabled: true,
		BPSHealthyGroupID: f.healthy, BPSDegradedGroupID: f.welfare, PauseOnDegraded: true})
	a := f.account(service.AccountTypeOAuth, service.PlatformOpenAIBPS, f.healthy)
	current, err := f.repo.GetByID(f.ctx, a.ID)
	require.NoError(t, err)
	now := time.Now()
	updates, sample := tiboGroupObservation("degraded", now, now)
	sample.ProbeIdentity = service.OpenAITiboProbeIdentity(current)
	updates[service.OpenAIAstraPolicyExtraKey].(map[string]any)["probe_identity"] = sample.ProbeIdentity
	require.NoError(t, f.client.Account.UpdateOneID(a.ID).SetCredentials(map[string]any{
		"access_token": "fixture-only", "chatgpt_account_id": "replacement-workspace",
	}).Exec(f.ctx))
	require.ErrorIs(t, f.repo.PersistTiboProbeResult(f.ctx, a.ID, updates, sample), service.ErrOpenAITiboProbeIdentityChanged)
	stored, err := f.repo.GetByID(f.ctx, a.ID)
	require.NoError(t, err)
	require.NotContains(t, stored.Extra, service.OpenAIAstraPolicyExtraKey)
	require.NotEqual(t, true, stored.Extra[service.OpenAITiboQualityIsolatedExtraKey])
	require.Contains(t, f.memberships(a.ID), f.healthy)
}
