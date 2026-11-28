package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Only an undecodable inner envelope is eligible for format correction.
type bpsToolEnvelopeError struct{ *bpsError }

func (e *bpsToolEnvelopeError) Unwrap() error { return e.bpsError }

func bpsRepairableTool(r *bpsRequest, response map[string]any, err error) (int, map[string]any, string, bool) {
	var envelopeError *bpsToolEnvelopeError
	if r.Structured != nil || r.Compact || len(r.Tools) == 0 || !errors.As(err, &envelopeError) {
		return 0, nil, "", false
	}
	output, _ := response["output"].([]any)
	for index, raw := range output {
		item, _ := raw.(map[string]any)
		args, code, ok := bpsRepairToolCode(item)
		if !ok || strings.HasPrefix(stringValue(args["summary"]), "codex2api.") {
			continue
		}
		if strings.HasPrefix(bpsTransportShape(code), "format=text_or_code;") {
			return index, item, code, true
		}
	}
	return 0, nil, "", false
}

func bpsRepairToolCode(item map[string]any) (map[string]any, string, bool) {
	name := stringValue(item["name"])
	callID, itemID := stringValue(item["call_id"]), stringValue(item["id"])
	if item["type"] != "function_call" || (name != "run_officejs" && name != "functions.run_officejs") ||
		callID == "" || strings.TrimSpace(callID) != callID || itemID == "" {
		return nil, "", false
	}
	args, ok := item["arguments"].(map[string]any)
	if !ok && bpsDecode([]byte(stringValue(item["arguments"])), &args) != nil {
		return nil, "", false
	}
	code, ok := args["code"].(string)
	return args, code, ok && strings.TrimSpace(code) != "" && len(code) <= bpsMaxEnvelopeBytes
}

