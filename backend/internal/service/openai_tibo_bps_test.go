package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestTiboBPSProbeUsesBPSContractAndIndependentSession(t *testing.T) {
	tc := newTiboRouteCase(t, false, false)
	tc.account.Platform = PlatformOpenAIBPS
	proxyID := int64(17)
	tc.account.ProxyID = &proxyID
	tc.account.Proxy = &Proxy{Protocol: "http", Host: "proxy.invalid", Port: 3128}
	require.True(t, IsOpenAITiboAccount(tc.account))
	first, err := tc.svc.buildOpenAITiboProbeRequest(context.Background(), tc.account)
	require.NoError(t, err)
	second, err := tc.svc.buildOpenAITiboProbeRequest(context.Background(), tc.account)
	require.NoError(t, err)
	firstBody, err := io.ReadAll(first.Body)
	require.NoError(t, err)
	secondBody, err := io.ReadAll(second.Body)
	require.NoError(t, err)
	require.Equal(t, OpenAIBPSResponsesURL, first.URL.String())
	require.Equal(t, "Bearer test-token", first.Header.Get("Authorization"))
	require.Equal(t, "test-chatgpt", first.Header.Get("chatgpt-account-id"))
	require.Equal(t, "chatgpt", first.Header.Get("x-basispoints-auth-mode"))
	require.Equal(t, "excel", first.Header.Get("x-openai-internal-basispoints-client-editor"))
	require.Empty(t, first.Header.Get("originator"))
	require.Equal(t, "gpt-6-astra", gjson.GetBytes(firstBody, "model").String())
	require.Equal(t, "explicit", gjson.GetBytes(firstBody, "model_selection").String())
	require.Equal(t, "xhigh", gjson.GetBytes(firstBody, "reasoning_effort").String())
	require.Contains(t, string(firstBody), openAITiboProbePrompt)
	require.NotEqual(t, gjson.GetBytes(firstBody, "metadata.task_id").String(), gjson.GetBytes(secondBody, "metadata.task_id").String())
}

func TestTiboBPSProbeClassifiesOnlyCompletedResponses(t *testing.T) {
	for _, tt := range []struct {
		name, contentType, body string
		status                  int
		want                    openAITiboVerdict
	}{
		{"healthy SSE", "text/event-stream", bpsEvent("response.completed", map[string]any{"response": bpsResponse(bpsText("True"))}), 200, openAITiboHealthy},
		{"degraded JSON", "application/json", bpsJSON(bpsResponse(bpsText("False"))), 200, openAITiboDegraded},
		{"done alias", "text/event-stream", bpsEvent("response.done", map[string]any{"response": bpsResponse(bpsText("True"))}), 200, openAITiboHealthy},
		{"terminal error", "text/event-stream", bpsEvent("error", map[string]any{"error": map[string]any{"code": "rate_limit_exceeded"}}), 200, openAITiboUnknown},
		{"missing terminal", "text/event-stream", bpsEvent("response.output_text.delta", map[string]any{"delta": "True"}), 200, openAITiboUnknown},
		{"rate limit", "application/json", `{}`, 429, openAITiboUnknown},
		{"unauthorized", "application/json", `{}`, 401, openAITiboUnknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tc := newTiboRouteCase(t, false, false)
			tc.account.Platform = PlatformOpenAIBPS
			upstream := &bpsHTTPStub{contentType: tt.contentType, body: tt.body, status: tt.status}
			tc.svc.httpUpstream = upstream
			got := tc.svc.probeOpenAITiboHTTP(context.Background(), tc.account)
			require.Equal(t, tt.want, got.verdict)
			require.Equal(t, tt.status, got.status)
			require.Equal(t, 1, upstream.requests)
			require.Equal(t, OpenAIBPSResponsesURL, upstream.request.URL.String())
		})
	}
}

func TestTiboBPSProbeSkipsUnavailableCredentialsAndProbeModel(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*Account)
	}{
		{"expired", func(a *Account) { a.Credentials["expires_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339) }},
		{"no workspace", func(a *Account) { delete(a.Credentials, "chatgpt_account_id") }},
		{"model remapped", func(a *Account) { a.Credentials["model_mapping"] = map[string]any{"gpt-6-astra": "another-model"} }},
		{"proxy unresolved", func(a *Account) { id := int64(91); a.ProxyID = &id; a.Proxy = nil }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tc := newTiboRouteCase(t, false, false)
			tc.account.Platform = PlatformOpenAIBPS
			tt.mutate(tc.account)
			require.Equal(t, openAITiboUnknown, tc.svc.probeOpenAITiboHTTP(context.Background(), tc.account).verdict)
			require.Empty(t, tc.upstream.requests)
		})
	}
}

func TestTiboBPSProbeRejectsOversizedAndContradictoryStream(t *testing.T) {
	healthy := bpsEvent("response.completed", map[string]any{"response": bpsResponse(bpsText("True"))})
	for _, body := range []string{
		healthy + strings.Repeat(" ", openAITiboProbeBodyLimit),
		healthy + bpsEvent("error", map[string]any{}),
		healthy + healthy,
	} {
		tc := newTiboRouteCase(t, false, false, &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))})
		tc.account.Platform = PlatformOpenAIBPS
		require.Equal(t, openAITiboUnknown, tc.svc.probeOpenAITiboHTTP(context.Background(), tc.account).verdict)
	}
}
