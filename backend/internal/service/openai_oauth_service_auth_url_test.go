package service

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

type openaiOAuthClientAuthURLStub struct{}

func (s *openaiOAuthClientAuthURLStub) ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI, proxyURL, clientID string) (*openai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func (s *openaiOAuthClientAuthURLStub) RefreshToken(ctx context.Context, refreshToken, proxyURL string) (*openai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func (s *openaiOAuthClientAuthURLStub) RefreshTokenWithClientID(ctx context.Context, refreshToken, proxyURL string, clientID string) (*openai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func TestOpenAIOAuthService_GenerateAuthURL_OpenAIKeepsCodexFlow(t *testing.T) {
	svc := NewOpenAIOAuthService(nil, &openaiOAuthClientAuthURLStub{})
	defer svc.Stop()

	result, err := svc.GenerateAuthURL(context.Background(), nil, "", PlatformOpenAI, "")
	require.NoError(t, err)
	require.NotEmpty(t, result.AuthURL)
	require.NotEmpty(t, result.SessionID)

	parsed, err := url.Parse(result.AuthURL)
	require.NoError(t, err)
	q := parsed.Query()
	require.Equal(t, openai.ClientID, q.Get("client_id"))
	require.Equal(t, "true", q.Get("codex_cli_simplified_flow"))

	session, ok := svc.sessionStore.Get(result.SessionID)
	require.True(t, ok)
	require.Equal(t, openai.ClientID, session.ClientID)
}

func TestOpenAIOAuthService_GenerateAuthURL_ExcelUsesOfficialClientAndCallback(t *testing.T) {
	svc := NewOpenAIOAuthService(nil, &openaiOAuthClientAuthURLStub{})
	defer svc.Stop()

	// A caller-supplied redirect URI must not override the official Excel callback.
	result, err := svc.GenerateAuthURL(context.Background(), nil, "http://localhost:1455/auth/callback", PlatformOpenAI, "excel")
	require.NoError(t, err)

	parsed, err := url.Parse(result.AuthURL)
	require.NoError(t, err)
	require.Equal(t, "https", parsed.Scheme)
	require.Equal(t, "auth.openai.com", parsed.Host)
	require.Equal(t, "/api/accounts/authorize", parsed.Path)
	q := parsed.Query()
	require.Equal(t, openai.ExcelClientID, q.Get("client_id"))
	require.Equal(t, openai.ExcelRedirectURI, q.Get("redirect_uri"))
	require.Equal(t, "S256", q.Get("code_challenge_method"))
	require.Equal(t, "PC", q.Get("platform"))
	require.Empty(t, q.Get("codex_cli_simplified_flow"))
	require.True(t, strings.HasPrefix(q.Get("state"), "bps."))
	require.True(t, strings.HasSuffix(q.Get("state"), ".PC"))

	session, ok := svc.sessionStore.Get(result.SessionID)
	require.True(t, ok)
	require.Equal(t, openai.ExcelClientID, session.ClientID)
	require.Equal(t, openai.ExcelRedirectURI, session.RedirectURI)
	require.Equal(t, q.Get("state"), session.State)
}
