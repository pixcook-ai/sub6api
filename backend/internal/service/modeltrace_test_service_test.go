//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
	infraerrors "github.com/MACOS-DO/sub4api/internal/pkg/errors"
	"github.com/MACOS-DO/sub4api/internal/pkg/modeltrace"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type modelTraceTestRepo struct {
	AccountRepository
	account *Account
}

func (r modelTraceTestRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}

func TestModelTraceNativeProtocolsAndThreeQuestionBudget(t *testing.T) {
	text := strings.Repeat("17,83,247,", 100)
	encodedText, err := json.Marshal(text)
	require.NoError(t, err)
	responsesStream := `data: {"type":"response.output_text.delta","delta":` + string(encodedText) + "}\n\n" +
		"data: {\"type\":\"response.completed\"}\n\n"
	chatStream := `data: {"choices":[{"delta":{"content":` + string(encodedText) + "}}]}\n\n" +
		"data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	messagesStream := `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":` + string(encodedText) + "}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	for _, test := range []struct {
		name, platform, accountType, protocol, path, promptPath, system, model, stream string
		chatFallback                                                                   bool
	}{
		{"OpenAI Responses key", PlatformOpenAI, AccountTypeAPIKey, "responses", "/v1/responses", "input.0.content.0.text", "none", "gpt-6-astra", responsesStream, false},
		{"OpenAI Chat Completions key", PlatformOpenAI, AccountTypeAPIKey, "chat_completions", "/v1/chat/completions", "messages.0.content", "none", "gpt-6-astra", chatStream, true},
		{"OpenAI Responses OAuth", PlatformOpenAI, AccountTypeOAuth, "responses", "/backend-api/codex/responses", "input.0.content.0.text", "required_responses_instructions", "gpt-6-astra", responsesStream, false},
		{"Anthropic Messages key", PlatformAnthropic, AccountTypeAPIKey, "messages", "/v1/messages", "messages.0.content.0.text", "none", "claude-sonnet-4-6", messagesStream, false},
		{"Anthropic Messages OAuth", PlatformAnthropic, AccountTypeOAuth, "messages", "/v1/messages", "messages.0.content.0.text", "required_claude_code", "claude-sonnet-4-6", messagesStream, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := &Account{ID: 77, Platform: test.platform, Type: test.accountType, Credentials: map[string]any{
				"api_key": "test-key", "access_token": "test-token", "base_url": "https://example.com",
				"model_mapping": map[string]any{"public-model": test.model},
			}}
			requestedModel := "public-model"
			if account.IsAnthropic() && account.IsOAuth() {
				requestedModel = test.model
			}
			if test.chatFallback {
				account.Extra = map[string]any{"openai_responses_supported": false}
			}
			upstream := &queuedHTTPUpstream{}
			for range 3 {
				upstream.responses = append(upstream.responses, newJSONResponse(http.StatusOK, test.stream))
			}
			svc := &AccountTestService{accountRepo: modelTraceTestRepo{account: account}, httpUpstream: upstream, cfg: &config.Config{}}
			c, recorder := newTestContext()
			require.NoError(t, svc.TestModelTraceAccountConnection(c, 77, requestedModel, ""))
			require.Len(t, upstream.requests, 3)
			require.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
			require.Equal(t, []string{"modeltrace_start", "modeltrace_sample", "modeltrace_sample", "modeltrace_sample", "modeltrace_complete"}, modelTraceEventTypes(recorder.Body.String()))
			result := readModelTraceResult(t, recorder.Body.String())
			require.Equal(t, "complete", result.Status)
			require.Equal(t, 3, result.Analysis.UsedOutputs)
			require.Len(t, result.Samples, 3)
			prompts := map[string]bool{}
			for i, request := range upstream.requests {
				body, err := io.ReadAll(request.Body)
				require.NoError(t, err)
				sample := result.Samples[i]
				require.Equal(t, test.path, request.URL.Path)
				require.Equal(t, test.model, gjson.GetBytes(body, "model").String())
				require.Equal(t, test.model, sample.MappedModel)
				require.Equal(t, test.protocol, sample.Environment.Protocol)
				require.Equal(t, test.system, sample.Environment.SystemPrompt)
				require.Equal(t, sample.Challenge.Prompt, gjson.GetBytes(body, test.promptPath).String())
				require.Equal(t, test.system == "required_responses_instructions", gjson.GetBytes(body, "instructions").Exists())
				require.Equal(t, test.system == "required_claude_code", gjson.GetBytes(body, "system").Exists())
				require.False(t, gjson.GetBytes(body, "temperature").Exists())
				require.False(t, gjson.GetBytes(body, "tools").Exists())
				prompts[sample.Challenge.Prompt] = true
			}
			require.Len(t, prompts, 3)
		})
	}
}

func TestModelTraceRejectsIncompleteSamplesWithoutRetryOrAccountMutation(t *testing.T) {
	text := strings.Repeat("17,83,247,", 100)
	content := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"" + text + "\"}\n\n"
	upstream := &queuedHTTPUpstream{responses: []*http.Response{
		newJSONResponse(http.StatusUnauthorized, `{"error":"sensitive upstream detail"}`),
		newJSONResponse(http.StatusOK, content+"data: {\"type\":\"response.done\",\"response\":{\"status\":\"incomplete\"}}\n\n"),
		newJSONResponse(http.StatusOK, content+"data: {\"type\":\"response.completed\"}\n\n"),
	}}
	account := &Account{ID: 77, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "test-token"}}
	svc := &AccountTestService{accountRepo: modelTraceTestRepo{account: account}, httpUpstream: upstream, cfg: &config.Config{}}
	c, recorder := newTestContext()
	require.NoError(t, svc.TestModelTraceAccountConnection(c, 77, "gpt-6-astra", ""))
	require.Len(t, upstream.requests, 3)
	require.NotContains(t, recorder.Body.String(), "sensitive upstream detail")
	result := readModelTraceResult(t, recorder.Body.String())
	require.Equal(t, "partial", result.Status)
	require.Equal(t, 1, result.Analysis.UsedOutputs)
	require.Equal(t, "generation_failed", result.Samples[0].Error)
	require.Equal(t, "generation_failed", result.Samples[1].Error)
	require.Equal(t, 300, result.Samples[1].ParsedNumbers)
	require.True(t, result.Samples[2].Accepted)
}

func TestModelTraceValidationAndCancellation(t *testing.T) {
	upstream := &queuedHTTPUpstream{}
	account := &Account{ID: 77, Platform: PlatformOpenAIBPS, Type: AccountTypeOAuth}
	svc := &AccountTestService{accountRepo: modelTraceTestRepo{account: account}, httpUpstream: upstream}
	c, recorder := newTestContext()
	err := svc.TestModelTraceAccountConnection(c, 77, "gpt-6-astra", "")
	require.Equal(t, "MODELTRACE_UNSUPPORTED", infraerrors.Reason(err))
	require.Empty(t, recorder.Body.String())

	account.Platform = PlatformOpenAI
	svc.modelTraceAccounts.Store(int64(77), struct{}{})
	c, recorder = newTestContext()
	err = svc.TestModelTraceAccountConnection(c, 77, "gpt-6-astra", "")
	require.Equal(t, "MODELTRACE_BUSY", infraerrors.Reason(err))
	require.Empty(t, recorder.Body.String())
	require.Empty(t, upstream.requests)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	err = runModelTraceSamples(ctx, make([]modeltrace.Challenge, 3), func(sampleCtx context.Context, _ modeltrace.Challenge) ModelTraceSample {
		calls++
		_, hasDeadline := sampleCtx.Deadline()
		require.True(t, hasDeadline)
		cancel()
		return ModelTraceSample{}
	}, func(ModelTraceSample) { t.Fatal("cancelled sample emitted a result") })
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, calls)
}

func modelTraceEventTypes(stream string) []string {
	var types []string
	for _, line := range strings.Split(stream, "\n") {
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			types = append(types, gjson.Get(data, "type").String())
		}
	}
	return types
}

func readModelTraceResult(t *testing.T, stream string) ModelTraceResult {
	t.Helper()
	for _, line := range strings.Split(stream, "\n") {
		if data, ok := strings.CutPrefix(line, "data:"); ok && gjson.Get(data, "type").String() == "modeltrace_complete" {
			var result ModelTraceResult
			require.NoError(t, json.Unmarshal([]byte(gjson.Get(data, "data").Raw), &result))
			return result
		}
	}
	t.Fatal("missing modeltrace_complete event")
	return ModelTraceResult{}
}
