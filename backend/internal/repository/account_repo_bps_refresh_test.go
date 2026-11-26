package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountRepositoryBPSRefreshUsesCompleteCASAndAtomicOutbox(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)
	id := int64(7)
	expected := &service.Account{ID: 42, Platform: service.PlatformOpenAIBPS, Type: service.AccountTypeOAuth, Status: service.StatusError, ErrorMessage: "credential error", ProxyID: &id, Credentials: map[string]any{"access_token": "old", "refresh_token": "old-rt", "chatgpt_account_id": "workspace"}}
	applied, err := repo.UpdateOpenAIBPSOAuthCredentialsIfUnchanged(context.Background(), expected, map[string]any{"access_token": "new"}, true, true)
	require.NoError(t, err)
	require.False(t, applied)
	require.Len(t, exec.execQueries, 1)
	query := normalizeSQLWhitespace(exec.execQueries[0])
	for _, predicate := range []string{"a.platform=$7 AND a.type=$8", "a.credentials=$9::jsonb", "a.proxy_id IS NOT DISTINCT FROM $10", "a.status=$11", "COALESCE(a.error_message,'')=$12", "COALESCE(a.extra->($3::text),'null'::jsonb)=$13::jsonb", "WITH updated AS (", "INSERT INTO scheduler_outbox"} {
		require.Contains(t, query, predicate)
	}
	require.NotContains(t, query, "schedulable=")
	require.NotContains(t, query, "temp_unschedulable")
	require.Equal(t, `{"access_token":"old","chatgpt_account_id":"workspace","refresh_token":"old-rt"}`, exec.execArgs[0][8])
	require.Equal(t, &id, exec.execArgs[0][9])
	require.Equal(t, service.StatusError, exec.execArgs[0][10])
}
func TestAccountRepositoryBPSRefreshFailureScopesEntireGrant(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)
	expected := &service.Account{ID: 42, Platform: service.PlatformOpenAIBPS, Type: service.AccountTypeOAuth, Status: service.StatusActive, Credentials: map[string]any{"access_token": "old", "refresh_token": "rt"}}
	applied, err := repo.SetOpenAIBPSOAuthRefreshErrorIfUnchanged(context.Background(), expected, service.OpenAIBPSCredentialState{Status: "auth_failed", ErrorCode: "invalid_grant"})
	require.NoError(t, err)
	require.False(t, applied)
	query := normalizeSQLWhitespace(exec.execQueries[0])
	require.Contains(t, query, "a.credentials=$9::jsonb AND a.proxy_id IS NOT DISTINCT FROM $10")
	require.Contains(t, query, "a.platform=$6 AND a.type=$7 AND a.status=$8")
	require.Contains(t, query, "INSERT INTO scheduler_outbox")
	require.NotContains(t, query, "schedulable=")
}
func TestAccountRepositoryBPSRefreshSQLFailureReturnsError(t *testing.T) {
	exec := &recordingSQLExecutor{err: errors.New("outbox unavailable")}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)
	expected := &service.Account{ID: 42, Platform: service.PlatformOpenAIBPS, Type: service.AccountTypeOAuth, Status: service.StatusActive}
	_, err := repo.UpdateOpenAIBPSOAuthCredentialsIfUnchanged(context.Background(), expected, map[string]any{"access_token": "new"}, false, false)
	require.Error(t, err)
}
