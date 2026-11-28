//go:build integration

package repository

import (
	"context"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
	"testing"
)

func (s *AccountRepoSuite) createBPSRefreshAccount() *service.Account {
	a := &service.Account{Name: "bps-refresh-cas", Platform: service.PlatformOpenAIBPS, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: false, Credentials: map[string]any{"access_token": "old-access", "refresh_token": "old-refresh", "chatgpt_account_id": "chosen-workspace"}}
	a.Extra = map[string]any{service.OpenAIBPSCredentialStateExtraKey: map[string]any{"status": "auth_failed", "credential_identity": service.OpenAIBPSCredentialIdentity(service.OpenAIBPSCredentialSnapshotFromAccount(a)), "managed_status_error": true}, "admin_setting": "keep"}
	created := mustCreateAccount(s.T(), s.client, a)
	s.Require().NoError(s.client.Account.UpdateOneID(created.ID).SetSchedulable(false).Exec(s.ctx))
	observed, err := s.repo.GetByID(s.ctx, created.ID)
	s.Require().NoError(err)
	return observed
}
func (s *AccountRepoSuite) TestBPSRefreshCASAppliesPreservesPauseAndExtra() {
	observed := s.createBPSRefreshAccount()
	_, err := s.repo.sql.ExecContext(s.ctx, "TRUNCATE scheduler_outbox")
	s.Require().NoError(err)
	applied, err := s.repo.UpdateOpenAIBPSOAuthCredentialsIfUnchanged(s.ctx, observed, map[string]any{"access_token": "new-access", "refresh_token": "rotated-refresh", "chatgpt_account_id": "chosen-workspace"}, true, false)
	s.Require().NoError(err)
	s.Require().True(applied)
	got, err := s.repo.GetByID(s.ctx, observed.ID)
	s.Require().NoError(err)
	s.Require().Equal("rotated-refresh", got.GetCredential("refresh_token"))
	s.Require().False(got.Schedulable)
	s.Require().Equal("keep", got.Extra["admin_setting"])
	s.Require().NotContains(got.Extra, service.OpenAIBPSCredentialStateExtraKey)
	var count int
	s.Require().NoError(scanSingleRow(s.ctx, s.repo.sql, "SELECT COUNT(*) FROM scheduler_outbox WHERE account_id=$1", []any{observed.ID}, &count))
	s.Require().Equal(1, count)
}
func (s *AccountRepoSuite) TestBPSRefreshCASConcurrentReauthorizationWins() {
	observed := s.createBPSRefreshAccount()
	s.Require().NoError(s.repo.UpdateCredentials(s.ctx, observed.ID, map[string]any{"access_token": "admin-access", "refresh_token": "admin-refresh"}))
	applied, err := s.repo.UpdateOpenAIBPSOAuthCredentialsIfUnchanged(s.ctx, observed, map[string]any{"access_token": "stale-upstream"}, true, false)
	s.Require().NoError(err)
	s.Require().False(applied)
	got, err := s.repo.GetByID(s.ctx, observed.ID)
	s.Require().NoError(err)
	s.Require().Equal("admin-access", got.GetCredential("access_token"))
	applied, err = s.repo.SetOpenAIBPSOAuthRefreshErrorIfUnchanged(s.ctx, observed, service.OpenAIBPSCredentialState{Status: "auth_failed", ErrorCode: "invalid_grant"})
	s.Require().NoError(err)
	s.Require().False(applied)
	got, err = s.repo.GetByID(s.ctx, observed.ID)
	s.Require().NoError(err)
	s.Require().Equal(service.StatusActive, got.Status)
}
func TestBPSRefreshCASOutboxFailureRollsBackGrant(t *testing.T) {
	client := testEntClient(t)
	account := mustCreateAccount(t, client, &service.Account{Name: "bps-cas-outbox-rollback", Platform: service.PlatformOpenAIBPS, Type: service.AccountTypeOAuth, Status: service.StatusActive, Credentials: map[string]any{"access_token": "old-access", "refresh_token": "old-refresh"}})
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM scheduler_outbox WHERE account_id=$1", account.ID)
		_ = client.Account.DeleteOneID(account.ID).Exec(context.Background())
	})
	repo := newAccountRepositoryWithSQL(client, &failAtomicSchedulerOutboxSQLExecutor{sqlExecutor: integrationDB}, nil)
	observed, err := repo.GetByID(context.Background(), account.ID)
	require.NoError(t, err)
	applied, err := repo.UpdateOpenAIBPSOAuthCredentialsIfUnchanged(context.Background(), observed, map[string]any{"access_token": "uncommitted"}, false, false)
	require.Error(t, err)
	require.False(t, applied)
	got, err := repo.GetByID(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, "old-access", got.GetCredential("access_token"))
}
