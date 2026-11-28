package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	dbent "github.com/MACOS-DO/sub4api/ent"
	"github.com/MACOS-DO/sub4api/internal/pkg/logger"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/lib/pq"
)

const tiboAutoGroupSampleKey = "codex_tibo_verdict:auto_group"

var _ service.TiboAutoGroupRepository = (*accountRepository)(nil)

func (r *accountRepository) PersistTiboProbeResult(ctx context.Context, id int64, updates map[string]any, sample service.TiboAutoGroupSample) error {
	for attempt := 0; ; attempt++ {
		err := r.persistTiboProbeResult(ctx, id, updates, sample)
		var pgerr *pq.Error
		if err == nil || attempt >= 2 || !errors.As(err, &pgerr) ||
			(pgerr.Code != "23505" && pgerr.Code != "40P01" && pgerr.Code != "40001") {
			return err
		}
		// Retry the entire transaction with the same probe identity and fresh
		// membership checks after a concurrent insert, deadlock or serialization failure.
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

func (r *accountRepository) persistTiboProbeResult(ctx context.Context, id int64, updates map[string]any, sample service.TiboAutoGroupSample) error {
	if sample.Verdict != "healthy" && sample.Verdict != "degraded" || sample.StartedAt.IsZero() ||
		sample.CompletedAt.Before(sample.StartedAt) || sample.CompletedAt.After(time.Now()) {
		return fmt.Errorf("invalid fresh Tibo probe sample")
	}
	verdict, at := service.LatestOpenAIAstraSample(updates[service.OpenAIAstraPolicyExtraKey])
	if verdict != sample.Verdict || !at.Equal(sample.CompletedAt) {
		return fmt.Errorf("tibo probe sample does not match its observation")
	}
	if sample.ProbeIdentity != "" {
		var observation struct {
			ProbeIdentity string `json:"probe_identity"`
		}
		encoded, err := json.Marshal(updates[service.OpenAIAstraPolicyExtraKey])
		if err != nil {
			return fmt.Errorf("encode Tibo probe observation identity: %w", err)
		}
		if err := json.Unmarshal(encoded, &observation); err != nil {
			return fmt.Errorf("decode Tibo probe observation identity: %w", err)
		}
		if sample.ProbeIdentity == "invalid" || observation.ProbeIdentity != sample.ProbeIdentity {
			return service.ErrOpenAITiboProbeIdentityChanged
		}
	}
	// This capability owns its transaction. It is never called by admin writes
	// or by historical sample recovery.
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	client := tx.Client()
	// Match group lifecycle lock order: selected groups before accounts. The
	// settings lock also serializes automatic actions with a concurrent disable.
	settings, err := readTiboAutomationSettings(ctx, client)
	if err != nil {
		return err
	}
	var groupsAvailable map[string]bool
	if settings.Enabled && settings.AutoGroupEnabled {
		groupsAvailable, err = lockTiboAutomationGroups(ctx, client, settings)
		if err != nil {
			return err
		}
	}
	var platform, credentialType, status string
	var raw, rawCredentials []byte
	var proxyID sql.NullInt64
	err = scanSingleRow(ctx, client, `SELECT platform, type, status, extra, credentials, proxy_id FROM accounts
		WHERE id=$1 AND deleted_at IS NULL FOR NO KEY UPDATE`, []any{id}, &platform, &credentialType, &status, &raw, &rawCredentials, &proxyID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return service.ErrAccountNotFound
		}
		return err
	}
	var extra map[string]any
	if err := json.Unmarshal(raw, &extra); err != nil {
		return err
	}
	var credentials map[string]any
	if len(rawCredentials) > 0 {
		if err := json.Unmarshal(rawCredentials, &credentials); err != nil {
			return err
		}
	}
	current := &service.Account{ID: id, Platform: platform, Type: credentialType, Status: status, Extra: extra, Credentials: credentials}
	if proxyID.Valid {
		current.ProxyID = &proxyID.Int64
	}
	if sample.ProbeIdentity != "" && (sample.ProbeIdentity == "invalid" || sample.ProbeIdentity != service.OpenAITiboProbeIdentity(current)) {
		// The result belongs to the old upstream identity. Store neither its history
		// nor any automatic action against the replacement credentials/proxy.
		logger.LegacyPrintf("repository.account", "[TiboProbe] account=%d skipped=identity_changed", id)
		return service.ErrOpenAITiboProbeIdentityChanged
	}
	eligible := status == service.StatusActive && service.IsOpenAITiboAccount(current)
	var previous struct {
		StartedAt time.Time `json:"started_at"`
	}
	if marker, ok := extra[tiboAutoGroupSampleKey]; ok {
		encoded, err := json.Marshal(marker)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(encoded, &previous); err != nil {
			return err
		}
	}
	if !previous.StartedAt.IsZero() && !sample.StartedAt.After(previous.StartedAt) {
		if err := tx.Rollback(); err != nil {
			return err
		}
		// A previous commit can have succeeded while its immediate cache
		// refresh failed. Retrying must refresh, without reapplying the move.
		r.syncSchedulerAccountSnapshot(ctx, id)
		return nil
	}
	changed := false
	var fromID, toID int64
	reason := "ineligible_account"
	if settings.Enabled && settings.AutoGroupEnabled && groupsAvailable[platform] && eligible && credentialType == service.AccountTypeOAuth {
		healthy, degraded := settings.GroupIDsForPlatform(platform)
		fromID, toID, changed, reason, err = moveTiboAccountGroup(ctx, client, id, sample.Verdict, healthy, degraded)
		if err != nil {
			return err
		}
	}
	// Copy the caller's update; no shared probe state is mutated.
	write := make(map[string]any, len(updates)+1)
	for key, value := range updates {
		write[key] = value
	}
	// Quality isolation is independent of the administrator's scheduling switch.
	// A recovery only removes the marker owned by this automation.
	isolated, _ := extra[service.OpenAITiboQualityIsolatedExtraKey].(bool)
	if settings.Enabled && eligible {
		if sample.Verdict == "degraded" && settings.PauseOnDegraded && !isolated {
			write[service.OpenAITiboQualityIsolatedExtraKey] = true
			write[service.OpenAITiboQualityIsolationAtExtraKey] = sample.CompletedAt.UTC().Format(time.RFC3339Nano)
		} else if sample.Verdict == "healthy" && settings.AutoResumeOnHealthy && isolated {
			write[service.OpenAITiboQualityIsolatedExtraKey] = false
			write[service.OpenAITiboQualityIsolationAtExtraKey] = sample.CompletedAt.UTC().Format(time.RFC3339Nano)
		}
	}
	// Business skips consume this probe too. Restoring a group or manually
	// enrolling an account must not make a historical retry move it later.
	write[tiboAutoGroupSampleKey] = map[string]any{
		"started_at":   sample.StartedAt.UTC().Format(time.RFC3339Nano),
		"completed_at": sample.CompletedAt.UTC().Format(time.RFC3339Nano), "verdict": sample.Verdict,
	}
	if err := r.UpdateExtra(dbent.NewTxContext(ctx, tx), id, write); err != nil {
		return err
	}
	if changed {
		if err := enqueueSchedulerOutbox(ctx, client, service.SchedulerOutboxEventAccountGroupsChanged,
			&id, nil, buildSchedulerGroupPayload([]int64{fromID, toID})); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if changed {
		logger.LegacyPrintf("repository.account", "[TiboAutoGroup] account=%d verdict=%s from=%d to=%d", id, sample.Verdict, fromID, toID)
	} else if reason == "target_unavailable" {
		logger.LegacyPrintf("repository.account", "[TiboAutoGroup] account=%d skipped=%s", id, reason)
	}
	r.syncSchedulerAccountSnapshot(ctx, id)
	return nil
}

func moveTiboAccountGroup(ctx context.Context, client *dbent.Client, id int64, verdict string, healthy, degraded int64) (from, to int64, changed bool, reason string, err error) {
	from, to = healthy, degraded
	if verdict == "healthy" {
		from, to = to, from
	}
	rows, err := client.QueryContext(ctx, `SELECT group_id FROM account_groups
		WHERE account_id=$1 AND group_id IN ($2,$3) ORDER BY group_id FOR UPDATE`, id, from, to)
	if err != nil {
		return 0, 0, false, "", err
	}
	hasSource, hasTarget := false, false
	for rows.Next() {
		var groupID int64
		if err = rows.Scan(&groupID); err != nil {
			_ = rows.Close()
			return 0, 0, false, "", err
		}
		hasSource = hasSource || groupID == from
		hasTarget = hasTarget || groupID == to
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return 0, 0, false, "", err
	}
	if !hasSource {
		if hasTarget {
			return from, to, false, "already_target", nil
		}
		return from, to, false, "outside_target_groups", nil
	}
	// UPDATE the existing source link instead of deleting and recreating it.
	// Concurrent manual DELETE/BindGroups uses the same row lock: if the source
	// disappeared, never insert a replacement that would pull the account back.
	query := `UPDATE account_groups SET group_id=$3 WHERE account_id=$1 AND group_id=$2`
	args := []any{id, from, to}
	if hasTarget {
		query = `DELETE FROM account_groups WHERE account_id=$1 AND group_id=$2`
		args = args[:2]
	}
	result, err := client.ExecContext(ctx, query, args...)
	if err != nil {
		return from, to, false, "", err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return from, to, false, "", err
	}
	if affected == 0 {
		return from, to, false, "outside_target_groups", nil
	}
	return from, to, true, "moved", nil
}

// readTiboAutomationSettings is intentionally transactional rather than reading
// the service cache. A newly disabled action must not run from a cached config.
func readTiboAutomationSettings(ctx context.Context, client *dbent.Client) (service.TiboAutomationSettings, error) {
	rows, err := client.QueryContext(ctx, `SELECT value FROM settings WHERE key=$1 FOR SHARE`, service.SettingKeyOpenAITiboSettings)
	if err != nil {
		return service.DefaultOpenAITiboSettings(), err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return service.DefaultOpenAITiboSettings(), err
		}
		return service.DefaultOpenAITiboSettings(), nil
	}
	var raw string
	if err := rows.Scan(&raw); err != nil {
		return service.DefaultOpenAITiboSettings(), err
	}
	if err := rows.Err(); err != nil {
		return service.DefaultOpenAITiboSettings(), err
	}
	return service.DecodeOpenAITiboSettings(raw)
}

// Group rows are locked before the account row, matching group lifecycle writes.
// Missing/deactivated groups skip only grouping; isolation remains independent.
func lockTiboAutomationGroups(ctx context.Context, client *dbent.Client, settings service.TiboAutomationSettings) (map[string]bool, error) {
	// Lock both platforms in one ID order before reading the account. A concurrent
	// platform edit cannot select an unlocked pair or move to another platform.
	rows, err := client.QueryContext(ctx, `SELECT id, platform FROM groups
  WHERE ((id IN ($1,$2) AND platform='openai') OR (id IN ($3,$4) AND platform='openai_bps'))
    AND status='active' AND deleted_at IS NULL
  ORDER BY id FOR SHARE`, settings.HealthyGroupID, settings.DegradedGroupID, settings.BPSHealthyGroupID, settings.BPSDegradedGroupID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	counts := make(map[string]int, 2)
	for rows.Next() {
		var id int64
		var platform string
		if err := rows.Scan(&id, &platform); err != nil {
			return nil, err
		}
		counts[platform]++
	}
	return map[string]bool{service.PlatformOpenAI: counts[service.PlatformOpenAI] == 2,
		service.PlatformOpenAIBPS: counts[service.PlatformOpenAIBPS] == 2}, rows.Err()
}

// ClearOpenAITiboQualityIsolation clears only the automation-owned marker. A
// manual scheduling pause, quota cooldown or account error is never undone.
func (r *accountRepository) ClearOpenAITiboQualityIsolation(ctx context.Context, id int64) error {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	client := tx.Client()
	var raw []byte
	if err := scanSingleRow(ctx, client, `SELECT extra FROM accounts
  WHERE id=$1 AND (platform='openai' OR (platform='openai_bps' AND type='oauth'))
    AND deleted_at IS NULL FOR NO KEY UPDATE`, []any{id}, &raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return service.ErrAccountNotFound
		}
		return err
	}
	var extra map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &extra); err != nil {
			return err
		}
	}
	// Fence probe requests started before this manual clear. Otherwise a response
	// already in flight could immediately restore the marker just cleared.
	now := time.Now().UTC()
	var previous struct {
		StartedAt time.Time `json:"started_at"`
	}
	if marker := extra[tiboAutoGroupSampleKey]; marker != nil {
		encoded, err := json.Marshal(marker)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(encoded, &previous); err != nil {
			return err
		}
		if previous.StartedAt.After(now) {
			now = previous.StartedAt
		}
	}
	write := map[string]any{
		service.OpenAITiboQualityIsolatedExtraKey:    false,
		service.OpenAITiboQualityIsolationAtExtraKey: now.Format(time.RFC3339Nano),
		tiboAutoGroupSampleKey: map[string]any{
			"started_at":   now.Format(time.RFC3339Nano),
			"completed_at": now.Format(time.RFC3339Nano),
			"verdict":      "manual_clear",
		},
	}
	if err := r.UpdateExtra(dbent.NewTxContext(ctx, tx), id, write); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.syncSchedulerAccountSnapshot(ctx, id)
	return nil
}

