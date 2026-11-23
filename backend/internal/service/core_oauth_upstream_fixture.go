//go:build coreoauthfixture

package service

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/tlsfingerprint"
)

type coreOAuthFixtureUpstream struct {
	base   HTTPUpstream
	codex  *url.URL
	client *http.Client
}

func coreOAuthRuntimeUpstream(upstream HTTPUpstream) HTTPUpstream {
	s := &coreOAuthFixtureUpstream{base: upstream, client: &http.Client{
		Timeout:       30 * time.Second,
		Transport:     &http.Transport{},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	if cfg, err := CoreOAuthFixtureLoadConfig(); err == nil && cfg.CodexBaseURL != "" {
		s.codex, _ = url.Parse(cfg.CodexBaseURL)
	}
	return s
}

// Keep the production request builder intact and replace its destination only in the fixture.
func (s *coreOAuthFixtureUpstream) Do(req *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
	rejected := errors.New("synthetic upstream request rejected")
	if req == nil || req.URL == nil || proxy != "" {
		return nil, rejected
	}
	if req.URL.String() == chatgptCodexURL {
		if s.codex == nil || req.Method != http.MethodPost || !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer fixture-at-") {
			return nil, rejected
		}
		out := req.Clone(req.Context())
		target := *s.codex
		target.Path = req.URL.Path
		out.URL, out.Host, out.RequestURI = &target, target.Host, ""
		return s.client.Do(out)
	}
	// Query parameters carry control cursors; validate the destination without
	// removing them from the request that reaches the real worker.
	destination := *req.URL
	destination.RawQuery, destination.ForceQuery = "", false
	if !CoreOAuthFixtureLoopbackURL(destination.String()) {
		return nil, rejected
	}
	return s.base.Do(req, "", id, concurrency)
}

func (s *coreOAuthFixtureUpstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return s.Do(req, proxy, id, concurrency)
}
