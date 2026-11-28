package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIBPSInBandFailureThroughForward(t *testing.T) {
	for _, tc := range []struct {
		code        string
		status      int
		clientCode  string
		model, auth bool
	}{
		{"rate_limit_exceeded", 429, "bps_rate_limited", false, false},
		{"model_not_allowed", 403, "bps_model_not_allowed", true, false},
		{"basispoints_model_access_changed", 403, "bps_model_not_allowed", true, false},
		{"token_expired", 401, "bps_invalid_credentials", false, true},
		{"token_revoked", 401, "bps_invalid_credentials", false, true},
		{"invalid_api_key", 401, "bps_invalid_credentials", false, true},
		{"unknown_authentication_error", 502, "bps_response_failed", false, false},
	} {
		for _, started := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream_%t", tc.code, started), func(t *testing.T) {
				s, a := bpsFixture()
				repo := &bpsFailureRepo{}
				s.accountRepo = repo
				c, rec := bpsContext(1, "/responses")
				response := map[string]any{"id": "resp_failed_fixture", "status": "failed", "error": map[string]any{"code": tc.code, "message": "sensitive test-secret authentication_error"}, "usage": map[string]any{"input_tokens": 7, "output_tokens": 2}}
				up := &bpsHTTPStub{contentType: "application/json", body: bpsJSON(response)}
				if started {
					up.contentType = "text/event-stream"
					up.body = bpsEvent("response.created", map[string]any{"response": map[string]any{"id": "resp_failed_fixture", "status": "in_progress", "output": []any{}}}) + bpsEvent("response.failed", map[string]any{"response": response})
				}
				s.httpUpstream = up
				result, err := s.Forward(c.Request.Context(), c, a, bpsRequestBody("hello", started))
				require.Error(t, err)
				var failover *UpstreamFailoverError
				require.False(t, errors.As(err, &failover), "accepted generations must not be replayed")
				require.NotNil(t, result)
				require.Equal(t, 7, result.Usage.InputTokens)
				require.Equal(t, 2, result.Usage.OutputTokens)
				require.Equal(t, "response.failed", result.UpstreamTerminalEvent)
				require.False(t, result.SucceededForScheduling())
				require.Equal(t, 200, c.GetInt("bps_upstream_status"), "actual upstream HTTP stays independent from logical error")
				require.Equal(t, 1, up.requests)
				require.NotContains(t, rec.Body.String(), "test-secret")
				require.NotContains(t, rec.Body.String(), "sensitive")
				require.Contains(t, rec.Body.String(), tc.clientCode)
				require.Equal(t, tc.model, len(repo.limitedModels) > 0)
				require.Equal(t, tc.auth, len(repo.authErrors) > 0)
				if tc.status == http.StatusTooManyRequests {
					require.InDelta(t, 60, time.Until(repo.cooldown).Seconds(), 2)
					require.Contains(t, rec.Body.String(), `"retry_after_seconds":60`)
				} else {
					require.True(t, repo.cooldown.IsZero())
				}
				if tc.model {
					require.Equal(t, []string{"gpt-6-astra"}, repo.limitedModels)
				}
				if started {
					require.Equal(t, 200, rec.Code)
					require.Contains(t, rec.Body.String(), "event: response.failed")
					require.Contains(t, rec.Body.String(), `"input_tokens":7`)
				} else {
					require.Equal(t, tc.status, rec.Code)
				}
			})
		}
	}
}

type bpsNoEOFFailureBody struct {
	io.Reader
	readAfterTerminal bool
}

func (b *bpsNoEOFFailureBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		b.readAfterTerminal = true
		return 0, fmt.Errorf("must stop at cancellation terminal")
	}
	return n, err
}
func (b *bpsNoEOFFailureBody) Close() error { return nil }

type bpsTerminalHTTP struct {
	HTTPUpstream
	body     io.ReadCloser
	header   http.Header
	cancel   context.CancelFunc
	requests int
}

