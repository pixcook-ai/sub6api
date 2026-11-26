//go:build unit

package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

type bpsRefreshAPIRepo struct {
	refreshAPIAccountRepo
	beforeCAS        func()
	casCalls         int
	failureCalls     int
	beforeFailureCAS func()
}

func (r *bpsRefreshAPIRepo) UpdateOpenAIBPSOAuthCredentialsIfUnchanged(_ context.Context, expected *Account, credentials map[string]any, clearDiagnosis, recoverError bool) (bool, error) {
	r.casCalls++
	if r.beforeCAS != nil {
		r.beforeCAS()
	}
	a := r.account
	if a.Platform != expected.Platform || a.Type != expected.Type || a.Status != expected.Status || a.ErrorMessage != expected.ErrorMessage || !reflect.DeepEqual(a.ProxyID, expected.ProxyID) || !reflect.DeepEqual(a.Credentials, expected.Credentials) || !reflect.DeepEqual(a.Extra[OpenAIBPSCredentialStateExtraKey], expected.Extra[OpenAIBPSCredentialStateExtraKey]) {
		return false, nil
	}
	a.Credentials = shallowCopyMap(credentials)
	if clearDiagnosis {
		a.Extra = shallowCopyMap(a.Extra)
		delete(a.Extra, OpenAIBPSCredentialStateExtraKey)
	}
	if recoverError {
		a.Status = StatusActive
		a.ErrorMessage = ""
	}
	return true, nil
}
func bpsAPIAccount() *Account {
	a := bpsRefreshAccount(openai.ExcelClientID, "old-rt", 24*time.Hour)
	a.Status = StatusActive
	a.Schedulable = false
	return a
}
func TestRefreshOpenAIBPSAccount_ForceAndPreservePause(t *testing.T) {
	a := bpsAPIAccount()
	repo := &bpsRefreshAPIRepo{refreshAPIAccountRepo: refreshAPIAccountRepo{account: a}}
	client := &openaiBPSRefreshClientStub{response: &openai.TokenResponse{AccessToken: "renewed", ExpiresIn: 3600}}
	api := NewOAuthRefreshAPI(repo, &refreshAPICacheStub{lockResult: true})
	got, err := api.RefreshOpenAIBPSAccount(context.Background(), a, NewOpenAIOAuthService(nil, client))
	require.NoError(t, err)
	require.Equal(t, "renewed", got.GetCredential("access_token"))
	require.Equal(t, "old-rt", got.GetCredential("refresh_token"))
	require.Equal(t, "workspace-selected", got.GetCredential("chatgpt_account_id"))
	require.False(t, got.Schedulable)
	require.Equal(t, 1, repo.casCalls)
	require.Zero(t, repo.updateCredentialsCalls)
}
func TestRefreshOpenAIBPSAccount_RecoversOnlyCredentialError(t *testing.T) {
	for _, status := range []string{StatusError, StatusDisabled} {
		t.Run(status, func(t *testing.T) {
			a := bpsAPIAccount()
			a.Status = status
			a.ErrorMessage = bpsCredentialAuthFailedMessage
			a.Extra = map[string]any{OpenAIBPSCredentialStateExtraKey: map[string]any{"status": "auth_failed", "managed_status_error": true, "credential_identity": OpenAIBPSCredentialIdentity(OpenAIBPSCredentialSnapshotFromAccount(a))}}
			repo := &bpsRefreshAPIRepo{refreshAPIAccountRepo: refreshAPIAccountRepo{account: a}}
			client := &openaiBPSRefreshClientStub{response: &openai.TokenResponse{AccessToken: "renewed", ExpiresIn: 3600}}
			got, err := NewOAuthRefreshAPI(repo, &refreshAPICacheStub{lockResult: true}).RefreshOpenAIBPSAccount(context.Background(), a, NewOpenAIOAuthService(nil, client))
			if status == StatusDisabled {
				require.Error(t, err)
				require.Nil(t, got)
				require.Zero(t, client.calls)
				return
			}
			require.NoError(t, err)
			require.Equal(t, StatusActive, got.Status)
			require.Empty(t, got.ErrorMessage)
			require.False(t, got.Schedulable)
			require.NotContains(t, got.Extra, OpenAIBPSCredentialStateExtraKey)
		})
	}
}
func TestRefreshIfNeeded_BPSLockFailureStopsUpstream(t *testing.T) {
	for _, cache := range []*refreshAPICacheStub{nil, {lockErr: errors.New("redis down")}, {lockResult: false}} {
		a := bpsAPIAccount()
		a.Credentials["expires_at"] = time.Now().Add(-time.Hour).Format(time.RFC3339)
		repo := &bpsRefreshAPIRepo{refreshAPIAccountRepo: refreshAPIAccountRepo{account: a}}
		client := &openaiBPSRefreshClientStub{response: &openai.TokenResponse{AccessToken: "renewed", ExpiresIn: 3600}}
		var tokenCache GeminiTokenCache
		if cache != nil {
			tokenCache = cache
		}
		_, err := NewOAuthRefreshAPI(repo, tokenCache).RefreshOpenAIBPSAccount(context.Background(), a, NewOpenAIOAuthService(nil, client))
		require.Error(t, err)
		require.Zero(t, client.calls)
		require.Zero(t, repo.casCalls)
	}
}
func TestRefreshIfNeeded_BPSConcurrentEditWins(t *testing.T) {
	for _, field := range []string{"credentials", "proxy", "status", "diagnosis"} {
		t.Run(field, func(t *testing.T) {
			a := bpsAPIAccount()
			repo := &bpsRefreshAPIRepo{refreshAPIAccountRepo: refreshAPIAccountRepo{account: a}}
			repo.beforeCAS = func() {
				switch field {
				case "credentials":
					a.Credentials = map[string]any{"access_token": "admin-edited"}
				case "proxy":
					id := int64(99)
					a.ProxyID = &id
				case "status":
					a.Status = StatusDisabled
				case "diagnosis":
					a.Extra = map[string]any{OpenAIBPSCredentialStateExtraKey: map[string]any{"credential_identity": "newer-diagnosis"}}
				}
			}
			client := &openaiBPSRefreshClientStub{response: &openai.TokenResponse{AccessToken: "stale-upstream", RefreshToken: "rotated", ExpiresIn: 3600}}
			_, err := NewOAuthRefreshAPI(repo, &refreshAPICacheStub{lockResult: true}).RefreshOpenAIBPSAccount(context.Background(), a, NewOpenAIOAuthService(nil, client))
			require.Error(t, err)
			require.NotEqual(t, "stale-upstream", a.GetCredential("access_token"))
			require.Zero(t, repo.updateCredentialsCalls)
		})
	}
}
func TestRefreshOpenAIBPSAccount_RejectsNoRefreshAuthority(t *testing.T) {
	for _, clientID := range []string{openai.ExcelClientID, openai.ClientID} {
		a := bpsAPIAccount()
		a.Credentials["client_id"] = clientID
		if clientID == openai.ExcelClientID {
			delete(a.Credentials, "refresh_token")
		}
		repo := &bpsRefreshAPIRepo{refreshAPIAccountRepo: refreshAPIAccountRepo{account: a}}
		client := &openaiBPSRefreshClientStub{}
		_, err := NewOAuthRefreshAPI(repo, &refreshAPICacheStub{lockResult: true}).RefreshOpenAIBPSAccount(context.Background(), a, NewOpenAIOAuthService(nil, client))
		require.Error(t, err)
		require.Zero(t, client.calls)
	}
}

