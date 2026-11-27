//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	dbent "github.com/MACOS-DO/sub4api/ent"
	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
)

type tiboGroupFixture struct {
	t                       *testing.T
	ctx                     context.Context
	client                  *dbent.Client
	repo                    *accountRepository
	cache                   *schedulerCacheRecorder
	welfare, healthy, other int64
}

func newTiboGroupFixture(t *testing.T) *tiboGroupFixture {
	return newTiboGroupFixtureForPlatform(t, service.PlatformOpenAI)
}

func newTiboGroupFixtureForPlatform(t *testing.T, platform string) *tiboGroupFixture {
	t.Helper()
	f := &tiboGroupFixture{t: t, ctx: context.Background(), client: testEntClient(t), cache: &schedulerCacheRecorder{}}
	f.repo = newAccountRepositoryWithSQL(f.client, integrationDB, f.cache)
	for i, name := range []string{"custom-degraded-" + t.Name(), "custom-healthy-" + t.Name(), "tibo-unrelated-" + t.Name()} {
		g := mustCreateGroup(t, f.client, &service.Group{Name: name, Platform: platform})
		id := g.ID
		t.Cleanup(func() {
			_, err := integrationDB.ExecContext(context.Background(), "DELETE FROM groups WHERE id=$1", id)
			require.NoError(t, err)
		})
		switch i {
		case 0:
			f.welfare = id
		case 1:
			f.healthy = id
		case 2:
			f.other = id
		}
	}
	settings := NewSettingRepository(f.client)
	previous, previousErr := settings.GetValue(f.ctx, service.SettingKeyOpenAITiboSettings)
	if previousErr != nil {
		require.ErrorIs(t, previousErr, service.ErrSettingNotFound)
	}
	t.Cleanup(func() {
		if previousErr == nil {
			require.NoError(t, settings.Set(context.Background(), service.SettingKeyOpenAITiboSettings, previous))
		} else {
			require.NoError(t, settings.Delete(context.Background(), service.SettingKeyOpenAITiboSettings))
		}
	})
	configuration := service.TiboAutomationSettings{Enabled: true, AutoGroupEnabled: true}
	if platform == service.PlatformOpenAIBPS {
		configuration.BPSHealthyGroupID, configuration.BPSDegradedGroupID = f.healthy, f.welfare
	} else {
		configuration.HealthyGroupID, configuration.DegradedGroupID = f.healthy, f.welfare
	}
	f.settings(configuration)
	return f
}

func (f *tiboGroupFixture) settings(settings service.TiboAutomationSettings) {
	f.t.Helper()
	raw, err := json.Marshal(settings)
	require.NoError(f.t, err)
	require.NoError(f.t, NewSettingRepository(f.client).Set(f.ctx, service.SettingKeyOpenAITiboSettings, string(raw)))
}

func (f *tiboGroupFixture) account(kind, platform string, groups ...int64) *service.Account {
	f.t.Helper()
	a := mustCreateAccount(f.t, f.client, &service.Account{Name: "tibo-test", Platform: platform, Type: kind,
		Extra: map[string]any{"unrelated": "keep"}, Credentials: map[string]any{"access_token": "fixture-only"}})
	f.t.Cleanup(func() {
		_, err := integrationDB.ExecContext(context.Background(), "DELETE FROM accounts WHERE id=$1", a.ID)
		require.NoError(f.t, err)
	})
	for i, g := range groups {
		_, err := f.client.AccountGroup.Create().SetAccountID(a.ID).SetGroupID(g).SetPriority(77 + i).SetCreatedAt(time.Now().Add(-time.Hour)).Save(f.ctx)
		require.NoError(f.t, err)
	}
	return a
}

func tiboGroupObservation(verdict string, start, finish time.Time) (map[string]any, service.TiboAutoGroupSample) {
	return map[string]any{service.OpenAIAstraPolicyExtraKey: map[string]any{
		"model": "gpt-6-astra", "verdict": "healthy", "checked_at": finish.UTC().Format(time.RFC3339Nano),
		"recent_samples": []map[string]any{{"verdict": verdict, "at": finish.UTC().Format(time.RFC3339Nano)}},
	}}, service.TiboAutoGroupSample{Verdict: verdict, StartedAt: start, CompletedAt: finish}
}

func (f *tiboGroupFixture) sample(id int64, verdict string) {
	f.t.Helper()
	now := time.Now()
	updates, sample := tiboGroupObservation(verdict, now.Add(-time.Millisecond), now)
	require.NoError(f.t, f.repo.PersistTiboProbeResult(f.ctx, id, updates, sample))
}

func (f *tiboGroupFixture) memberships(id int64) map[int64]service.AccountGroup {
	f.t.Helper()
	a, err := f.repo.GetByID(f.ctx, id)
	require.NoError(f.t, err)
	out := map[int64]service.AccountGroup{}
	for _, g := range a.AccountGroups {
		g.Account = nil
		g.Group = nil
		out[g.GroupID] = g
	}
	return out
}

