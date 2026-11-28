package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
)

// --- Plan Repository ---

type scheduledTestPlanRepository struct {
	db *sql.DB
}

const scheduledPlanColumns = "id, account_id, model_id, cron_expression, enabled, max_results, auto_recover, last_run_at, next_run_at, created_at, updated_at, pelican_config"
const scheduledResultColumns = "id, plan_id, status, response_text, error_message, latency_ms, started_at, finished_at, created_at, pelican_config"

func scheduledIQConfig(config *service.PelicanTestConfig) (any, error) {
	if config == nil {
		return nil, nil
	}
	value, err := json.Marshal(config)
	return string(value), err
}

func NewScheduledTestPlanRepository(db *sql.DB) service.ScheduledTestPlanRepository {
	return &scheduledTestPlanRepository{db: db}
}

func (r *scheduledTestPlanRepository) Create(ctx context.Context, plan *service.ScheduledTestPlan) (*service.ScheduledTestPlan, error) {
	config, err := scheduledIQConfig(plan.PelicanConfig)
	if err != nil {
		return nil, err
	}
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO scheduled_test_plans (account_id, model_id, cron_expression, enabled, max_results, auto_recover, next_run_at, created_at, updated_at, pelican_config)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW(), $8)
		RETURNING `+scheduledPlanColumns, plan.AccountID, plan.ModelID, plan.CronExpression, plan.Enabled, plan.MaxResults, plan.AutoRecover, plan.NextRunAt, config)
	return scanPlan(row)
}

func (r *scheduledTestPlanRepository) GetByID(ctx context.Context, id int64) (*service.ScheduledTestPlan, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT `+scheduledPlanColumns+`
		FROM scheduled_test_plans WHERE id = $1
	`, id)
	return scanPlan(row)
}

func (r *scheduledTestPlanRepository) ListByAccountID(ctx context.Context, accountID int64) ([]*service.ScheduledTestPlan, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+scheduledPlanColumns+`
		FROM scheduled_test_plans WHERE account_id = $1
		ORDER BY created_at DESC
	`, accountID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanPlans(rows)
}

func (r *scheduledTestPlanRepository) ListDue(ctx context.Context, now time.Time) ([]*service.ScheduledTestPlan, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+scheduledPlanColumns+`
		FROM scheduled_test_plans
		WHERE enabled = true AND next_run_at <= $1 AND (pelican_running_until IS NULL OR pelican_running_until <= $1)
		ORDER BY next_run_at ASC
	`, now)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanPlans(rows)
}

func (r *scheduledTestPlanRepository) Update(ctx context.Context, plan *service.ScheduledTestPlan) (*service.ScheduledTestPlan, error) {
	config, err := scheduledIQConfig(plan.PelicanConfig)
	if err != nil {
		return nil, err
	}
	row := r.db.QueryRowContext(ctx, `
		UPDATE scheduled_test_plans
		SET model_id = $2, cron_expression = $3, enabled = $4, max_results = $5, auto_recover = $6, next_run_at = $7, pelican_config = $8, updated_at = NOW()
		WHERE id = $1
		RETURNING `+scheduledPlanColumns, plan.ID, plan.ModelID, plan.CronExpression, plan.Enabled, plan.MaxResults, plan.AutoRecover, plan.NextRunAt, config)
	return scanPlan(row)
}

func (r *scheduledTestPlanRepository) Delete(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM scheduled_test_plans WHERE id = $1`, id)
	return err
}

func (r *scheduledTestPlanRepository) UpdateAfterRun(ctx context.Context, id int64, lastRunAt time.Time, nextRunAt time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE scheduled_test_plans SET last_run_at = $2, next_run_at = $3, updated_at = NOW() WHERE id = $1
	`, id, lastRunAt, nextRunAt)
	return err
}

func (r *scheduledTestPlanRepository) ClaimIQPlan(ctx context.Context, id int64, updatedAt, now, leaseUntil time.Time) (bool, error) {
	result, err := r.db.ExecContext(ctx, `
		UPDATE scheduled_test_plans SET pelican_running_until = $4
		WHERE id = $1 AND updated_at = $2 AND enabled AND next_run_at <= $3
			AND (pelican_running_until IS NULL OR pelican_running_until <= $3)
	`, id, updatedAt, now, leaseUntil)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (r *scheduledTestPlanRepository) FinishIQPlan(ctx context.Context, plan *service.ScheduledTestPlan, leaseUntil, finishedAt, nextRunAt time.Time, results []*service.ScheduledTestResult) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var maxResults int
	var autoRecover bool
	err = tx.QueryRowContext(ctx, `
		UPDATE scheduled_test_plans SET pelican_running_until = NULL,
			last_run_at = $4, next_run_at = CASE WHEN updated_at = $2 THEN $5 ELSE next_run_at END
		WHERE id = $1 AND pelican_running_until = $3
		RETURNING max_results, (updated_at = $2 AND enabled AND auto_recover)
	`, plan.ID, plan.UpdatedAt, leaseUntil, finishedAt, nextRunAt).Scan(&maxResults, &autoRecover)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, result := range results {
		config, err := scheduledIQConfig(result.PelicanConfig)
		if err != nil {
			return false, err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO scheduled_test_results (plan_id, status, response_text, error_message, latency_ms, started_at, finished_at, created_at, pelican_config)
			VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), $8)
		`, plan.ID, result.Status, result.ResponseText, result.ErrorMessage, result.LatencyMs, result.StartedAt, result.FinishedAt, config)
		if err != nil {
			return false, err
		}
	}
	_, err = tx.ExecContext(ctx, `
		DELETE FROM scheduled_test_results WHERE plan_id = $1 AND id IN (
			SELECT id FROM scheduled_test_results WHERE plan_id = $1 ORDER BY created_at DESC, id DESC OFFSET $2
		)
	`, plan.ID, maxResults)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return autoRecover, nil
}

// --- Result Repository ---

type scheduledTestResultRepository struct {
	db *sql.DB
}

func NewScheduledTestResultRepository(db *sql.DB) service.ScheduledTestResultRepository {
	return &scheduledTestResultRepository{db: db}
}

func (r *scheduledTestResultRepository) Create(ctx context.Context, result *service.ScheduledTestResult) (*service.ScheduledTestResult, error) {
	config, err := scheduledIQConfig(result.PelicanConfig)
	if err != nil {
		return nil, err
	}
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO scheduled_test_results (plan_id, status, response_text, error_message, latency_ms, started_at, finished_at, created_at, pelican_config)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), $8)
		RETURNING `+scheduledResultColumns, result.PlanID, result.Status, result.ResponseText, result.ErrorMessage, result.LatencyMs, result.StartedAt, result.FinishedAt, config)
	return scanScheduledResult(row)
}