func (r *bpsRefreshAPIRepo) SetOpenAIBPSOAuthRefreshErrorIfUnchanged(_ context.Context, expected *Account, state OpenAIBPSCredentialState) (bool, error) {
	r.failureCalls++
	if r.beforeFailureCAS != nil {
		r.beforeFailureCAS()
	}
	a := r.account
	if a.Status != expected.Status || a.Platform != expected.Platform || a.Type != expected.Type || !reflect.DeepEqual(a.Credentials, expected.Credentials) || !reflect.DeepEqual(a.ProxyID, expected.ProxyID) {
		return false, nil
	}
	a.Status = StatusError
	a.ErrorMessage = OpenAIBPSCredentialErrorMessage(state)
	return true, nil
}
func TestBPSBackgroundRefresh_DoesNotClearPause(t *testing.T) {
	a := bpsAPIAccount()
	a.Credentials["expires_at"] = time.Now().Add(-time.Hour).Format(time.RFC3339)
	until := time.Now().Add(time.Hour)
	a.TempUnschedulableUntil = &until
	repo := &bpsRefreshAPIRepo{refreshAPIAccountRepo: refreshAPIAccountRepo{account: a}}
	client := &openaiBPSRefreshClientStub{response: &openai.TokenResponse{AccessToken: "renewed", ExpiresIn: 3600}}
	oauth := NewOpenAIOAuthService(nil, client)
	svc := NewTokenRefreshService(repo, nil, oauth, nil, nil, nil, nil, nil, nil)
	svc.SetRefreshAPI(NewOAuthRefreshAPI(repo, &refreshAPICacheStub{lockResult: true}))
	executor := NewOpenAIBPSTokenRefresher(oauth)
	err := svc.refreshWithRetry(context.Background(), a, executor, executor, time.Hour)
	require.NoError(t, err)
	require.False(t, a.Schedulable)
	require.Equal(t, &until, a.TempUnschedulableUntil)
	require.Equal(t, 1, repo.casCalls)
}
func TestBPSBackgroundRefresh_StaleFailureCannotDisableEditedGrant(t *testing.T) {
	a := bpsAPIAccount()
	a.Credentials["expires_at"] = time.Now().Add(-time.Hour).Format(time.RFC3339)
	repo := &bpsRefreshAPIRepo{refreshAPIAccountRepo: refreshAPIAccountRepo{account: a}}
	repo.beforeFailureCAS = func() { a.Credentials = map[string]any{"access_token": "admin-replaced", "refresh_token": "admin-rt"} }
	client := &openaiBPSRefreshClientStub{err: errors.New("invalid_grant: expired refresh token")}
	oauth := NewOpenAIOAuthService(nil, client)
	svc := NewTokenRefreshService(repo, nil, oauth, nil, nil, nil, nil, nil, nil)
	svc.SetRefreshAPI(NewOAuthRefreshAPI(repo, &refreshAPICacheStub{lockResult: true}))
	executor := NewOpenAIBPSTokenRefresher(oauth)
	err := svc.refreshWithRetry(context.Background(), a, executor, executor, time.Hour)
	require.ErrorIs(t, err, errRefreshSkipped)
	require.Equal(t, StatusActive, a.Status)
	require.Equal(t, "admin-replaced", a.GetCredential("access_token"))
	require.Zero(t, repo.updateCalls)
	require.Equal(t, 1, repo.failureCalls)
}
func TestRefreshOpenAIBPSAccount_MissingCASStopsBeforeUpstream(t *testing.T) {
	a := bpsAPIAccount()
	repo := &refreshAPIAccountRepo{account: a}
	client := &openaiBPSRefreshClientStub{}
	_, err := NewOAuthRefreshAPI(repo, &refreshAPICacheStub{lockResult: true}).RefreshOpenAIBPSAccount(context.Background(), a, NewOpenAIOAuthService(nil, client))
	require.Error(t, err)
	require.Zero(t, client.calls)
}

