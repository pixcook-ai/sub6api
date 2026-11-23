//go:build unit

package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/pkg/openai_compat"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type managedPreparationErrorUpstream struct {
	service.HTTPUpstream
	err          error
	beforeReturn func()
	calls        int
	generations  int
	firstStatus  int
	accountIDs   []int64
}

func (u *managedPreparationErrorUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.calls++
	u.accountIDs = append(u.accountIDs, accountID)
	if u.beforeReturn != nil {
		u.beforeReturn()
	}
	if u.calls == 1 {
		if u.firstStatus != 0 {
			return &http.Response{StatusCode: u.firstStatus, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"real upstream failure"}}`))}, nil
		}
		return nil, u.err
	}
	u.generations++
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_ok","object":"response","status":"completed","model":"gpt-5.1","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`))}, nil
}

type managedPreparationHealthSettings struct{ service.SettingRepository }

func (*managedPreparationHealthSettings) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
}
func (*managedPreparationHealthSettings) GetValue(context.Context, string) (string, error) {
	return `{"enabled":true,"window_minutes":1,"failure_threshold":3,"cooldown_minutes":5}`, nil
}

type managedPreparationHealthCache struct{ failures int }

func (c *managedPreparationHealthCache) RecordOpenAIAPIKeyHealthFailure(context.Context, int64, int, int) (int64, bool, error) {
	c.failures++
	return 1, false, nil
}

func newManagedPreparationHandler(t *testing.T, upstream service.HTTPUpstream) (*OpenAIGatewayHandler, *managedPreparationHealthCache) {
	t.Helper()
	h := newOpenAIResponsesFailoverTestHandler(t, upstream)
	accounts := []service.Account{}
	for _, id := range []int64{1, 2} {
		accounts = append(accounts, service.Account{ID: id, Name: fmt.Sprint("preparation-", id), Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: int(id), Credentials: map[string]any{"api_key": "test-key", "base_url": "https://gateway.example", "pool_mode": true, "pool_mode_retry_status_codes": []any{}}, Extra: map[string]any{openai_compat.ExtraKeyResponsesMode: string(openai_compat.ResponsesSupportModeForceResponses)}})
	}
	repo := openAIImagesFailoverAccountRepo{accounts: accounts}
	health := &managedPreparationHealthCache{}
	rate := service.NewRateLimitService(repo, nil, h.cfg, nil, nil)
	rate.SetSettingService(service.NewSettingService(&managedPreparationHealthSettings{}, h.cfg))
	rate.SetOpenAIAPIKeyHealthCache(health)
	h.gatewayService = service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, h.cfg, nil, nil, nil, rate, h.billingCacheService, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
	return h, health
}

func TestManagedFilePreparationResponsesRevokedBeforeGeneration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &managedPreparationErrorUpstream{err: fmt.Errorf("private-token private-url: %w", &service.ManagedFilePreparationError{Err: service.ErrBPSRequestAuthorizationRevoked})}
	h := newOpenAIResponsesFailoverTestHandler(t, upstream)
	c, recorder := newOpenAIResponsesFailoverTestContext(t, context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.1","stream":false,"input":"hello"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	upstream.beforeReturn = func() {
		c.Request = c.Request.WithContext(service.WithManagedFilePlan(c.Request.Context(), &service.ManagedFilePlan{}))
	}
	h.Responses(c)
	require.Equal(t, http.StatusForbidden, recorder.Code, recorder.Body.String())
	require.Equal(t, "permission_denied", gjson.GetBytes(recorder.Body.Bytes(), "error.code").String())
	require.Equal(t, 1, upstream.calls, "local preparation must never replay inference")
	require.NotContains(t, recorder.Body.String(), "private-token")
	require.NotContains(t, recorder.Body.String(), "private-url")
}

