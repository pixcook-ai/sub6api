package repository

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/MACOS-DO/sub4api/ent"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestCoreOAuthRepositoryBulkRejectsReservedExtra(t *testing.T) {
	for _, key := range []string{service.CoreOAuthAccountExtraKey, service.CoreOAuthRuntimeShellExtraKey, service.ManagedDeploymentAccountExtraKey} {
		for name, value := range map[string]any{"nil": nil, "malformed": "malformed", "empty object": map[string]any{}} {
			t.Run(key+"/"+name, func(t *testing.T) {
				db, mock, err := sqlmock.New()
				require.NoError(t, err)
				defer db.Close()
				repo := &accountRepository{sql: db}

				changed, err := repo.BulkUpdate(context.Background(), []int64{17}, service.AccountBulkUpdate{Extra: map[string]any{key: value}})

				require.ErrorIs(t, err, service.ErrCoreOAuthConflict)
				require.Zero(t, changed)
				require.NoError(t, mock.ExpectationsWereMet())
			})
		}
	}
}

func TestCoreOAuthRepositoryBulkResumeFencesMixedBatch(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &accountRepository{sql: db}

	// Simulate a batch with a core-owned row: the SQL gate must reject every row.
	mock.ExpectExec(`WITH managed_targets AS MATERIALIZED .*FOR NO KEY UPDATE.*UPDATE accounts.*NOT EXISTS .*core_oauth`).
		WithArgs(true, "{17,18}").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT id FROM accounts WHERE id = ANY\(\$1\) AND deleted_at IS NULL AND .*core_oauth.*core_oauth_runtime_shell`).
		WithArgs("{17,18}").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(18))
	resume := true
	changed, err := repo.BulkUpdate(context.Background(), []int64{17, 18}, service.AccountBulkUpdate{Schedulable: &resume})
	require.ErrorIs(t, err, service.ErrCoreOAuthConflict)
	require.Zero(t, changed)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCoreOAuthRepositoryEditReadsPauseWhileHoldingAccountLock(t *testing.T) {
	for _, billing := range []bool{false, true} {
		t.Run(map[bool]string{false: "Update", true: "UpdateWithAccountBillingSettings"}[billing], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })
			repo := newAccountRepositoryWithSQL(client, db, nil)
			account := &service.Account{
				ID: 27, Name: "test", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "sk-test"},
				Extra:       map[string]any{service.CoreOAuthAccountExtraKey: map[string]any{"contract": service.CoreOAuthContract}},
				Concurrency: 1, Priority: 1, Status: service.StatusActive, Schedulable: true,
			}
			mock.ExpectBegin()
			mock.ExpectQuery(`(?s)SELECT.*FOR NO KEY UPDATE`).
				WithArgs(int64(27), service.PlatformOpenAI, service.AccountTypeAPIKey, `{"api_key":"sk-test"}`, nil).
				WillReturnRows(sqlmock.NewRows([]string{"identity_unchanged", "ollama_group_unchanged", "ollama_proxy_unchanged", "enabled", "rate_sync_enabled", "snapshot", "ollama_session", "ollama_auto", "ollama_snapshot", "opencode_group_unchanged", "opencode_auto", "opencode_snapshot", "current_extra"}).
					AddRow(true, false, true, nil, nil, nil, nil, nil, nil, false, nil, nil, []byte(`{"core_oauth":{"contract":"sub6-core-oauth/v1"}}`)))
			// A second read under the existing row lock must see the later manual pause.
			mock.ExpectQuery(`SELECT .*"schedulable" FROM "accounts" WHERE .*"id" = \$1 AND .*"deleted_at" IS NULL LIMIT 2`).
				WithArgs(int64(27)).WillReturnRows(sqlmock.NewRows([]string{"id", "schedulable"}).AddRow(27, false))
			writeFailure := errors.New("stop after protected scheduling value was selected")
			mock.ExpectExec(`(?s)UPDATE .*accounts.*SET.*WHERE .*id.*`).WillReturnError(writeFailure)
			mock.ExpectRollback()
			if billing {
				err = repo.UpdateWithAccountBillingSettings(context.Background(), account, nil, nil, nil)
			} else {
				err = repo.Update(context.Background(), account)
			}
			require.ErrorIs(t, err, writeFailure)
			require.False(t, account.Schedulable)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestCoreOAuthRepositorySingleResumeRequiresUnownedAccount(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)

	// The ownership predicate belongs in the write itself so ownership installed
	// after a caller's read cannot be bypassed by the scheduling endpoint.
	mock.ExpectExec(`(?s)UPDATE .*accounts.*schedulable.*WHERE .*id.*NOT.*core_oauth.*core_oauth_runtime_shell`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT .*"id" FROM "accounts" WHERE .*"id" = \$1 AND .*"deleted_at" IS NULL LIMIT 2`).
		WithArgs(int64(27)).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(27))
	err = repo.SetSchedulable(context.Background(), 27, true)
	require.ErrorIs(t, err, service.ErrCoreOAuthConflict)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCoreOAuthRepositorySingleResumeMissingReturnsNotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)
	mock.ExpectExec(`(?s)UPDATE .*accounts.*NOT.*core_oauth.*core_oauth_runtime_shell`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT .*"id" FROM "accounts" WHERE .*"id" = \$1 AND .*"deleted_at" IS NULL LIMIT 2`).
		WithArgs(int64(27)).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	require.ErrorIs(t, repo.SetSchedulable(context.Background(), 27, true), service.ErrAccountNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCoreOAuthRepositoryBulkResumeMissingTargetsIsNoOp(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := &accountRepository{sql: db}
	mock.ExpectExec(`WITH managed_targets AS MATERIALIZED .*FOR NO KEY UPDATE.*UPDATE accounts.*NOT EXISTS .*core_oauth`).
		WithArgs(true, "{17,18}").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT id FROM accounts WHERE id = ANY\(\$1\) AND deleted_at IS NULL AND .*core_oauth.*core_oauth_runtime_shell`).
		WithArgs("{17,18}").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	resume := true
	changed, err := repo.BulkUpdate(context.Background(), []int64{17, 18}, service.AccountBulkUpdate{Schedulable: &resume})
	require.NoError(t, err)
	require.Zero(t, changed)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCoreOAuthRepositoryStaleEditCannotOverwriteInstalledCoreBinding(t *testing.T) {
	core := map[string]any{"contract": service.CoreOAuthContract, "token_revision": 2}
	installedExtra := map[string]any{service.CoreOAuthAccountExtraKey: core, service.ManagedDeploymentAccountExtraKey: map[string]any{"binding_ref": "installed"}}
	currentExtra, err := json.Marshal(installedExtra)
	require.NoError(t, err)
	for _, billing := range []bool{false, true} {
		for _, tc := range []struct {
			name              string
			extra             map[string]any
			identityUnchanged bool
		}{
			{"ordinary snapshot", nil, false},
			{"shell snapshot", map[string]any{service.CoreOAuthRuntimeShellExtraKey: map[string]any{"provider": service.CoreOAuthProviderExcel, "group_id": 1}}, false},
			{"stale core marker", map[string]any{service.CoreOAuthAccountExtraKey: map[string]any{"contract": service.CoreOAuthContract, "token_revision": 1}}, true},
			{"changed runtime binding", map[string]any{service.CoreOAuthAccountExtraKey: core, service.ManagedDeploymentAccountExtraKey: map[string]any{"binding_ref": "old"}}, true},
			{"changed credentials or transport identity", installedExtra, false},
		} {
			t.Run(map[bool]string{false: "Update/", true: "UpdateWithBilling/"}[billing]+tc.name, func(t *testing.T) {
				db, mock, err := sqlmock.New()
				require.NoError(t, err)
				t.Cleanup(func() { _ = db.Close() })
				client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
				t.Cleanup(func() { _ = client.Close() })
				repo := newAccountRepositoryWithSQL(client, db, nil)
				account := &service.Account{ID: 27, Name: "stale edit", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"api_key": "synthetic-old-key"}, Extra: tc.extra, Concurrency: 1, Status: service.StatusActive}
				mock.ExpectBegin()
				mock.ExpectQuery(`(?s)SELECT.*FOR NO KEY UPDATE`).
					WithArgs(int64(27), service.PlatformOpenAI, service.AccountTypeAPIKey, `{"api_key":"synthetic-old-key"}`, nil).
					WillReturnRows(sqlmock.NewRows([]string{"identity_unchanged", "ollama_group_unchanged", "ollama_proxy_unchanged", "enabled", "rate_sync_enabled", "snapshot", "ollama_session", "ollama_auto", "ollama_snapshot", "opencode_group_unchanged", "opencode_auto", "opencode_snapshot", "current_extra"}).
						AddRow(tc.identityUnchanged, false, true, nil, nil, nil, nil, nil, nil, false, nil, nil, currentExtra))
				mock.ExpectRollback()
				if billing {
					err = repo.UpdateWithAccountBillingSettings(context.Background(), account, nil, nil, nil)
				} else {
					err = repo.Update(context.Background(), account)
				}
				require.ErrorIs(t, err, service.ErrCoreOAuthConflict)
				require.NoError(t, mock.ExpectationsWereMet())
			})
		}
	}
}

