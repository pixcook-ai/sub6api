package repository

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

const controllerRepositoryOperation = "36dc40ec-a9f6-4e46-a4e0-ab899ce1b85e"

func controllerRepositoryRows(identity service.ManagedOAuthIdentity, active bool) *sqlmock.Rows {
	raw, _ := json.Marshal(identity)
	return sqlmock.NewRows([]string{"account_id", "operation_id", "identity", "active"}).AddRow(int64(17), controllerRepositoryOperation, string(raw), active)
}

func TestManagedOAuthControllerRepositoryRegisterAndLostResponseRetry(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &accountRepository{sql: db}
	state := managedRepositoryState()
	before, _ := json.Marshal(state)
	identity, _ := json.Marshal(state.ManagedOAuthIdentity)
	hash := strings.Repeat("a", 64)
	for i := 0; i < 2; i++ {
		mock.ExpectQuery(regexp.QuoteMeta(putManagedOAuthControllerKeySQL)).WithArgs(int64(17), string(before), controllerRepositoryOperation, hash, string(identity)).WillReturnRows(controllerRepositoryRows(state.ManagedOAuthIdentity, true))
		got, err := repo.PutManagedOAuthControllerKey(context.Background(), 17, state, controllerRepositoryOperation, hash)
		require.NoError(t, err)
		require.Equal(t, state.ManagedOAuthIdentity, got.ManagedOAuthIdentity)
		require.True(t, got.Active)
		public, err := json.Marshal(got)
		require.NoError(t, err)
		require.NotContains(t, string(public), hash)
		require.NotContains(t, string(public), "key_hash")
	}
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestManagedOAuthControllerRepositoryConflictsFailClosed(t *testing.T) {
	for _, scenario := range []string{"stale_state", "different_payload", "revoked_operation", "duplicate_hash"} {
		t.Run(scenario, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			repo := &accountRepository{sql: db}
			expect := mock.ExpectQuery(regexp.QuoteMeta(putManagedOAuthControllerKeySQL))
			if scenario == "duplicate_hash" {
				expect.WillReturnError(&pq.Error{Code: "23505"})
			} else {
				expect.WillReturnRows(sqlmock.NewRows([]string{"account_id", "operation_id", "identity", "active"}))
			}
			got, err := repo.PutManagedOAuthControllerKey(context.Background(), 17, managedRepositoryState(), controllerRepositoryOperation, strings.Repeat("a", 64))
			require.Nil(t, got)
			require.ErrorIs(t, err, service.ErrManagedOAuthConflict)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestManagedOAuthControllerRepositoryRevokeIsIdempotentAndUnknownConflicts(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &accountRepository{sql: db}
	for i := 0; i < 2; i++ {
		mock.ExpectQuery(regexp.QuoteMeta(revokeManagedOAuthControllerKeySQL)).WithArgs(int64(17), controllerRepositoryOperation).WillReturnRows(controllerRepositoryRows(managedRepositoryState().ManagedOAuthIdentity, false))
		got, err := repo.RevokeManagedOAuthControllerKey(context.Background(), 17, controllerRepositoryOperation)
		require.NoError(t, err)
		require.False(t, got.Active)
	}
	mock.ExpectQuery(regexp.QuoteMeta(revokeManagedOAuthControllerKeySQL)).WillReturnRows(sqlmock.NewRows([]string{"account_id", "operation_id", "identity", "active"}))
	_, err = repo.RevokeManagedOAuthControllerKey(context.Background(), 17, controllerRepositoryOperation)
	require.ErrorIs(t, err, service.ErrManagedOAuthConflict)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestManagedOAuthControllerRepositoryLookupAndScanErrors(t *testing.T) {
	for _, scenario := range []string{"active", "revoked", "unknown", "invalid_identity", "invalid_operation", "scan_error", "query_error", "row_error"} {
		t.Run(scenario, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			repo := &accountRepository{sql: db}
			expect := mock.ExpectQuery(regexp.QuoteMeta(findManagedOAuthControllerKeySQL)).WithArgs(strings.Repeat("a", 64))
			rows := controllerRepositoryRows(managedRepositoryState().ManagedOAuthIdentity, true)
			switch scenario {
			case "unknown":
				rows = sqlmock.NewRows([]string{"account_id", "operation_id", "identity", "active"})
			case "revoked":
				rows = controllerRepositoryRows(managedRepositoryState().ManagedOAuthIdentity, false)
			case "invalid_identity":
				rows = controllerRepositoryRows(service.ManagedOAuthIdentity{}, true)
			case "invalid_operation":
				rows = sqlmock.NewRows([]string{"account_id", "operation_id", "identity", "active"}).AddRow(int64(17), "bad-operation", `{}`, true)
			case "scan_error":
				rows = sqlmock.NewRows([]string{"account_id", "operation_id", "identity", "active"}).AddRow("invalid-number", controllerRepositoryOperation, `{}`, true)
			case "query_error":
				expect.WillReturnError(errors.New("database offline"))
			case "row_error":
				rows.RowError(0, errors.New("read failed"))
			}
			if scenario != "query_error" {
				expect.WillReturnRows(rows)
			}
			got, err := repo.FindManagedOAuthControllerKey(context.Background(), strings.Repeat("a", 64))
			if scenario == "active" || scenario == "revoked" {
				require.NoError(t, err)
				require.Equal(t, scenario == "active", got.Active)
			} else {
				require.Error(t, err)
				require.Nil(t, got)
				if scenario == "unknown" {
					require.ErrorIs(t, err, service.ErrManagedOAuthControllerKeyUnknown)
				} else {
					require.NotErrorIs(t, err, service.ErrManagedOAuthControllerKeyUnknown)
				}
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestManagedOAuthControllerRepositoryRejectsInvalidInputsBeforeSQL(t *testing.T) {
	repo := &accountRepository{}
	for _, hash := range []string{"secret", strings.Repeat("A", 64), strings.Repeat("z", 64)} {
		_, err := repo.FindManagedOAuthControllerKey(context.Background(), hash)
		require.ErrorIs(t, err, service.ErrManagedOAuthInvalid)
		_, err = repo.PutManagedOAuthControllerKey(context.Background(), 17, managedRepositoryState(), controllerRepositoryOperation, hash)
		require.ErrorIs(t, err, service.ErrManagedOAuthInvalid)
	}
	_, err := repo.RevokeManagedOAuthControllerKey(context.Background(), 17, "00000000-0000-0000-0000-000000000000")
	require.ErrorIs(t, err, service.ErrManagedOAuthInvalid)
}

func TestManagedOAuthControllerRepositorySQLAndMigrationBoundaries(t *testing.T) {
	require.Contains(t, putManagedOAuthControllerKeySQL, "FOR NO KEY UPDATE")
	require.Contains(t, revokeManagedOAuthControllerKeySQL, "FOR NO KEY UPDATE")
	require.Contains(t, putManagedOAuthControllerKeySQL, "a.extra->'managed_oauth'=$2::jsonb")
	require.Contains(t, putManagedOAuthControllerKeySQL, "a.deleted_at IS NULL")
	require.Contains(t, putManagedOAuthControllerKeySQL, "a.platform='openai' AND a.type='oauth'")
	require.Contains(t, putManagedOAuthControllerKeySQL, "COALESCE(a.credentials->>'refresh_token','')=''")
	require.Contains(t, putManagedOAuthControllerKeySQL, "k.key_hash=EXCLUDED.key_hash AND k.identity=EXCLUDED.identity AND k.revoked_at IS NULL")
	require.Contains(t, revokeManagedOAuthControllerKeySQL, "COALESCE(k.revoked_at,NOW())")
	require.NotContains(t, findManagedOAuthControllerKeySQL, "AND revoked_at IS NULL")
	for _, query := range []string{putManagedOAuthControllerKeySQL, revokeManagedOAuthControllerKeySQL, findManagedOAuthControllerKeySQL} {
		require.NotContains(t, query, "schedulable")
		require.NotContains(t, query, "access_token")
		require.NotContains(t, query, "DELETE FROM")
	}
	migration, err := os.ReadFile("../../migrations/252_managed_oauth_controller_keys.sql")
	require.NoError(t, err)
	require.Contains(t, string(migration), "key_hash TEXT NOT NULL UNIQUE")
	require.Contains(t, string(migration), "PRIMARY KEY (account_id,operation_id)")
	require.NotContains(t, string(migration), "access_token")
}
