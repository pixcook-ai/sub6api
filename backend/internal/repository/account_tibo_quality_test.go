package repository

import (
	"context"
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

func TestTiboQualityIsolationReadUsesNarrowAuthorityQuery(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		isolated, missingParent bool
		wantErr                 bool
	}{
		{name: "default disabled account"},
		{name: "persisted isolation survives disabled monitor", isolated: true},
		{name: "parent missing fails closed", missingParent: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer func() { _ = db.Close() }()
			repo := &accountRepository{sql: db}
			mock.ExpectQuery(regexp.QuoteMeta(openAITiboQualityIsolationSQL)).WithArgs(int64(42)).
				WillReturnRows(sqlmock.NewRows([]string{"isolated", "missing_parent"}).AddRow(tc.isolated, tc.missingParent))
			got, err := repo.GetOpenAITiboQualityIsolation(context.Background(), 42)
			if tc.wantErr {
				require.ErrorIs(t, err, service.ErrAccountNotFound)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.isolated, got)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestTiboQualityIsolationAuthorityFailureDoesNotAssumeHealthy(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	repo := &accountRepository{sql: db}
	unavailable := errors.New("database unavailable")
	mock.ExpectQuery(regexp.QuoteMeta(openAITiboQualityIsolationSQL)).WithArgs(int64(42)).WillReturnError(unavailable)
	_, err = repo.GetOpenAITiboQualityIsolation(context.Background(), 42)
	require.ErrorIs(t, err, unavailable)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestTiboQualityIsolationReadUsesContextTransaction(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	mock.ExpectBegin()
	tx, err := client.Tx(context.Background())
	require.NoError(t, err)
	mock.ExpectQuery(regexp.QuoteMeta(openAITiboQualityIsolationSQL)).WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"isolated", "missing_parent"}).AddRow(true, false))
	// No fallback sql executor: this must use the transaction supplied in context.
	repo := &accountRepository{client: client}
	isolated, err := repo.GetOpenAITiboQualityIsolation(dbent.NewTxContext(context.Background(), tx), 42)
	require.NoError(t, err)
	require.True(t, isolated)
	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}