func TestTiboAutoGroupMappingAndMetadata(t *testing.T) {
	f := newTiboGroupFixture(t)
	a := f.account(service.AccountTypeOAuth, service.PlatformOpenAI, f.healthy, f.other)
	_, err := f.client.Account.UpdateOneID(a.ID).SetSchedulable(false).Save(f.ctx)
	require.NoError(t, err)
	before := f.memberships(a.ID)
	f.sample(a.ID, "degraded")
	after := f.memberships(a.ID)
	require.Len(t, after, 2)
	require.NotContains(t, after, f.healthy)
	require.Equal(t, before[f.other], after[f.other])
	require.Equal(t, before[f.healthy].Priority, after[f.welfare].Priority)
	require.Equal(t, before[f.healthy].CreatedAt, after[f.welfare].CreatedAt)
	current, err := f.repo.GetByID(f.ctx, a.ID)
	require.NoError(t, err)
	require.False(t, current.Schedulable)
	require.Equal(t, a.Credentials, current.Credentials)
	require.Equal(t, "keep", current.Extra["unrelated"])
	require.ElementsMatch(t, []int64{f.welfare, f.other}, f.cache.accounts[a.ID].GroupIDs)
	var payload string
	require.NoError(t, integrationDB.QueryRowContext(f.ctx, `SELECT payload::text FROM scheduler_outbox WHERE event_type=$1 AND account_id=$2 ORDER BY id DESC LIMIT 1`, service.SchedulerOutboxEventAccountGroupsChanged, a.ID).Scan(&payload))
	require.Contains(t, payload, fmt.Sprint(f.healthy))
	require.Contains(t, payload, fmt.Sprint(f.welfare))
	f.sample(a.ID, "degraded")
	require.Equal(t, after, f.memberships(a.ID), "same-color sample must not rebuild links")
	f.sample(a.ID, "healthy")
	require.Equal(t, before, f.memberships(a.ID), "round trip preserves priority and creation timestamp")
	both := f.account(service.AccountTypeOAuth, service.PlatformOpenAI, f.welfare, f.healthy, f.other)
	beforeBoth := f.memberships(both.ID)
	f.sample(both.ID, "healthy")
	afterBoth := f.memberships(both.ID)
	require.Len(t, afterBoth, 2)
	require.Equal(t, beforeBoth[f.healthy], afterBoth[f.healthy])
	require.Equal(t, beforeBoth[f.other], afterBoth[f.other])
}

func TestTiboAutoGroupScopeAndUnavailableTargets(t *testing.T) {
	f := newTiboGroupFixture(t)
	for _, tc := range []struct {
		name, kind, platform string
		groups               []int64
	}{
		{"other-only", service.AccountTypeOAuth, service.PlatformOpenAI, []int64{f.other}},
		{"ungrouped", service.AccountTypeOAuth, service.PlatformOpenAI, nil},
		{"api-key", service.AccountTypeAPIKey, service.PlatformOpenAI, []int64{f.healthy, f.other}},
		{"setup", service.AccountTypeSetupToken, service.PlatformOpenAI, []int64{f.healthy}},
		{"claude", service.AccountTypeOAuth, service.PlatformAnthropic, []int64{f.healthy}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := f.account(tc.kind, tc.platform, tc.groups...)
			before := f.memberships(a.ID)
			f.sample(a.ID, "degraded")
			require.Equal(t, before, f.memberships(a.ID))
		})
	}
	a := f.account(service.AccountTypeOAuth, service.PlatformOpenAI, f.healthy)
	before := f.memberships(a.ID)
	_, err := f.client.Group.UpdateOneID(f.welfare).SetStatus(service.StatusDisabled).Save(f.ctx)
	require.NoError(t, err)
	f.sample(a.ID, "degraded")
	require.Equal(t, before, f.memberships(a.ID))
	_, err = f.client.Group.UpdateOneID(f.welfare).SetStatus(service.StatusActive).SetName("renamed-welfare").Save(f.ctx)
	require.NoError(t, err)
	f.sample(a.ID, "degraded")
	afterRename := f.memberships(a.ID)
	require.NotContains(t, afterRename, f.healthy)
	require.Contains(t, afterRename, f.welfare, "renaming a configured group must not disable its ID mapping")
}