func (u *bpsTerminalHTTP) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.requests++
	if u.cancel != nil {
		u.cancel()
	}
	header := make(http.Header)
	for key, values := range u.header {
		for _, value := range values {
			header.Add(key, value)
		}
	}
	header.Set("Content-Type", "text/event-stream")
	return &http.Response{StatusCode: 200, Header: header, Body: u.body}, nil
}

func TestOpenAIBPSCancelledTerminalThroughForward(t *testing.T) {
	s, a := bpsFixture()
	c, rec := bpsContext(1, "/responses")
	body := &bpsNoEOFFailureBody{Reader: strings.NewReader(bpsEvent("response.created", map[string]any{"response": map[string]any{"id": "resp_cancel_fixture", "status": "in_progress", "output": []any{}}}) + bpsEvent("response.cancelled", map[string]any{"response": map[string]any{"id": "resp_cancel_fixture", "status": "cancelled", "usage": map[string]any{"input_tokens": 9, "output_tokens": 3}}}))}
	up := &bpsTerminalHTTP{body: body}
	s.httpUpstream = up
	result, err := s.Forward(c.Request.Context(), c, a, bpsRequestBody("hello", true))
	require.Error(t, err)
	require.NotNil(t, result)
	require.Equal(t, "response.cancelled", result.UpstreamTerminalEvent)
	require.False(t, result.SucceededForScheduling())
	require.Equal(t, 9, result.Usage.InputTokens)
	require.Equal(t, 3, result.Usage.OutputTokens)
	require.False(t, body.readAfterTerminal)
	require.Equal(t, 1, up.requests)
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), "bps_response_cancelled")
	require.NotContains(t, rec.Body.String(), "event: response.completed")
	require.Contains(t, rec.Body.String(), `"input_tokens":9`)
}

func TestOpenAIBPSTopLevelErrorThroughForward(t *testing.T) {
	s, a := bpsFixture()
	c, rec := bpsContext(1, "/responses")
	up := &bpsHTTPStub{contentType: "text/event-stream", body: bpsEvent("error", map[string]any{
		"error": map[string]any{"code": "rate_limit_exceeded", "message": "sensitive test-secret"},
		"usage": map[string]any{"input_tokens": 4, "output_tokens": 1},
	})}
	s.httpUpstream = up
	result, err := s.Forward(c.Request.Context(), c, a, bpsRequestBody("hello", false))
	require.Error(t, err)
	require.Equal(t, 429, rec.Code)
	require.Equal(t, "bps_rate_limited", gjson.Get(rec.Body.String(), "error.code").String())
	require.Equal(t, 4, result.Usage.InputTokens)
	require.Equal(t, 1, result.Usage.OutputTokens)
	require.Equal(t, "error", result.UpstreamTerminalEvent)
	require.False(t, result.SucceededForScheduling())
	require.NotContains(t, rec.Body.String(), "test-secret")
	require.NotContains(t, rec.Body.String(), "sensitive")
	require.Equal(t, 1, up.requests)
}

func TestOpenAIBPSClientCancellationKeepsCause(t *testing.T) {
	s, a := bpsFixture()
	c, rec := bpsContext(1, "/responses")
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	repo := &bpsFailureRepo{}
	s.accountRepo = repo
	s.httpUpstream = &bpsTerminalHTTP{cancel: cancel, body: io.NopCloser(strings.NewReader(bpsEvent("response.failed", map[string]any{"response": map[string]any{"status": "failed", "error": map[string]any{"code": "token_expired"}}})))}
	_, err := s.Forward(ctx, c, a, bpsRequestBody("hello", false))
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, repo.authErrors)
	require.Empty(t, repo.limitedModels)
	require.Empty(t, rec.Body.String())
	require.True(t, repo.cooldown.Before(time.Now()))
}

