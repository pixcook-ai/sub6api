package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

type bpsRequestProxyCaptureUpstream struct {
	HTTPUpstream
	proxies []string
	body    string
	request *http.Request
	err     error
}

func (u *bpsRequestProxyCaptureUpstream) Do(req *http.Request, proxy string, _ int64, _ int) (*http.Response, error) {
	u.request = req
	u.proxies = append(u.proxies, proxy)
	if u.err != nil {
		return nil, u.err
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(u.body))}, nil
}

func TestBPSRequestProxyUnavailableDoesNotUseSavedSingleExit(t *testing.T) {
	for _, endpoint := range []string{"responses", "model_catalog"} {
		t.Run(endpoint, func(t *testing.T) {
			gateway, account := bpsFixture()
			saved := &Proxy{ID: 90, Protocol: "http", Host: "saved.example.test", Port: 8090, Status: StatusActive}
			account.ProxyID, account.Proxy = &saved.ID, saved
			account.Extra = map[string]any{OpenAIProxyModeKey: OpenAIProxyModeRequest, OpenAIProxyIDsKey: []int64{11}}
			upstream := &bpsRequestProxyCaptureUpstream{body: bpsJSON(bpsResponse(bpsText("unexpected")))}
			gateway.httpUpstream = upstream
			if endpoint == "responses" {
				c, response := bpsContext(1, "/v1/responses")
				_, err := gateway.forwardOpenAIBPS(c.Request.Context(), c, account, bpsRequestBody("hello", false))
				require.Error(t, err)
				require.Equal(t, http.StatusServiceUnavailable, response.Code)
				require.Contains(t, response.Body.String(), "openai_proxy_unavailable")
			} else {
				_, err := gateway.FetchOpenAIBPSModelIDs(context.Background(), account)
				require.Error(t, err)
			}
			require.Empty(t, upstream.proxies, "unavailable selected proxies must not fall back to the saved single proxy or direct")
		})
	}
}

func TestBPSSingleProxyKeepsExistingExit(t *testing.T) {
	gateway, account := bpsFixture()
	saved := &Proxy{ID: 90, Protocol: "http", Host: "saved.example.test", Port: 8090, Status: StatusActive}
	account.ProxyID, account.Proxy = &saved.ID, saved
	account.Extra = map[string]any{OpenAIProxyModeKey: OpenAIProxyModeSingle, OpenAIProxyIDsKey: []int64{11}}
	upstream := &bpsRequestProxyCaptureUpstream{body: bpsJSON(bpsResponse(bpsText("OK")))}
	gateway.httpUpstream = upstream
	c, response := bpsContext(1, "/v1/responses")
	_, err := gateway.forwardOpenAIBPS(c.Request.Context(), c, account, bpsRequestBody("hello", false))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, []string{saved.URL()}, upstream.proxies)
}

type bpsRequestProxyRepository struct {
	ProxyRepository
	members []Proxy
}

func (r *bpsRequestProxyRepository) ListByIDs(context.Context, []int64) ([]Proxy, error) {
	return r.members, nil
}

func TestBPSRequestProxyUsesSelectedExitForResponsesAndModelCatalog(t *testing.T) {
	for _, endpoint := range []string{"responses", "model_catalog"} {
		t.Run(endpoint, func(t *testing.T) {
			gateway, account := bpsFixture()
			selected := Proxy{ID: 88511, Protocol: "http", Host: "selected.example.test", Port: 8080, Status: StatusActive}
			saved := Proxy{ID: 88590, Protocol: "http", Host: "saved.example.test", Port: 8090, Status: StatusActive}
			account.ProxyID, account.Proxy = &saved.ID, &saved
			account.Extra = map[string]any{OpenAIProxyModeKey: OpenAIProxyModeRequest, OpenAIProxyIDsKey: []int64{selected.ID}}
			gateway.SetOpenAIRequestProxyRepository(&bpsRequestProxyRepository{members: []Proxy{selected}})
			upstream := &bpsRequestProxyCaptureUpstream{body: bpsJSON(bpsResponse(bpsText("OK")))}
			gateway.httpUpstream = upstream
			if endpoint == "responses" {
				c, _ := bpsContext(1, "/v1/responses")
				_, err := gateway.forwardOpenAIBPS(c.Request.Context(), c, account, bpsRequestBody("hello", false))
				require.NoError(t, err)
			} else {
				upstream.body = `{"allowed":true,"model_catalog":{"models":[{"id":"gpt-6-astra"}]}}`
				ids, err := gateway.FetchOpenAIBPSModelIDs(context.Background(), account)
				require.NoError(t, err)
				require.Equal(t, []string{"gpt-6-astra"}, ids)
			}
			require.Equal(t, []string{selected.URL()}, upstream.proxies)
			require.ErrorIs(t, upstream.request.Context().Err(), context.Canceled, "the request transport scope must end after consuming its final response")
		})
	}
}

