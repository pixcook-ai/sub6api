//go:build unit

package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/model"
	"github.com/MACOS-DO/sub4api/internal/pkg/ctxkey"
	"github.com/MACOS-DO/sub4api/internal/pkg/tlsfingerprint"
	middleware "github.com/MACOS-DO/sub4api/internal/server/middleware"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type messagesContractUpstream func(req *http.Request, accountID int64) *http.Response

func (f messagesContractUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	return f(req, accountID), nil
}

func (f messagesContractUpstream) DoWithTLS(req *http.Request, _ string, accountID int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return f(req, accountID), nil
}

type messagesErrorRuleRepo struct {
	service.ErrorPassthroughRepository
	rule *model.ErrorPassthroughRule
}

func (r messagesErrorRuleRepo) List(context.Context) ([]*model.ErrorPassthroughRule, error) {
	return []*model.ErrorPassthroughRule{r.rule}, nil
}

func newAnthropicMessagesErrorGatewayFixture(t *testing.T, forceRemove []string, accountCount int, upstream messagesContractUpstream) (*GatewayHandler, func(*gin.Context)) {
	t.Helper()
	group := &service.Group{
		ID:       2001,
		Hydrated: true,
		Platform: service.PlatformAnthropic,
		Status:   service.StatusActive,
	}
	accounts := make([]*service.Account, 0, accountCount)
	for i := 0; i < accountCount; i++ {
		accountID := int64(1001 + i)
		accounts = append(accounts, &service.Account{
			ID:          accountID,
			Name:        fmt.Sprintf("claude-contract-%d", accountID),
			Type:        service.AccountTypeAPIKey,
			Platform:    service.PlatformAnthropic,
			Concurrency: 1,
			Priority:    i + 1,
			Status:      service.StatusActive,
			Schedulable: true,
			Credentials: map[string]any{
				"api_key":                      "synthetic-upstream-key",
				"pool_mode":                    true,
				"pool_mode_retry_status_codes": []any{},
			},
			Extra:         map[string]any{"anthropic_passthrough": true},
			AccountGroups: []service.AccountGroup{{AccountID: accountID, GroupID: group.ID}},
		})
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Security.ResponseHeaders = config.ResponseHeaderConfig{Enabled: true, ForceRemove: forceRemove}
	h, cleanup := newTestGatewayHandlerWithUpstream(t, group, accounts, cfg, upstream)
	h.settingService = service.NewSettingService(&settingHandlerPublicRepoStub{}, cfg)
	t.Cleanup(cleanup)
	return h, func(c *gin.Context) {
		apiKey := &service.APIKey{
			ID:      3001,
			UserID:  4001,
			GroupID: &group.ID,
			Status:  service.StatusActive,
			User:    &service.User{ID: 4001, Concurrency: 10, Balance: 100},
			Group:   group,
		}
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.Group, group))
		c.Set(string(middleware.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 10})
		h.Messages(c)
	}
}

func TestAnthropicMessagesGatewayTerminalError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, failure := range []struct {
		name        string
		status      int
		kind        string
		clientCode  int
		forceRemove []string
		withRule    bool
	}{
		{name: "rate_limit", status: 429, kind: "rate_limit_error", clientCode: 429},
		{name: "native_overload", status: 529, kind: "overloaded_error", clientCode: 529},
		{name: "other_overload", status: 529, kind: "provider_specific", clientCode: 503},
		{name: "filtered", status: 429, kind: "rate_limit_error", clientCode: 429, forceRemove: []string{"request-id", "x-should-retry", "retry-after"}},
		{name: "explicit_rule", status: 529, kind: "overloaded_error", clientCode: 503, withRule: true},
	} {
		t.Run(failure.name, func(t *testing.T) {
			var accounts []int64
			h, serve := newAnthropicMessagesErrorGatewayFixture(t, failure.forceRemove, 2, func(_ *http.Request, accountID int64) *http.Response {
				accounts = append(accounts, accountID)
				headers := make(http.Header)
				headers.Set("request-id", fmt.Sprintf("req_account_%d", accountID))
				headers.Set("x-should-retry", "false")
				headers.Set("retry-after", "13")
				return &http.Response{
					StatusCode: failure.status,
					Header:     headers,
					Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"` + failure.kind + `","message":"synthetic upstream error"}}`)),
				}
			})
			if failure.withRule {
				message := "Mapped by the configured rule"
				h.errorPassthroughService = service.NewErrorPassthroughService(messagesErrorRuleRepo{rule: &model.ErrorPassthroughRule{
					Enabled:       true,
					ErrorCodes:    []int{529},
					Platforms:     []string{service.PlatformAnthropic},
					MatchMode:     model.MatchModeAny,
					ResponseCode:  &failure.clientCode,
					CustomMessage: &message,
				}}, nil)
			}
			router := gin.New()
			router.POST("/v1/messages", serve)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5-20250929","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(recorder, request)

			require.Len(t, accounts, 2)
			require.NotEqual(t, accounts[0], accounts[1], "the gateway still switches accounts even if the client should not retry")
			assert.Equal(t, failure.clientCode, recorder.Code)
			wantKind := failure.kind
			if failure.status == 529 {
				wantKind = "overloaded_error"
			}
			if failure.withRule {
				wantKind = "upstream_error"
				assert.Equal(t, "Mapped by the configured rule", gjson.Get(recorder.Body.String(), "error.message").String())
			}
			assert.Equal(t, wantKind, gjson.Get(recorder.Body.String(), "error.type").String())
			wantID, wantRetry, wantRetryAfter := fmt.Sprintf("req_account_%d", accounts[1]), "false", "13"
			if len(failure.forceRemove) > 0 {
				wantID, wantRetry, wantRetryAfter = "", "", ""
			}
			headers := recorder.Result().Header
			assert.Equal(t, wantID, headers.Get("request-id"))
			assert.Equal(t, wantRetry, headers.Get("x-should-retry"))
			assert.Equal(t, wantRetryAfter, headers.Get("retry-after"))
			assert.Equal(t, wantID, gjson.Get(recorder.Body.String(), "request_id").String())
		})
	}
}
