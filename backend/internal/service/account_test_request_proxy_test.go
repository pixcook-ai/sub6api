//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

func TestAccountTestRequestProxyUnavailableNeverUsesSingleExit(t *testing.T) {
	c, recorder := newTestContext()
	upstream := &queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(http.StatusOK, "data: {\"type\":\"response.completed\"}\n\n")}}
	legacyID := int64(90)
	account := &Account{
		ID: 885, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1,
		Credentials: map[string]any{"access_token": "fake-token"},
		ProxyID:     &legacyID,
		Proxy:       &Proxy{ID: legacyID, Protocol: "http", Host: "legacy.example.test", Port: 8080, Status: StatusActive},
		Extra:       map[string]any{"openai_proxy_mode": "request", "openai_proxy_ids": []any{float64(77)}},
	}
	svc := &AccountTestService{httpUpstream: upstream}
	err := svc.testOpenAIAccountConnection(c, account, "gpt-5.4", "", "")
	require.Error(t, err, "request proxy configuration must fail closed when its gateway is unavailable")
	require.Empty(t, upstream.requests, "neither the saved single proxy nor direct egress may be used")
	require.NotContains(t, recorder.Body.String(), "\"success\":true")
}

type accountTestRequestProxyNetworkUpstream struct {
	bpsRequestProxyNetworkUpstream
	lastRequest *http.Request
}

func (u *accountTestRequestProxyNetworkUpstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.lastRequest = req
	return u.Do(req, proxy, accountID, concurrency)
}

func TestAccountTestRequestProxyLoopbackKeepsSingleUnusedAndReleases(t *testing.T) {
	var selectedSends, savedSends atomic.Int32
	selected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		selectedSends.Add(1)
		if r.Header.Get("Authorization") != "Bearer fake-token" {
			http.Error(w, "unexpected account", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"probe\",\"status\":\"completed\",\"output\":[]}}\n\n"))
	}))
	t.Cleanup(selected.Close)
	saved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		savedSends.Add(1)
		http.Error(w, "saved single exit must stay unused", http.StatusBadGateway)
	}))
	t.Cleanup(saved.Close)
	proxyFor := func(id int64, raw string) Proxy {
		u, err := url.Parse(raw)
		require.NoError(t, err)
		port, err := strconv.Atoi(u.Port())
		require.NoError(t, err)
		return Proxy{ID: id, Protocol: "http", Host: u.Hostname(), Port: port, Status: StatusActive}
	}
	chosen, legacy := proxyFor(885701, selected.URL), proxyFor(885790, saved.URL)
	account := &Account{
		ID: 8857, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1,
		Credentials: map[string]any{"access_token": "fake-token"},
		ProxyID:     &legacy.ID, Proxy: &legacy,
		Extra: map[string]any{"openai_proxy_mode": "request", "openai_proxy_ids": []int64{chosen.ID}},
	}
	repo := &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}}}
	upstream := &accountTestRequestProxyNetworkUpstream{}
	svc := &AccountTestService{
		accountRepo: repo, httpUpstream: upstream,
		openaiGatewayService: &OpenAIGatewayService{proxyRepo: &bpsRequestProxyRepository{members: []Proxy{chosen}}},
	}
	c, recorder := newTestContext()
	original := c.Request
	require.NoError(t, svc.TestAccountConnection(c, account.ID, "gpt-5.4", "", ""))
	require.Equal(t, int32(1), selectedSends.Load())
	require.Zero(t, savedSends.Load())
	require.Contains(t, recorder.Body.String(), "test_complete")
	require.Same(t, original, c.Request, "a completed probe must not leave a finished route on its caller")
	require.ErrorIs(t, upstream.lastRequest.Context().Err(), context.Canceled)
}

func TestOpenAIRequestProxyTiboProbeUnavailableNeverUsesSingleExit(t *testing.T) {
	tc := newTiboRouteCase(t, true, false, cookieWSHTTPResponse("True"))
	tc.account.Extra["openai_proxy_mode"] = "request"
	tc.account.Extra["openai_proxy_ids"] = []int64{885702}
	sample := tc.svc.probeOpenAITiboHTTP(t.Context(), tc.account)
	require.Equal(t, openAITiboUnknown, sample.verdict)
	require.Empty(t, tc.upstream.requests, "diagnostic probes must fail before sending outside their configured group")
}
