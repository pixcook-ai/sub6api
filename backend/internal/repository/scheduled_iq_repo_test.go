//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestScheduledTestIQLeaseFencesStaleCompletion(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	repo := &scheduledTestPlanRepository{db: db}
	now := time.Now().UTC().Truncate(time.Microsecond)
	lease := now.Add(6 * time.Minute)
	plan := &service.ScheduledTestPlan{ID: 1, UpdatedAt: now.Add(-time.Hour)}

	mock.ExpectExec(`UPDATE scheduled_test_plans SET pelican_running_until = \$4 WHERE id = \$1 AND updated_at = \$2 AND enabled AND next_run_at <= \$3 AND \(pelican_running_until IS NULL OR pelican_running_until <= \$3\)`).
		WithArgs(plan.ID, plan.UpdatedAt, now, lease).WillReturnResult(sqlmock.NewResult(0, 1))
	claimed, err := repo.ClaimIQPlan(context.Background(), plan.ID, plan.UpdatedAt, now, lease)
	require.NoError(t, err)
	require.True(t, claimed)

	mock.ExpectBegin()
	mock.ExpectQuery(`UPDATE scheduled_test_plans SET pelican_running_until = NULL.*WHERE id = \$1 AND pelican_running_until = \$3`).
		WithArgs(plan.ID, plan.UpdatedAt, lease, now, now.Add(time.Hour)).
		WillReturnRows(sqlmock.NewRows([]string{"max_results", "auto_recover"}))
	mock.ExpectRollback()
	recoverable, err := repo.FinishIQPlan(context.Background(), plan, lease, now, now.Add(time.Hour), []*service.ScheduledTestResult{{Status: "success"}})
	require.NoError(t, err)
	require.False(t, recoverable)
	require.NoError(t, mock.ExpectationsWereMet())
}
