package service

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type bpsResponseObserverContextKey struct{}

// bpsTestUsageText summarizes token usage and elapsed time for the admin test log.
func bpsTestUsageText(result *OpenAIForwardResult) string {
	u := result.Usage
	return fmt.Sprintf("Tokens: input %d, output %d, cache read %d, cache write %d · %d ms", u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens, result.Duration.Milliseconds())
}

// Use the production adapter, Redis state and transport for connectivity tests.
// A fresh scope ensures tests never attach to a real client's conversation.
// Regular tests stream the output piece by piece; compact stays unary.
func (s *AccountTestService) testOpenAIBPSAccountConnection(c *gin.Context, account *Account, model, prompt, mode string) error {
	if s.openaiGatewayService == nil {
		return s.sendErrorAndEnd(c, "BPS gateway is unavailable")
	}
	if model == "" {
		model = OpenAIBPSDefaultModels()[0]
	}
	if !account.IsModelSupported(model) {
		return s.sendErrorAndEnd(c, "Model is not allowed by this account")
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = "Reply with OK."
	}
	s.sendEvent(c, TestEvent{Type: "test_start", Model: model})
	path := "/v1/responses"
	if mode == "compact" {
		path += "/compact"
	}
	stream := mode != "compact"
	payload := map[string]any{"model": model, "input": prompt, "stream": stream, "prompt_cache_key": "bps-probe-" + uuid.NewString()}
	if options, ok := iqQuestionOptionsFromContext(c.Request.Context()); ok && options.reasoningEffort != "" {
		payload["reasoning_effort"] = options.reasoningEffort
	}
	body := []byte(bpsJSON(payload))
	rec := httptest.NewRecorder()
	writer := &bpsTestStreamWriter{ResponseRecorder: rec}
	var completed map[string]any
	if stream {
		writer.onDelta = func(delta string) {
			s.sendEvent(c, TestEvent{Type: "content", Text: delta})
		}
		writer.onCompleted = func(response map[string]any) { completed = response }
	}
	probe, _ := gin.CreateTestContext(writer)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
	defer cancel()
	ctx = context.WithValue(ctx, bpsResponseObserverContextKey{}, func(status int, actualModel, requestID string) {
		s.sendEvent(c, TestEvent{Type: "upstream_response", UpstreamStatus: status, UpstreamModel: actualModel, RequestID: requestID})
	})
	probe.Request = httptest.NewRequest("POST", path, strings.NewReader(string(body))).WithContext(ctx)
	if err := s.openaiGatewayService.PrepareOpenAIBPSRouting(probe, body); err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	result, err := s.openaiGatewayService.forwardOpenAIBPS(probe.Request.Context(), probe, account, body)
	if err != nil {
		event := TestEvent{Type: "error", Error: "BPS upstream request failed", Code: "bps_upstream_error", UpstreamStatus: probe.GetInt("bps_upstream_status"), UpstreamErrorCode: probe.GetString("bps_upstream_error_code"), UpstreamModel: probe.GetString("bps_upstream_model"), RequestID: probe.GetString("bps_upstream_request_id")}
		var typed *bpsError
		if errors.As(err, &typed) {
			event.Error, event.Code = typed.Message, typed.Code
		}
		s.sendEvent(c, event)
		return fmt.Errorf("%s", event.Error)
	}
	if mode == "compact" {
		var response map[string]any
		if bpsDecode(rec.Body.Bytes(), &response) != nil {
			return s.sendErrorAndEnd(c, "Invalid BPS response")
		}
		s.sendEvent(c, TestEvent{Type: "content", Text: "BPS conversation summary saved successfully."})
	} else if writer.deltas == 0 && completed != nil {
		// The upstream sent no text deltas; fall back to the final text.
		if text := bpsOutputText(completed); text != "" {
			s.sendEvent(c, TestEvent{Type: "content", Text: text})
		}
	}
	if result != nil {
		s.sendEvent(c, TestEvent{Type: "status", Text: bpsTestUsageText(result)})
	}
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}
