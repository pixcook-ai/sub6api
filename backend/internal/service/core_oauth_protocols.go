package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/openai"
)

type coreOAuthBPSProtocol struct{}

func NewCoreOAuthBPSProtocol() CoreOAuthProtocol { return &coreOAuthBPSProtocol{} }
func (p *coreOAuthBPSProtocol) Begin(context.Context, CoreOAuthTransport) (*CoreOAuthProtocolAttempt, error) {
	tx, err := openai.NewBPSOAuthTransaction()
	if err != nil {
		return nil, &CoreOAuthProtocolError{}
	}
	return &CoreOAuthProtocolAttempt{AuthorizationURL: tx.AuthorizationURL, RedirectURI: tx.RedirectURI, ExpiresAt: tx.ExpiresAt, State: tx}, nil
}
func (p *coreOAuthBPSProtocol) ExchangeCallback(ctx context.Context, attempt *CoreOAuthProtocolAttempt, callback string, expected CoreOAuthIdentity, transport CoreOAuthTransport) (*CoreOAuthGrant, error) {
	tx, ok := attempt.State.(*openai.BPSOAuthTransaction)
	if !ok {
		return nil, &CoreOAuthProtocolError{}
	}
	client, err := openai.NewBPSOAuthClient(transport.ProxyURL)
	if err != nil {
		return nil, &CoreOAuthProtocolError{}
	}
	token, err := client.ExchangeCallback(ctx, tx, callback, openai.BPSOAuthIdentity{Subject: expected.Subject, WorkspaceID: expected.WorkspaceID, Email: expected.Email})
	if err != nil {
		return nil, coreOAuthBPSError(err)
	}
	return coreOAuthBPSGrant(token), nil
}
func (p *coreOAuthBPSProtocol) Refresh(ctx context.Context, previous CoreOAuthGrant, transport CoreOAuthTransport) (*CoreOAuthGrant, error) {
	client, err := openai.NewBPSOAuthClient(transport.ProxyURL)
	if err != nil {
		return nil, &CoreOAuthProtocolError{}
	}
	token, err := client.Refresh(ctx, openai.BPSOAuthToken{TokenResponse: openai.TokenResponse{AccessToken: previous.AccessToken, RefreshToken: previous.RefreshToken, IDToken: previous.IDToken, TokenType: "Bearer"}, BPSOAuthIdentity: openai.BPSOAuthIdentity{Subject: previous.Subject, WorkspaceID: previous.WorkspaceID, Email: previous.Email}, ClientID: previous.ClientID, ExpiresAt: previous.ExpiresAt})
	if err != nil {
		return nil, coreOAuthBPSError(err)
	}
	return coreOAuthBPSGrant(token), nil
}
func coreOAuthBPSError(err error) error {
	var typed *openai.BPSOAuthError
	if errors.As(err, &typed) {
		return &CoreOAuthProtocolError{Permanent: typed.Permanent, OutcomeUnknown: typed.OutcomeUnknown}
	}
	return &CoreOAuthProtocolError{OutcomeUnknown: true}
}
func coreOAuthBPSGrant(t *openai.BPSOAuthToken) *CoreOAuthGrant {
	if t == nil {
		return nil
	}
	return &CoreOAuthGrant{AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, IDToken: t.IDToken, ClientID: t.ClientID, Subject: t.Subject, WorkspaceID: t.WorkspaceID, Email: t.Email, ExpiresAt: t.ExpiresAt}
}

type coreOAuthCodexProtocol struct{ oauth *OpenAIOAuthService }

