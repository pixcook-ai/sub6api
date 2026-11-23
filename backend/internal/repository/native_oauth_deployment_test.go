package repository

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/MACOS-DO/sub4api/internal/pkg/openai"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestCoreOAuthRuntimeInstallRollsBackAllWritesWhenLaterCASFails(t *testing.T) {
	for _, failedStep := range []string{"codex", "deployment"} {
		t.Run(failedStep, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			repo := &accountRepository{sql: db}
			bps, codex, d := coreRuntimeInstallFixture()
			deployment := &service.ManagedDeploymentUpdate{ManagedDeployment: d, ExpectedRouteRevision: 0}

			mock.ExpectBegin()
			expectRuntimeAccountInstall(mock, bps, true, 1)
			if failedStep == "codex" {
				expectRuntimeAccountInstall(mock, codex, true, 0)
			} else {
				expectRuntimeAccountInstall(mock, codex, true, 1)
				d := deployment.ManagedDeployment
				bpsJSON, _ := json.Marshal(d.BPS)
				codexJSON, _ := json.Marshal(d.Codex)
				mock.ExpectQuery(regexp.QuoteMeta(putManagedDeploymentSQL)).WithArgs(d.DeploymentID, d.GroupID, d.RouteRevision, d.SelectedProvider, string(bpsJSON), string(codexJSON), d.BPS.AccountID, d.Codex.AccountID, deployment.ExpectedRouteRevision, service.ManagedOAuthContract).WillReturnRows(sqlmock.NewRows([]string{"id", "group", "revision", "selected", "bps", "codex"}))
			}
			mock.ExpectRollback()

			_, err = repo.InstallCoreOAuthRuntime(context.Background(), bps, codex, deployment)
			require.Error(t, err)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestCoreOAuthRuntimeInstallRejectsInactiveAccountBeforeCAS(t *testing.T) {
	for _, status := range []string{"", service.StatusDisabled, service.StatusError} {
		t.Run(status, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			repo := &accountRepository{sql: db}
			bps, codex, deployment := coreRuntimeInstallFixture()
			bps.Expected.Status = status
			mock.ExpectBegin()
			mock.ExpectRollback()
			_, err = repo.InstallCoreOAuthRuntime(context.Background(), bps, codex, &service.ManagedDeploymentUpdate{ManagedDeployment: deployment, ExpectedRouteRevision: 0})
			require.ErrorIs(t, err, service.ErrCoreOAuthConflict)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func coreRuntimeInstallFixture() (service.CoreOAuthRuntimeAccountUpdate, service.CoreOAuthRuntimeAccountUpdate, service.ManagedDeployment) {
	bpsBinding := service.CoreOAuthAccountBinding{Contract: service.CoreOAuthContract, ProfileRef: "22222222-2222-4222-8222-222222222222", Provider: service.CoreOAuthProviderExcel, Subject: "subject", WorkspaceID: "workspace", IdentityEpoch: "33333333-3333-4333-8333-333333333333", TokenRevision: 1, ExpiresAt: "2099-01-01T00:00:00Z"}
	codexBinding := bpsBinding
	codexBinding.ProfileRef = "44444444-4444-4444-8444-444444444444"
	codexBinding.Provider = service.CoreOAuthProviderCodex
	d := repositoryDeploymentFixture()
	d.DeploymentID = "deployment_" + uuid.NewString()
	d.GroupID = 1
	d.BPS.AccountID, d.Codex.AccountID = 11, 12
	d.BPS.ProfileRef = "oauth_" + bpsBinding.ProfileRef
	d.Codex.ProfileRef = "oauth_" + codexBinding.ProfileRef
	d.BPS.Subject, d.Codex.Subject = "subject", "subject"
	d.BPS.WorkspaceID, d.Codex.WorkspaceID = "workspace", "workspace"
	d.BPS.IdentityEpoch, d.Codex.IdentityEpoch = bpsBinding.IdentityEpoch, codexBinding.IdentityEpoch
	bpsExtra := map[string]any{service.CoreOAuthAccountExtraKey: bpsBinding, service.ManagedDeploymentAccountExtraKey: managedBindingExtra(d.BPS)}
	codexExtra := map[string]any{service.CoreOAuthAccountExtraKey: codexBinding, service.ManagedDeploymentAccountExtraKey: managedBindingExtra(d.Codex)}
	bpsShellExtra := map[string]any{service.CoreOAuthRuntimeShellExtraKey: map[string]any{"provider": service.CoreOAuthProviderExcel, "group_id": int64(1)}}
	codexShellExtra := map[string]any{service.CoreOAuthRuntimeShellExtraKey: map[string]any{"provider": service.CoreOAuthProviderCodex, "group_id": int64(1)}}
	bpsExpected := &service.Account{ID: 11, Status: service.StatusActive, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Schedulable: false, GroupIDs: []int64{1}, Extra: bpsShellExtra, Credentials: map[string]any{"api_key": "inference"}}
	codexExpected := &service.Account{ID: 12, Status: service.StatusActive, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Schedulable: false, GroupIDs: []int64{1}, Extra: codexShellExtra, Credentials: map[string]any{"access_token": ""}}
	bps := service.CoreOAuthRuntimeAccountUpdate{Expected: bpsExpected, Credentials: map[string]any{"api_key": "inference"}, Extra: bpsExtra}
	codex := service.CoreOAuthRuntimeAccountUpdate{Expected: codexExpected, Credentials: map[string]any{"access_token": "access", "expires_at": "2099-01-01T00:00:00Z", "chatgpt_account_id": "workspace", "client_id": openai.ClientID}, Extra: codexExtra}
	return bps, codex, d
}

func managedBindingExtra(binding service.ManagedDeploymentBinding) map[string]any {
	encoded, _ := json.Marshal(binding)
	var values map[string]any
	_ = json.Unmarshal(encoded, &values)
	delete(values, "account_id")
	return values
}

func expectRuntimeAccountInstall(mock sqlmock.Sqlmock, update service.CoreOAuthRuntimeAccountUpdate, activate bool, rows int64) {
	marshal := func(value map[string]any) string {
		encoded, _ := json.Marshal(value)
		return string(encoded)
	}
	mock.ExpectExec(regexp.QuoteMeta(installCoreOAuthRuntimeAccountSQL)).WithArgs(marshal(update.Credentials), marshal(update.Extra), update.Expected.ID, update.Expected.Type, marshal(update.Expected.Credentials), marshal(update.Expected.Extra), service.SchedulerOutboxEventAccountChanged, activate, update.Expected.Schedulable).WillReturnResult(sqlmock.NewResult(0, rows))
}
