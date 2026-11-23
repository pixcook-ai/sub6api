//go:build coreoauthfixture

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCoreOAuthFixtureUpstreamForwardsWorkerEventCursor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/_control/v1/auth-events", r.URL.Path)
		require.Equal(t, "after=0", r.URL.RawQuery)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(WithHTTPUpstreamRedirectsDisabled(context.Background()), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/_control/v1/auth-events?after=0", nil)
	require.NoError(t, err)
	upstream := &coreOAuthFixtureUpstream{base: &coreWorkerTestUpstream{client: server.Client()}}
	response, err := upstream.Do(req, "", 1, 1)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
}
