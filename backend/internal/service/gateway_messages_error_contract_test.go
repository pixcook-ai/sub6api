package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAnthropicMessagesForwardCarriesTerminalErrorHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []struct {
		name        string
		passthrough bool
	}{{name: "standard"}, {name: "passthrough", passthrough: true}} {
		for _, failure := range []struct {
			name        string
			status      int
			kind        string
			forceRemove []string
		}{
			{name: "direct_unprocessable", status: 422, kind: "invalid_request_error"},
			{name: "rate_limit", status: 429, kind: "rate_limit_error"},
			{name: "overloaded", status: 529, kind: "overloaded_error"},
			{name: "filtered", status: 429, kind: "rate_limit_error", forceRemove: []string{"request-id", "x-should-retry", "retry-after"}},
		} {
			t.Run(mode.name+"/"+failure.name, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				resp := &http.Response{
					StatusCode: failure.status,
					Header:     anthropicMessagesContractHeaders(),
					Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"` + failure.kind + `","message":"synthetic upstream error"}}`)),
				}
				svc := &GatewayService{
					cfg:                 &config.Config{},
					httpUpstream:        &anthropicHTTPUpstreamRecorder{resp: resp},
					rateLimitService:    &RateLimitService{},
					tlsFPProfileService: &TLSFingerprintProfileService{},
					responseHeaderFilter: responseheaders.CompileHeaderFilter(config.ResponseHeaderConfig{
						Enabled:     true,
						ForceRemove: failure.forceRemove,
					}),
				}
				account := newAnthropicAPIKeyAccountForTest()
				account.Extra["anthropic_passthrough"] = mode.passthrough
				account.Credentials["pool_mode"] = true
				body := NewRequestBodyRef([]byte(`{"model":"claude-sonnet-4-5-20250929","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
				parsed, err := ParseGatewayRequest(body, PlatformAnthropic)
				require.NoError(t, err)

				_, err = svc.Forward(c.Request.Context(), c, account, parsed)
				require.Error(t, err)
				var got http.Header
				if failure.status == http.StatusUnprocessableEntity {
					require.Equal(t, http.StatusBadGateway, recorder.Code)
					require.Equal(t, "upstream_error", gjson.Get(recorder.Body.String(), "error.type").String())
					assert.Equal(t, "req_messages_fixture", gjson.Get(recorder.Body.String(), "request_id").String())
					got = recorder.Result().Header
				} else {
					var failover *UpstreamFailoverError
					require.ErrorAs(t, err, &failover)
					require.Equal(t, failure.status, failover.StatusCode)
					require.False(t, c.Writer.Written())
					require.Empty(t, c.Writer.Header().Get("request-id"))
					got = failover.ResponseHeaders
				}
				for _, name := range []string{"request-id", "x-should-retry", "retry-after"} {
					want := resp.Header.Get(name)
					if len(failure.forceRemove) > 0 {
						want = ""
					}
					assert.Equal(t, want, got.Get(name), name)
				}
				assert.Empty(t, got.Get("Content-Encoding"), "rewritten errors must not inherit upstream compression")
			})
		}
	}
}

func TestAnthropicMessagesRetryExhaustedErrorPreservesHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	svc := &GatewayService{cfg: &config.Config{}}
	resp := &http.Response{
		StatusCode: http.StatusConflict,
		Header:     anthropicMessagesContractHeaders(),
		Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"api_error","message":"synthetic conflict"}}`)),
	}

	_, err := svc.handleRetryExhaustedError(context.Background(), resp, c, newAnthropicAPIKeyAccountForTest())
	require.Error(t, err)
	require.Equal(t, http.StatusBadGateway, recorder.Code)
	for _, name := range []string{"request-id", "x-should-retry", "retry-after"} {
		assert.Equal(t, resp.Header.Get(name), recorder.Result().Header.Get(name), name)
	}
	assert.Equal(t, "req_messages_fixture", gjson.Get(recorder.Body.String(), "request_id").String())
}

func anthropicMessagesContractHeaders() http.Header {
	headers := make(http.Header)
	headers.Set("request-id", "req_messages_fixture")
	headers.Set("x-should-retry", "false")
	headers.Set("retry-after", "13")
	headers.Set("content-encoding", "gzip")
	return headers
}
