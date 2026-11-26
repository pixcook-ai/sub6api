package service

import (
	"context"
	"fmt"
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
	if !r.CanRefresh(account) {
		return nil, fmt.Errorf("BPS refresh requires an official Excel grant and refresh token")
	}
	if r.oauth == nil || r.oauth.oauthClient == nil {
		return nil, fmt.Errorf("BPS OAuth client is not configured")
	}
	var proxyURL string
	if account.ProxyID != nil {
		if r.oauth.proxyRepo == nil {
			return nil, bpsRefreshProxyUnavailable("BPS refresh proxy repository is not configured")
		}
		proxy, err := r.oauth.proxyRepo.GetByID(ctx, *account.ProxyID)
		if err != nil || proxy == nil {
			return nil, bpsRefreshProxyUnavailable("BPS refresh configured proxy is unavailable")
		}
		proxyURL = proxy.URL()
		if strings.TrimSpace(proxyURL) == "" {
			return nil, bpsRefreshProxyUnavailable("BPS refresh configured proxy URL is empty")
		}
	}
	// Excel grants do not authorize the ordinary Codex enrichment/privacy calls.
	tokenResp, err := r.oauth.oauthClient.RefreshTokenWithClientID(ctx, strings.TrimSpace(account.GetCredential("refresh_token")), proxyURL, openai.ExcelClientID)
	if err != nil {
		return nil, err
	}
	if tokenResp == nil || strings.TrimSpace(tokenResp.AccessToken) == "" {
		return nil, fmt.Errorf("BPS refresh returned no access token")
	}
	tokenInfo := &OpenAITokenInfo{AccessToken: tokenResp.AccessToken, RefreshToken: tokenResp.RefreshToken, ClientID: openai.ExcelClientID, ExpiresAt: openAIRefreshedTokenExpiresAt(tokenResp.AccessToken, tokenResp.ExpiresIn)}
	if tokenInfo.ExpiresAt <= time.Now().Unix() {
		return nil, fmt.Errorf("BPS refresh returned no future expiry")
	}
	creds := r.oauth.BuildAccountCredentials(tokenInfo)
	// The operator-selected workspace must survive a refresh.
	delete(creds, "chatgpt_account_id")
	return MergeCredentials(account.Credentials, creds), nil
}