func TestTiboAutoGroupOldAndDuplicateSamples(t *testing.T) {
	f := newTiboGroupFixture(t)
	a := f.account(service.AccountTypeOAuth, service.PlatformOpenAI, f.welfare, f.other)
	now := time.Now()
	newUpdate, newSample := tiboGroupObservation("healthy", now.Add(-time.Second), now.Add(-time.Millisecond))
	require.NoError(t, f.repo.PersistTiboProbeResult(f.ctx, a.ID, newUpdate, newSample))
	oldUpdate, oldSample := tiboGroupObservation("degraded", now.Add(-2*time.Second), now)
	require.NoError(t, f.repo.PersistTiboProbeResult(f.ctx, a.ID, oldUpdate, oldSample))
	require.Contains(t, f.memberships(a.ID), f.healthy)
	current, err := f.repo.GetByID(f.ctx, a.ID)
	require.NoError(t, err)
	verdict, _ := service.LatestOpenAIAstraSample(current.Extra[service.OpenAIAstraPolicyExtraKey])
	require.Equal(t, "healthy", verdict, "late old probe must not overwrite newer history")
	require.NoError(t, f.repo.BindGroups(f.ctx, a.ID, []int64{f.welfare, f.other}))
	before := f.memberships(a.ID)
	require.NoError(t, f.repo.PersistTiboProbeResult(f.ctx, a.ID, newUpdate, newSample))
	require.Equal(t, before, f.memberships(a.ID), "duplicate must not override later manual move")
	// Generic historical persistence has no automatic movement authority.
	require.NoError(t, f.repo.UpdateExtra(f.ctx, a.ID, newUpdate))
	require.Equal(t, before, f.memberships(a.ID))
}

func TestTiboAutoGroupManualRemovalWhileProbeWaits(t *testing.T) {
	f := newTiboGroupFixture(t)
	a := f.account(service.AccountTypeOAuth, service.PlatformOpenAI, f.healthy, f.other)
	manual, err := integrationDB.BeginTx(f.ctx, nil)
	require.NoError(t, err)
	defer manual.Rollback()
	var id int64
	require.NoError(t, manual.QueryRowContext(f.ctx, `SELECT group_id FROM account_groups WHERE account_id=$1 AND group_id=$2 FOR UPDATE`, a.ID, f.healthy).Scan(&id))
	now := time.Now()
	updates, sample := tiboGroupObservation("degraded", now.Add(-time.Second), now)
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.repo.PersistTiboProbeResult(ctx, a.ID, updates, sample) }()
	require.Eventually(t, func() bool {
		var waiting bool
		err := integrationDB.QueryRowContext(f.ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT group_id FROM account_groups%')`).Scan(&waiting)
		return err == nil && waiting
	}, 5*time.Second, 10*time.Millisecond, "probe actually blocked on the source membership")
	_, err = manual.ExecContext(f.ctx, `DELETE FROM account_groups WHERE account_id=$1 AND group_id=$2`, a.ID, f.healthy)
	require.NoError(t, err)
	require.NoError(t, manual.Commit())
	require.NoError(t, <-done)
	remaining := f.memberships(a.ID)
	require.Len(t, remaining, 1)
	require.Contains(t, remaining, f.other)
}

