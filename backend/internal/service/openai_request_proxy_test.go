package service

import (
	"bytes"
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
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIRequestProxyForwardUnavailableDoesNotSendOutsideSelectedPool(t *testing.T) {
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[]}}\n\n"))}}
	svc := openAIClientToolsTestService(upstream)
	account := requestProxyOAuthFixture()
	account.Extra["openai_excel_bps"] = false
	account.Extra["openai_proxy_mode"] = "request"
	account.Extra["openai_proxy_ids"] = []int64{987654321}
	account.Proxy = &Proxy{Protocol: "http", Host: "unused.example", Port: 8080}
	body := []byte(`{"model":"gpt-5.4","input":"test","stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	_, err := svc.Forward(context.Background(), c, account, body)
	require.Error(t, err)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "openai_proxy_unavailable")
	require.Nil(t, upstream.lastReq, "selected pool failure must not send directly or through the old account proxy")
}

func TestOpenAIRequestProxyCompatibilityUnavailableNeverSends(t *testing.T) {
	for _, entry := range []string{"chat", "messages", "input_tokens", "count_tokens"} {
		t.Run(entry, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{}
			svc := openAIClientToolsTestService(upstream)
			account := requestProxyOAuthFixture()
			account.Extra["openai_excel_bps"] = false
			account.Extra["openai_proxy_mode"] = "request"
			account.Extra["openai_proxy_ids"] = []int64{987654321}
			body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"test"}],"input":"test","max_tokens":32}`)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/"+entry, bytes.NewReader(body))
			var err error
			switch entry {
			case "chat":
				_, err = svc.ForwardAsChatCompletions(t.Context(), c, account, body, "", "")
			case "messages":
				_, err = svc.ForwardAsAnthropic(t.Context(), c, account, body, "", "")
			case "input_tokens":
				err = svc.ForwardResponsesInputTokens(t.Context(), c, account, body)
			case "count_tokens":
				err = svc.ForwardCountTokensAsAnthropic(t.Context(), c, account, body, "")
			}
			require.Error(t, err)
			require.Equal(t, http.StatusServiceUnavailable, rec.Code)
			require.Contains(t, rec.Body.String(), "openai_proxy_unavailable")
			require.Empty(t, upstream.requests)
		})
	}
}

func TestOpenAIRequestProxyWebSocketExplicitlyRejectsBeforeUpstream(t *testing.T) {
	upstream := &httpUpstreamRecorder{}
	svc := openAIClientToolsTestService(upstream)
	account := requestProxyOAuthFixture()
	account.Extra["openai_excel_bps"] = false
	account.Extra["openai_proxy_mode"] = "request"
	account.Extra["openai_proxy_ids"] = []int64{1, 2}
	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = r
		serverErr <- svc.ProxyResponsesWebSocketFromClient(r.Context(), c, conn, account, "test-token", []byte(`{"type":"response.create","model":"gpt-5.4","input":"test"}`), nil)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer func() { _ = client.CloseNow() }()
	select {
	case err := <-serverErr:
		var closeErr *OpenAIWSClientCloseError
		require.ErrorAs(t, err, &closeErr)
		require.Contains(t, err.Error(), "requires HTTP/SSE")
	case <-ctx.Done():
		t.Fatal("WebSocket request proxy rejection did not finish")
	}
	require.Empty(t, upstream.requests)
}

