//go:build integration

package repository

import (
	"testing"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestTiboQualityIsolationNarrowReadPersistsAcrossMonitorDisable(t *testing.T) {
	f := newTiboGroupFixture(t)
	parent := f.account(service.AccountTypeOAuth, service.PlatformOpenAI)
	shadow := f.account(service.AccountTypeOAuth, service.PlatformOpenAI)
	isolated, err := f.repo.GetOpenAITiboQualityIsolation(f.ctx, parent.ID)
	require.NoError(t, err)
	require.False(t, isolated, "missing quality key must be false, not SQL NULL")
	_, err = integrationDB.ExecContext(f.ctx, "UPDATE accounts SET extra = extra || '{\"openai_tibo_quality_isolated\":true}'::jsonb WHERE id=$1", parent.ID)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(f.ctx, "UPDATE accounts SET parent_account_id=$1, quota_dimension='spark' WHERE id=$2", parent.ID, shadow.ID)
	require.NoError(t, err)
	// Settings are intentionally disabled after persisting isolation.
	f.settings(service.TiboAutomationSettings{})
	for _, id := range []int64{parent.ID, shadow.ID} {
		isolated, err = f.repo.GetOpenAITiboQualityIsolation(f.ctx, id)
		require.NoError(t, err)
		require.True(t, isolated)
	}
	_, err = integrationDB.ExecContext(f.ctx, "UPDATE accounts SET extra=extra - 'openai_tibo_quality_isolated' WHERE id=$1", parent.ID)
	require.NoError(t, err)
	isolated, err = f.repo.GetOpenAITiboQualityIsolation(f.ctx, shadow.ID)
	require.NoError(t, err)
	require.False(t, isolated, "authority must reflect explicit recovery without loading full accounts")

	// A referenced parent cannot disappear physically: exercise the real FK
	// rather than constructing an impossible dangling relation in the fixture.
	_, err = integrationDB.ExecContext(f.ctx, "DELETE FROM accounts WHERE id=$1", parent.ID)
	var foreignKeyErr *pq.Error
	require.ErrorAs(t, err, &foreignKeyErr)
	require.Equal(t, pq.ErrorCode("23001"), foreignKeyErr.Code, "ON DELETE RESTRICT reports restrict_violation")
	require.Equal(t, "fk_accounts_parent_account_id", foreignKeyErr.Constraint)

	// Soft deletion is permitted by that FK, but both a deleted account and
	// a surviving shadow with an unavailable parent must fail final admission.
	_, err = integrationDB.ExecContext(f.ctx, "UPDATE accounts SET deleted_at=NOW() WHERE id=$1", parent.ID)
	require.NoError(t, err)
	for _, id := range []int64{parent.ID, shadow.ID} {
		isolated, err = f.repo.GetOpenAITiboQualityIsolation(f.ctx, id)
		require.ErrorIs(t, err, service.ErrAccountNotFound)
		require.False(t, isolated, "missing authority is an error, not an affirmative healthy verdict")
	}
}
