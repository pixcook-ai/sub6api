package service

import (
	"bytes"
	"context"
	"encoding/json"
	"mime"
	"strings"
	"sync"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
	"github.com/tidwall/gjson"
)

const nativeTerminalMediaType15 = "application/vnd.pixcook.bps-native-terminal+json"

// The delivery gate is attempt-local and must run before any public payload
// mutation. It never owns authorization, a database lease or retry policy.
type nativeTerminalDelivery15 struct {
	receipt         *verifiedNativeTerminal15
	gatewayTerminal []byte
	snapshot        []byte
}
type nativeTerminalGate15 struct {
	key                      string
	expected                 managedfiles.NativeTerminalBinding15
	expectedNativeWireSHA256 string
	maxBytes                 int
	pending                  *nativeTerminalDelivery15
	terminal                 bool
	rejected                 bool
}

// Only trusted admission code may construct this gate. A fresh nonce, exact
// account pin and authorization callback remain admission responsibilities.
func newNativeTerminalGate15(key string, expected managedfiles.NativeTerminalBinding15, expectedNativeWireSHA256 string, maxBytes int) (*nativeTerminalGate15, error) {
	if len(key) < 32 || len(key) > 1024 || strings.TrimSpace(key) != key || maxBytes <= 0 || maxBytes > 16<<20 || managedfiles.ValidateNativeTerminalBinding15(expected) != nil || !nativeTerminalDigest15(expectedNativeWireSHA256) {
		return nil, ErrStoredResponseInvalid
	}
	return &nativeTerminalGate15{key: key, expected: expected, expectedNativeWireSHA256: expectedNativeWireSHA256, maxBytes: maxBytes}, nil
}
func (g *nativeTerminalGate15) reject() (bool, *nativeTerminalDelivery15, error) {
	if g != nil {
		g.pending = nil
		g.rejected = true
	}
	return true, nil, ErrStoredResponseInvalid
}

// Consume returns drop=true for private metadata. It exposes a verified proof
// only after the following completed frame matches the signed exact bytes.
// Syntactic SSE comments do not call Consume; semantic keepalive events do.
func (g *nativeTerminalGate15) Consume(event string, data []byte, now time.Time) (bool, *nativeTerminalDelivery15, error) {
	if g == nil || g.rejected || g.terminal {
		return g.reject()
	}
	if strings.HasPrefix(event, "bps.private.") {
		if event != managedfiles.NativeTerminalEvent15 || g.pending != nil {
			return g.reject()
		}
		receipt, terminal, snapshot, err := verifyNativeTerminal15(data, g.key, g.expected, g.expectedNativeWireSHA256, now, g.maxBytes)
		if err != nil {
			return g.reject()
		}
		g.pending = &nativeTerminalDelivery15{receipt: receipt, gatewayTerminal: terminal, snapshot: snapshot}
		return true, nil, nil
	}
	if g.pending != nil {
		if event != "response.completed" || len(data) > g.maxBytes+(16<<10) {
			return g.reject()
		}
		value, err := managedfiles.Parse(data)
		if err != nil {
			return g.reject()
		}
		root, ok := value.(map[string]any)
		if !ok || root["type"] != "response.completed" {
			return g.reject()
		}
		for key, value := range root {
			switch key {
			case "type", "response":
			case "sequence_number":
				if !storedNonnegativeInteger(value) {
					return g.reject()
				}
			default:
				return g.reject()
			}
		}
		var envelope struct {
			Response json.RawMessage `json:"response"`
		}
		if json.Unmarshal(data, &envelope) != nil || !bytes.Equal(envelope.Response, g.pending.gatewayTerminal) {
			return g.reject()
		}
		delivery := g.pending
		g.pending = nil
		g.terminal = true
		return false, delivery, nil
	}
	if event == "response.completed" || event == "response.done" {
		return g.reject()
	}
	switch event {
	case "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error":
		// Existing BPS failure validators and billing settlement still own these.
		// No successful native history is issued for such an outcome.
		g.terminal = true
	}
	return false, nil, nil
}

// Finish must run on every EOF/disconnect before any public success decision.
func (g *nativeTerminalGate15) Finish() error {
	if g == nil || g.rejected || g.pending != nil || !g.terminal {
		if g != nil {
			g.pending = nil
			g.rejected = true
		}
		return ErrStoredResponseInvalid
	}
	return nil
}

