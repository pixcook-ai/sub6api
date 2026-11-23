package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type storedResponseCaptureKey struct{}
type storedResponseAttempt struct {
	capture        *StoredResponseCapture
	stop           func() error
	stopOnce       sync.Once
	stopErr        error
	failureWritten bool
}

func (a *storedResponseAttempt) stopLease() error {
	a.stopOnce.Do(func() { a.stopErr = a.stop() })
	return a.stopErr
}

// Terminal payloads are released only after renewal has joined and persistence
// succeeds. Cleanup records an interrupted outcome without starting generation.
func withStoredResponseCapture(parent context.Context, capture *StoredResponseCapture) (context.Context, func() error) {
	attemptParent, cancel := context.WithCancel(parent)
	ctx, stop := capture.KeepLease(attemptParent)
	attempt := &storedResponseAttempt{capture: capture, stop: stop}
	ctx = context.WithValue(ctx, storedResponseCaptureKey{}, attempt)
	return ctx, func() error {
		defer cancel()
		stopErr := attempt.stopLease()
		cleanupCtx, done := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
		defer done()
		return errors.Join(stopErr, capture.Abort(cleanupCtx))
	}
}

func storedResponseAttemptFrom(ctx context.Context) *storedResponseAttempt {
	if ctx == nil {
		return nil
	}
	attempt, _ := ctx.Value(storedResponseCaptureKey{}).(*storedResponseAttempt)
	return attempt
}

func captureStoredResponseJSON(ctx context.Context, raw []byte) ([]byte, error) {
	attempt := storedResponseAttemptFrom(ctx)
	if attempt == nil {
		return raw, nil
	}
	syncNativeTerminalCaptureUsage15(ctx)
	if gjson.GetBytes(raw, "error").IsObject() && gjson.GetBytes(raw, "response").IsObject() {
		raw = storedResponseSafeFailureJSON(raw)
	}
	path := "usage"
	if gjson.GetBytes(raw, "response").IsObject() {
		path = "response.usage"
	}
	raw = storedResponsePayloadWithObservedUsage(ctx, raw, path)
	if err := attempt.stopLease(); err != nil {
		return nil, &StoredResponseLocalError{err}
	}
	captured, err := attempt.capture.CaptureJSON(ctx, raw)
	if err != nil {
		return nil, &StoredResponseLocalError{err}
	}
	return captured, nil
}

func captureStoredResponseEvent(ctx context.Context, event string, raw []byte) ([]byte, error) {
	attempt := storedResponseAttemptFrom(ctx)
	if attempt == nil {
		return raw, nil
	}
	syncNativeTerminalCaptureUsage15(ctx)
	if event == "response.failed" {
		failure := storedResponseSafeFailureJSON(raw)
		var envelope map[string]json.RawMessage
		_ = json.Unmarshal(failure, &envelope)
		var response map[string]json.RawMessage
		_ = json.Unmarshal(envelope["response"], &response)
		response["object"] = json.RawMessage(strconv.Quote("response"))
		response["output"] = json.RawMessage("[]")
		response["error"] = envelope["error"]
		responseRaw, _ := json.Marshal(response)
		payload := map[string]json.RawMessage{"type": json.RawMessage(strconv.Quote(event)), "response": responseRaw}
		if sequence := gjson.GetBytes(raw, "sequence_number"); validOpenAIFailureCount(sequence) {
			payload["sequence_number"] = json.RawMessage(sequence.Raw)
		}
		raw, _ = json.Marshal(payload)
	}
	if err := validateStoredResponseAttemptEvent15(attempt, event, raw); err != nil {
		return nil, &StoredResponseLocalError{err}
	}
	if gjson.GetBytes(raw, "response").IsObject() {
		raw = storedResponsePayloadWithObservedUsage(ctx, raw, "response.usage")
	}
	switch event {
	case "response.completed", "response.incomplete", "response.failed":
		if err := attempt.stopLease(); err != nil {
			return nil, &StoredResponseLocalError{err}
		}
	}
	captured, err := attempt.capture.CaptureEvent(ctx, event, raw)
	if err != nil {
		return nil, &StoredResponseLocalError{err}
	}
	return captured, nil
}