func TestCoreOAuthRepositoryBulkIdentityMutationFencesInstalledCore(t *testing.T) {
	proxyID := int64(12)
	for _, tc := range []struct {
		name    string
		updates service.AccountBulkUpdate
	}{
		{"credentials", service.AccountBulkUpdate{Credentials: map[string]any{"api_key": "synthetic-stale-key"}}},
		{"extra", service.AccountBulkUpdate{Extra: map[string]any{"untrusted": true}}},
		{"proxy", service.AccountBulkUpdate{ProxyID: &proxyID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			repo := &accountRepository{sql: db}
			// A prior ordinary/shell read must not bypass current ownership.
			mock.ExpectExec(`(?s)WITH managed_targets AS MATERIALIZED .*FOR NO KEY UPDATE.*UPDATE accounts.*NOT EXISTS .*core_oauth`).
				WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery(`SELECT id FROM accounts WHERE id = ANY\(\$1\) AND deleted_at IS NULL AND .*core_oauth`).
				WithArgs("{17,18}").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(18))
			changed, err := repo.BulkUpdate(context.Background(), []int64{17, 18}, tc.updates)
			require.ErrorIs(t, err, service.ErrCoreOAuthConflict)
			require.Zero(t, changed)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestCoreOAuthRepositoryDirectCredentialsRespectCurrentOwnership(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra map[string]any
		want  error
	}{
		{"core", map[string]any{service.CoreOAuthAccountExtraKey: nil}, service.ErrCoreOAuthConflict},
		{"shell", map[string]any{service.CoreOAuthRuntimeShellExtraKey: nil}, service.ErrCoreOAuthConflict},
		{"managed", map[string]any{service.ManagedOAuthExtraKey: nil}, service.ErrManagedOAuthConflict},
		{"missing", nil, service.ErrAccountNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })
			repo := newAccountRepositoryWithSQL(client, db, nil)
			mock.ExpectBegin()
			mock.ExpectExec(`(?s)UPDATE accounts.*WHERE id = \$2 AND deleted_at IS NULL AND NOT.*managed_oauth.*core_oauth.*core_oauth_runtime_shell`).
				WithArgs(`{"access_token":"synthetic-stale-at"}`, int64(27)).WillReturnResult(sqlmock.NewResult(0, 0))
			rows := sqlmock.NewRows([]string{"id", "extra"})
			if tc.extra != nil {
				raw, err := json.Marshal(tc.extra)
				require.NoError(t, err)
				rows.AddRow(27, raw)
			}
			mock.ExpectQuery(`SELECT .*"extra" FROM "accounts" WHERE .*"id" = \$1 AND .*"deleted_at" IS NULL LIMIT 2`).
				WithArgs(int64(27)).WillReturnRows(rows)
			mock.ExpectRollback()
			require.ErrorIs(t, repo.UpdateCredentials(context.Background(), 27, map[string]any{"access_token": "synthetic-stale-at"}), tc.want)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestCoreOAuthRepositoryExtraCannotMintReservedOwnership(t *testing.T) {
	for _, key := range []string{service.CoreOAuthAccountExtraKey, service.CoreOAuthRuntimeShellExtraKey, service.ManagedDeploymentAccountExtraKey} {
		for name, value := range map[string]any{"nil": nil, "malformed": "malformed", "object": map[string]any{}} {
			t.Run(key+"/"+name, func(t *testing.T) {
				db, mock, err := sqlmock.New()
				require.NoError(t, err)
				t.Cleanup(func() { _ = db.Close() })
				client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
				t.Cleanup(func() { _ = client.Close() })
				repo := newAccountRepositoryWithSQL(client, db, nil)
				require.ErrorIs(t, repo.UpdateExtra(context.Background(), 27, map[string]any{key: value}), service.ErrCoreOAuthConflict)
				require.NoError(t, mock.ExpectationsWereMet())
			})
		}
	}
}

func TestCoreOAuthRepositorySchedulingExcludesDeletedAccounts(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "pause", true: "resume"}[enabled], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })
			repo := newAccountRepositoryWithSQL(client, db, nil)
			stopped := errors.New("stop after deleted-row exclusion")
			mock.ExpectExec(`(?s)UPDATE .*accounts.*WHERE .*id.*deleted_at.*IS NULL`).WillReturnError(stopped)
			require.ErrorIs(t, repo.SetSchedulable(context.Background(), 27, enabled), stopped)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