func TestTiboAutoGroupOutboxFailureRollsBackAndRetries(t *testing.T) {
	f := newTiboGroupFixture(t)
	a := f.account(service.AccountTypeOAuth, service.PlatformOpenAI, f.healthy, f.other)
	before := f.memberships(a.ID)
	_, err := integrationDB.ExecContext(f.ctx, `CREATE FUNCTION test_tibo_outbox_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.event_type='account_groups_changed' THEN RAISE EXCEPTION 'test injected Tibo notification failure'; END IF;
		RETURN NEW; END $$;
		CREATE TRIGGER test_tibo_outbox_failure BEFORE INSERT ON scheduler_outbox FOR EACH ROW EXECUTE FUNCTION test_tibo_outbox_failure()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(f.ctx, `DROP TRIGGER IF EXISTS test_tibo_outbox_failure ON scheduler_outbox; DROP FUNCTION IF EXISTS test_tibo_outbox_failure()`)
		require.NoError(t, err)
	})
	now := time.Now()
	updates, sample := tiboGroupObservation("degraded", now.Add(-time.Second), now)
	require.ErrorContains(t, f.repo.PersistTiboProbeResult(f.ctx, a.ID, updates, sample), "test injected")
	require.Equal(t, before, f.memberships(a.ID))
	current, err := f.repo.GetByID(f.ctx, a.ID)
	require.NoError(t, err)
	require.NotContains(t, current.Extra, service.OpenAIAstraPolicyExtraKey)
	require.NotContains(t, current.Extra, tiboAutoGroupSampleKey)
	_, err = integrationDB.ExecContext(f.ctx, `DROP TRIGGER test_tibo_outbox_failure ON scheduler_outbox`)
	require.NoError(t, err)
	require.NoError(t, f.repo.PersistTiboProbeResult(f.ctx, a.ID, updates, sample))
	require.Contains(t, f.memberships(a.ID), f.welfare)
}

func TestTiboAutoGroupSkippedSampleIsConsumed(t *testing.T) {
	f := newTiboGroupFixture(t)
	for _, outside := range []bool{false, true} {
		groups := []int64{f.healthy}
		if outside {
			groups = []int64{f.other}
		}
		a := f.account(service.AccountTypeOAuth, service.PlatformOpenAI, groups...)
		if !outside {
			_, err := f.client.Group.UpdateOneID(f.welfare).SetStatus(service.StatusDisabled).Save(f.ctx)
			require.NoError(t, err)
		}
		now := time.Now()
		updates, sample := tiboGroupObservation("degraded", now.Add(-time.Second), now)
		require.NoError(t, f.repo.PersistTiboProbeResult(f.ctx, a.ID, updates, sample))
		_, err := f.client.Group.UpdateOneID(f.welfare).SetStatus(service.StatusActive).Save(f.ctx)
		require.NoError(t, err)
		require.NoError(t, f.repo.BindGroups(f.ctx, a.ID, []int64{f.healthy, f.other}))
		require.NoError(t, f.repo.PersistTiboProbeResult(f.ctx, a.ID, updates, sample))
		require.Contains(t, f.memberships(a.ID), f.healthy, "restoration must wait for a new probe")
		finish := time.Now()
		fresh, freshSample := tiboGroupObservation("degraded", sample.StartedAt.Add(time.Millisecond), finish)
		require.NoError(t, f.repo.PersistTiboProbeResult(f.ctx, a.ID, fresh, freshSample))
		require.Contains(t, f.memberships(a.ID), f.welfare)
	}
}

type tiboFailingSnapshotCache struct {
	*schedulerCacheRecorder
	fail bool
}

func (c *tiboFailingSnapshotCache) SetAccount(ctx context.Context, a *service.Account) error {
	if c.fail {
		return errors.New("test Tibo cache unavailable")
	}
	return c.schedulerCacheRecorder.SetAccount(ctx, a)
}

func TestTiboAutoGroupCommittedCacheFailureRetry(t *testing.T) {
	f := newTiboGroupFixture(t)
	a := f.account(service.AccountTypeOAuth, service.PlatformOpenAI, f.healthy, f.other)
	cache := &tiboFailingSnapshotCache{schedulerCacheRecorder: f.cache, fail: true}
	f.repo.schedulerCache = cache
	now := time.Now()
	updates, sample := tiboGroupObservation("degraded", now.Add(-time.Second), now)
	require.NoError(t, f.repo.PersistTiboProbeResult(f.ctx, a.ID, updates, sample), "the committed outbox survives a best-effort cache failure")
	after := f.memberships(a.ID)
	require.Contains(t, after, f.welfare, "DB commit survives immediate cache failure")
	cache.fail = false
	require.NoError(t, f.repo.PersistTiboProbeResult(f.ctx, a.ID, updates, sample))
	require.Equal(t, after, f.memberships(a.ID))
	require.ElementsMatch(t, []int64{f.welfare, f.other}, cache.accounts[a.ID].GroupIDs)
}

type tiboGatedOutbox struct {
	service.SchedulerOutboxRepository
	enabled     atomic.Bool
	groupEvents atomic.Int64
}

func (r *tiboGatedOutbox) ListAfterAndReleaseDedup(ctx context.Context, after int64, limit int) ([]service.SchedulerOutboxEvent, error) {
	if !r.enabled.Load() {
		return nil, nil
	}
	events, err := r.SchedulerOutboxRepository.ListAfterAndReleaseDedup(ctx, after, limit)
	for _, event := range events {
		if event.EventType == service.SchedulerOutboxEventAccountGroupsChanged {
			r.groupEvents.Add(1)
		}
	}
	return events, err
}

func TestTiboAutoGroupOutboxPublishesAllGroups(t *testing.T) {
	f := newTiboGroupFixture(t)
	a := f.account(service.AccountTypeOAuth, service.PlatformOpenAI, f.healthy, f.other)
	// Empty snapshots intentionally count as cache misses. Keep independent
	// control accounts in both buckets so hit=true is positive publication evidence.
	f.account(service.AccountTypeAPIKey, service.PlatformOpenAI, f.healthy)
	f.account(service.AccountTypeAPIKey, service.PlatformOpenAI, f.welfare)
	_, err := integrationDB.ExecContext(f.ctx, "TRUNCATE scheduler_outbox")
	require.NoError(t, err)
	cache := NewSchedulerCache(testRedis(t))
	outbox := &tiboGatedOutbox{SchedulerOutboxRepository: NewSchedulerOutboxRepository(integrationDB)}
	cfg := &config.Config{RunMode: config.RunModeStandard, Gateway: config.GatewayConfig{Scheduling: config.GatewaySchedulingConfig{
		OutboxPollIntervalSeconds: 1, FullRebuildIntervalSeconds: 0, DbFallbackEnabled: true,
	}}}
	groups := newGroupRepositoryWithSQL(f.client, integrationDB)
	worker := service.NewSchedulerSnapshotService(cache, outbox, f.repo, groups, cfg)
	worker.Start()
	t.Cleanup(worker.Stop)
	contains := func(group int64, mode string, want bool) bool {
		accounts, hit, err := cache.GetSnapshot(f.ctx, service.SchedulerBucket{GroupID: group, Platform: service.PlatformOpenAI, Mode: mode})
		if err != nil || !hit {
			return false
		}
		found := false
		for _, account := range accounts {
			found = found || account.ID == a.ID
		}
		return found == want
	}
	// Wait for every target/external bucket produced by the startup rebuild
	// before changing membership; startup cannot mask a missing old-group event.
	require.Eventually(t, func() bool {
		for _, mode := range []string{service.SchedulerModeSingle, service.SchedulerModeForced} {
			if !contains(f.healthy, mode, true) || !contains(f.welfare, mode, false) || !contains(f.other, mode, true) {
				return false
			}
		}
		return true
	}, 8*time.Second, 20*time.Millisecond)
	f.repo.schedulerCache = nil // prove durable events alone publish the change
	f.sample(a.ID, "degraded")
	require.True(t, contains(f.healthy, service.SchedulerModeSingle, true), "consumer is paused and old cache really exists")
	require.True(t, contains(f.welfare, service.SchedulerModeSingle, false))
	require.Zero(t, outbox.groupEvents.Load())
	outbox.enabled.Store(true)
	require.Eventually(t, func() bool {
		return outbox.groupEvents.Load() > 0 && contains(f.healthy, service.SchedulerModeSingle, false) &&
			contains(f.welfare, service.SchedulerModeSingle, true) && contains(f.other, service.SchedulerModeSingle, true)
	}, 8*time.Second, 20*time.Millisecond, "actual PostgreSQL outbox worker must rebuild old, new and preserved groups in Redis")
}

func TestTiboQualityIsolationWithoutGroupsAndManualPause(t *testing.T) {
	f := newTiboGroupFixture(t)
	f.settings(service.TiboAutomationSettings{Enabled: true, PauseOnDegraded: true, AutoResumeOnHealthy: true})
	for _, kind := range []string{service.AccountTypeOAuth, service.AccountTypeSetupToken, service.AccountTypeAPIKey} {
		t.Run(kind, func(t *testing.T) {
			a := f.account(kind, service.PlatformOpenAI, f.other)
			_, err := f.client.Account.UpdateOneID(a.ID).SetSchedulable(false).Save(f.ctx)
			require.NoError(t, err)
			before := f.memberships(a.ID)
			f.sample(a.ID, "degraded")
			isolated, err := f.repo.GetByID(f.ctx, a.ID)
			require.NoError(t, err)
			require.Equal(t, true, isolated.Extra[service.OpenAITiboQualityIsolatedExtraKey])
			require.False(t, isolated.Schedulable)
			require.Equal(t, before, f.memberships(a.ID), "group IDs are optional for quality isolation")
			require.Equal(t, true, f.cache.accounts[a.ID].Extra[service.OpenAITiboQualityIsolatedExtraKey])
			f.sample(a.ID, "healthy")
			recovered, err := f.repo.GetByID(f.ctx, a.ID)
			require.NoError(t, err)
			require.Equal(t, false, recovered.Extra[service.OpenAITiboQualityIsolatedExtraKey])
			require.False(t, recovered.Schedulable, "recovery must preserve a manual scheduling pause")
		})
	}
}

func TestTiboQualityIsolationRequiresNewSampleAfterConfiguration(t *testing.T) {
	f := newTiboGroupFixture(t)
	a := f.account(service.AccountTypeOAuth, service.PlatformOpenAI, f.healthy)
	f.settings(service.TiboAutomationSettings{})
	now := time.Now()
	updates, sample := tiboGroupObservation("degraded", now.Add(-time.Second), now)
	require.NoError(t, f.repo.PersistTiboProbeResult(f.ctx, a.ID, updates, sample))
	current, err := f.repo.GetByID(f.ctx, a.ID)
	require.NoError(t, err)
	require.NotEqual(t, true, current.Extra[service.OpenAITiboQualityIsolatedExtraKey])
	f.settings(service.TiboAutomationSettings{Enabled: true, PauseOnDegraded: true})
	require.NoError(t, f.repo.PersistTiboProbeResult(f.ctx, a.ID, updates, sample))
	current, err = f.repo.GetByID(f.ctx, a.ID)
	require.NoError(t, err)
	require.NotEqual(t, true, current.Extra[service.OpenAITiboQualityIsolatedExtraKey], "enabling an action cannot replay an earlier probe")
	f.sample(a.ID, "degraded")
	current, err = f.repo.GetByID(f.ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, true, current.Extra[service.OpenAITiboQualityIsolatedExtraKey])
}

func TestTiboQualityIsolationManualRecoveryFencesInFlightProbe(t *testing.T) {
	f := newTiboGroupFixture(t)
	f.settings(service.TiboAutomationSettings{Enabled: true, PauseOnDegraded: true})
	a := f.account(service.AccountTypeOAuth, service.PlatformOpenAI, f.other)
	f.sample(a.ID, "degraded")
	f.sample(a.ID, "healthy")
	current, err := f.repo.GetByID(f.ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, true, current.Extra[service.OpenAITiboQualityIsolatedExtraKey], "automatic recovery is opt-in")
	// A probe started before the clear but finishing later cannot restore isolation.
	started := time.Now()
	require.NoError(t, f.repo.ClearOpenAITiboQualityIsolation(f.ctx, a.ID))
	finished := time.Now()
	updates, sample := tiboGroupObservation("degraded", started, finished)
	require.NoError(t, f.repo.PersistTiboProbeResult(f.ctx, a.ID, updates, sample))
	current, err = f.repo.GetByID(f.ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, false, current.Extra[service.OpenAITiboQualityIsolatedExtraKey])
	f.sample(a.ID, "degraded")
	current, err = f.repo.GetByID(f.ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, true, current.Extra[service.OpenAITiboQualityIsolatedExtraKey], "a genuinely new probe can isolate again")
}

func TestTiboQualityIsolationPersistsThroughStaleAdminEdit(t *testing.T) {
	f := newTiboGroupFixture(t)
	f.settings(service.TiboAutomationSettings{Enabled: true, PauseOnDegraded: true})
	a := f.account(service.AccountTypeOAuth, service.PlatformOpenAI, f.other)
	stale, err := f.repo.GetByID(f.ctx, a.ID)
	require.NoError(t, err)
	f.sample(a.ID, "degraded")
	stale.Name = "edited while probe completed"
	stale.Extra[service.OpenAITiboQualityIsolatedExtraKey] = false
	require.NoError(t, f.repo.Update(f.ctx, stale))
	current, err := f.repo.GetByID(f.ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, stale.Name, current.Name)
	require.Equal(t, true, current.Extra[service.OpenAITiboQualityIsolatedExtraKey], "an ordinary admin save has no authority to forge runtime quality state")
	verdict, _ := service.LatestOpenAIAstraSample(current.Extra[service.OpenAIAstraPolicyExtraKey])
	require.Equal(t, "degraded", verdict)
}

func TestTiboQualityIsolationSkipsForeignAccounts(t *testing.T) {
	f := newTiboGroupFixture(t)
	f.settings(service.TiboAutomationSettings{Enabled: true, PauseOnDegraded: true})
	for _, kind := range []string{"cpr", "bps"} {
		a := f.account(kind, service.PlatformOpenAI, f.other)
		f.sample(a.ID, "degraded")
		current, err := f.repo.GetByID(f.ctx, a.ID)
		require.NoError(t, err)
		require.NotEqual(t, true, current.Extra[service.OpenAITiboQualityIsolatedExtraKey])
	}
	bps := f.account(service.AccountTypeAPIKey, service.PlatformOpenAI, f.other)
	require.NoError(t, f.client.Account.UpdateOneID(bps.ID).SetCredentials(map[string]any{"api_key": "fixture-only", "bps_files_contract": "bps-files/1"}).Exec(f.ctx))
	f.sample(bps.ID, "degraded")
	current, err := f.repo.GetByID(f.ctx, bps.ID)
	require.NoError(t, err)
	require.NotEqual(t, true, current.Extra[service.OpenAITiboQualityIsolatedExtraKey])
}

func TestTiboQualityIsolationMatchingProbeIdentity(t *testing.T) {
	f := newTiboGroupFixture(t)
	f.settings(service.TiboAutomationSettings{Enabled: true, AutoGroupEnabled: true, HealthyGroupID: f.healthy, DegradedGroupID: f.welfare, PauseOnDegraded: true})
	a := f.account(service.AccountTypeOAuth, service.PlatformOpenAI, f.healthy, f.other)
	current, err := f.repo.GetByID(f.ctx, a.ID)
	require.NoError(t, err)
	now := time.Now()
	updates, sample := tiboGroupObservation("degraded", now.Add(-time.Millisecond), now)
	sample.ProbeIdentity = service.OpenAITiboProbeIdentity(current)
	updates[service.OpenAIAstraPolicyExtraKey].(map[string]any)["probe_identity"] = sample.ProbeIdentity
	require.NotEmpty(t, sample.ProbeIdentity)
	require.NoError(t, f.repo.PersistTiboProbeResult(f.ctx, a.ID, updates, sample))
	stored, err := f.repo.GetByID(f.ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, true, stored.Extra[service.OpenAITiboQualityIsolatedExtraKey])
	require.Contains(t, stored.Extra, service.OpenAIAstraPolicyExtraKey)
	require.Contains(t, f.memberships(a.ID), f.welfare)
}

func TestTiboQualityIsolationRejectsProbeFromReplacedIdentity(t *testing.T) {
	for _, change := range []string{"credentials", "proxy binding"} {
		t.Run(change, func(t *testing.T) {
			f := newTiboGroupFixture(t)
			f.settings(service.TiboAutomationSettings{Enabled: true, AutoGroupEnabled: true, HealthyGroupID: f.healthy, DegradedGroupID: f.welfare, PauseOnDegraded: true})
			a := f.account(service.AccountTypeOAuth, service.PlatformOpenAI, f.healthy, f.other)
			original, err := f.repo.GetByID(f.ctx, a.ID)
			require.NoError(t, err)
			started := time.Now()
			identity := service.OpenAITiboProbeIdentity(original)
			if change == "credentials" {
				_, err = f.client.Account.UpdateOneID(a.ID).SetCredentials(map[string]any{"access_token": "replacement-fixture-only"}).Save(f.ctx)
				require.NoError(t, err)
			} else {
				proxy := mustCreateProxy(t, f.client, &service.Proxy{Name: "Tibo identity fixture"})
				t.Cleanup(func() {
					_, err := integrationDB.ExecContext(context.Background(), "UPDATE accounts SET proxy_id=NULL WHERE id=$1", a.ID)
					require.NoError(t, err)
					_, err = integrationDB.ExecContext(context.Background(), "DELETE FROM proxies WHERE id=$1", proxy.ID)
					require.NoError(t, err)
				})
				_, err = f.client.Account.UpdateOneID(a.ID).SetProxyID(proxy.ID).Save(f.ctx)
				require.NoError(t, err)
			}
			before := f.memberships(a.ID)
			updates, sample := tiboGroupObservation("degraded", started, time.Now())
			sample.ProbeIdentity = identity
			updates[service.OpenAIAstraPolicyExtraKey].(map[string]any)["probe_identity"] = sample.ProbeIdentity
			require.ErrorIs(t, f.repo.PersistTiboProbeResult(f.ctx, a.ID, updates, sample), service.ErrOpenAITiboProbeIdentityChanged)
			stored, err := f.repo.GetByID(f.ctx, a.ID)
			require.NoError(t, err)
			require.NotEqual(t, identity, service.OpenAITiboProbeIdentity(stored))
			require.Equal(t, before, f.memberships(a.ID))
			require.NotContains(t, stored.Extra, service.OpenAIAstraPolicyExtraKey, "an old identity cannot publish its history onto a replacement")
			require.NotContains(t, stored.Extra, tiboAutoGroupSampleKey)
			require.NotEqual(t, true, stored.Extra[service.OpenAITiboQualityIsolatedExtraKey])
			// A subsequent probe of the replacement identity remains admissible;
			// rejecting the old result must not consume its timestamp or new history.
			now := time.Now()
			freshUpdates, freshSample := tiboGroupObservation("degraded", now, now)
			freshSample.ProbeIdentity = service.OpenAITiboProbeIdentity(stored)
			freshUpdates[service.OpenAIAstraPolicyExtraKey].(map[string]any)["probe_identity"] = freshSample.ProbeIdentity
			require.NoError(t, f.repo.PersistTiboProbeResult(f.ctx, a.ID, freshUpdates, freshSample))
			accepted, err := f.repo.GetByID(f.ctx, a.ID)
			require.NoError(t, err)
			require.Equal(t, true, accepted.Extra[service.OpenAITiboQualityIsolatedExtraKey])
			require.Contains(t, f.memberships(a.ID), f.welfare)
		})
	}
}

func TestTiboQualityIsolationRejectsMismatchedObservationIdentity(t *testing.T) {
	f := newTiboGroupFixture(t)
	f.settings(service.TiboAutomationSettings{Enabled: true, PauseOnDegraded: true})
	for _, observationIdentity := range []string{"", "foreign-identity"} {
		t.Run("observation-"+observationIdentity, func(t *testing.T) {
			a := f.account(service.AccountTypeOAuth, service.PlatformOpenAI, f.other)
			now := time.Now()
			updates, sample := tiboGroupObservation("degraded", now, now)
			current, err := f.repo.GetByID(f.ctx, a.ID)
			require.NoError(t, err)
			sample.ProbeIdentity = service.OpenAITiboProbeIdentity(current)
			if observationIdentity != "" {
				updates[service.OpenAIAstraPolicyExtraKey].(map[string]any)["probe_identity"] = observationIdentity
			}
			require.ErrorIs(t, f.repo.PersistTiboProbeResult(f.ctx, a.ID, updates, sample), service.ErrOpenAITiboProbeIdentityChanged)
			stored, err := f.repo.GetByID(f.ctx, a.ID)
			require.NoError(t, err)
			require.NotContains(t, stored.Extra, service.OpenAIAstraPolicyExtraKey)
			require.NotContains(t, stored.Extra, tiboAutoGroupSampleKey)
			require.NotEqual(t, true, stored.Extra[service.OpenAITiboQualityIsolatedExtraKey])
		})
	}
}

func TestTiboObservationPersistsOnlyHistoryWithoutActions(t *testing.T) {
	f := newTiboGroupFixture(t)
	f.settings(service.TiboAutomationSettings{Enabled: true, AutoGroupEnabled: true, HealthyGroupID: f.healthy, DegradedGroupID: f.welfare, PauseOnDegraded: true})
	a := f.account(service.AccountTypeOAuth, service.PlatformOpenAI, f.healthy, f.other)
	current, err := f.repo.GetByID(f.ctx, a.ID)
	require.NoError(t, err)
	identity := service.OpenAITiboProbeIdentity(current)
	now := time.Now()
	updates, _ := tiboGroupObservation("degraded", now, now)
	updates[service.OpenAIAstraPolicyExtraKey].(map[string]any)["probe_identity"] = identity
	// The passive port never grants action or active-sample watermark authority,
	// even if an internal caller accidentally supplies extra update keys.
	updates[service.OpenAITiboQualityIsolatedExtraKey] = true
	updates[tiboAutoGroupSampleKey] = map[string]any{"started_at": now.UTC().Format(time.RFC3339Nano)}
	before := f.memberships(a.ID)
	require.NoError(t, f.repo.PersistTiboObservation(f.ctx, a.ID, updates, identity))
	stored, err := f.repo.GetByID(f.ctx, a.ID)
	require.NoError(t, err)
	require.Contains(t, stored.Extra, service.OpenAIAstraPolicyExtraKey)
	verdict, _ := service.LatestOpenAIAstraSample(stored.Extra[service.OpenAIAstraPolicyExtraKey])
	require.Equal(t, "degraded", verdict)
	require.NotContains(t, stored.Extra, service.OpenAITiboQualityIsolatedExtraKey)
	require.NotContains(t, stored.Extra, tiboAutoGroupSampleKey)
	require.Equal(t, before, f.memberships(a.ID))
	require.Contains(t, f.cache.accounts[a.ID].Extra, service.OpenAIAstraPolicyExtraKey)
}

func TestTiboObservationRejectsReplacedIdentityWithoutHistory(t *testing.T) {
	for _, change := range []string{"credentials", "proxy binding"} {
		t.Run(change, func(t *testing.T) {
			f := newTiboGroupFixture(t)
			a := f.account(service.AccountTypeOAuth, service.PlatformOpenAI, f.healthy, f.other)
			original, err := f.repo.GetByID(f.ctx, a.ID)
			require.NoError(t, err)
			identity := service.OpenAITiboProbeIdentity(original)
			if change == "credentials" {
				_, err = f.client.Account.UpdateOneID(a.ID).SetCredentials(map[string]any{"access_token": "replacement-passive-fixture"}).Save(f.ctx)
				require.NoError(t, err)
			} else {
				proxy := mustCreateProxy(t, f.client, &service.Proxy{Name: "Tibo passive identity fixture"})
				t.Cleanup(func() {
					_, err := integrationDB.ExecContext(context.Background(), "UPDATE accounts SET proxy_id=NULL WHERE id=$1", a.ID)
					require.NoError(t, err)
					_, err = integrationDB.ExecContext(context.Background(), "DELETE FROM proxies WHERE id=$1", proxy.ID)
					require.NoError(t, err)
				})
				_, err = f.client.Account.UpdateOneID(a.ID).SetProxyID(proxy.ID).Save(f.ctx)
				require.NoError(t, err)
			}
			now := time.Now()
			updates, _ := tiboGroupObservation("degraded", now, now)
			updates[service.OpenAIAstraPolicyExtraKey].(map[string]any)["probe_identity"] = identity
			require.ErrorIs(t, f.repo.PersistTiboObservation(f.ctx, a.ID, updates, identity), service.ErrOpenAITiboProbeIdentityChanged)
			stored, err := f.repo.GetByID(f.ctx, a.ID)
			require.NoError(t, err)
			require.NotContains(t, stored.Extra, service.OpenAIAstraPolicyExtraKey)
			require.NotContains(t, stored.Extra, tiboAutoGroupSampleKey)
			require.NotEqual(t, true, stored.Extra[service.OpenAITiboQualityIsolatedExtraKey])
		})
	}
}
