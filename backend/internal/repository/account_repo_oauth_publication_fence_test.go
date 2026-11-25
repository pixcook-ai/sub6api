package repository

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"regexp"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/MACOS-DO/sub4api/ent"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestManagedOAuthPublicationFenceRepositoryAtomicCommitAndRollback(t *testing.T) {
	for _, failure := range []string{"", "lock", "stale", "pause", "revoke", "outbox", "commit"} {
		t.Run(failure, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			r := &accountRepository{client: client, sql: db}
			state := managedRepositoryState()
			before, _ := json.Marshal(state)
			next := state
			next.PublicationFenceOperationID = controllerRepositoryOperation
			after, _ := json.Marshal(next)
			mock.ExpectBegin()
			lock := mock.ExpectQuery(regexp.QuoteMeta(lockManagedOAuthPublicationSQL)).WithArgs(int64(17), string(before))
			if failure == "lock" {
				lock.WillReturnError(errors.New("offline"))
			} else {
				rows := sqlmock.NewRows([]string{"id"})
				if failure != "stale" {
					rows.AddRow(int64(17))
				}
				lock.WillReturnRows(rows)
			}
			if failure != "lock" && failure != "stale" {
				for _, step := range []struct {
					name, query string
					args        []driver.Value
				}{
					{"pause", fenceManagedOAuthPublicationSQL, []driver.Value{int64(17), string(after)}},
					{"revoke", revokeAllManagedOAuthControllerKeysSQL, []driver.Value{int64(17)}},
					{"outbox", enqueueManagedOAuthPublicationFenceSQL, []driver.Value{int64(17), service.SchedulerOutboxEventAccountChanged}},
				} {
					expect := mock.ExpectExec(regexp.QuoteMeta(step.query)).WithArgs(step.args...)
					if failure == step.name {
						expect.WillReturnError(errors.New("failed"))
						break
					}
					expect.WillReturnResult(sqlmock.NewResult(0, 1))
				}
			}
			if failure == "" {
				mock.ExpectCommit()
			} else if failure == "commit" {
				mock.ExpectCommit().WillReturnError(errors.New("lost commit result"))
			} else {
				mock.ExpectRollback()
			}
			applied, err := r.FenceManagedOAuthPublicationIfMatch(context.Background(), 17, state, controllerRepositoryOperation)
			if failure == "" {
				require.NoError(t, err)
				require.True(t, applied)
			} else {
				require.Error(t, err)
				require.False(t, applied)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
