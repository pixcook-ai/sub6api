package repository

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	infraerrors "github.com/MACOS-DO/sub4api/internal/pkg/errors"
	"github.com/MACOS-DO/sub4api/internal/pkg/openai"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/imroc/req/v3"
)

// NewOpenAIOAuthClient creates a new OpenAI OAuth client
func NewOpenAIOAuthClient() service.OpenAIOAuthClient {
	return &openaiOAuthService{tokenURL: openai.TokenURL}
}

type openaiOAuthService struct {
	tokenURL string
}

func (s *openaiOAuthService) ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI, proxyURL, clientID string) (*openai.TokenResponse, error) {
	client, err := createOpenAIReqClient(proxyURL)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_OAUTH_CLIENT_INIT_FAILED", "create HTTP client: %v", err)
	}

	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		clientID = openai.ClientID
	}
	if redirectURI == "" {
		redirectURI = openai.DefaultRedirectURI
		if clientID == openai.BPSExcelClientID {
			redirectURI = openai.BPSExcelCallback
		}
	}

	formData := url.Values{}
	formData.Set("grant_type", "authorization_code")
	formData.Set("client_id", clientID)
	formData.Set("code", code)
	formData.Set("redirect_uri", redirectURI)
	formData.Set("code_verifier", codeVerifier)

	var tokenResp openai.TokenResponse

	request, tokenURL, err := s.tokenRequest(ctx, client, clientID)
	if err != nil {
		return nil, err
	}
	resp, err := request.SetFormDataFromValues(formData).SetSuccessResult(&tokenResp).Post(tokenURL)

	if err != nil {
		if shouldReturnOpenAINoProxyHint(ctx, proxyURL, err) {
			return nil, newOpenAINoProxyHintError(err)
		}
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_OAUTH_REQUEST_FAILED", "request failed: %v", err)
	}

	if !resp.IsSuccessState() {
		reason := "OPENAI_OAUTH_TOKEN_EXCHANGE_FAILED"
		message := "token exchange failed: provider rejected request with status %d"
		var providerError struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(resp.Bytes(), &providerError) == nil && providerError.Error.Code == "unsupported_country_region_territory" {
			reason = "OPENAI_OAUTH_REGION_UNSUPPORTED"
			message = "token exchange failed: selected network region is unsupported (status %d)"
		}
		return nil, infraerrors.Newf(http.StatusBadGateway, reason, message, resp.StatusCode)
	}

	return &tokenResp, nil
}

func (s *openaiOAuthService) RefreshToken(ctx context.Context, refreshToken, proxyURL string) (*openai.TokenResponse, error) {
	return s.RefreshTokenWithClientID(ctx, refreshToken, proxyURL, "")
}

func (s *openaiOAuthService) RefreshTokenWithClientID(ctx context.Context, refreshToken, proxyURL string, clientID string) (*openai.TokenResponse, error) {
	// 调用方应始终传入正确的 client_id；为兼容旧数据，未指定时默认使用 OpenAI ClientID
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		clientID = openai.ClientID
	}
	return s.refreshTokenWithClientID(ctx, refreshToken, proxyURL, clientID)
}

func (s *openaiOAuthService) refreshTokenWithClientID(ctx context.Context, refreshToken, proxyURL, clientID string) (*openai.TokenResponse, error) {
	client, err := createOpenAIReqClient(proxyURL)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_OAUTH_CLIENT_INIT_FAILED", "create HTTP client: %v", err)
	}

	refreshRequest := struct {
		ClientID     string `json:"client_id"`
		GrantType    string `json:"grant_type"`
		RefreshToken string `json:"refresh_token"`
	}{ClientID: clientID, GrantType: "refresh_token", RefreshToken: refreshToken}

	var tokenResp openai.TokenResponse

	request, tokenURL, err := s.tokenRequest(ctx, client, clientID)
	if err != nil {
		return nil, err
	}
	if clientID == openai.BPSExcelClientID {
		request.SetFormDataFromValues(url.Values{
			"client_id":     {clientID},
			"grant_type":    {"refresh_token"},
			"refresh_token": {refreshToken},
		})
	} else {
		request.SetHeader("Content-Type", "application/json").SetBodyJsonMarshal(refreshRequest)
	}
	resp, err := request.SetSuccessResult(&tokenResp).Post(tokenURL)

	if err != nil {
		if shouldReturnOpenAINoProxyHint(ctx, proxyURL, err) {
			return nil, newOpenAINoProxyHintError(err)
		}
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_OAUTH_REQUEST_FAILED", "request failed: %v", err)
	}

	if !resp.IsSuccessState() {
		return nil, openai.NewRefreshTokenError(resp.StatusCode, resp.Bytes())
	}

	return &tokenResp, nil
}

// The Excel client has its own token endpoint contract; never reuse the
// Codex request identity for its authorization-code or refresh grants.
func (s *openaiOAuthService) tokenRequest(ctx context.Context, client *req.Client, clientID string) (*req.Request, string, error) {
	request := client.R().SetContext(ctx)
	if clientID != openai.BPSExcelClientID {
		authUA, authOriginator := service.CodexCanonicalAuthIdentity()
		return request.SetHeader("User-Agent", authUA).SetHeader("originator", authOriginator), s.tokenURL, nil
	}
	endpoint, err := url.Parse(s.tokenURL)
	if err != nil {
		return nil, "", infraerrors.New(http.StatusBadGateway, "OPENAI_OAUTH_CLIENT_INIT_FAILED", "Invalid OAuth token endpoint")
	}
	query := endpoint.Query()
	query.Set("unified", "true")
	endpoint.RawQuery = query.Encode()
	request.SetHeader("User-Agent", openai.BPSOAuthUserAgent).
		SetHeader("Accept-Language", openai.BPSOAuthAcceptLanguage).
		SetHeader("Accept", "application/json")
	return request, endpoint.String(), nil
}

func createOpenAIReqClient(proxyURL string) (*req.Client, error) {
	return getSharedReqClient(reqClientOptions{
		ProxyURL: proxyURL,
		Timeout:  120 * time.Second,
	})
}

func shouldReturnOpenAINoProxyHint(ctx context.Context, proxyURL string, err error) bool {
	if strings.TrimSpace(proxyURL) != "" || err == nil {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	return !errors.Is(err, context.Canceled)
}

func newOpenAINoProxyHintError(cause error) error {
	return infraerrors.New(
		http.StatusBadGateway,
		"OPENAI_OAUTH_PROXY_REQUIRED",
		"OpenAI OAuth request failed: no proxy is configured and this server could not reach OpenAI directly. Select a proxy that can access OpenAI, then retry; if the authorization code has expired, regenerate the authorization URL.",
	).WithCause(cause)
}
