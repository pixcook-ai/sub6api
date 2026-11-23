package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/MACOS-DO/sub4api/internal/pkg/httputil"
	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
	"github.com/tidwall/gjson"
)

// Client metadata is captured before request normalization. An upstream response
// cannot introduce task metadata by copying it into a public Response object.
type bpsClientMetadataContextKey struct{}

type bpsClientMetadataSnapshot struct {
	metadata map[string]any
}

func withBPSClientMetadata(ctx context.Context, body []byte) (context.Context, func(), error) {
	// Only this snapshot owns the scope: later Forward allocations must keep
	// their existing request/attempt lifetimes. Reserve before even JSON scanning.
	budget, release := httputil.WithRequestBodyMemoryScope(ctx)
	if err := httputil.ReserveRequestBodyMemory(budget, 4<<10); err != nil {
		release()
		return ctx, func() {}, err
	}
	var source []byte
	if plan := StoredResponsePlanFromContext(ctx); plan != nil && plan.Request != nil {
		source = plan.Request.localMetadata
	}
	if raw, exists, valid := bpsRootClientMetadata(body); valid && exists {
		source = raw
	}
	snapshot := &bpsClientMetadataSnapshot{}
	if len(source) != 0 {
		compact, err := bpsCompactClientMetadata(budget, source)
		if err != nil {
			release()
			return ctx, func() {}, err
		}
		if len(compact) != 0 {
			if value, err := managedfiles.Parse(compact); err == nil {
				if metadata, err := storedResponseMetadata(value); err == nil {
					snapshot.metadata = metadata
				}
			}
		}
	}
	return context.WithValue(ctx, bpsClientMetadataContextKey{}, snapshot), release, nil
}

// Borrow just the root metadata span. Large strings, tool schemas and attachments
// are traversed without decoding them or constructing a second request tree.
// Request policy still validates unrelated object keys; metadata ambiguity fails
// closed here, including escaped spellings of the same root key.
func bpsRootClientMetadata(raw []byte) (metadata []byte, exists, valid bool) {
	if len(raw) == 0 || len(raw) > managedfiles.MaxJSONBytes || !utf8.Valid(raw) {
		return nil, false, false
	}
	i := bpsSkipJSONSpace(raw, 0)
	if i == len(raw) || raw[i] != '{' {
		return nil, false, false
	}
	for i++; ; {
		i = bpsSkipJSONSpace(raw, i)
		if i == len(raw) {
			return nil, false, false
		}
		if raw[i] == '}' {
			return metadata, exists, json.Valid(raw)
		}
		if raw[i] != '"' {
			return nil, false, false
		}
		keyStart := i
		i++
		for i < len(raw) && raw[i] != '"' {
			if raw[i] == '\\' {
				i++
			}
			i++
		}
		if i >= len(raw) {
			return nil, false, false
		}
		key := raw[keyStart : i+1]
		i = bpsSkipJSONSpace(raw, i+1)
		if i == len(raw) || raw[i] != ':' {
			return nil, false, false
		}
		start := bpsSkipJSONSpace(raw, i+1)
		end, ok := bpsClientMetadataValueEnd(raw, start)
		if !ok {
			return nil, false, false
		}
		if bpsClientMetadataKey(key) {
			if exists {
				return nil, false, false
			}
			metadata, exists = raw[start:end], true
		}
		i = bpsSkipJSONSpace(raw, end)
		if i < len(raw) && raw[i] == ',' {
			i++
			continue
		}
		if i >= len(raw) || raw[i] != '}' {
			return nil, false, false
		}
	}
}

func bpsSkipJSONSpace(raw []byte, i int) int {
	for i < len(raw) && (raw[i] == ' ' || raw[i] == '\n' || raw[i] == '\r' || raw[i] == '\t') {
		i++
	}
	return i
}