// UnwrapJSON handles negotiated private successful JSON responses before usage,
// ID/model/tool transformations. Failure/incomplete public JSON stays on the
// existing guarded failure path and never calls this successful unwrap adapter.
func (g *nativeTerminalGate15) UnwrapJSON(contentType string, raw []byte, now time.Time) (*nativeTerminalDelivery15, error) {
	if g == nil || g.rejected || g.terminal || g.pending != nil {
		_, _, err := g.reject()
		return nil, err
	}
	media, params, err := mime.ParseMediaType(contentType)
	if err != nil || media != nativeTerminalMediaType15 || len(params) != 0 {
		_, _, err := g.reject()
		return nil, err
	}
	receipt, terminal, snapshot, err := verifyNativeTerminal15(raw, g.key, g.expected, g.expectedNativeWireSHA256, now, g.maxBytes)
	if err != nil {
		_, _, err := g.reject()
		return nil, err
	}
	g.terminal = true
	return &nativeTerminalDelivery15{receipt: receipt, gatewayTerminal: terminal, snapshot: snapshot}, nil
}

// Both attempt and network contexts must carry this same attempt-local state.
type nativeTerminalAttemptKey15 struct{}
type nativeTerminalAttempt15 struct {
	mu            sync.Mutex
	gate          *nativeTerminalGate15
	delivery      *nativeTerminalDelivery15
	observedUsage any
}

func withNativeTerminalAttempt15(ctx context.Context, gate *nativeTerminalGate15) (context.Context, error) {
	if ctx == nil || gate == nil || ctx.Value(nativeTerminalAttemptKey15{}) != nil {
		return nil, ErrStoredResponseInvalid
	}
	return context.WithValue(ctx, nativeTerminalAttemptKey15{}, &nativeTerminalAttempt15{gate: gate}), nil
}
func nativeTerminalAttemptFrom15(ctx context.Context) *nativeTerminalAttempt15 {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(nativeTerminalAttemptKey15{}).(*nativeTerminalAttempt15)
	return state
}
func nativeTerminalDeliveryFromContext15(ctx context.Context) *nativeTerminalDelivery15 {
	if state := nativeTerminalAttemptFrom15(ctx); state != nil {
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.delivery
	}
	return nil
}
func consumeNativeTerminalEvent15(ctx context.Context, event string, data []byte, now time.Time) (bool, error) {
	state := nativeTerminalAttemptFrom15(ctx)
	if state == nil {
		if strings.HasPrefix(event, "bps.private.") {
			return true, ErrStoredResponseInvalid
		}
		return false, nil
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return true, state.gate.Finish()
	}
	drop, delivery, err := state.gate.Consume(event, data, now)
	if err == nil && state.gate.pending != nil {
		observeNativeTerminalUsage15(ctx, state.gate.pending.gatewayTerminal)
	}
	if err != nil {
		return true, err
	}
	if delivery != nil {
		state.mu.Lock()
		if state.delivery != nil {
			state.mu.Unlock()
			return true, ErrStoredResponseInvalid
		}
		state.delivery = delivery
		state.mu.Unlock()
	}
	return drop, nil
}
func finishNativeTerminalAttempt15(ctx context.Context) error {
	if state := nativeTerminalAttemptFrom15(ctx); state != nil {
		return state.gate.Finish()
	}
	return nil
}

// Usage is an authenticated upstream fact even if the following public terminal
// is lost/mismatched. It never grants access to private recovery or completion.
func observeNativeTerminalUsage15(ctx context.Context, terminal []byte) {
	state := nativeTerminalAttemptFrom15(ctx)
	if state == nil {
		return
	}
	usage := safeOpenAIFailureUsage(gjson.GetBytes(terminal, "usage"))
	if usage == nil {
		return
	}
	state.mu.Lock()
	state.observedUsage = mergeStoredResponseUsage(state.observedUsage, usage)
	state.mu.Unlock()
}
func nativeTerminalUsage15(ctx context.Context) (*OpenAIUsage, bool) {
	state := nativeTerminalAttemptFrom15(ctx)
	if state == nil {
		return nil, false
	}
	state.mu.Lock()
	observed, ok := state.observedUsage.(map[string]any)
	state.mu.Unlock()
	if !ok || len(observed) == 0 {
		return nil, false
	}
	return storedResponseOpenAIUsage(observed), true
}
func syncNativeTerminalCaptureUsage15(ctx context.Context) {
	state := nativeTerminalAttemptFrom15(ctx)
	attempt := storedResponseAttemptFrom(ctx)
	if state == nil || attempt == nil {
		return
	}
	state.mu.Lock()
	observed, ok := state.observedUsage.(map[string]any)
	state.mu.Unlock()
	if !ok {
		return
	}
	// Wire counters retain raw JSON for serialization. Capture validation needs
	// decoded numbers when cleanup restores the same observed usage.
	raw, err := json.Marshal(observed)
	if err != nil {
		return
	}
	decoded, err := decodeStoredResponseJSON(raw)
	usage, ok := decoded.(map[string]any)
	if err != nil || !ok {
		return
	}
	attempt.capture.observedUsage = mergeStoredResponseUsage(attempt.capture.observedUsage, usage)
}