func TestOpenAIBPSTerminalDiagnosticsAndRetryHints(t *testing.T) {
	for _, tc := range []struct {
		name          string
		header        http.Header
		responseError map[string]any
		retrySeconds  int
	}{
		{"header takes precedence", http.Header{"Retry-After": {"23"}}, map[string]any{"code": "rate_limit_exceeded", "resets_in_seconds": 40}, 23},
		{"provider reset field", nil, map[string]any{"type": "rate_limit_error", "resets_in_seconds": "31"}, 31},
		{"round milliseconds", nil, map[string]any{"code": "rate_limit_exceeded", "retry_after_ms": 1250}, 2},
		{"unknown error", nil, map[string]any{"code": "generation_stopped", "type": "engine_error"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, account := bpsFixture()
			repo := &bpsFailureRepo{}
			s.accountRepo = repo
			headers := http.Header{"x-request-id": {"req_provider"}}
			for key, values := range tc.header {
				headers[key] = values
			}
			tc.responseError["message"] = "private upstream payload test-secret"
			stream := bpsEvent("response.created", map[string]any{"response": map[string]any{"id": "resp_same", "status": "in_progress"}}) +
				bpsEvent("response.failed", map[string]any{"response": map[string]any{"id": "resp_same", "status": "failed", "error": tc.responseError}})
			upstream := &bpsTerminalHTTP{body: io.NopCloser(strings.NewReader(stream)), header: headers}
			s.httpUpstream = upstream
			c, recorder := bpsContext(1, "/responses")
			_, err := s.Forward(c.Request.Context(), c, account, bpsRequestBody("hello", true))
			require.Error(t, err)
			require.Equal(t, 1, upstream.requests)
			require.True(t, IsResponseCommitted(c))
			require.Equal(t, 200, recorder.Code)
			require.Empty(t, recorder.Result().Header.Get("Retry-After"))
			require.Equal(t, 1, strings.Count(recorder.Body.String(), "event: response.failed"))
			require.NotContains(t, recorder.Body.String(), "private upstream payload")
			value, exists := c.Get(OpsUpstreamErrorsKey)
			require.True(t, exists)
			events, ok := value.([]*OpsUpstreamErrorEvent)
			require.True(t, ok)
			require.Len(t, events, 1)
			require.Equal(t, 200, events[0].UpstreamStatusCode)
			require.Equal(t, "response.failed", events[0].UpstreamEvent)
			require.Equal(t, "req_provider", events[0].UpstreamRequestID)
			require.Equal(t, stringValue(tc.responseError["code"]), events[0].Detail)
			require.Equal(t, stringValue(tc.responseError["type"]), events[0].UpstreamErrorType)
			require.Equal(t, tc.retrySeconds, events[0].RetryAfterSeconds)
			if tc.retrySeconds > 0 {
				require.Contains(t, recorder.Body.String(), fmt.Sprintf("%q:%d", "retry_after_seconds", tc.retrySeconds))
			}
		})
	}
}

type bpsBoundCooldownRepo struct {
	AccountRepository
	account *Account
}

func (r bpsBoundCooldownRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}

func TestOpenAIBPSBoundAccountCooldownResponse(t *testing.T) {
	s, account := bpsFixture()
	reset := time.Now().Add(40 * time.Second)
	account.RateLimitResetAt = &reset
	s.accountRepo = bpsBoundCooldownRepo{account: account}
	for _, tc := range []struct {
		name, identity string
		status         int
	}{
		{"same bound account", "workspace-1", 429},
		{"credential identity changed", "older-workspace", 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, recorder := bpsContext(1, "/responses")
			binding := &bpsSessionBinding{AccountID: account.ID, Identity: tc.identity}
			c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), bpsRoutingContextKey{}, bpsRoutingContext{Binding: binding}))
			s.WriteOpenAIBPSBoundAccountError(c)
			require.Equal(t, tc.status, recorder.Code)
			if tc.status == 429 {
				require.Equal(t, "40", recorder.Result().Header.Get("Retry-After"))
				require.Equal(t, int64(40), gjson.Get(recorder.Body.String(), "error.retry_after_seconds").Int())
			}
		})
	}
}
