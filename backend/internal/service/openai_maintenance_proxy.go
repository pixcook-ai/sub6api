package service

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	infraerrors "github.com/MACOS-DO/sub4api/internal/pkg/errors"
)

// Maintenance validates the member held by its logical request lease without
// changing the account's saved single proxy.
func resolveOpenAIMaintenanceProxy(ctx context.Context, account *Account, repo ProxyRepository, expectedURL ...string) (string, error) {
	if account == nil {
		return "", nil
	}
	route, hasRoute := openAIRequestProxyFromContext(ctx, account.ID)
	unavailable := func() (string, error) {
		return "", infraerrors.New(http.StatusServiceUnavailable, "OPENAI_PROXY_UNAVAILABLE", "No selected OpenAI proxy is available; direct fallback is disabled").WithCause(errOpenAIRequestProxyUnavailable)
	}
	if scope, _ := ctx.Value(openAIRequestProxyContextKey{}).(*openAIRequestProxyRoute); scope != nil && scope.accountID != account.ID {
		return unavailable()
	}
	if !account.OpenAIRequestProxyEnabled() {
		// A saved mode change cannot replace an in-flight request's exit.
		if hasRoute {
			return unavailable()
		}
		return "", nil
	}

	if repo == nil {
		return unavailable()
	}
	if hasRoute {
		if _, err := ValidateOpenAIRequestProxyEgress(ctx, account.ID, route.proxy); err != nil {
			return "", err
		}
		if len(expectedURL) > 0 && expectedURL[0] != route.proxy {
			return unavailable()
		}
		expectedURL = []string{route.proxy}
	}
	now := time.Now()
	for _, id := range account.OpenAIRequestProxyIDs() {
		if hasRoute && id != route.proxyID {
			continue
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		proxy, err := repo.GetByID(ctx, id)
		if err != nil || proxy == nil || !proxy.IsActive() || proxy.IsExpired(now) {
			continue
		}
		if strings.TrimSpace(proxy.Host) == "" || proxy.Port <= 0 || proxy.Port > 65535 {
			continue
		}
		switch strings.ToLower(proxy.Protocol) {
		case "http", "https", "socks5", "socks5h":
		default:
			continue
		}
		raw := proxy.URL()
		if len(expectedURL) > 0 && raw != expectedURL[0] {
			continue
		}
		parsed, err := url.Parse(raw)
		if err == nil && parsed.Hostname() != "" {
			if hasRoute {
				if _, err := ValidateOpenAIRequestProxyEgress(ctx, account.ID, raw); err != nil {
					return "", err
				}
			}
			return raw, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return unavailable()
}

// Routing rejection cannot be treated as an ordinary refresh outage and bypassed
// using an old access token, since that would escape the saved egress policy.
func isOpenAIRequestProxyRoutingError(err error) bool {
	if errors.Is(err, errOpenAIRequestProxyUnavailable) {
		return true
	}
	switch infraerrors.Reason(err) {
	case "OPENAI_PROXY_UNAVAILABLE", "OPENAI_PROXY_CONNECTION_CONFLICT", "OPENAI_OAUTH_PROXY_UNAVAILABLE":
		return true
	default:
		return false
	}
}

// This runs immediately after the refresh lock's database reread, including
// paths where fresh credentials make an upstream refresh unnecessary.
func validateOpenAIRefreshRequestProxy(ctx context.Context, account *Account) error {
	if account == nil {
		return nil
	}
	route, hasRoute := openAIRequestProxyFromContext(ctx, account.ID)
	if !hasRoute {
		return nil
	}
	if !account.OpenAIRequestProxyEnabled() {
		return infraerrors.ServiceUnavailable("OPENAI_PROXY_UNAVAILABLE", "OpenAI proxy mode changed during the request").WithCause(errOpenAIRequestProxyUnavailable)
	}
	matched := false
	for _, id := range account.OpenAIRequestProxyIDs() {
		if id == route.proxyID {
			matched = true
			break
		}
	}
	if !matched {
		return infraerrors.ServiceUnavailable("OPENAI_PROXY_UNAVAILABLE", "OpenAI proxy group changed during the request").WithCause(errOpenAIRequestProxyUnavailable)
	}
	_, err := ValidateOpenAIRequestProxyEgress(ctx, account.ID, route.proxy)
	return err
}