func NewCoreOAuthCodexProtocol(oauth *OpenAIOAuthService) CoreOAuthProtocol {
	return &coreOAuthCodexProtocol{oauth: oauth}
}
func (p *coreOAuthCodexProtocol) Begin(ctx context.Context, transport CoreOAuthTransport) (*CoreOAuthProtocolAttempt, error) {
	if p.oauth == nil {
		return nil, ErrCoreOAuthUnavailable
	}
	result, err := p.oauth.GenerateAuthURL(ctx, transport.ProxyID, openai.DefaultRedirectURI, PlatformOpenAI)
	if err != nil {
		return nil, &CoreOAuthProtocolError{}
	}
	return &CoreOAuthProtocolAttempt{AuthorizationURL: result.AuthURL, RedirectURI: openai.DefaultRedirectURI, ExpiresAt: time.Now().Add(openai.SessionTTL), State: result.SessionID}, nil
}
func (p *coreOAuthCodexProtocol) ExchangeCallback(ctx context.Context, attempt *CoreOAuthProtocolAttempt, callback string, expected CoreOAuthIdentity, transport CoreOAuthTransport) (*CoreOAuthGrant, error) {
	session, ok := attempt.State.(string)
	if !ok || p.oauth == nil {
		return nil, &CoreOAuthProtocolError{}
	}
	u, err := url.Parse(callback)
	if err != nil || u.User != nil || u.Fragment != "" || u.Scheme+"://"+u.Host+u.EscapedPath() != openai.DefaultRedirectURI {
		return nil, &CoreOAuthProtocolError{}
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, &CoreOAuthProtocolError{}
	}
	for _, items := range values {
		if len(items) != 1 {
			return nil, &CoreOAuthProtocolError{}
		}
	}
	if values.Get("code") == "" || values.Get("state") == "" || values.Has("error") {
		return nil, &CoreOAuthProtocolError{}
	}
	token, err := p.oauth.ExchangeCode(ctx, &OpenAIExchangeCodeInput{SessionID: session, Code: values.Get("code"), State: values.Get("state"), RedirectURI: openai.DefaultRedirectURI, ProxyID: transport.ProxyID})
	if err != nil {
		return nil, &CoreOAuthProtocolError{OutcomeUnknown: true}
	}
	return coreOAuthNativeGrant(token, CoreOAuthGrant{}), nil
}
func (p *coreOAuthCodexProtocol) Refresh(ctx context.Context, previous CoreOAuthGrant, transport CoreOAuthTransport) (*CoreOAuthGrant, error) {
	if p.oauth == nil {
		return nil, &CoreOAuthProtocolError{}
	}
	token, err := p.oauth.RefreshTokenWithClientID(ctx, previous.RefreshToken, transport.ProxyURL, openai.ClientID)
	if err != nil {
		return nil, coreOAuthCodexRefreshError(err)
	}
	return coreOAuthNativeGrant(token, previous), nil
}

func coreOAuthCodexRefreshError(err error) *CoreOAuthProtocolError {
	var rejection *openai.RefreshTokenError
	if errors.As(err, &rejection) && rejection != nil &&
		rejection.StatusCode >= http.StatusBadRequest && rejection.StatusCode < http.StatusInternalServerError {
		return &CoreOAuthProtocolError{Permanent: rejection.Permanent}
	}
	return &CoreOAuthProtocolError{OutcomeUnknown: true}
}
func coreOAuthNativeGrant(token *OpenAITokenInfo, previous CoreOAuthGrant) *CoreOAuthGrant {
	if token == nil {
		return nil
	}
	grant := &CoreOAuthGrant{AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, IDToken: token.IDToken, ClientID: openai.ClientID, Subject: previous.Subject, WorkspaceID: token.ChatGPTAccountID, Email: token.Email, ExpiresAt: time.Unix(token.ExpiresAt, 0).UTC()}
	if grant.RefreshToken == "" {
		grant.RefreshToken = previous.RefreshToken
	}
	if grant.IDToken == "" {
		grant.IDToken = previous.IDToken
	}
	if grant.Email == "" {
		grant.Email = previous.Email
	}
	if grant.WorkspaceID == "" {
		grant.WorkspaceID = previous.WorkspaceID
	}
	parts := strings.Split(token.AccessToken, ".")
	if len(parts) == 3 {
		if raw, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			var claims struct {
				Subject string `json:"sub"`
			}
			if json.Unmarshal(raw, &claims) == nil && claims.Subject != "" {
				grant.Subject = claims.Subject
			}
		}
	}
	if grant.Subject == "" && token.IDToken != "" {
		if claims, err := openai.ParseIDToken(token.IDToken); err == nil {
			grant.Subject = claims.Sub
		}
	}
	return grant
}
