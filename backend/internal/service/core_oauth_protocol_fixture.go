//go:build coreoauthfixture

package service

// This file is excluded from every normal production build. Its protocol is a
// synthetic integration seam and provides no evidence about official OAuth.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type CoreOAuthFixtureConfig struct {
	Contract      string `json:"contract"`
	Synthetic     bool   `json:"synthetic"`
	RunID         string `json:"run_id"`
	OAuthBaseURL  string `json:"oauth_base_url"`
	ControlSecret string `json:"control_secret"`
}

func CoreOAuthFixtureReadPrivate(path string, target any) error {
	bad := errors.New("invalid synthetic fixture input")
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return bad
	}
	actual, err := filepath.EvalSymlinks(path)
	if err != nil || actual != path {
		return bad
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 1<<20 {
		return bad
	}
	file, err := os.Open(path)
	if err != nil {
		return bad
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return bad
	}
	raw, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return bad
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		return bad
	}
	return nil
}

func CoreOAuthFixtureLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || strings.TrimSpace(raw) != raw {
		return false
	}
	port, err := strconv.Atoi(u.Port())
	return err == nil && port > 0 && port <= 65535 && net.ParseIP(u.Hostname()).IsLoopback()
}

func CoreOAuthFixtureLoadConfig() (*CoreOAuthFixtureConfig, error) {
	var cfg CoreOAuthFixtureConfig
	if CoreOAuthFixtureReadPrivate(os.Getenv("SUB6_CORE_OAUTH_FIXTURE_CONFIG"), &cfg) != nil || cfg.Contract != "sub6-core-oauth-fixture/1" || !cfg.Synthetic || !regexp.MustCompile(`^[a-f0-9]{24}$`).MatchString(cfg.RunID) || !CoreOAuthFixtureLoopbackURL(cfg.OAuthBaseURL) || !regexp.MustCompile(`^fixture-control-[a-f0-9]{48}$`).MatchString(cfg.ControlSecret) {
		return nil, errors.New("synthetic OAuth fixture configuration required")
	}
	u, _ := url.Parse(cfg.OAuthBaseURL)
	if u.Path != "" {
		return nil, errors.New("synthetic OAuth fixture origin required")
	}
	return &cfg, nil
}

func newCoreOAuthRuntimeService(repo CoreOAuthRepository, encryptor SecretEncryptor, _ *OpenAIOAuthService, proxies ProxyRepository) (*CoreOAuthService, error) {
	cfg, err := CoreOAuthFixtureLoadConfig()
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	protocols := map[string]CoreOAuthProtocol{}
	for _, provider := range []string{CoreOAuthProviderCodex, CoreOAuthProviderExcel} {
		protocols[provider] = &coreOAuthFixtureProtocol{cfg: *cfg, provider: provider, client: client}
	}
	return NewCoreOAuthService(repo, encryptor, protocols, proxies)
}

