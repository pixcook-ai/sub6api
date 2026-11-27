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

func TestAnthropicPassthroughMessagesPreservesAllowedRequestID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const requestID = "req_upstream123"
	tests := []struct {
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
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"request-id": {requestID}},
				Body:       io.NopCloser(strings.NewReader(`{"type":"message","usage":{"input_tokens":1}}`)),
			}
			svc := &GatewayService{responseHeaderFilter: tt.filter}

			_, err := svc.handleNonStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), resp, c, nil)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, tt.want, rec.Result().Header.Get("Request-Id"))
		})
	}
}
