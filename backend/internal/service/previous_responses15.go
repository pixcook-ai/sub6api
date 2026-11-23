package service

import (
	"bytes"
	"encoding/json"
)

// previousResponseText15 is a private recovery format. It excludes request-level
// instructions and display-only message IDs. Native tool/file recovery needs a
// different versioned contract and cannot be inferred from public output.
type previousResponseText15 struct {
	Version int             `json:"version"`
	Kind    string          `json:"kind"`
	Input   json.RawMessage `json:"input"`
	Output  json.RawMessage `json:"output"`
}

// BuildStoredResponseTextRecovery15 builds only canonical client text history.
// Callers must still authorize the resource before reading or replaying it.
func BuildStoredResponseTextRecovery15(input, public []byte, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 || int64(len(input)) > maxBytes || int64(len(public)) > maxBytes {
		return nil, ErrStoredResponseTooLarge
	}
	value, err := decodeStoredResponseJSON(input)
	if err != nil || !storedResponseTextInput(value) {
		return nil, ErrStoredResponseUnsupported
	}
	responseValue, err := decodeStoredResponseJSON(public)
	if err != nil {
		return nil, err
	}
	response, ok := responseValue.(map[string]any)
	if !ok || response["object"] != "response" || response["status"] != "completed" {
		return nil, ErrStoredResponseUnsupported
	}
	if err := validateStoredPublicResponse(response); err != nil {
		return nil, err
	}
	output := response["output"].([]any)
	replay := make([]any, 0, len(output))
	for _, raw := range output {
		item := raw.(map[string]any)
		if status, exists := item["status"]; exists && status != "completed" {
			return nil, ErrStoredResponseUnsupported
		}
		content := make([]any, 0)
		for _, partRaw := range item["content"].([]any) {
			part := partRaw.(map[string]any)
			switch part["type"] {
			case "output_text":
				content = append(content, map[string]any{"type": "output_text", "text": part["text"]})
			case "refusal":
				// The text-only input contract carries refusal text as assistant
				// text; the public display retains the original refusal type.
				content = append(content, map[string]any{"type": "output_text", "text": part["refusal"]})
			default:
				return nil, ErrStoredResponseUnsupported
			}
		}
		replay = append(replay, map[string]any{"type": "message", "role": "assistant", "content": content})
	}
	canonicalInput, _ := json.Marshal(value)
	canonicalOutput, _ := json.Marshal(replay)
	encoded, err := json.Marshal(previousResponseText15{1, "text", canonicalInput, canonicalOutput})
	if err != nil {
		return nil, err
	}
	if int64(len(encoded)) > maxBytes {
		return nil, ErrStoredResponseTooLarge
	}
	return encoded, nil
}

// ValidateStoredResponseTextRecovery15 re-derives the approved text-only material
// at the durable repository boundary, rejecting stale or injected private fields.
func ValidateStoredResponseTextRecovery15(input, public, recovery []byte, maxBytes int64) error {
	expected, err := BuildStoredResponseTextRecovery15(input, public, maxBytes)
	if err != nil {
		return err
	}
	actual, err := readPreviousResponseText15(recovery, maxBytes)
	if err != nil {
		return err
	}
	canonical, _ := json.Marshal(actual)
	if !bytes.Equal(expected, canonical) {
		return ErrStoredResponseInvalid
	}
	return nil
}

func readPreviousResponseText15(raw []byte, maxBytes int64) (*previousResponseText15, error) {
	if maxBytes <= 0 || int64(len(raw)) > maxBytes {
		return nil, ErrStoredResponseTooLarge
	}
	value, err := decodeStoredResponseJSON(raw)
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok || len(object) != 4 {
		return nil, ErrStoredResponseInvalid
	}
	for key := range object {
		if key != "version" && key != "kind" && key != "input" && key != "output" {
			return nil, ErrStoredResponseInvalid
		}
	}
	if object["version"] != json.Number("1") || object["kind"] != "text" || !storedResponseTextInput(object["input"]) {
		return nil, ErrStoredResponseUnsupported
	}
	if err := validateStoredResponseOutput(object["output"]); err != nil {
		return nil, err
	}
	input, _ := json.Marshal(object["input"])
	output, _ := json.Marshal(object["output"])
	return &previousResponseText15{1, "text", input, output}, nil
}

// expandPreviousResponseText15 is an internal tracer seam. Its input is already
// authorized private material. It does not enable public previous_response_id or
// replace fresh owner/model/account checks in the chain authorizer.
func expandPreviousResponseText15(material, current []byte, maxBytes int64) ([]byte, error) {
	previous, err := readPreviousResponseText15(material, maxBytes)
	if err != nil {
		return nil, err
	}
	if int64(len(current)) > maxBytes {
		return nil, ErrStoredResponseTooLarge
	}
	request, err := normalizePreviousResponseCurrent15(current)
	if err != nil {
		return nil, err
	}
	value, err := decodeStoredResponseJSON(request.UpstreamBody)
	if err != nil {
		return nil, err
	}
	object := value.(map[string]any)
	if !storedResponseTextInput(object["input"]) {
		return nil, ErrStoredResponseUnsupported
	}
	appendInput := func(target []any, raw json.RawMessage) ([]any, error) {
		value, e := decodeStoredResponseJSON(raw)
		if e != nil {
			return nil, e
		}
		if text, ok := value.(string); ok {
			return append(target, map[string]any{"type": "message", "role": "user", "content": text}), nil
		}
		items, ok := value.([]any)
		if !ok {
			return nil, ErrStoredResponseUnsupported
		}
		return append(target, items...), nil
	}
	history, err := appendInput(nil, previous.Input)
	if err != nil {
		return nil, err
	}
	history, err = appendInput(history, previous.Output)
	if err != nil {
		return nil, err
	}
	currentInput, _ := json.Marshal(object["input"])
	history, err = appendInput(history, currentInput)
	if err != nil {
		return nil, err
	}
	object["input"] = history
	encoded, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	if int64(len(encoded)) > maxBytes {
		return nil, ErrStoredResponseTooLarge
	}
	return encoded, nil
}

// Stateless ordinary requests may use tools, but this tracer must validate the
// same pure-text contract regardless of the current round's storage preference.
func normalizePreviousResponseCurrent15(raw []byte) (*StoredResponseRequest, error) {
	value, err := decodeStoredResponseJSON(raw)
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, ErrStoredResponseInvalid
	}
	if store, exists := object["store"]; exists {
		if _, ok := store.(bool); !ok {
			return nil, ErrStoredResponseInvalid
		}
	}
	object["store"] = true
	canonical, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	return NormalizeStoredResponseRequest(canonical)
}