type coreOAuthFixtureProtocol struct {
	cfg      CoreOAuthFixtureConfig
	provider string
	client   *http.Client
}
type coreOAuthFixtureAttempt struct {
	ID    string
	State string
}
type coreOAuthFixtureGrant struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	IDToken      string    `json:"id_token"`
	ClientID     string    `json:"client_id"`
	Subject      string    `json:"subject"`
	WorkspaceID  string    `json:"workspace_id"`
	Email        string    `json:"email"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func (p *coreOAuthFixtureProtocol) call(ctx context.Context, path string, input, output any) error {
	payload, err := json.Marshal(input)
	if err != nil {
		return &CoreOAuthProtocolError{}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.OAuthBaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return &CoreOAuthProtocolError{}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Fixture-Control", p.cfg.ControlSecret)
	response, err := p.client.Do(request)
	if err != nil {
		return &CoreOAuthProtocolError{OutcomeUnknown: true}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return &CoreOAuthProtocolError{Permanent: response.StatusCode == 400, OutcomeUnknown: response.StatusCode >= 500}
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 65537))
	decoder.DisallowUnknownFields()
	if decoder.Decode(output) != nil || decoder.Decode(new(any)) != io.EOF {
		return &CoreOAuthProtocolError{OutcomeUnknown: true}
	}
	return nil
}
func (p *coreOAuthFixtureProtocol) Begin(ctx context.Context, _ CoreOAuthTransport) (*CoreOAuthProtocolAttempt, error) {
	var out struct {
		AttemptID string    `json:"attempt_id"`
		State     string    `json:"state"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := p.call(ctx, "/oauth/begin", map[string]any{"provider": p.provider}, &out); err != nil {
		return nil, err
	}
	if !regexp.MustCompile(`^fixture-state-[a-f0-9]{32}$`).MatchString(out.State) || !regexp.MustCompile(`^fixture-attempt-[a-f0-9]{32}$`).MatchString(out.AttemptID) || !out.ExpiresAt.After(time.Now()) {
		return nil, &CoreOAuthProtocolError{}
	}
	redirect := "http://localhost:1455/auth/callback"
	authorizePath := "/oauth/authorize"
	if p.provider == CoreOAuthProviderExcel {
		redirect = "https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/auth/callback"
		authorizePath = "/api/accounts/authorize"
	}
	// The runner parses this metadata but never opens it. All protocol I/O uses call.
	metadata := url.URL{Scheme: "https", Host: "auth.openai.com", Path: authorizePath, RawQuery: url.Values{"state": {out.State}}.Encode()}
	return &CoreOAuthProtocolAttempt{AuthorizationURL: metadata.String(), RedirectURI: redirect, ExpiresAt: out.ExpiresAt, State: coreOAuthFixtureAttempt{ID: out.AttemptID, State: out.State}}, nil
}
func (p *coreOAuthFixtureProtocol) grant(in coreOAuthFixtureGrant) (*CoreOAuthGrant, error) {
	client := "app_EMoamEEZ73f0CkXaXp7hrann"
	if p.provider == CoreOAuthProviderExcel {
		client = "app_fnr0pYvVwwFDocDumLG3H2Bp"
	}
	if !strings.HasPrefix(in.AccessToken, "fixture-at-") || !strings.HasPrefix(in.RefreshToken, "fixture-rt-") || !strings.HasPrefix(in.IDToken, "fixture-id-") || in.ClientID != client || !strings.HasPrefix(in.Subject, "fixture-user-") || !strings.HasPrefix(in.WorkspaceID, "fixture-workspace-") || !strings.HasSuffix(in.Email, "@example.invalid") || !in.ExpiresAt.After(time.Now()) {
		return nil, &CoreOAuthProtocolError{}
	}
	return &CoreOAuthGrant{AccessToken: in.AccessToken, RefreshToken: in.RefreshToken, IDToken: in.IDToken, ClientID: in.ClientID, Subject: in.Subject, WorkspaceID: in.WorkspaceID, Email: in.Email, ExpiresAt: in.ExpiresAt}, nil
}
func (p *coreOAuthFixtureProtocol) ExchangeCallback(ctx context.Context, attempt *CoreOAuthProtocolAttempt, callback string, expected CoreOAuthIdentity, _ CoreOAuthTransport) (*CoreOAuthGrant, error) {
	state, ok := attempt.State.(coreOAuthFixtureAttempt)
	if !ok {
		return nil, &CoreOAuthProtocolError{}
	}
	u, err := url.Parse(callback)
	if err != nil || u.User != nil || u.Fragment != "" || u.Scheme+"://"+u.Host+u.EscapedPath() != attempt.RedirectURI {
		return nil, &CoreOAuthProtocolError{Permanent: true}
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(values) != 2 || values.Get("state") != state.State || !strings.HasPrefix(values.Get("code"), "fixture-code-") {
		return nil, &CoreOAuthProtocolError{Permanent: true}
	}
	for _, v := range values {
		if len(v) != 1 {
			return nil, &CoreOAuthProtocolError{Permanent: true}
		}
	}
	var out coreOAuthFixtureGrant
	if err = p.call(ctx, "/oauth/exchange", map[string]any{"provider": p.provider, "attempt_id": state.ID, "state": state.State, "code": values.Get("code")}, &out); err != nil {
		return nil, err
	}
	grant, err := p.grant(out)
	if err == nil && ((expected.Subject != "" && grant.Subject != expected.Subject) || (expected.WorkspaceID != "" && grant.WorkspaceID != expected.WorkspaceID)) {
		return nil, &CoreOAuthProtocolError{Permanent: true}
	}
	return grant, err
}
func (p *coreOAuthFixtureProtocol) Refresh(ctx context.Context, old CoreOAuthGrant, _ CoreOAuthTransport) (*CoreOAuthGrant, error) {
	var out coreOAuthFixtureGrant
	if err := p.call(ctx, "/oauth/refresh", map[string]any{"provider": p.provider, "refresh_token": old.RefreshToken}, &out); err != nil {
		return nil, err
	}
	grant, err := p.grant(out)
	if err == nil && (grant.Subject != old.Subject || grant.WorkspaceID != old.WorkspaceID) {
		return nil, &CoreOAuthProtocolError{Permanent: true}
	}
	return grant, err
}