func TestBPSRequestProxyTransportFailureDoesNotReplayThroughAnotherAccount(t *testing.T) {
	gateway, account := bpsFixture()
	selected := Proxy{ID: 88512, Protocol: "http", Host: "selected.example.test", Port: 8080, Status: StatusActive}
	account.Extra = map[string]any{OpenAIProxyModeKey: OpenAIProxyModeRequest, OpenAIProxyIDsKey: []int64{selected.ID}}
	gateway.SetOpenAIRequestProxyRepository(&bpsRequestProxyRepository{members: []Proxy{selected}})
	upstream := &bpsRequestProxyCaptureUpstream{err: errors.New("connection dropped after send")}
	gateway.httpUpstream = upstream
	c, response := bpsContext(1, "/v1/responses")
	_, err := gateway.forwardOpenAIBPS(c.Request.Context(), c, account, bpsRequestBody("hello", false))
	require.Error(t, err)
	var failover *UpstreamFailoverError
	require.False(t, errors.As(err, &failover), "unknown delivery must not instruct a cross-exit replay")
	require.Equal(t, http.StatusBadGateway, response.Code)
	require.Equal(t, []string{selected.URL()}, upstream.proxies)
	require.ErrorIs(t, upstream.request.Context().Err(), context.Canceled)
}

// The test proxy terminates ordinary HTTP locally. Production request payloads
// and the selected proxy URL pass through the real net/http proxy transport;
// no provider request is made.
type bpsRequestProxyNetworkUpstream struct{ HTTPUpstream }

func (*bpsRequestProxyNetworkUpstream) Do(request *http.Request, proxy string, _ int64, _ int) (*http.Response, error) {
	parsed, err := url.Parse(proxy)
	if err != nil || parsed.Host == "" {
		return nil, errors.New("selected loopback proxy is required")
	}
	transport := &http.Transport{Proxy: http.ProxyURL(parsed), DisableKeepAlives: true}
	client := &http.Client{Transport: transport}
	next := request.Clone(request.Context())
	target := *request.URL
	target.Scheme = "http"
	next.URL = &target
	return client.Do(next)
}

func TestBPSRequestProxyLoopbackUsesSelectedExitAndKeepsSavedSingleUnused(t *testing.T) {
	var selectedSends, singleSends atomic.Int32
	selectedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		selectedSends.Add(1)
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Errorf("unexpected loopback Authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/basispoints/api/responses/access" {
			_, _ = io.WriteString(w, `{"allowed":true,"model_catalog":{"models":[{"id":"gpt-6-astra"}]}}`)
			return
		}
		_, _ = io.WriteString(w, bpsJSON(bpsResponse(bpsText("OK"))))
	}))
	defer selectedServer.Close()
	singleServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		singleSends.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer singleServer.Close()
	proxy := func(id int64, raw string) Proxy {
		parsed, err := url.Parse(raw)
		require.NoError(t, err)
		port, err := strconv.Atoi(parsed.Port())
		require.NoError(t, err)
		return Proxy{ID: id, Protocol: parsed.Scheme, Host: parsed.Hostname(), Port: port, Status: StatusActive}
	}
	selected, saved := proxy(88513, selectedServer.URL), proxy(88593, singleServer.URL)
	gateway, account := bpsFixture()
	account.ProxyID, account.Proxy = &saved.ID, &saved
	account.Extra = map[string]any{OpenAIProxyModeKey: OpenAIProxyModeRequest, OpenAIProxyIDsKey: []int64{selected.ID}}
	gateway.SetOpenAIRequestProxyRepository(&bpsRequestProxyRepository{members: []Proxy{selected}})
	gateway.httpUpstream = &bpsRequestProxyNetworkUpstream{}
	c, response := bpsContext(1, "/v1/responses")
	_, err := gateway.forwardOpenAIBPS(c.Request.Context(), c, account, bpsRequestBody("hello", false))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.Code)
	ids, err := gateway.FetchOpenAIBPSModelIDs(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []string{"gpt-6-astra"}, ids)
	require.EqualValues(t, 2, selectedSends.Load())
	require.Zero(t, singleSends.Load())
}
