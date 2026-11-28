package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	infraerrors "github.com/MACOS-DO/sub4api/internal/pkg/errors"
)

type openAIBPSManualRefreshKey struct{}

// An account-local failure before sending a token request is not provider health evidence.
type openAIBPSAccountRefreshError struct{ err error }

func (e *openAIBPSAccountRefreshError) Error() string { return e.err.Error() }
func (e *openAIBPSAccountRefreshError) Unwrap() error { return e.err }
func bpsRefreshProxyUnavailable(message string) error {
	return &openAIBPSAccountRefreshError{err: infraerrors.New(503, "BPS_REFRESH_PROXY_UNAVAILABLE", message)}
}

// OpenAIBPSOAuthRefreshRepository prevents a delayed provider response from replacing an administrator edit.
type OpenAIBPSOAuthRefreshRepository interface {
	UpdateOpenAIBPSOAuthCredentialsIfUnchanged(context.Context, *Account, map[string]any, bool, bool) (bool, error)
}

func infraBPSRefreshInactiveError() error {
	return infraerrors.New(409, "BPS_REFRESH_ACCOUNT_INACTIVE", "Only active BPS accounts or recognized credential errors can be refreshed")
}

func isOpenAIBPSManualRefresh(ctx context.Context) bool {
	manual, _ := ctx.Value(openAIBPSManualRefreshKey{}).(bool)
	return manual
}

func bpsRefreshDiagnosisCleanup(account *Account) (clearDiagnosis, recoverError bool) {
	state, hasState := account.Extra[OpenAIBPSCredentialStateExtraKey].(map[string]any)
	clearDiagnosis = hasState && stringValue(state["credential_identity"]) == OpenAIBPSCredentialIdentity(OpenAIBPSCredentialSnapshotFromAccount(account))
	recoverError = isManagedBPSCredentialError(account) && (!hasState || clearDiagnosis)
	return
}

// RefreshOpenAIBPSAccount forces one manual refresh through the shared lock and DB reread boundary.
func (api *OAuthRefreshAPI) RefreshOpenAIBPSAccount(ctx context.Context, account *Account, oauth *OpenAIOAuthService) (*Account, error) {
	executor := NewOpenAIBPSTokenRefresher(oauth)
	if !executor.CanRefresh(account) {
		return nil, infraerrors.BadRequest("BPS_MANUAL_TOKEN_ONLY", "BPS token refresh requires an official Excel grant with refresh_token; replace access_token for manually imported accounts")
	}
	if api == nil {
		return nil, infraerrors.New(503, "BPS_REFRESH_UNAVAILABLE", "BPS token refresh is not configured")
	}
	ctx = context.WithValue(ctx, openAIBPSManualRefreshKey{}, true)
	ctx, cancel := context.WithTimeout(ctx, clampRefreshAttemptToLockLease(45*time.Second, api.lockTTL))
	defer cancel()
	result, err := api.RefreshIfNeeded(ctx, account, executor, 0)
	if err != nil {
		return nil, err
	}
	if result == nil || result.LockHeld {
		return nil, infraerrors.New(409, "BPS_REFRESH_BUSY", "BPS token refresh is already running; retry shortly")
	}
	if !result.Refreshed {
		return nil, infraerrors.New(409, "BPS_REFRESH_STATE_CHANGED", "BPS account changed during refresh; reload the account and retry")
	}
	return result.Account, nil
}

func (api *OAuthRefreshAPI) persistOpenAIBPSRefresh(ctx context.Context, cacheKey string, attempted *Account, credentials map[string]any) (*OAuthRefreshResult, error) {
	repo, ok := api.accountRepo.(OpenAIBPSOAuthRefreshRepository)
	if !ok {
		return nil, &providerConfigurationRefreshError{err: errors.New("BPS OAuth refresh CAS repository is not configured")}
	}
	clearDiagnosis, recoverError := bpsRefreshDiagnosisCleanup(attempted)
	applied, err := repo.UpdateOpenAIBPSOAuthCredentialsIfUnchanged(ctx, attempted, credentials, clearDiagnosis, recoverError)
	if err != nil {
		return nil, &providerCycleContainmentRefreshError{err: fmt.Errorf("BPS refresh succeeded but credential persistence failed: %w", err)}
	}
	if !applied {
		current, readErr := api.accountRepo.GetByID(ctx, attempted.ID)
		if readErr != nil || current == nil {
			return nil, &oauthRefreshStateUnavailableError{err: errors.New("BPS refresh CAS lost and current account state is unavailable")}
		}
		return &OAuthRefreshResult{Account: current}, nil
	}
	// Reuse the bounded cache invalidation and durable reread after a token rotation.
	durable, err := api.loadGrokDurableAccountAfterPersist(ctx, cacheKey, attempted.ID)
	if err != nil || durable == nil {
		return nil, &providerCycleContainmentRefreshError{err: errors.New("BPS refresh persisted but durable account state is unavailable")}
	}
	return &OAuthRefreshResult{Refreshed: true, NewCredentials: credentials, Account: durable}, nil
}

// OpenAIBPSOAuthRefreshFailureRepository scopes terminal errors to the attempted grant and proxy.
type OpenAIBPSOAuthRefreshFailureRepository interface {
	SetOpenAIBPSOAuthRefreshErrorIfUnchanged(context.Context, *Account, OpenAIBPSCredentialState) (bool, error)
}

func (s *TokenRefreshService) handleOpenAIBPSRefreshFailure(ctx context.Context, account *Account, refreshErr error) error {
	var localError *openAIBPSAccountRefreshError
	if errors.As(refreshErr, &localError) {
		return refreshErr
	}
	if !isNonRetryableRefreshError(refreshErr) {
		return &providerCycleContainmentRefreshError{err: refreshErr}
	}
	repo, ok := s.accountRepo.(OpenAIBPSOAuthRefreshFailureRepository)
	if !ok {
		return &providerConfigurationRefreshError{err: errors.New("BPS refresh failure CAS repository is not configured")}
	}
	applied, err := repo.SetOpenAIBPSOAuthRefreshErrorIfUnchanged(ctx, account, newOpenAIBPSCredentialFailure("invalid_grant", time.Now().UTC()))
	if err != nil {
		return &providerCycleContainmentRefreshError{err: err}
	}
	if !applied {
		return errRefreshSkipped
	}
	s.notifyAccountSchedulingBlocked(account, time.Time{}, "token_refresh_non_retryable")
	cacheFailed := s.cacheInvalidator == nil
	if s.cacheInvalidator != nil {
		cacheFailed = s.cacheInvalidator.InvalidateToken(ctx, account) != nil
	}
	// The ordinary reauth hook persists outside this BPS grant/proxy CAS boundary.
	return &accountPermanentRefreshError{err: refreshErr, persistentlyBlocked: true, cacheInvalidationFailed: cacheFailed}
}
