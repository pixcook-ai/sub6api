package repository

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/MACOS-DO/sub4api/internal/pkg/openai"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
)

func managedRepositoryState() service.ManagedOAuthState {
	return service.ManagedOAuthState{ManagedOAuthIdentity: service.ManagedOAuthIdentity{Contract: service.ManagedOAuthContract, BackendID: "backend", BindingRef: "binding", ProfileRef: "profile", Provider: "codex", Subject: "subject", WorkspaceID: "workspace", IdentityEpoch: "epoch"}, Events: []service.ManagedOAuthAuthEvent{}}
}
func TestManagedOAuthRepositoryInstallAtomicCAS(t *testing.T) {
	for _, affected := range []int64{0, 1} {
		t.Run(map[int64]string{0: "conflict", 1: "installed"}[affected], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			repo := &accountRepository{sql: db}
			before := managedRepositoryState()
			next := before
			next.TokenRevision = 7
			expiry := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
			next.ExpiresAt = expiry.Format(time.RFC3339)
			next.LastOperationID = "operation"
			next.LastOperationHash = strings.Repeat("a", 64)
			beforeJSON, _ := json.Marshal(before)
			nextJSON, _ := json.Marshal(next)
			mock.ExpectExec(regexp.QuoteMeta(installManagedOAuthSQL)).WithArgs("synthetic-at", strconv.FormatInt(expiry.Unix(), 10), next.WorkspaceID, openai.ClientID, service.ManagedOAuthExtraKey, string(nextJSON), int64(17), string(beforeJSON), service.SchedulerOutboxEventAccountChanged).WillReturnResult(sqlmock.NewResult(0, affected))
			installed, err := repo.InstallManagedOAuthCredentialsIfMatch(context.Background(), 17, before, next, "synthetic-at", next.ExpiresAt)
			require.NoError(t, err)
			require.Equal(t, affected == 1, installed)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
func TestManagedOAuthRepositoryEventAtomicCAS(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &accountRepository{sql: db}
	before := managedRepositoryState()
	before.TokenRevision = 2
	next := before
	next.AuthBlocked = true
	next.EventCursor = 1
	beforeJSON, _ := json.Marshal(before)
	nextJSON, _ := json.Marshal(next)
	mock.ExpectExec(regexp.QuoteMeta(recordManagedOAuthEventSQL)).WithArgs(service.ManagedOAuthExtraKey, string(nextJSON), int64(17), string(beforeJSON), "rejected-at", service.SchedulerOutboxEventAccountChanged).WillReturnResult(sqlmock.NewResult(0, 1))
	changed, err := repo.RecordManagedOAuthEventIfMatch(context.Background(), 17, before, next, "rejected-at")
	require.NoError(t, err)
	require.True(t, changed)
	require.NoError(t, mock.ExpectationsWereMet())
	require.NotContains(t, recordManagedOAuthEventSQL, "schedulable")
	require.NotContains(t, installManagedOAuthSQL, "schedulable")
	require.Contains(t, installManagedOAuthSQL, "INSERT INTO scheduler_outbox")
}
func TestManagedOAuthRepositoryWriteFailurePropagates(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &accountRepository{sql: db}
	before := managedRepositoryState()
	next := before
	next.TokenRevision = 1
	failure := errors.New("outbox insert failed")
	mock.ExpectExec(regexp.QuoteMeta(installManagedOAuthSQL)).WillReturnError(failure)
	changed, err := repo.InstallManagedOAuthCredentialsIfMatch(context.Background(), 17, before, next, "at", time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	require.ErrorIs(t, err, failure)
	require.False(t, changed)
	require.NoError(t, mock.ExpectationsWereMet())
}
func TestManagedOAuthRepositoryBulkFencesWholeBatch(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := &accountRepository{sql: db}
	_, err = repo.BulkUpdate(context.Background(), []int64{17}, service.AccountBulkUpdate{Extra: map[string]any{service.ManagedOAuthExtraKey: nil}})
	require.ErrorIs(t, err, service.ErrManagedOAuthRefresh)
	mock.ExpectExec(`WITH managed_targets AS MATERIALIZED .*FOR NO KEY UPDATE.*UPDATE accounts.*NOT EXISTS .*managed_oauth`).WithArgs(sqlmock.AnyArg(), "{17,18}").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT id FROM accounts WHERE id = ANY\(\$1\) AND deleted_at IS NULL AND .*core_oauth`).
		WithArgs("{17,18}").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	changed, err := repo.BulkUpdate(context.Background(), []int64{17, 18}, service.AccountBulkUpdate{Credentials: map[string]any{"refresh_token": "never-write"}})
	require.ErrorIs(t, err, service.ErrManagedOAuthConflict)
	require.Zero(t, changed)
	require.NoError(t, mock.ExpectationsWereMet())
}
