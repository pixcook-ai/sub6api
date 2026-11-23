package repository

import (
	"context"
	"encoding/json"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
	"regexp"
	"testing"
)

func repositoryDeploymentFixture() service.ManagedDeployment {
	b := service.ManagedDeploymentBinding{Provider: "bps_excel", AccountID: 11, BackendID: "bps", BindingRef: "bind_11111111-1111-4111-8111-111111111111", ProfileRef: "oauth_22222222-2222-4222-8222-222222222222", IdentityEpoch: "33333333-3333-4333-8333-333333333333", Subject: "sub", WorkspaceID: "ws"}
	c := b
	c.Provider = "codex"
	c.AccountID = 12
	return service.ManagedDeployment{Contract: service.ManagedDeploymentContract, DeploymentID: "deployment_44444444-4444-4444-8444-444444444444", GroupID: 1, RouteRevision: 1, SelectedProvider: "bps_excel", BPS: b, Codex: c}
}
func TestManagedDeploymentRepositoryReturnsWrittenSnapshotWithoutReread(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	r := &accountRepository{sql: db}
	d := repositoryDeploymentFixture()
	b, _ := json.Marshal(d.BPS)
	c, _ := json.Marshal(d.Codex)
	m.ExpectQuery(regexp.QuoteMeta(putManagedDeploymentSQL)).WithArgs(d.DeploymentID, d.GroupID, d.RouteRevision, d.SelectedProvider, string(b), string(c), int64(11), int64(12), int64(0), service.ManagedOAuthContract).WillReturnRows(sqlmock.NewRows([]string{"id", "group", "revision", "selected", "bps", "codex"}).AddRow(d.DeploymentID, d.GroupID, d.RouteRevision, d.SelectedProvider, b, c))
	actual, e := r.PutManagedDeployment(context.Background(), &service.ManagedDeploymentUpdate{ManagedDeployment: d})
	require.NoError(t, e)
	require.Equal(t, d, *actual)
	require.NoError(t, m.ExpectationsWereMet())
}
func TestManagedDeploymentRepositoryGroupCollisionIsConflict(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	r := &accountRepository{sql: db}
	m.ExpectQuery(regexp.QuoteMeta(putManagedDeploymentSQL)).WillReturnError(&pq.Error{Code: "23505"})
	_, e = r.PutManagedDeployment(context.Background(), &service.ManagedDeploymentUpdate{ManagedDeployment: repositoryDeploymentFixture()})
	require.ErrorIs(t, e, service.ErrManagedDeploymentConflict)
	require.NoError(t, m.ExpectationsWereMet())
}
func TestManagedDeploymentRepositoryResourceWritesNeverMoveIdentity(t *testing.T) {
	for _, affected := range []int64{0, 1} {
		db, m, e := sqlmock.New()
		require.NoError(t, e)
		r := &accountRepository{sql: db}
		s := &service.ManagedDeploymentSession{DeploymentID: "deployment", UserID: 1, APIKeyID: 2, SessionKey: "hashed-session"}
		m.ExpectExec(regexp.QuoteMeta(bindManagedDeploymentResourceSQL)).WithArgs(s.DeploymentID, int64(1), int64(2), "response", service.ManagedDeploymentOwnerHash("resp_test"), s.SessionKey).WillReturnResult(sqlmock.NewResult(0, affected))
		e = r.BindManagedDeploymentResource(context.Background(), s, "response", "resp_test")
		if affected == 0 {
			require.ErrorIs(t, e, service.ErrManagedDeploymentConflict)
		} else {
			require.NoError(t, e)
		}
		require.NoError(t, m.ExpectationsWereMet())
		db.Close()
	}
}
func TestManagedDeploymentRepositorySessionUsesAtomicUpsert(t *testing.T) {
	db, m, e := sqlmock.New()
	require.NoError(t, e)
	defer db.Close()
	r := &accountRepository{sql: db}
	d := repositoryDeploymentFixture()
	b, _ := json.Marshal(d.BPS)
	m.ExpectQuery(regexp.QuoteMeta(bindManagedDeploymentSessionSQL)).WithArgs(int64(1), int64(2), int64(3), "hash", false).WillReturnRows(sqlmock.NewRows([]string{"id", "revision", "binding"}).AddRow(d.DeploymentID, int64(1), b))
	s, e := r.BindManagedDeploymentSession(context.Background(), 1, 2, 3, "hash", false)
	require.NoError(t, e)
	require.Equal(t, d.BPS, s.Binding)
	require.Equal(t, int64(2), s.UserID)
	require.Equal(t, int64(3), s.APIKeyID)
	require.NoError(t, m.ExpectationsWereMet())
}