type bpsReauthHookRecorder struct{ calls int }

func (h *bpsReauthHookRecorder) OnRefreshPermanentFailure(*Account) { h.calls++ }
func TestBPSBackgroundRefresh_PermanentFailureDoesNotStartOrdinaryReauth(t *testing.T) {
	a := bpsAPIAccount()
	a.Credentials["expires_at"] = time.Now().Add(-time.Hour).Format(time.RFC3339)
	repo := &bpsRefreshAPIRepo{refreshAPIAccountRepo: refreshAPIAccountRepo{account: a}}
	client := &openaiBPSRefreshClientStub{err: errors.New("invalid_grant: revoked")}
	oauth := NewOpenAIOAuthService(nil, client)
	svc := NewTokenRefreshService(repo, nil, oauth, nil, nil, nil, nil, nil, nil)
	svc.SetRefreshAPI(NewOAuthRefreshAPI(repo, &refreshAPICacheStub{lockResult: true}))
	hook := &bpsReauthHookRecorder{}
	svc.SetOpenAIReauthHook(hook)
	executor := NewOpenAIBPSTokenRefresher(oauth)
	err := svc.refreshWithRetry(context.Background(), a, executor, executor, time.Hour)
	var permanent *accountPermanentRefreshError
	require.ErrorAs(t, err, &permanent)
	require.Equal(t, 1, repo.failureCalls)
	require.Equal(t, StatusError, a.Status)
	require.False(t, a.Schedulable)
	require.Zero(t, hook.calls)
}

