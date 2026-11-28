package service

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type requestProxyRetryUpstream struct {
	HTTPUpstream
	requests int
	proxies  []string
}

func (u *requestProxyRetryUpstream) Do(_ *http.Request, proxy string, _ int64, _ int) (*http.Response, error) {
	u.requests++
	u.proxies = append(u.proxies, proxy)
	status, body := http.StatusOK, bpsJSON(bpsResponse(bpsText("OK")))
	if u.requests == 1 {
		status, body = http.StatusTooManyRequests, `{"error":{"message":"retry after explicit rejection"}}`
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestOpenAIRequestProxySequentialForwardKeepsHandlerRetryContextUsable(t *testing.T) {
	for _, nextAccount := range []string{"same_account", "different_account"} {
		t.Run(nextAccount, func(t *testing.T) {
			gateway, first := bpsFixture()
			selected := Proxy{ID: 88521, Protocol: "http", Host: "selected.example.test", Port: 8080, Status: StatusActive}
			first.Extra = map[string]any{OpenAIProxyModeKey: OpenAIProxyModeRequest, OpenAIProxyIDsKey: []int64{selected.ID}}
			gateway.SetOpenAIRequestProxyRepository(&bpsRequestProxyRepository{members: []Proxy{selected}})
			upstream := &requestProxyRetryUpstream{}
			gateway.httpUpstream = upstream
			c, response := bpsContext(1, "/v1/responses")
			body := bpsRequestBody("hello", false)
			_, err := gateway.Forward(c.Request.Context(), c, first, body)
			require.Error(t, err)
			var rejected *UpstreamFailoverError
			require.True(t, errors.As(err, &rejected), "explicit HTTP 429 must remain eligible for normal handler retry")
			require.Nil(t, c.Request.Context().Value(openAIRequestProxyContextKey{}), "finished route must not leak into the handler's next attempt")
			routing, ok := c.Request.Context().Value(bpsRoutingContextKey{}).(bpsRoutingContext)
			require.True(t, ok, "cleanup must retain BPS routing context installed during forwarding")
			require.NotEmpty(t, routing.Scope)
			second := first
			if nextAccount == "different_account" {
				copied := *first
				copied.ID++
				second = &copied
			}
			_, err = gateway.Forward(c.Request.Context(), c, second, body)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, response.Code)
			require.Equal(t, []string{selected.URL(), selected.URL()}, upstream.proxies)
			require.Nil(t, c.Request.Context().Value(openAIRequestProxyContextKey{}))
		})
	}
}