func (s *OpenAIGatewayService) bpsRepairTool(
	ctx context.Context, c *gin.Context, account *Account, r *bpsRequest, response, rejected map[string]any,
	raw, proxyURL string, idle time.Duration, maxSize int,
) (map[string]any, []any, map[string]any, error) {
	failure := &bpsError{502, "bps_tool_repair_failed", "BPS could not correct the tool call format; no client tool was executed"}
	var wire map[string]any
	if bpsDecode(r.Body, &wire) != nil {
		return nil, nil, nil, failure
	}
	input, inputOK := wire["input"].([]any)
	metadata, metadataOK := wire["metadata"].(map[string]any)
	output, outputOK := response["output"].([]any)
	if !inputOK || !metadataOK || metadata == nil || !outputOK {
		return nil, nil, nil, failure
	}
	callID := stringValue(rejected["call_id"])
	feedback := []any{
		map[string]any{"type": "function_call_output", "id": bpsFunctionOutputID(callID), "call_id": callID,
			"output": "{\"executed\":false,\"error\":{\"code\":\"invalid_client_tool_transport\",\"message\":\"The tool call did not use a client tool envelope.\"}}"},
		bpsMessage("developer", "The preceding native tool call was rejected before execution. Correct only its transport format. Return exactly one call to the intended tool in the existing client catalog. For CUSTOM, use its exact summary marker and original raw input. For FUNCTION, use one JSON envelope with the original raw text as its code or cmd argument. Preserve the text byte for byte. Do not infer another operation, execute Office code, or repeat commentary."),
	}
	wire["input"] = append(append(input, output...), feedback...)
	metadata["agent_iteration"] = strconv.Itoa(r.AgentIteration + 1)

	repairCtx, cancel := context.WithTimeout(ctx, idle)
	defer cancel()
	req, err := buildOpenAIBPSRequest(repairCtx, account, []byte(bpsJSON(wire)))
	if err != nil {
		return nil, nil, nil, err
	}
	route, scoped := openAIRequestProxyFromContext(ctx, account.ID)
	if scoped {
		req, err = route.bind(req)
		if err != nil {
			return nil, nil, nil, failure
		}
	}
	if err := s.checkOpenAITiboQualityAdmission(ctx, account, r.Body); err != nil {
		return nil, nil, nil, failure
	}
	upstream, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	if scoped {
		route.track(upstream)
	}
	if upstream != nil && upstream.Body != nil {
		defer func() { _ = upstream.Body.Close() }()
	}
	if err != nil || upstream == nil || upstream.Body == nil {
		if ctx.Err() != nil {
			return nil, nil, nil, ctx.Err()
		}
		return nil, nil, nil, failure
	}
	stop := context.AfterFunc(repairCtx, func() { _ = upstream.Body.Close() })
	defer stop()
	if upstream.StatusCode < http.StatusOK || upstream.StatusCode >= http.StatusMultipleChoices {
		httpErr := s.bpsHTTPError(ctx, c, account, upstream, r.Model)
		var providerFailure *bpsError
		if errors.As(httpErr, &providerFailure) {
			return nil, nil, nil, providerFailure
		}
		// The primary stream may already be visible; never fail over this turn.
		return nil, nil, nil, failure
	}

	var corrected, usage map[string]any
	pending := map[string]bool{}
	err = readBPSEvents(upstream, maxSize, func(kind string, payload map[string]any) error {
		if err := repairCtx.Err(); err != nil {
			return err
		}
		response, _ := payload["response"].(map[string]any)
		if reported, ok := payload["usage"].(map[string]any); ok {
			usage = reported
		}
		if reported, ok := response["usage"].(map[string]any); ok {
			usage = reported
		}
		if kind == "response.output_item.done" {
			item, _ := payload["item"].(map[string]any)
			if item["type"] == "function_call" || item["type"] == "custom_tool_call" {
				if len(pending) >= 1024 {
					return failure
				}
				pending[stringValue(item["id"])+"\x00"+stringValue(item["call_id"])] = true
			}
		}
		switch kind {
		case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "error":
			corrected = response
			if kind != "response.completed" && kind != "response.done" {
				return s.bpsTerminalError(repairCtx, c, account, kind, payload, r.Model, upstream)
			}
		}
		return nil
	})
	if ctx.Err() != nil {
		return nil, nil, usage, ctx.Err()
	}
	if err != nil {
		var upstreamFailure *bpsError
		if errors.As(err, &upstreamFailure) {
			return nil, nil, usage, upstreamFailure
		}
		return nil, nil, usage, failure
	}
	if corrected["status"] != "completed" {
		return nil, nil, usage, failure
	}
	history := append([]any{rejected}, feedback...)
	var candidate map[string]any
	correctedOutput, _ := corrected["output"].([]any)
	for _, rawItem := range correctedOutput {
		item, _ := rawItem.(map[string]any)
		switch item["type"] {
		case "message":
			// The original turn owns the visible text.
		case "reasoning":
			if candidate == nil && stringValue(item["encrypted_content"]) != "" {
				history = append(history, map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": item["encrypted_content"]})
			}
		case "function_call":
			if candidate != nil || stringValue(item["id"]) == stringValue(rejected["id"]) || stringValue(item["call_id"]) == callID {
				return nil, nil, usage, failure
			}
			if _, _, ok := bpsRepairToolCode(item); !ok {
				return nil, nil, usage, failure
			}
			candidate = item
			delete(pending, stringValue(item["id"])+"\x00"+stringValue(item["call_id"]))
		default:
			return nil, nil, usage, failure
		}
	}
	if candidate == nil || len(pending) != 0 {
		return nil, nil, usage, failure
	}
	client, err := bpsConvertTool(candidate, r)
	if err != nil || !bpsRepairPreservesInput(client, raw) {
		return nil, nil, usage, failure
	}
	return candidate, history, usage, nil
}

func bpsRepairPreservesInput(call map[string]any, raw string) bool {
	if call["type"] == "custom_tool_call" {
		return call["input"] == raw
	}
	var args map[string]any
	if call["type"] != "function_call" || bpsDecode([]byte(stringValue(call["arguments"])), &args) != nil {
		return false
	}
	code, hasCode := args["code"]
	cmd, hasCmd := args["cmd"]
	return (hasCode && !hasCmd && code == raw) || (hasCmd && !hasCode && cmd == raw)
}

// Both BPS submissions are billable. Keep numeric usage details in the
// downstream response as well as in gateway accounting.
func bpsAddRepairUsage(total, usage map[string]any) map[string]any {
	if total == nil {
		total = make(map[string]any)
	}
	for key, value := range usage {
		switch count := value.(type) {
		case map[string]any:
			previous, _ := total[key].(map[string]any)
			total[key] = bpsAddRepairUsage(previous, count)
		case json.Number:
			previous, _ := total[key].(json.Number)
			before, _ := previous.Int64()
			if added, err := count.Int64(); err == nil && added >= 0 && before >= 0 && before <= (1<<63-1)-added {
				total[key] = json.Number(strconv.FormatInt(before+added, 10))
			}
		}
	}
	return total
}