func (r *scheduledTestResultRepository) ListByPlanID(ctx context.Context, planID int64, limit int) ([]*service.ScheduledTestResult, error) {
	return r.listByPlanID(ctx, planID, limit, scheduledResultColumns)
}

func (r *scheduledTestResultRepository) ListResultSummaries(ctx context.Context, planID int64, limit int) ([]*service.ScheduledTestResult, error) {
	const columns = "id, plan_id, status, '', error_message, latency_ms, started_at, finished_at, created_at, pelican_config"
	return r.listByPlanID(ctx, planID, limit, columns)
}

func (r *scheduledTestResultRepository) listByPlanID(ctx context.Context, planID int64, limit int, columns string) ([]*service.ScheduledTestResult, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+columns+`
		FROM scheduled_test_results
		WHERE plan_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2
	`, planID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []*service.ScheduledTestResult
	for rows.Next() {
		result, err := scanScheduledResult(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, rows.Err()
}

func (r *scheduledTestResultRepository) GetResult(ctx context.Context, planID, resultID int64) (*service.ScheduledTestResult, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT `+scheduledResultColumns+` FROM scheduled_test_results WHERE plan_id = $1 AND id = $2
	`, planID, resultID)
	return scanScheduledResult(row)
}

func (r *scheduledTestResultRepository) ListPelicanHistory(ctx context.Context, beforeID int64, limit int, accountID int64) ([]*service.PelicanHistoryResult, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT r.id, r.plan_id, r.status, r.error_message, r.latency_ms,
			r.started_at, r.finished_at, r.created_at, r.pelican_config, a.id, a.name
		FROM scheduled_test_results r
		JOIN scheduled_test_plans p ON p.id = r.plan_id
		JOIN accounts a ON a.id = p.account_id
		WHERE r.pelican_config IS NOT NULL AND a.deleted_at IS NULL
			AND ($1::bigint = 0 OR r.id < $1)
			AND ($3::bigint = 0 OR p.account_id = $3)
		ORDER BY r.id DESC LIMIT $2
	`, beforeID, limit, accountID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	results := make([]*service.PelicanHistoryResult, 0)
	for rows.Next() {
		result := new(service.PelicanHistoryResult)
		var config []byte
		if err := rows.Scan(&result.ID, &result.PlanID, &result.Status, &result.ErrorMessage,
			&result.LatencyMs, &result.StartedAt, &result.FinishedAt, &result.CreatedAt,
			&config, &result.AccountID, &result.AccountName); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(config, &result.PelicanConfig); err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, rows.Err()
}

func (r *scheduledTestResultRepository) PruneExpiredIQResults(ctx context.Context, before time.Time) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM scheduled_test_results WHERE pelican_config IS NOT NULL AND created_at < $1`, before)
	return err
}

func (r *scheduledTestResultRepository) PruneOldResults(ctx context.Context, planID int64, keepCount int) error {
	_, err := r.db.ExecContext(ctx, `
		DELETE FROM scheduled_test_results
		WHERE id IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (PARTITION BY plan_id ORDER BY created_at DESC, id DESC) AS rn
				FROM scheduled_test_results
				WHERE plan_id = $1
			) ranked
			WHERE rn > $2
		)
	`, planID, keepCount)
	return err
}

// --- scan helpers ---

type scannable interface {
	Scan(dest ...any) error
}

func scanPlan(row scannable) (*service.ScheduledTestPlan, error) {
	p := &service.ScheduledTestPlan{}
	var config []byte
	if err := row.Scan(
		&p.ID, &p.AccountID, &p.ModelID, &p.CronExpression, &p.Enabled, &p.MaxResults, &p.AutoRecover,
		&p.LastRunAt, &p.NextRunAt, &p.CreatedAt, &p.UpdatedAt, &config,
	); err != nil {
		return nil, err
	}
	if config != nil {
		if err := json.Unmarshal(config, &p.PelicanConfig); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func scanScheduledResult(row scannable) (*service.ScheduledTestResult, error) {
	result := &service.ScheduledTestResult{}
	var config []byte
	if err := row.Scan(
		&result.ID, &result.PlanID, &result.Status, &result.ResponseText, &result.ErrorMessage,
		&result.LatencyMs, &result.StartedAt, &result.FinishedAt, &result.CreatedAt, &config,
	); err != nil {
		return nil, err
	}
	if config != nil {
		if err := json.Unmarshal(config, &result.PelicanConfig); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func scanPlans(rows *sql.Rows) ([]*service.ScheduledTestPlan, error) {
	var plans []*service.ScheduledTestPlan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		plans = append(plans, p)
	}
	return plans, rows.Err()
}