func bpsClientMetadataValueEnd(raw []byte, start int) (int, bool) {
	depth, quoted := 0, false
	for i := start; i < len(raw); i++ {
		if quoted {
			if raw[i] == '\\' {
				i++
			} else if raw[i] == '"' {
				quoted = false
			}
			continue
		}
		switch raw[i] {
		case '"':
			quoted = true
		case '{', '[':
			depth++
			if depth >= managedfiles.MaxJSONDepth {
				return 0, false
			}
		case '}', ']':
			if depth == 0 {
				return i, true
			}
			depth--
		case ',':
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

func bpsClientMetadataKey(raw []byte) bool {
	want, at := "metadata", 0
	for i := 1; i < len(raw)-1; i++ {
		b := raw[i]
		if b == '\\' {
			if i+5 >= len(raw)-1 || raw[i+1] != 'u' || raw[i+2] != '0' || raw[i+3] != '0' {
				return false
			}
			b = 0
			for _, digit := range raw[i+4 : i+6] {
				b <<= 4
				switch {
				case digit >= '0' && digit <= '9':
					b += digit - '0'
				case digit >= 'a' && digit <= 'f':
					b += digit - 'a' + 10
				case digit >= 'A' && digit <= 'F':
					b += digit - 'A' + 10
				default:
					return false
				}
			}
			i += 5
		}
		if at == len(want) || b != want[at] {
			return false
		}
		at++
	}
	return at == len(want)
}

func bpsCompactClientMetadata(ctx context.Context, raw []byte) ([]byte, error) {
	// An astral code point may use two six-byte JSON escapes. Whitespace
	// outside strings does not count, and is never copied into the decoder.
	const maxBytes = 16*(12*(64+512)+8) + 2
	var compact []byte
	for pass := 0; pass < 2; pass++ {
		quoted, count := false, 0
		for i := 0; i < len(raw); i++ {
			b := raw[i]
			if !quoted && (b == ' ' || b == '\n' || b == '\r' || b == '\t') {
				continue
			}
			count++
			if count > maxBytes {
				return nil, nil
			}
			if pass == 1 {
				compact = append(compact, b)
			}
			if quoted && b == '\\' && i+1 < len(raw) {
				i++
				count++
				if count > maxBytes {
					return nil, nil
				}
				if pass == 1 {
					compact = append(compact, raw[i])
				}
			} else if b == '"' {
				quoted = !quoted
			}
		}
		if pass == 0 {
			if err := httputil.ReserveRequestBodyMemory(ctx, int64(count)*32); err != nil {
				return nil, err
			}
			compact = make([]byte, 0, count)
		}
	}
	return compact, nil
}

func bpsClientMetadataFrom(contexts []context.Context) *bpsClientMetadataSnapshot {
	if len(contexts) == 0 || contexts[0] == nil {
		return nil
	}
	snapshot, _ := contexts[0].Value(bpsClientMetadataContextKey{}).(*bpsClientMetadataSnapshot)
	return snapshot
}

func projectBPSClientFields(response map[string]any, contexts []context.Context) {
	delete(response, "metadata")
	if snapshot := bpsClientMetadataFrom(contexts); snapshot != nil && snapshot.metadata != nil {
		metadata := make(map[string]any, len(snapshot.metadata))
		for key, value := range snapshot.metadata {
			metadata[key] = value
		}
		response["metadata"] = metadata
	}
	response["store"] = false
}

func validBPSClientFields(response map[string]any) bool {
	if value, exists := response["metadata"]; exists {
		if _, err := storedResponseMetadata(value); err != nil {
			return false
		}
	}
	if value, exists := response["store"]; exists && value != false {
		return false
	}
	return true
}

// The archive gate runs before client projection: supplied metadata must already
// match this request so native task details cannot enter the public recording.
func bpsFailureClientFieldsMatch(response map[string]any, contexts []context.Context) bool {
	if !validBPSClientFields(response) {
		return false
	}
	value, exists := response["metadata"]
	if !exists || value == nil {
		return true
	}
	metadata, err := storedResponseMetadata(value)
	if err != nil {
		return false
	}
	snapshot := bpsClientMetadataFrom(contexts)
	if snapshot == nil || snapshot.metadata == nil {
		return len(metadata) == 0
	}
	if len(metadata) != len(snapshot.metadata) {
		return false
	}
	for key, value := range metadata {
		if expected, exists := snapshot.metadata[key]; !exists || expected != value {
			return false
		}
	}
	return true
}

// This response-only marker describes the selected API-key upstream's bounded
// failure envelope. It is not authentication or proof of exactly-once execution.
func hasBPSFailureContract(account *Account, headers http.Header) bool {
	values := headers.Values("X-Bps-Response-Contract")
	return account != nil && account.Platform == PlatformOpenAI && account.Type == AccountTypeAPIKey && len(values) == 1 && values[0] == "failure-v1"
}

// Decode without duplicate decoded keys: otherwise validation and later JSON
// consumers can disagree on the status, usage or error selected for settlement.
func decodeBPSFailureObject(raw []byte) (map[string]any, bool) {
	if !utf8.Valid(raw) {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var read func(int) (any, error)
	read = func(depth int) (any, error) {
		if depth > 32 {
			return nil, errors.New("failure envelope nesting limit")
		}
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		delimiter, container := token.(json.Delim)
		if !container {
			return token, nil
		}
		if delimiter == '{' {
			object := map[string]any{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, errors.New("invalid object key")
				}
				if _, exists := object[key]; exists {
					return nil, errors.New("duplicate object key")
				}
				value, err := read(depth + 1)
				if err != nil {
					return nil, err
				}
				object[key] = value
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim('}') {
				return nil, errors.New("invalid object end")
			}
			return object, nil
		}
		if delimiter == '[' {
			values := []any{}
			for decoder.More() {
				value, err := read(depth + 1)
				if err != nil {
					return nil, err
				}
				values = append(values, value)
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim(']') {
				return nil, errors.New("invalid array end")
			}
			return values, nil
		}
		return nil, errors.New("invalid container")
	}
	value, err := read(0)
	if err != nil {
		return nil, false
	}
	if _, err = decoder.Token(); err != io.EOF {
		return nil, false
	}
	object, ok := value.(map[string]any)
	return object, ok
}

// Messages are the gateway's fixed public classifier output, never raw upstream
// diagnostics. Only these combinations can bypass ordinary error replay.
func bpsFailureErrorStatus(value map[string]any, fallback int) int {
	if len(value) < 3 || len(value) > 8 {
		return 0
	}
	for key, raw := range value {
		switch key {
		case "type", "code", "message", "status":
		case "failure_stage", "rejected_tool", "repair_skipped_reason", "upstream_terminal_event":
			text, ok := raw.(string)
			if !ok || len(text) > 256 || value["code"] != "basispoints_protocol_error" || value["type"] != "server_error" {
				return 0
			}
			if key == "failure_stage" && text != "bridge_protocol_conversion" && text != "bridge_tool_validation" {
				return 0
			}
		default:
			return 0
		}
	}
	kind, _ := value["type"].(string)
	code, _ := value["code"].(string)
	message, _ := value["message"].(string)
	// SSE Details carries semantic status; JSON encodes UpstreamError without
	// it, using the actual HTTP status. Both are emitted by the same classifier.
	status := fallback
	if raw, exists := value["status"]; exists {
		number, ok := raw.(json.Number)
		if !ok {
			return 0
		}
		parsed, err := number.Int64()
		if err != nil || parsed < 400 || parsed > 599 {
			return 0
		}
		status = int(parsed)
		if fallback != 0 && status != fallback {
			return 0
		}
	}
	if status != 0 {
		knownCode := false
		switch code {
		case "invalid_request", "invalid_request_error", "bad_request_error", "invalid_argument", "invalid_value", "unsupported_value", "invalid_prompt", "context_length_exceeded", "string_above_max_length", "previous_response_not_found", "thinking_signature_invalid", "invalid_encrypted_content", "cyber_policy", "content_policy_violation", "authentication_error", "invalid_api_key", "token_expired", "invalid_token", "unauthorized", "permission_error", "permission_denied", "basispoints_model_access_changed", "access_denied", "forbidden", "model_not_found", "model_not_found_error", "unsupported_model", "message_too_big", "rate_limit_error", "rate_limit_exceeded", "insufficient_quota", "quota_exceeded", "usage_limit_reached", "tokens", "requests", "server_error", "internal_error", "internal_server_error", "server_is_overloaded", "overloaded_error", "service_unavailable", "api_error", "basispoints_upstream_error", "basispoints_upstream_cancelled", "basispoints_protocol_error":
			knownCode = true
		}
		if !knownCode {
			return 0
		}
		// Explicit upstream status can change the classifier category while
		// retaining its known code. Match its fixed message, never raw text.
		if status == 401 && kind == "authentication_error" && message == "Basispoints authentication failed. Refresh the ChatGPT login credentials." {
			return status
		}
		if status == 403 && kind == "permission_error" && message == "Basispoints denied access for this account." {
			return status
		}
		if status == 429 && kind == "rate_limit_error" && message == "Rate limit reached. Please try again later." {
			return status
		}

		if status >= 500 && kind == "server_error" {
			if message == "Basispoints upstream request failed." || code == "basispoints_protocol_error" && message == "Basispoints returned an invalid response stream." || code == "basispoints_upstream_cancelled" && message == "Basispoints cancelled the response; request was not replayed." {
				return status
			}
		}
		if status != 401 && status != 403 && status != 429 && status < 500 && kind == "invalid_request_error" && message == "Basispoints upstream request failed." {
			return status
		}
		// The remaining specialized public messages retain their native status.
	}

	classified := func() int {
		switch kind {
		case "tokens":
			if (code == "rate_limit_exceeded" || code == "rate_limit_error") && message == "Rate limit reached on tokens per minute (TPM). Please try again later." {
				return 429
			}
		case "requests":
			if (code == "rate_limit_exceeded" || code == "rate_limit_error") && message == "Rate limit reached on requests per minute (RPM). Please try again later." {
				return 429
			}
		case "rate_limit_error":
			if (code == "rate_limit_exceeded" || code == "rate_limit_error") && message == "Rate limit reached. Please try again later." {
				return 429
			}
		case "insufficient_quota":
			if (code == "insufficient_quota" || code == "quota_exceeded") && message == "Basispoints account quota is exhausted." {
				return 429
			}
		case "server_error":
			if code == "basispoints_protocol_error" && message == "Basispoints returned an invalid response stream." {
				return 502
			}
			if (code == "basispoints_upstream_error" || code == "server_error" || code == "internal_error" || code == "server_is_overloaded") && message == "Basispoints upstream request failed." {
				return 502
			}
		case "authentication_error":
			switch code {
			case "invalid_api_key", "invalid_token", "token_expired", "authentication_error", "unauthorized":
				if message == "Basispoints authentication failed. Refresh the ChatGPT login credentials." {
					return 401
				}
			}
		case "permission_error":
			switch code {
			case "permission_denied", "permission_error", "access_denied", "forbidden":
				if message == "Basispoints denied access for this account." {
					return 403
				}
			}
		case "invalid_request_error":
			switch code {
			case "invalid_request_error":
				if message == "Basispoints rejected the request." {
					return 400
				}
			case "context_length_exceeded":
				if message == "Basispoints request exceeds the model context limit." {
					return 400
				}
			case "model_not_found", "unsupported_model":
				if message == "The requested model is unavailable for this Basispoints account." {
					return 400
				}
			}
		}
		return 0
	}()
	if status != 0 && classified != status {
		return 0
	}
	return classified
}

func bpsFailureResponse(value map[string]any) (map[string]any, bool) {
	if !validBPSClientFields(value) {
		return nil, false
	}
	status, _ := value["status"].(string)
	if status != "failed" && status != "cancelled" {
		return nil, false
	}
	result := map[string]any{"status": status}
	if id, exists := value["id"]; exists {
		text, ok := id.(string)
		if !ok || len(text) > 256 {
			return nil, false
		}
		result["id"] = text
	}
	if raw, exists := value["usage"]; exists {
		encoded, err := json.Marshal(raw)
		if err != nil {
			return nil, false
		}
		if usage := safeOpenAIFailureUsage(gjson.ParseBytes(encoded)); usage != nil {
			result["usage"] = usage
		}
	}
	return result, true
}

func bpsNonStreamingFailure(account *Account, resp *http.Response, raw []byte, contexts ...context.Context) ([]byte, bool) {
	if !hasBPSFailureContract(account, resp.Header) {
		return nil, false
	}
	object, ok := decodeBPSFailureObject(raw)
	if !ok || len(object) != 2 {
		return nil, false
	}
	failure, ok := object["error"].(map[string]any)
	if !ok || bpsFailureErrorStatus(failure, resp.StatusCode) != resp.StatusCode {
		return nil, false
	}
	response, ok := object["response"].(map[string]any)
	if !ok {
		return nil, false
	}
	for key := range response {
		if key != "id" && key != "status" && key != "usage" && key != "metadata" && key != "store" {
			return nil, false
		}
	}
	diagnostic, ok := bpsFailureResponse(response)
	if !ok {
		return nil, false
	}
	projectBPSClientFields(diagnostic, contexts)
	encoded, err := json.Marshal(map[string]any{"error": failure, "response": diagnostic})
	return encoded, err == nil
}

func bpsStreamingFailure(account *Account, resp *http.Response, raw []byte) bool {
	if resp.StatusCode != http.StatusOK || !hasBPSFailureContract(account, resp.Header) {
		return false
	}
	object, ok := decodeBPSFailureObject(raw)
	if !ok || (object["type"] != "response.failed" && object["type"] != "response.cancelled") {
		return false
	}
	response, ok := object["response"].(map[string]any)
	if !ok {
		return false
	}
	failure, ok := response["error"].(map[string]any)
	if !ok || bpsFailureErrorStatus(failure, 0) == 0 {
		return false
	}
	if response["status"] != strings.TrimPrefix(object["type"].(string), "response.") {
		return false
	}
	_, ok = bpsFailureResponse(response)
	return ok
}

// The bridge also emits a classified bare error as its terminal. Normalize it
// before ordinary pre-output retry logic; do not bless arbitrary raw errors.
func normalizeBPSBareError(account *Account, resp *http.Response, raw []byte, contexts ...context.Context) ([]byte, bool) {
	if resp.StatusCode != 200 || !hasBPSFailureContract(account, resp.Header) {
		return nil, false
	}
	object, ok := decodeBPSFailureObject(raw)
	if !ok || object["type"] != "error" {
		return nil, false
	}
	for key := range object {
		switch key {
		case "type", "status", "error", "code", "message", "response", "sequence_number":
		default:
			return nil, false
		}
	}
	failure, ok := object["error"].(map[string]any)
	if !ok {
		return nil, false
	}
	status := bpsFailureErrorStatus(failure, 0)
	if status == 0 {
		return nil, false
	}
	if outer, exists := object["status"]; exists {
		number, ok := outer.(json.Number)
		if !ok {
			return nil, false
		}
		value, err := number.Int64()
		if err != nil || int64(status) != value {
			return nil, false
		}
	}
	for _, key := range []string{"code", "message"} {
		if outer, exists := object[key]; exists && outer != failure[key] {
			return nil, false
		}
	}
	response := map[string]any{"status": "failed", "output": []any{}, "error": failure}
	if nested, exists := object["response"]; exists {
		fields, ok := nested.(map[string]any)
		if !ok {
			return nil, false
		}
		diagnostic, ok := bpsFailureResponse(fields)
		if !ok || diagnostic["status"] != "failed" {
			return nil, false
		}
		if nestedError, exists := fields["error"]; exists {
			encoded, err := json.Marshal(nestedError)
			if err != nil {
				return nil, false
			}
			outer, _ := json.Marshal(failure)
			if !bytes.Equal(encoded, outer) {
				return nil, false
			}
		}
		for key, value := range diagnostic {
			response[key] = value
		}
	}
	projectBPSClientFields(response, contexts)
	normalized := map[string]any{"type": "response.failed", "response": response}
	if sequence, exists := object["sequence_number"]; exists {
		normalized["sequence_number"] = sequence
	}
	encoded, err := json.Marshal(normalized)
	return encoded, err == nil
}

// Failure sanitization deliberately removes upstream metadata. Restore only the
// captured client tags after that privacy filter, while upstream storage remains
// explicitly disabled. Successful Responses keep their existing gateway path.
func restoreBPSFailureClientFields(ctx context.Context, account *Account, resp *http.Response, raw []byte) ([]byte, bool) {
	if bpsClientMetadataFrom([]context.Context{ctx}) == nil || !bpsStreamingFailure(account, resp, raw) {
		return raw, false
	}
	object, ok := decodeBPSFailureObject(raw)
	if !ok {
		return raw, false
	}
	response, ok := object["response"].(map[string]any)
	if !ok {
		return raw, false
	}
	projectBPSClientFields(response, []context.Context{ctx})
	encoded, err := json.Marshal(object)
	if err != nil {
		return raw, false
	}
	return encoded, !bytes.Equal(raw, encoded)
}