func TestOpenAIRequestProxyExplicitSingleKeepsExistingProxy(t *testing.T) {
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"single\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"))}}
	svc := openAIClientToolsTestService(upstream)
	account := requestProxyOAuthFixture()
	account.Extra["openai_excel_bps"] = false
	account.Extra["openai_proxy_mode"] = "single"
	account.Extra["openai_proxy_ids"] = []int64{987654321}
	account.Extra["openai_excel_bps_proxy_mode"] = "request"
	account.Extra["openai_excel_bps_proxy_ids"] = []int64{987654321}
	proxyID := int64(77)
	account.ProxyID = &proxyID
	account.Proxy = &Proxy{Protocol: "http", Host: "single.example", Port: 8080}
	body := []byte(`{"model":"gpt-5.4","input":"test","stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	_, err := svc.Forward(t.Context(), c, account, body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, account.Proxy.URL(), upstream.lastProxyURL)
}

func requestProxyOAuthFixture() *Account {
	return &Account{ID: 861, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		Credentials: map[string]any{"access_token": "test-access-token", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)},
		Extra:       map[string]any{}}
}

func TestOpenAIRequestProxyLoopbackBalancesBusyExitsAndRetainsLogicalScope(t *testing.T) {
	var sends [2]atomic.Int32
	catalog := make([]Proxy, 2)
	for i := range catalog {
		index := i
		proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sends[index].Add(1)
			require.Equal(t, "upstream.example.test", r.URL.Host)
			_, _ = io.WriteString(w, "OK")
		}))
		t.Cleanup(proxyServer.Close)
		parsed, err := url.Parse(proxyServer.URL)
		require.NoError(t, err)
		port, err := strconv.Atoi(parsed.Port())
		require.NoError(t, err)
		catalog[i] = Proxy{ID: int64(885610 + i), Protocol: "http", Host: parsed.Hostname(), Port: port, Status: StatusActive}
	}
	account := requestProxyOAuthFixture()
	account.Extra = map[string]any{OpenAIProxyModeKey: OpenAIProxyModeRequest, OpenAIProxyIDsKey: []int64{catalog[0].ID, catalog[1].ID}}
	gateway := &OpenAIGatewayService{proxyRepo: &bpsRequestProxyRepository{members: catalog}, httpUpstream: &bpsRequestProxyNetworkUpstream{}}
	contexts := make([]context.Context, 3)
	releases := make([]func(), 3)
	for i := range contexts {
		scoped, release, err := gateway.beginOpenAIRequestProxy(t.Context(), nil, account)
		require.NoError(t, err)
		contexts[i], releases[i] = scoped, release
		t.Cleanup(release)
		req, err := http.NewRequestWithContext(scoped, http.MethodGet, "http://upstream.example.test/test", nil)
		require.NoError(t, err)
		response, err := gateway.doOpenAIUpstream(req, "http://old-single.invalid:8080", account)
		require.NoError(t, err)
		_, err = io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
	}
	// Both exits remain available while busy; the third request reuses one.
	require.Equal(t, int32(3), sends[0].Load()+sends[1].Load())
	require.GreaterOrEqual(t, sends[0].Load(), int32(1))
	require.GreaterOrEqual(t, sends[1].Load(), int32(1))
	// A continuation keeps its parent's exit instead of re-acquiring the idle member.
	req, err := http.NewRequestWithContext(contexts[0], http.MethodGet, "http://upstream.example.test/continuation", nil)
	require.NoError(t, err)
	route, _ := openAIRequestProxyFromContext(contexts[0], account.ID)
	before := sends[0].Load()
	if route.proxyID == catalog[1].ID {
		before = sends[1].Load()
	}
	response, err := gateway.doOpenAIUpstream(req, "", account)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	after := sends[0].Load()
	if route.proxyID == catalog[1].ID {
		after = sends[1].Load()
	}
	require.Equal(t, before+1, after)
	releases[0]()
	_, err = gateway.doOpenAIUpstream(req, "", account)
	require.ErrorIs(t, err, errOpenAIRequestProxyUnavailable)
}

func TestOpenAIRequestProxyTransportFailureDoesNotPermitCrossExitReplay(t *testing.T) {
	account := requestProxyOAuthFixture()
	selected := Proxy{ID: 885620, Protocol: "http", Host: "selected.example.test", Port: 8080, Status: StatusActive}
	account.Extra = map[string]any{OpenAIProxyModeKey: OpenAIProxyModeRequest, OpenAIProxyIDsKey: []int64{selected.ID}}
	upstream := &httpUpstreamRecorder{err: errors.New("connection lost after request write")}
	gateway := openAIClientToolsTestService(upstream)
	gateway.proxyRepo = &bpsRequestProxyRepository{members: []Proxy{selected}}
	body := []byte(`{"model":"gpt-5.4","input":"hello","stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	_, err := gateway.Forward(t.Context(), c, account, body)
	require.Error(t, err)
	var failover *UpstreamFailoverError
	require.False(t, errors.As(err, &failover), "unknown delivery must not replay through another account")
	require.Len(t, upstream.requests, 1)
	require.Equal(t, selected.URL(), upstream.lastProxyURL)
}

func TestOpenAIRequestProxyCancellationClosesInflightTransportAndReleasesLease(t *testing.T) {
	account := requestProxyOAuthFixture()
	selected := Proxy{ID: 885630, Protocol: "http", Host: "selected.example.test", Port: 8080, Status: StatusActive}
	account.Extra = map[string]any{OpenAIProxyModeKey: OpenAIProxyModeRequest, OpenAIProxyIDsKey: []int64{selected.ID}}
	gateway := &OpenAIGatewayService{proxyRepo: &bpsRequestProxyRepository{members: []Proxy{selected}}}
	parent, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	scoped, release, err := gateway.beginOpenAIRequestProxy(parent, nil, account)
	require.NoError(t, err)
	t.Cleanup(release)
	route, ok := openAIRequestProxyFromContext(scoped, account.ID)
	require.True(t, ok)
	req, err := http.NewRequestWithContext(context.WithoutCancel(scoped), http.MethodPost, "http://upstream.example.test/test", nil)
	require.NoError(t, err)
	req, err = route.bind(req)
	require.NoError(t, err)
	cancel()
	select {
	case <-req.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("cancellation failed to close the bound transport")
	}
	require.Eventually(t, func() bool {
		openAIRequestProxyScheduler.Lock()
		defer openAIRequestProxyScheduler.Unlock()
		return openAIRequestProxyScheduler.inFlight[selected.ID] == 0
	}, time.Second, time.Millisecond)
	_, err = route.bind(req)
	require.ErrorIs(t, err, errOpenAIRequestProxyUnavailable)
}
