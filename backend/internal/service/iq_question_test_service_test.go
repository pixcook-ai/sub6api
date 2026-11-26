//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/openai_compat"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestIQQuestionSendsPromptAndReasoning(t *testing.T) {
	account := &Account{
		ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "test-key"},
		Extra:       map[string]any{openai_compat.ExtraKeyResponsesSupported: true},
	}
	repo := &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}}}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(200,
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\ndata: {\"type\":\"response.completed\"}\n\n")}}
	service := &AccountTestService{accountRepo: repo, httpUpstream: upstream, cfg: &config.Config{}}
	c, response := newTestContext()
	const prompt = "  糖果逻辑测试\n只输出整数。\n"
	require.NoError(t, service.TestPelicanAccountConnection(c, account.ID, "gpt-5.4", prompt, "medium"))
	require.Len(t, upstream.requests, 1)
	body, err := upstream.requests[0].GetBody()
	require.NoError(t, err)
	defer func() { _ = body.Close() }()
	requestBody := make([]byte, upstream.requests[0].ContentLength)
	_, err = io.ReadFull(body, requestBody)
	require.NoError(t, err)
	require.Equal(t, prompt, gjson.GetBytes(requestBody, "input.0.content.0.text").String())
	require.Equal(t, "medium", gjson.GetBytes(requestBody, "reasoning.effort").String())
	require.Equal(t, "text/event-stream", response.Result().Header.Get("Content-Type"))
	require.Contains(t, response.Body.String(), `"text":"21"`)
	require.Contains(t, response.Body.String(), `"success":true`)
	require.Nil(t, repo.updatedExtra)
}

func TestIQQuestionScheduleKeepsIncompleteAnswerFailed(t *testing.T) {
	account := &Account{
		ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "test-key"},
		Extra:       map[string]any{openai_compat.ExtraKeyResponsesSupported: true},
	}
	repo := &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}}}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(200,
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\ndata: {\"type\":\"response.incomplete\"}\n\n")}}
	service := &AccountTestService{accountRepo: repo, httpUpstream: upstream, cfg: &config.Config{}}
	plan := &ScheduledTestPlan{
		AccountID: account.ID, ModelID: "gpt-5.4",
		PelicanConfig: &PelicanTestConfig{QuestionKind: "candy", Prompt: "糖果逻辑测试", ReasoningEffort: "medium", ParallelCount: 1},
	}
	result := service.runScheduledIQSample(context.Background(), plan)
	require.Equal(t, "failed", result.Status)
	require.Equal(t, "21", result.ResponseText)
	require.Equal(t, "gpt-5.4", result.PelicanConfig.ModelID)
	require.Empty(t, plan.PelicanConfig.ModelID)
	require.Equal(t, "medium", result.PelicanConfig.ReasoningEffort)
	require.NotEmpty(t, result.ErrorMessage)
}

func TestIQQuestionScheduleRecognizesLegacyCandyPrompt(t *testing.T) {
	account := &Account{
		ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "test-key"},
		Extra:       map[string]any{openai_compat.ExtraKeyResponsesSupported: true},
	}
	repo := &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}}}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(200,
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"29\"}\n\ndata: {\"type\":\"response.completed\"}\n\n")}}
	service := &AccountTestService{accountRepo: repo, httpUpstream: upstream, cfg: &config.Config{}}
	plan := &ScheduledTestPlan{
		AccountID: account.ID, ModelID: "gpt-5.4",
		PelicanConfig: &PelicanTestConfig{Prompt: CandyPrompt},
	}
	result := service.runScheduledIQSample(context.Background(), plan)
	require.Equal(t, "candy", result.PelicanConfig.QuestionKind)
	require.Equal(t, "29", result.ResponseText)
	require.Equal(t, "failed", result.Status)
	require.Equal(t, "answer_mismatch: expected 21", result.ErrorMessage)
}

func TestIQQuestionSendsClaudeVertexPayload(t *testing.T) {
	account := &Account{ID: 8, Platform: PlatformAnthropic, Type: AccountTypeServiceAccount, Credentials: map[string]any{
		"service_account_json": `{"type":"service_account","project_id":"vertex-proj","private_key_id":"kid","private_key":"-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----\n","client_email":"svc@vertex-proj.iam.gserviceaccount.com"}`,
		"location":             "global",
	}}
	key, err := parseVertexServiceAccountKey(account)
	require.NoError(t, err)
	cache := newClaudeTokenCacheStub()
	cache.tokens[vertexServiceAccountCacheKey(account, key)] = "fixture-vertex-token"
	repo := &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{account.ID: account}}}
	upstream := &httpUpstreamRecorder{resp: newJSONResponse(200,
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"<html></html>\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")}
	service := &AccountTestService{accountRepo: repo, httpUpstream: upstream, cfg: &config.Config{},
		claudeTokenProvider: NewClaudeTokenProvider(nil, cache, nil)}
	c, response := newTestContext()
	require.NoError(t, service.TestPelicanAccountConnection(c, account.ID, "claude-opus-5-5", "Draw the pelican", "high"))
	require.Contains(t, upstream.lastReq.URL.Path, "/publishers/anthropic/models/claude-opus-5-5:streamRawPredict")
	require.Equal(t, "Draw the pelican", gjson.GetBytes(upstream.lastBody, "messages.0.content.0.text").String())
	require.Equal(t, "high", gjson.GetBytes(upstream.lastBody, "output_config.effort").String())
	require.Contains(t, response.Body.String(), `"success":true`)
}
