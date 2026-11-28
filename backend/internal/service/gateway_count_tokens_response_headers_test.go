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
	"github.com/stretchr/testify/require"
)

func TestAnthropicCountTokensSuccessPreservesAllowedRequestID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const requestID = "req_count_tokens_success"
	modes := []struct {
		name        string
		passthrough bool
	}{
		{name: "standard"},
		{name: "api_key_passthrough", passthrough: true},
	}
	filters := []struct {
		name   string
		filter *responseheaders.CompiledHeaderFilter
		want   string
	}{
		{name: "default", want: requestID},
		{
			name: "force_remove",
			filter: responseheaders.CompileHeaderFilter(config.ResponseHeaderConfig{
				Enabled:     true,
				ForceRemove: []string{"request-id"},
			}),
		},
	}
	for _, mode := range modes {
		for _, filter := range filters {
			t.Run(mode.name+"/"+filter.name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)
				const model = "claude-sonnet-4-5-20250929"
				body := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hello"}]}`)
				parsed := &ParsedRequest{Body: NewRequestBodyRef(body), Model: model}
				const responseBody = `{"input_tokens":42}`
				upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"request-id": {requestID}},
					Body:       io.NopCloser(strings.NewReader(responseBody)),
				}}
				svc := &GatewayService{
					cfg:                  &config.Config{},
					httpUpstream:         upstream,
					responseHeaderFilter: filter.filter,
					rateLimitService:     &RateLimitService{},
				}
				account := newAnthropicAPIKeyAccountForTest()
				account.Extra["anthropic_passthrough"] = mode.passthrough

				require.NoError(t, svc.ForwardCountTokens(context.Background(), c, account, parsed))
				require.Equal(t, http.StatusOK, rec.Code)
				require.Equal(t, responseBody, rec.Body.String())
				require.Equal(t, filter.want, rec.Result().Header.Get("request-id"))
			})
		}
	}
}
