package service

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestTiboOAuthProbePreservesTargetIdentityAndWireContract(t *testing.T) {
	tc := newTiboRouteCase(t, false, false)
	ctx := withOpenAITiboSyntheticProbe(context.Background(), tc.account.ID)
	req, err := tc.svc.buildOpenAITiboProbeRequest(ctx, tc.account)
	require.NoError(t, err)
	require.True(t, isOpenAITiboSyntheticProbe(req.Context(), tc.account))
	require.Equal(t, chatgptCodexURL, req.URL.String())
	require.Equal(t, "chatgpt.com", req.Host)
	require.Equal(t, "Bearer test-token", req.Header.Get("Authorization"))
	require.Equal(t, "test-chatgpt", req.Header.Get("ChatGPT-Account-ID"))
	require.Empty(t, req.Header.Get(openAICodexTurnStateHeader), "a quality probe starts its own turn")
	raw, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	body := tiboDecodeUpstreamBody(req.Header, raw)
	requireTiboProbe(t, body)
	require.False(t, gjson.GetBytes(body, "store").Bool())
	require.True(t, gjson.GetBytes(body, "stream").Bool())
	require.Contains(t, gjson.GetBytes(body, "input").Raw, openAITiboProbePrompt)
	require.NotEmpty(t, req.Header.Get("User-Agent"))
	for _, name := range codexHTTPNonOfficialHeaders {
		require.Empty(t, req.Header.Get(name), "target final projection strips %s", name)
	}
}