func storedResponsePayloadWithObservedUsage(ctx context.Context, raw []byte, path string) []byte {
	attempt := storedResponseAttemptFrom(ctx)
	if attempt == nil {
		return raw
	}
	var merged any = attempt.capture.observedUsage
	if safe := safeOpenAIFailureUsage(gjson.GetBytes(raw, path)); safe != nil {
		merged = mergeStoredResponseUsage(merged, safe)
	}
	if values, ok := merged.(map[string]any); ok && len(values) > 0 {
		if updated, err := sjson.SetBytes(raw, path, values); err == nil {
			return updated
		}
	}
	return raw
}

func validateStoredResponseEventEnvelope(event string, raw []byte) error {
	value, err := decodeStoredResponseJSON(raw)
	if err != nil {
		return err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return ErrStoredResponseInvalid
	}
	allowed := map[string]bool{"type": true, "sequence_number": true}
	switch event {
	case "response.created", "response.queued", "response.in_progress", "response.completed", "response.incomplete", "response.failed":
		allowed["response"] = true
	case "response.output_text.delta", "response.output_text.done", "response.refusal.delta", "response.refusal.done":
		for _, key := range []string{"response_id", "item_id", "output_index", "content_index", "delta", "text", "refusal", "logprobs"} {
			allowed[key] = true
		}
	case "response.output_item.added", "response.output_item.done":
		for _, key := range []string{"response_id", "output_index", "item"} {
			allowed[key] = true
		}
		if err := validateStoredResponseOutput([]any{object["item"]}); err != nil {
			return err
		}
	case "response.content_part.added", "response.content_part.done":
		for _, key := range []string{"response_id", "item_id", "output_index", "content_index", "part"} {
			allowed[key] = true
		}
		if err := validateStoredResponseOutput([]any{map[string]any{"type": "message", "role": "assistant", "content": []any{object["part"]}}}); err != nil {
			return err
		}
	default:
		return ErrStoredResponseUnsupported
	}
	for key, value := range object {
		if !allowed[key] {
			return ErrStoredResponseUnsupported
		}
		switch key {
		case "type", "response_id", "item_id", "delta", "text", "refusal":
			if _, ok := value.(string); !ok {
				return ErrStoredResponseInvalid
			}
		case "sequence_number", "output_index", "content_index":
			if !storedNonnegativeInteger(value) {
				return ErrStoredResponseInvalid
			}
		case "logprobs":
			if value != nil {
				if values, ok := value.([]any); !ok || len(values) != 0 {
					return ErrStoredResponseUnsupported
				}
			}
		}
	}
	return nil
}

// Protocol diagnostics are bounded upstream facts, but only standard public
// error fields belong in stored Responses objects and client payloads.
func storedResponseSafeFailureJSON(raw []byte) []byte {
	failure := gjson.GetBytes(raw, "error")
	if !failure.IsObject() {
		failure = gjson.GetBytes(raw, "response.error")
	}
	safeError := map[string]any{}
	for _, key := range []string{"type", "code", "message", "param"} {
		value := failure.Get(key)
		if value.Type == gjson.String {
			safeError[key] = value.String()
		}
	}
	response := map[string]any{"status": "failed"}
	if id := gjson.GetBytes(raw, "response.id"); id.Type == gjson.String {
		response["id"] = id.String()
	}
	if usage := safeOpenAIFailureUsage(gjson.GetBytes(raw, "response.usage")); usage != nil {
		response["usage"] = usage
	}
	encoded, _ := json.Marshal(map[string]any{"error": safeError, "response": response})
	return encoded
}

// StoredResponseLocalError prevents storage and lease failures from becoming
// retryable upstream failures.
type StoredResponseLocalError struct{ Err error }

func (e *StoredResponseLocalError) Error() string { return "local response storage failed" }
func (e *StoredResponseLocalError) Unwrap() error { return e.Err }

func writeStoredResponseCaptureError(c *gin.Context, ctx context.Context) {
	if c == nil || c.Writer == nil {
		return
	}
	attempt := storedResponseAttemptFrom(ctx)
	if attempt != nil {
		if attempt.failureWritten {
			return
		}
		attempt.failureWritten = true
	}
	MarkResponseCommitted(c)
	if c.Writer.Written() {
		if attempt == nil {
			return
		}
		frame := buildOpenAIResponseFailedSSE(attempt.capture.PublicID(), "", []byte(`{"error":{"type":"server_error","code":"response_storage_error","message":"Response storage is temporarily unavailable."}}`), "")
		_, _ = c.Writer.Write([]byte(frame))
		c.Writer.Flush()
		return
	}
	c.Writer.Header().Del("Content-Length")
	c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"type": "server_error", "code": "response_storage_error", "message": "Response storage is temporarily unavailable."}})
}