// PersistTiboObservation holds the account row from identity validation through
// history persistence. It cannot perform quality isolation or group movement.
func (r *accountRepository) PersistTiboObservation(ctx context.Context, id int64, updates map[string]any, probeIdentity string) error {
	if probeIdentity == "" || probeIdentity == "invalid" {
		return service.ErrOpenAITiboProbeIdentityChanged
	}
	observation, ok := updates[service.OpenAIAstraPolicyExtraKey]
	if !ok {
		return fmt.Errorf("tibo observation missing history")
	}
	var value struct {
		ProbeIdentity string `json:"probe_identity"`
	}
	encoded, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(encoded, &value); err != nil || value.ProbeIdentity != probeIdentity {
		return service.ErrOpenAITiboProbeIdentityChanged
	}
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	client := tx.Client()
	var platform, typ string
	var credentialsRaw []byte
	var proxyID sql.NullInt64
	if err := scanSingleRow(ctx, client, `SELECT platform,type,credentials,proxy_id FROM accounts WHERE id=$1 AND deleted_at IS NULL FOR NO KEY UPDATE`, []any{id}, &platform, &typ, &credentialsRaw, &proxyID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return service.ErrAccountNotFound
		}
		return err
	}
	var credentials map[string]any
	if len(credentialsRaw) > 0 {
		if err := json.Unmarshal(credentialsRaw, &credentials); err != nil {
			return err
		}
	}
	current := &service.Account{ID: id, Platform: platform, Type: typ, Credentials: credentials}
	if proxyID.Valid {
		current.ProxyID = &proxyID.Int64
	}
	if service.OpenAITiboProbeIdentity(current) != probeIdentity {
		return service.ErrOpenAITiboProbeIdentityChanged
	}
	// Only the history payload is admitted through this port. The caller cannot
	// smuggle isolation flags or an active-sample watermark into a passive write.
	history := map[string]any{service.OpenAIAstraPolicyExtraKey: observation}
	if err := r.UpdateExtra(dbent.NewTxContext(ctx, tx), id, history); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.syncSchedulerAccountSnapshot(ctx, id)
	return nil
}

var _ service.TiboObservationRepository = (*accountRepository)(nil)
