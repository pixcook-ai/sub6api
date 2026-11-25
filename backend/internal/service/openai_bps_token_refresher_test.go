package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

type openaiBPSRefreshClientStub struct {
	calls    int32
	clientID string
	response *openai.TokenResponse
}

func (s *openaiBPSRefreshClientStub) ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI, proxyURL, clientID string) (*openai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func (s *openaiBPSRefreshClientStub) RefreshToken(ctx context.Context, refreshToken, proxyURL string) (*openai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func (s *openaiBPSRefreshClientStub) RefreshTokenWithClientID(ctx context.Context, refreshToken, proxyURL string, clientID string) (*openai.TokenResponse, error) {
	atomic.AddInt32(&s.calls, 1)
	s.clientID = clientID
	return s.response, nil
}

func bpsRefreshAccount(clientID, refreshToken string, expiresIn time.Duration) *Account {
	return &Account{
		ID:       2,
		Platform: PlatformOpenAIBPS,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":       "old-access",
			"refresh_token":      refreshToken,
			"client_id":          clientID,
			"chatgpt_account_id": "workspace-selected",
			"expires_at":         time.Now().Add(expiresIn).UTC().Format(time.RFC3339),
		},
	}
}

func TestOpenAIBPSTokenRefresher_CanRefreshOnlyExcelGrants(t *testing.T) {
	refresher := NewOpenAIBPSTokenRefresher(NewOpenAIOAuthService(nil, &openaiBPSRefreshClientStub{}))

	require.True(t, refresher.CanRefresh(bpsRefreshAccount(openai.ExcelClientID, "rt", time.Hour)))
	require.False(t, refresher.CanRefresh(bpsRefreshAccount(openai.ClientID, "rt", time.Hour)), "codex grants must not be refreshed here")
	require.False(t, refresher.CanRefresh(bpsRefreshAccount(openai.ExcelClientID, "", time.Hour)), "no refresh token")

	openaiAccount := bpsRefreshAccount(openai.ExcelClientID, "rt", time.Hour)
	openaiAccount.Platform = PlatformOpenAI
	require.False(t, refresher.CanRefresh(openaiAccount))
	require.False(t, refresher.CanRefresh(nil))
}

func TestOpenAIBPSTokenRefresher_NeedsRefreshOnlyNearExpiry(t *testing.T) {
	refresher := NewOpenAIBPSTokenRefresher(NewOpenAIOAuthService(nil, &openaiBPSRefreshClientStub{}))
	window := 30 * time.Minute

	require.False(t, refresher.NeedsRefresh(bpsRefreshAccount(openai.ExcelClientID, "rt", 9*24*time.Hour), window))
	require.True(t, refresher.NeedsRefresh(bpsRefreshAccount(openai.ExcelClientID, "rt", 10*time.Minute), window))
	require.True(t, refresher.NeedsRefresh(bpsRefreshAccount(openai.ExcelClientID, "rt", -time.Minute), window))

	noExpiry := bpsRefreshAccount(openai.ExcelClientID, "rt", time.Hour)
	delete(noExpiry.Credentials, "expires_at")
	require.False(t, refresher.NeedsRefresh(noExpiry, window))
}

func TestOpenAIBPSTokenRefresher_RefreshUsesExcelClientAndKeepsWorkspace(t *testing.T) {
	stub := &openaiBPSRefreshClientStub{response: &openai.TokenResponse{
		AccessToken:  "new-access",
		RefreshToken: "rotated-refresh",
		ExpiresIn:    3600,
	}}
	refresher := NewOpenAIBPSTokenRefresher(NewOpenAIOAuthService(nil, stub))
	account := bpsRefreshAccount(openai.ExcelClientID, "old-refresh", 10*time.Minute)

	creds, err := refresher.Refresh(context.Background(), account)
	require.NoError(t, err)

	require.Equal(t, int32(1), atomic.LoadInt32(&stub.calls))
	require.Equal(t, openai.ExcelClientID, stub.clientID, "refresh must use the Excel client")
	require.Equal(t, "new-access", creds["access_token"])
	require.Equal(t, "rotated-refresh", creds["refresh_token"], "rotated refresh token must be stored")
	require.Equal(t, "workspace-selected", creds["chatgpt_account_id"], "operator-selected workspace must survive refresh")
	require.Equal(t, openai.ExcelClientID, creds["client_id"])
	require.NotEqual(t, account.Credentials["expires_at"], creds["expires_at"], "expiry must advance")
}

func TestOpenAIBPSTokenRefresher_RefreshKeepsOldRefreshTokenWhenNotRotated(t *testing.T) {
	stub := &openaiBPSRefreshClientStub{response: &openai.TokenResponse{AccessToken: "new-access", ExpiresIn: 3600}}
	refresher := NewOpenAIBPSTokenRefresher(NewOpenAIOAuthService(nil, stub))

	creds, err := refresher.Refresh(context.Background(), bpsRefreshAccount(openai.ExcelClientID, "old-refresh", 10*time.Minute))
	require.NoError(t, err)
	require.Equal(t, "old-refresh", creds["refresh_token"], "an empty refresh response must not erase the stored token")
}
