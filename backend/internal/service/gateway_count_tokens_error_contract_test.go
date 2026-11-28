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

func TestForwardCountTokensAnthropicRetryableErrorContract(t *testing.T) {
	gin.SetMode(gin.TestMode)

	modes := []struct {
		name        string
		passthrough bool
	}{
		{name: "standard"},
		{name: "api_key_passthrough", passthrough: true},
	}
	failures := []struct {
		name              string
		status            int
		errorType         string
		retryAfter        string
		shouldRetry       string
		removeRetryAfter  bool
		removeRequestID   bool
		removeShouldRetry bool
	}{
		{name: "rate_limited", status: http.StatusTooManyRequests, errorType: "rate_limit_error", retryAfter: "12", shouldRetry: "false"},
		{name: "overloaded", status: 529, errorType: "overloaded_error", retryAfter: "19", shouldRetry: "true"},
		{name: "server_error_disallows_retry", status: http.StatusInternalServerError, errorType: "upstream_error", shouldRetry: "false"},
		{name: "overloaded_without_retry_after", status: 529, errorType: "overloaded_error"},
		{name: "rate_limited_with_retry_after_removed", status: http.StatusTooManyRequests, errorType: "rate_limit_error", retryAfter: "12", removeRetryAfter: true},
		{name: "overloaded_with_request_id_removed", status: 529, errorType: "overloaded_error", removeRequestID: true},
		{name: "rate_limited_with_retry_hint_removed", status: http.StatusTooManyRequests, errorType: "rate_limit_error", shouldRetry: "false", removeShouldRetry: true},
	}
	for _, mode := range modes {
		for _, failure := range failures {
			t.Run(mode.name+"/"+failure.name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)

				const model = "claude-sonnet-4-5-20250929"
				body := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hello"}]}`)
				parsed := &ParsedRequest{Body: NewRequestBodyRef(body), Model: model}
				headers := make(http.Header)
				const requestID = "req_count_tokens_contract"
				headers.Set("request-id", requestID)
				if failure.retryAfter != "" {
					headers.Set("Retry-After", failure.retryAfter)
				}
				if failure.shouldRetry != "" {
					headers["x-should-retry"] = []string{failure.shouldRetry}
				}
				upstream := &anthropicHTTPUpstreamRecorder{
					resp: &http.Response{
						StatusCode: failure.status,
						Header:     headers,
						Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"` + failure.errorType + `","message":"synthetic upstream error"}}`)),
					},
				}
				svc := &GatewayService{
					cfg:              &config.Config{},
					httpUpstream:     upstream,
					rateLimitService: &RateLimitService{},
				}
				wantRetryAfter := failure.retryAfter
				wantRequestID := requestID
				wantShouldRetry := failure.shouldRetry
				var forceRemove []string
				if failure.removeRetryAfter {
					forceRemove = append(forceRemove, "Retry-After")
					wantRetryAfter = ""
				}
				if failure.removeRequestID {
					forceRemove = append(forceRemove, "request-id")
					wantRequestID = ""
				}
				if failure.removeShouldRetry {
					forceRemove = append(forceRemove, "X-Should-Retry")
					wantShouldRetry = ""
				}
				svc.responseHeaderFilter = responseheaders.CompileHeaderFilter(config.ResponseHeaderConfig{
					Enabled:     true,
					ForceRemove: forceRemove,
				})
				account := newAnthropicAPIKeyAccountForTest()
				account.Extra["anthropic_passthrough"] = mode.passthrough
				// The response contract is independent of account cooldown storage.
				account.Credentials["pool_mode"] = true

				require.Error(t, svc.ForwardCountTokens(context.Background(), c, account, parsed))
				require.Equal(t, failure.status, rec.Code)
				require.Equal(t, "error", gjson.GetBytes(rec.Body.Bytes(), "type").String())
				assert.Equal(t, failure.errorType, gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
				assert.Equal(t, wantRetryAfter, rec.Result().Header.Get("Retry-After"))
				assert.Equal(t, wantShouldRetry, rec.Result().Header.Get("X-Should-Retry"))
				assert.Equal(t, wantRequestID, rec.Result().Header.Get("request-id"))
				assert.Equal(t, wantRequestID, gjson.GetBytes(rec.Body.Bytes(), "request_id").String())
			})
		}
	}
}