type bpsPoolRefreshRepo struct {
	AccountRepository
	accounts map[int64]*Account
	casIDs   []int64
}

func (r *bpsPoolRefreshRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	return r.accounts[id], nil
}
func (r *bpsPoolRefreshRepo) UpdateOpenAIBPSOAuthCredentialsIfUnchanged(_ context.Context, expected *Account, credentials map[string]any, _, _ bool) (bool, error) {
	r.casIDs = append(r.casIDs, expected.ID)
	r.accounts[expected.ID].Credentials = shallowCopyMap(credentials)
	return true, nil
}
func TestBPSBackgroundRefresh_BadProxyDoesNotTripHealthyAccountPool(t *testing.T) {
	bad := bpsAPIAccount()
	bad.ID = 11
	id := int64(99)
	bad.ProxyID = &id
	bad.Credentials["refresh_token"] = "bad-account-rt"
	bad.Credentials["expires_at"] = time.Now().Add(-time.Hour).Format(time.RFC3339)
	good := bpsAPIAccount()
	good.ID = 12
	good.Credentials["refresh_token"] = "healthy-account-rt"
	good.Credentials["expires_at"] = time.Now().Add(-time.Hour).Format(time.RFC3339)
	repo := &bpsPoolRefreshRepo{accounts: map[int64]*Account{bad.ID: bad, good.ID: good}}
	client := &openaiBPSRefreshClientStub{response: &openai.TokenResponse{AccessToken: "renewed-healthy", ExpiresIn: 3600}}
	oauth := NewOpenAIOAuthService(&bpsRefreshProxyRepo{err: errors.New("missing configured proxy")}, client)
	svc := NewTokenRefreshService(repo, nil, oauth, nil, nil, nil, nil, &config.Config{TokenRefresh: config.TokenRefreshConfig{ProviderConcurrency: 1, ProviderFailureThreshold: 1}}, nil)
	svc.SetRefreshAPI(NewOAuthRefreshAPI(repo, &refreshAPICacheStub{lockResult: true}))
	executor := NewOpenAIBPSTokenRefresher(oauth)
	pool := &tokenRefreshProviderState{service: svc, registration: tokenRefreshRegistration{platform: PlatformOpenAIBPS, refresher: executor, executor: executor}, poolGate: newTokenRefreshConcurrencyGate(1)}
	refreshed, skipped, failed := svc.processProviderAccounts(context.Background(), pool, []*Account{bad, good}, time.Hour)
	require.Equal(t, 1, refreshed)
	require.Zero(t, skipped)
	require.Equal(t, 1, failed)
	require.False(t, pool.isTripped())
	require.Equal(t, []int64{good.ID}, repo.casIDs)
	require.Equal(t, int32(1), client.calls)
	require.Equal(t, "healthy-account-rt", client.refreshToken, "the bad proxy grant must never reach direct egress")
	require.Equal(t, "old-access", bad.GetCredential("access_token"))
	require.Equal(t, "renewed-healthy", good.GetCredential("access_token"))
	require.Equal(t, StatusActive, bad.Status)
}
