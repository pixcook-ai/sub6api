package handler

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/tidwall/gjson"
)

func (h *OpenAIGatewayHandler) nativePreviousResponsesEnabled15() bool {
	return h != nil && h.cfg != nil && h.cfg.Gateway.StoredResponses.Enabled && h.cfg.Gateway.StoredResponses.NativePreviousResponsesEnabled
}
func validLocalResponseID15(id string) bool {
	if len(id) != len("resp_local_")+32 || !strings.HasPrefix(id, "resp_local_") || strings.ToLower(id) != id {
		return false
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(id, "resp_local_"))
	return err == nil && len(raw) == 16
}

// Ordinary upstream IDs retain the existing owner/scheduler path. A local ID
// opts into strict duplicate-aware parsing; only this field is removed.
func hasLocalPreviousCandidate15(body []byte) bool {
	found := false
	gjson.ParseBytes(body).ForEach(func(key, value gjson.Result) bool {
		if strings.EqualFold(key.String(), "previous_response_id") && value.Type == gjson.String && strings.HasPrefix(strings.TrimSpace(value.String()), "resp_local_") {
			found = true
			return false
		}
		return true
	})
	return found
}
func parseLocalPreviousRequest15(body []byte) (id string, current []byte, local bool, err error) {
	if !hasLocalPreviousCandidate15(body) {
		return "", nil, false, nil
	}
	previous := gjson.GetBytes(body, "previous_response_id")
	id = previous.String()
	if !validLocalResponseID15(id) {
		return "", nil, true, service.ErrStoredResponseInvalid
	}
	value, err := managedfiles.Parse(body)
	if err != nil {
		return "", nil, true, service.ErrStoredResponseInvalid
	}
	object, ok := value.(map[string]any)
	if !ok {
		return "", nil, true, service.ErrStoredResponseInvalid
	}
	if exact, ok := object["previous_response_id"].(string); !ok || exact != id {
		return "", nil, true, service.ErrStoredResponseInvalid
	}
	delete(object, "previous_response_id")
	current, err = json.Marshal(object)
	if err != nil {
		return "", nil, true, service.ErrStoredResponseInvalid
	}
	request, err := service.NormalizeStoredResponseNativeRequest15(current)
	if err != nil {
		return "", nil, true, err
	}
	if !request.Store {
		return "", nil, true, service.ErrStoredResponseUnsupported
	}
	return id, current, true, nil
}
func nativeHistoryLimit15(h *OpenAIGatewayHandler) int64 {
	if h == nil || h.cfg == nil {
		return 0
	}
	limit := h.cfg.Gateway.StoredResponses.MaxResponseBytes
	if limit > managedfiles.NativeResponsesMaxBytes15 {
		limit = managedfiles.NativeResponsesMaxBytes15
	}
	return limit
}

// This projection is used only for content audit and inflight estimation.
// Current instructions remain current; replay pairs/native OfficeJS never enter it.
func nativeHistoryAuditBody15(current, snapshot []byte, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 || int64(len(current)) > maxBytes || int64(len(snapshot)) > maxBytes {
		return nil, service.ErrStoredResponseTooLarge
	}
	value, err := managedfiles.Parse(current)
	if err != nil {
		return nil, service.ErrStoredResponseInvalid
	}
	request, ok := value.(map[string]any)
	if !ok {
		return nil, service.ErrStoredResponseInvalid
	}
	value, err = managedfiles.Parse(snapshot)
	if err != nil {
		return nil, service.ErrStoredResponseInvalid
	}
	root, ok := value.(map[string]any)
	if !ok {
		return nil, service.ErrStoredResponseInvalid
	}
	var turns []any
	switch root["version"] {
	case json.Number("1"), json.Number("4"):
		turns = []any{root}
	case json.Number("2"):
		turns, ok = root["turns"].([]any)
		if !ok {
			return nil, service.ErrStoredResponseInvalid
		}
	default:
		return nil, service.ErrStoredResponseUnsupported
	}
	if len(turns) == 0 || len(turns) > 31 {
		return nil, service.ErrStoredResponseTooLarge
	}
	input := make([]any, 0)
	var inheritedTools []any
	for _, raw := range turns {
		turn, ok := raw.(map[string]any)
		if !ok {
			return nil, service.ErrStoredResponseInvalid
		}
		inheritedTools, ok = turn["tools"].([]any)
		if !ok {
			return nil, service.ErrStoredResponseInvalid
		}
		for _, field := range []string{"input", "output"} {
			items, ok := turn[field].([]any)
			if !ok {
				return nil, service.ErrStoredResponseInvalid
			}
			input = append(input, items...)
			if len(input) > 4096 {
				return nil, service.ErrStoredResponseTooLarge
			}
		}
	}
	switch items := request["input"].(type) {
	case string:
		input = append(input, map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": items}}})
	case []any:
		input = append(input, items...)
	default:
		return nil, service.ErrStoredResponseInvalid
	}
	if len(input) > 4096 {
		return nil, service.ErrStoredResponseTooLarge
	}
	request["input"] = input
	if _, exists := request["tools"]; !exists {
		request["tools"] = inheritedTools
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, service.ErrStoredResponseInvalid
	}
	if int64(len(encoded)) > maxBytes {
		return nil, service.ErrStoredResponseTooLarge
	}
	return encoded, nil
}
func (h *OpenAIGatewayHandler) recheckNativeHistoryParent15(ctx context.Context, owner service.StoredResponseOwner, id, publicModel, forwardModel string, previous *service.StoredResponseNativeParent15) (*service.StoredResponseNativeParent15, error) {
	if previous == nil || previous.Account == nil {
		return nil, service.ErrStoredResponseInvalid
	}
	current, err := h.gatewayService.ResolveStoredResponseNativeParent15(ctx, owner, id, publicModel, forwardModel)
	if err != nil {
		return nil, err
	}
	if current == nil || current.Account == nil || current.Account.ID != previous.Account.ID || current.Binding != previous.Binding || !bytes.Equal(current.Snapshot, previous.Snapshot) {
		return nil, service.ErrBPSRequestAuthorizationRevoked
	}
	if _, err = h.gatewayService.RecheckBPSStoredResponseRoute(ctx, &owner.GroupID, previous.Account, forwardModel, service.BPSStoredResponseRoute{Local: true}); err != nil {
		return nil, err
	}
	return current, nil
}

type nativeHistoryPinKey15 struct{}

func withNativeHistoryPin15(ctx context.Context, accountID int64) context.Context {
	return context.WithValue(ctx, nativeHistoryPinKey15{}, accountID)
}
func nativeHistoryPinned15(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	id, _ := ctx.Value(nativeHistoryPinKey15{}).(int64)
	return id > 0
}
