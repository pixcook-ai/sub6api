package service

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type bpsResponseObserverContextKey struct{}

const AccountTestModeBPSTools = "bps_tools"
const bpsToolProbeName = "sub2api_probe_echo"
const bpsToolProbeMaxConcurrent = 3

var bpsTestDiagnosticID = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,160}$`)

func bpsTestIdentifier(value, token string) string {
	if !bpsTestDiagnosticID.MatchString(value) || (token != "" && strings.Contains(value, token)) {
		return ""
	}
	return value
}

func (s *AccountTestService) withBPSTestResponseObserver(ctx context.Context, c *gin.Context, account *Account, diagnostic *TestEvent) context.Context {
	token := account.GetCredential("access_token")
	return context.WithValue(ctx, bpsResponseObserverContextKey{}, func(status int, model, requestID string) {
		event := TestEvent{Type: "upstream_response", UpstreamStatus: status, UpstreamModel: bpsTestIdentifier(model, token), RequestID: bpsTestIdentifier(requestID, token)}
		if diagnostic != nil {
			*diagnostic = event
		}
		s.sendEvent(c, event)
	})
}

// bpsTestUsageText summarizes token usage and elapsed time for the admin test log.
func bpsTestUsageText(result *OpenAIForwardResult) string {
	u := result.Usage
	return fmt.Sprintf("Tokens: input %d, output %d, cache read %d, cache write %d · %d ms", u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens, result.Duration.Milliseconds())
}

// Use the production adapter, Redis state and transport for connectivity tests.
// A fresh scope ensures tests never attach to a real client's conversation.
// Regular tests stream the output piece by piece; compact stays unary.
func (s *AccountTestService) testOpenAIBPSAccountConnection(c *gin.Context, account *Account, model, prompt, mode string) error {
	_, releaseProxy, proxyErr := s.beginAccountTestRequestProxy(c, account)
	if proxyErr != nil {
		return s.sendErrorAndEnd(c, proxyErr.Error())
	}
	defer releaseProxy()
	if s.openaiGatewayService == nil {
		return s.sendErrorAndEnd(c, "BPS gateway is unavailable")
	}
	if model == "" {
		model = OpenAIBPSDefaultModels()[0]
	}
	if !account.IsModelSupported(model) {
		return s.sendErrorAndEnd(c, "Model is not allowed by this account")
	}
	if mode == AccountTestModeBPSTools {
		return s.testBPSToolRoundtrip(c, account, model)
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
	ctx = s.withBPSTestResponseObserver(ctx, c, account, nil)
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

// Adapted from ranxi2001/sub2api's BPS account probe (LGPL-3.0).
// The only local tool returns a nonce; no supplied code or tool is executed.
func (s *AccountTestService) testBPSToolRoundtrip(c *gin.Context, account *Account, model string) error {
	if s.openaiGatewayService.httpUpstream == nil {
		return s.sendErrorAndEnd(c, "BPS gateway is unavailable")
	}
	release, ok := s.beginBPSToolProbe(account.ID)
	if !ok {
		return s.sendErrorAndEnd(c, "BPS tool probe is already running for this account or at capacity")
	}
	defer release()
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
	defer cancel()
	nonce, session := uuid.NewString(), "bps-tool-probe-"+uuid.NewString()
	tools := []any{map[string]any{
		"type": "function", "name": bpsToolProbeName,
		"description": "Return the supplied nonce without any external side effect.",
		"parameters": map[string]any{
			"type": "object", "properties": map[string]any{"nonce": map[string]any{"type": "string", "enum": []any{nonce}}},
			"required": []any{"nonce"}, "additionalProperties": false,
		},
	}}
	s.sendEvent(c, TestEvent{Type: "test_start", Model: model})
	input := []any{bpsMessage("user", "Reply with exactly this nonce and nothing else: "+nonce)}
	text, err := s.runBPSToolProbeStep(ctx, c, account, model, session, input, nil, false)
	textOutput, outputOK := text.response["output"].([]any)
	if err != nil || !outputOK || !bpsToolProbeExactText(text.response, nonce) {
		return s.failBPSToolProbe(c, text, "bps_tool_probe_text_failed", "BPS text probe did not return the expected completed response")
	}
	s.sendEvent(c, TestEvent{Type: "status", Text: "BPS text response verified"})
	s.sendEvent(c, TestEvent{Type: "content", Text: nonce})
	input = append(input, textOutput...)
	input = append(input, bpsMessage("user", "Call "+bpsToolProbeName+" exactly once with nonce "+nonce+". After its result, reply with exactly that nonce and nothing else."))
	called, err := s.runBPSToolProbeStep(ctx, c, account, model, session, input, tools, true)
	if err != nil {
		return s.failBPSToolProbe(c, called, "bps_tool_probe_call_failed", "BPS tool call did not complete")
	}
	callID, ok := bpsToolProbeExactCall(called.response, nonce)
	callOutput, outputOK := called.response["output"].([]any)
	if !ok || !outputOK {
		return s.failBPSToolProbe(c, called, "bps_tool_probe_call_failed", "BPS did not return exactly one valid echo tool call")
	}
	s.sendEvent(c, TestEvent{Type: "status", Text: "BPS tool call verified"})
	input = append(input, callOutput...)
	input = append(input, map[string]any{"type": "function_call_output", "call_id": callID, "output": nonce})
	finished, err := s.runBPSToolProbeStep(ctx, c, account, model, session, input, tools, false)
	if err != nil || !bpsToolProbeExactText(finished.response, nonce) {
		return s.failBPSToolProbe(c, finished, "bps_tool_probe_result_failed", "BPS tool result did not return the expected completed response")
	}
	s.sendEvent(c, TestEvent{Type: "status", Text: "BPS tool result continuation verified"})
	s.sendEvent(c, TestEvent{Type: "content", Text: nonce})
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

func (s *AccountTestService) beginBPSToolProbe(accountID int64) (func(), bool) {
	s.bpsToolsProbeMu.Lock()
	defer s.bpsToolsProbeMu.Unlock()
	if _, busy := s.bpsToolsProbeAccounts[accountID]; busy || len(s.bpsToolsProbeAccounts) >= bpsToolProbeMaxConcurrent {
		return nil, false
	}
	if s.bpsToolsProbeAccounts == nil {
		s.bpsToolsProbeAccounts = make(map[int64]struct{})
	}
	s.bpsToolsProbeAccounts[accountID] = struct{}{}
	return func() {
		s.bpsToolsProbeMu.Lock()
		defer s.bpsToolsProbeMu.Unlock()
		delete(s.bpsToolsProbeAccounts, accountID)
	}, true
}

type bpsToolProbeStep struct {
	response   map[string]any
	diagnostic TestEvent
}

func (s *AccountTestService) failBPSToolProbe(c *gin.Context, step bpsToolProbeStep, code, message string) error {
	event := step.diagnostic
	event.Type, event.Code, event.Error = "error", code, message
	s.sendEvent(c, event)
	return fmt.Errorf("%s", message)
}

func (s *AccountTestService) runBPSToolProbeStep(ctx context.Context, c *gin.Context, account *Account, model, session string, input, tools []any, requireTool bool) (step bpsToolProbeStep, err error) {
	if err := ctx.Err(); err != nil {
		return step, err
	}
	payload := map[string]any{"model": model, "input": input, "stream": false, "prompt_cache_key": session}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	if requireTool {
		payload["tool_choice"] = "required"
	}
	body := []byte(bpsJSON(payload))
	rec := httptest.NewRecorder()
	probe, _ := gin.CreateTestContext(rec)
	ctx = s.withBPSTestResponseObserver(ctx, c, account, &step.diagnostic)
	probe.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(string(body))).WithContext(ctx)
	result, err := s.openaiGatewayService.forwardOpenAIBPS(ctx, probe, account, body)
	if result != nil {
		s.sendEvent(c, TestEvent{Type: "status", Text: bpsTestUsageText(result)})
	}
	if err != nil {
		semanticCode := probe.GetString("bps_upstream_error_code")
		var failure *bpsError
		if semanticCode == "" && errors.As(err, &failure) {
			semanticCode = failure.Code
		}
		step.diagnostic.UpstreamErrorCode = bpsTestIdentifier(semanticCode, account.GetCredential("access_token"))
		return step, err
	}
	var response map[string]any
	if rec.Code < 200 || rec.Code >= 300 || bpsDecode(rec.Body.Bytes(), &response) != nil || response["status"] != "completed" {
		return step, fmt.Errorf("BPS tool probe response did not complete")
	}
	step.response = response
	return step, nil
}

func bpsToolProbeExactText(response map[string]any, nonce string) bool {
	if response["status"] != "completed" {
		return false
	}
	output, ok := response["output"].([]any)
	if !ok {
		return false
	}
	var text strings.Builder
	sawText := false
	for _, raw := range output {
		item, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		if item["type"] == "reasoning" {
			continue
		}
		if item["type"] != "message" || item["role"] != "assistant" {
			return false
		}
		content, ok := item["content"].([]any)
		if !ok {
			return false
		}
		for _, raw := range content {
			part, ok := raw.(map[string]any)
			value, stringText := part["text"].(string)
			if !ok || part["type"] != "output_text" || !stringText {
				return false
			}
			sawText = true
			_, _ = text.WriteString(value)
		}
	}
	return sawText && strings.TrimSpace(text.String()) == nonce
}

func bpsToolProbeExactCall(response map[string]any, nonce string) (string, bool) {
	if response["status"] != "completed" {
		return "", false
	}
	output, ok := response["output"].([]any)
	if !ok {
		return "", false
	}
	callID := ""
	for _, raw := range output {
		item, ok := raw.(map[string]any)
		if !ok {
			return "", false
		}
		if item["type"] == "reasoning" {
			continue
		}
		id := stringValue(item["call_id"])
		if item["type"] != "function_call" || item["name"] != bpsToolProbeName || stringValue(item["namespace"]) != "" || callID != "" || id == "" || strings.TrimSpace(id) != id {
			return "", false
		}
		if value, exists := item["encrypted_function_args"]; exists {
			args, ok := value.([]any)
			if !ok || len(args) > 0 {
				return "", false
			}
		}
		var args map[string]any
		if bpsDecode([]byte(stringValue(item["arguments"])), &args) != nil || len(args) != 1 || args["nonce"] != nonce {
			return "", false
		}
		callID = id
	}
	return callID, callID != ""
}
