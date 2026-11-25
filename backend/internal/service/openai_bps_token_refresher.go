package service

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/openai"
)

// OpenAIBPSTokenRefresher renews only grants issued to the official Excel client.
// Pasted access tokens carry no refresh authority and are never handled here.
type OpenAIBPSTokenRefresher struct {
	oauth *OpenAIOAuthService
}

func NewOpenAIBPSTokenRefresher(oauth *OpenAIOAuthService) *OpenAIBPSTokenRefresher {
	return &OpenAIBPSTokenRefresher{oauth: oauth}
}

func (r *OpenAIBPSTokenRefresher) CacheKey(account *Account) string {
	return "openai_bps:account:" + strconv.FormatInt(account.ID, 10)
}

func (r *OpenAIBPSTokenRefresher) CanRefresh(account *Account) bool {
	return account != nil && account.IsOpenAIBPS() && account.Type == AccountTypeOAuth &&
		strings.TrimSpace(account.GetCredential("client_id")) == openai.ExcelClientID &&
		strings.TrimSpace(account.GetCredential("refresh_token")) != ""
}

func (r *OpenAIBPSTokenRefresher) NeedsRefresh(account *Account, refreshWindow time.Duration) bool {
	expiresAt := account.GetCredentialAsTime("expires_at")
	if expiresAt == nil {
		return false
	}
	return time.Until(*expiresAt) < refreshWindow
}

func (r *OpenAIBPSTokenRefresher) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	var proxyURL string
	if account.ProxyID != nil && r.oauth.proxyRepo != nil {
		if proxy, err := r.oauth.proxyRepo.GetByID(ctx, *account.ProxyID); err == nil && proxy != nil {
			proxyURL = proxy.URL()
		}
	}
	tokenInfo, err := r.oauth.RefreshTokenWithClientID(ctx, strings.TrimSpace(account.GetCredential("refresh_token")), proxyURL, openai.ExcelClientID)
	if err != nil {
		return nil, err
	}
	creds := r.oauth.BuildAccountCredentials(tokenInfo)
	// The operator-selected workspace must survive a refresh.
	delete(creds, "chatgpt_account_id")
	return MergeCredentials(account.Credentials, creds), nil
}