func TestManagedFilePreparationResponsesClassificationAndHeartbeat(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cause  error
		status int
		code   string
	}{
		{"revoked", service.ErrBPSRequestAuthorizationRevoked, 403, "permission_denied"},
		{"file revoked", service.ErrFileAuthorizationRevoked, 403, "permission_denied"},
		{"authorization unavailable", service.ErrBPSRequestAuthorizationUnavailable, 503, "managed_files_unavailable"},
		{"not found", service.ErrFileNotFound, 404, "file_not_found"},
		{"busy", service.ErrManagedFileBusy, 409, "managed_file_busy"},
		{"binding conflict", service.ErrManagedFileBindingConflict, 409, "managed_file_binding_conflict"},
		{"uncertain", service.ErrManagedFileUncertain, 409, "managed_file_uncertain"},
		{"lease conflict", service.ErrFileLeaseConflict, 409, "file_lease_conflict"},
		{"recovery budget", service.ErrManagedFileRecoveryBudget, 429, "managed_file_recovery_budget_exceeded"},
		{"unsupported", service.ErrManagedFileUnsupportedInput, 400, "managed_files_unsupported"},
		{"replay", service.ErrManagedFileGenerationReplay, 409, "managed_file_generation_replay"},
		{"not ready", service.ErrManagedFileNotReady, 503, "managed_files_unavailable"},
		{"metadata failure", errors.New("private-token private-url"), 503, "managed_files_unavailable"},
		{"nested upstream marker", &service.UpstreamFailoverError{StatusCode: 502, ResponseBody: []byte("private-token private-url")}, 503, "managed_files_unavailable"},
	} {
		for _, committed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/heartbeat=%t", tc.name, committed), func(t *testing.T) {
				upstream := &managedPreparationErrorUpstream{err: fmt.Errorf("private-token private-url: %w", &service.ManagedFilePreparationError{Err: fmt.Errorf("private-token private-url: %w", tc.cause)})}
				h, health := newManagedPreparationHandler(t, upstream)
				c, recorder := newOpenAIResponsesFailoverTestContext(t, nil)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"gpt-5.1","input":"hello","stream":%t}`, committed)))
				c.Request.Header.Set("Content-Type", "application/json")
				upstream.beforeReturn = func() {
					c.Request = c.Request.WithContext(service.WithManagedFilePlan(c.Request.Context(), &service.ManagedFilePlan{}))
					if committed {
						c.Header("Content-Type", "text/event-stream")
						_, _ = io.WriteString(c.Writer, ": keepalive\n\n")
						c.Writer.Flush()
					}
				}
				h.Responses(c)
				require.Equal(t, 1, upstream.calls, "local preparation must never replay")
				require.Zero(t, upstream.generations, "local preparation must not generate")
				require.Equal(t, []int64{1}, upstream.accountIDs, "local preparation must not switch accounts")
				require.Zero(t, health.failures, "local preparation must not count against account health")
				require.NotContains(t, recorder.Body.String(), "private-token")
				require.NotContains(t, recorder.Body.String(), "private-url")
				if committed {
					require.Equal(t, 200, recorder.Code)
					require.Equal(t, 1, strings.Count(recorder.Body.String(), "event: response.failed\n"), recorder.Body.String())
					require.NotContains(t, recorder.Body.String(), "event: error")
					data := recorder.Body.String()[strings.Index(recorder.Body.String(), "data: ")+6:]
					require.Equal(t, tc.code, gjson.Get(data, "response.error.code").String())
				} else {
					require.Equal(t, tc.status, recorder.Code, recorder.Body.String())
					require.Equal(t, tc.code, gjson.GetBytes(recorder.Body.Bytes(), "error.code").String())
					require.NotContains(t, recorder.Body.String(), "event:")
				}
			})
		}
	}
}

func TestManagedFilePreparationResponsesUnmarkedFailureKeepsUpstreamPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{"unmarked authorization cause", service.ErrBPSRequestAuthorizationRevoked, 0},
		{"actual upstream failure", nil, 520},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &managedPreparationErrorUpstream{err: tc.err, firstStatus: tc.status}
			h, health := newManagedPreparationHandler(t, upstream)
			c, recorder := newOpenAIResponsesFailoverTestContext(t, nil)
			h.Responses(c)
			require.Equal(t, 200, recorder.Code, recorder.Body.String())
			require.Equal(t, 2, upstream.calls)
			require.Equal(t, 1, upstream.generations)
			require.Equal(t, []int64{1, 2}, upstream.accountIDs, "unmarked upstream errors retain account failover")
			require.Positive(t, health.failures, "real upstream failures must retain account health handling")
		})
	}
}

func TestManagedFilePreparationResponsesPreservesCommittedTerminal(t *testing.T) {
	upstream := &managedPreparationErrorUpstream{err: &service.ManagedFilePreparationError{Err: service.ErrBPSRequestAuthorizationRevoked}}
	h, health := newManagedPreparationHandler(t, upstream)
	c, recorder := newOpenAIResponsesFailoverTestContext(t, nil)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.1","input":"hello","stream":true}`))
	c.Request.Header.Set("Content-Type", "application/json")
	var committed string
	upstream.beforeReturn = func() {
		c.Header("Content-Type", "text/event-stream")
		require.True(t, writeResponsesFailedSSE(c, "permission_denied", "permission_denied", "Request authorization is no longer valid"))
		service.MarkResponseCommitted(c)
		committed = recorder.Body.String()
	}
	h.Responses(c)
	require.Equal(t, 1, upstream.calls)
	require.Zero(t, upstream.generations)
	require.Zero(t, health.failures)
	require.Equal(t, committed, recorder.Body.String(), "an existing safe terminal must not be appended or replaced")
	require.Equal(t, 1, strings.Count(recorder.Body.String(), "event: response.failed\n"))
}
