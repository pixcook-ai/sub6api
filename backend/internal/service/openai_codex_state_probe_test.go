package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestProbeOpenAICodexStateUsesSelectedAccountAndContinuesTicket(t *testing.T) {
	const completed = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"reported-model\"}}\n\n"
	for _, tc := range []struct {
		name        string
		status      int
		stream      string
		secondState string
		want        OpenAICodexStateVerdict
		failure     string
	}{
		{name: "renewed", status: http.StatusOK, stream: completed, secondState: "new-ticket", want: OpenAICodexStateDegraded},
		{name: "retained", status: http.StatusOK, stream: completed, want: OpenAICodexStateHealthy},
		{name: "truncated", status: http.StatusOK, stream: strings.TrimSpace(completed), secondState: "new-ticket", want: OpenAICodexStateInconclusive, failure: "stream_error"},
		{name: "failed", status: http.StatusOK, stream: completed + "data: {\"type\":\"response.failed\"}\n\n", secondState: "new-ticket", want: OpenAICodexStateInconclusive, failure: "stream_error"},
		{name: "rate limited", status: http.StatusTooManyRequests, stream: completed, secondState: "new-ticket", want: OpenAICodexStateInconclusive, failure: "rate_limited"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := ticketTestAccount(41)
			account.Credentials["model_mapping"] = map[string]any{"test-alias": "gpt-6-astra"}
			proxyID := int64(12)
			account.ProxyID = &proxyID
			account.Proxy = &Proxy{Protocol: "http", Host: "proxy.example", Port: 8765}
			repo := &nativeProbeAccountRepo{known: account}
			var sessions []string
			upstream := &iqProbeUpstream{dispatch: func(req *http.Request, proxyURL string, accountID int64) (*http.Response, error) {
				require.Equal(t, "http://proxy.example:8765", proxyURL)
				require.Equal(t, account.ID, accountID)
				require.Equal(t, "chatgpt.com", req.URL.Host)
				require.Equal(t, "Bearer tok", req.Header.Get("Authorization"))
				require.Equal(t, "acc-1", req.Header.Get("chatgpt-account-id"))
				require.Empty(t, req.Header.Get(responsesLiteHeaderKey))
				request, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				require.Equal(t, "gpt-6-astra", gjson.GetBytes(request, "model").String())
				require.Equal(t, "Reply with OK.", gjson.GetBytes(request, "input.0.content.0.text").String())
				sessions = append(sessions, req.Header.Get("session_id"))
				headers := make(http.Header)
				if len(sessions) == 1 {
					require.Empty(t, req.Header.Get(openAICodexTurnStateHeader))
					require.Empty(t, req.Header.Get("Cookie"))
					headers.Set(openAICodexTurnStateHeader, "first-ticket")
					headers.Add("Set-Cookie", "__cflb=route-1; Path=/")
					headers.Add("Set-Cookie", "__oailb=route-2; Path=/")
					headers.Add("Set-Cookie", "_cfuvid=unrelated; Path=/")
					return &http.Response{StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(strings.NewReader(completed))}, nil
				}
				require.Equal(t, "first-ticket", req.Header.Get(openAICodexTurnStateHeader))
				require.Equal(t, "__cflb=route-1; __oailb=route-2", req.Header.Get("Cookie"))
				headers.Set(openAICodexTurnStateHeader, tc.secondState)
				return &http.Response{StatusCode: tc.status, Header: headers, Body: io.NopCloser(strings.NewReader(tc.stream))}, nil
			}}
			svc := &AccountTestService{accountRepo: repo, openaiGatewayService: &OpenAIGatewayService{accountRepo: repo, httpUpstream: upstream}}

			result, err := svc.ProbeOpenAICodexState(context.Background(), account.ID, "test-alias")

			require.NoError(t, err)
			require.Len(t, sessions, 2)
			require.NotEmpty(t, sessions[0])
			require.NotEmpty(t, sessions[1])
			require.NotEqual(t, sessions[0], sessions[1])
			require.Equal(t, tc.want, result.Verdict)
			require.Equal(t, tc.failure, result.Failure)
			require.True(t, result.Minted)
			require.Equal(t, tc.want == OpenAICodexStateDegraded, result.NewTicket)
			require.False(t, result.FinishedAt.Before(result.StartedAt))
			if tc.failure == "" {
				require.Equal(t, "reported-model", result.ReportedModel)
			}
		})
	}
}

func TestProbeOpenAICodexStateHonorsAccountRestrictions(t *testing.T) {
	account := ticketTestAccount(41)
	account.Extra = map[string]any{"synthetic_ui_test": true}
	svc := &AccountTestService{accountRepo: &nativeProbeAccountRepo{known: account}}
	result, err := svc.ProbeOpenAICodexState(context.Background(), account.ID, "")
	require.NoError(t, err)
	require.Equal(t, OpenAICodexStateInconclusive, result.Verdict)
	require.Equal(t, "unsupported", result.Failure)

	svc.iqStateProbeAccounts.Store(account.ID, struct{}{})
	_, err = svc.ProbeOpenAICodexState(context.Background(), account.ID, "")
	require.ErrorIs(t, err, ErrOpenAICodexStateProbeBusy)
}

type iqProbeUpstream struct {
	HTTPUpstream
	dispatch func(*http.Request, string, int64) (*http.Response, error)
}

func (u *iqProbeUpstream) Do(req *http.Request, proxyURL string, accountID int64, _ int) (*http.Response, error) {
	return u.dispatch(req, proxyURL, accountID)
}
