package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"
)

// StoredResponseRequest contains only normalized client content. UpstreamBody
// always carries store:false: local persistence never promises upstream storage.
type StoredResponseRequest struct {
	Store                           bool
	Model                           string
	previousResponseID              string
	Input, Parameters, UpstreamBody json.RawMessage
	localMetadata                   json.RawMessage
}

func NormalizeStoredResponseRequest(body []byte) (*StoredResponseRequest, error) {
	value, err := decodeStoredResponseJSON(body)
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, ErrStoredResponseInvalid
	}
	store := true
	if v, exists := object["store"]; exists {
		var valid bool
		store, valid = v.(bool)
		if !valid {
			return nil, ErrStoredResponseInvalid
		}
	}
	model, ok := object["model"].(string)
	if !ok || model == "" {
		return nil, ErrStoredResponseInvalid
	}
	result := &StoredResponseRequest{Store: store, Model: model}
	object["store"] = false
	if !store {
		result.UpstreamBody, err = json.Marshal(object)
		return result, err
	}
	if !storedResponseTextInput(object["input"]) {
		return nil, ErrStoredResponseUnsupported
	}
	params := map[string]any{}
	for key, value := range object {
		switch key {
		case "model", "input", "store":
		case "stream":
			if _, ok := value.(bool); !ok {
				return nil, ErrStoredResponseInvalid
			}
		case "instructions", "prompt_cache_key":
			if _, ok := value.(string); !ok {
				return nil, ErrStoredResponseInvalid
			}
			params[key] = value
		case "text", "reasoning":
			params[key] = value
		case "reasoning_effort", "tool_choice", "truncation":
			if _, ok := value.(string); !ok {
				return nil, ErrStoredResponseInvalid
			}
			// Existing BPS policy still validates effective option semantics.
			params[key] = value
		case "tools", "include":
			list, ok := value.([]any)
			if !ok || len(list) != 0 {
				return nil, ErrStoredResponseUnsupported
			}
		case "metadata":
			metadata, err := storedResponseMetadata(value)
			if err != nil {
				return nil, err
			}
			if metadata != nil {
				params[key] = metadata
				result.localMetadata, err = json.Marshal(metadata)
				if err != nil {
					return nil, err
				}
			}
		case "stream_options":
			v, ok := value.(map[string]any)
			if !ok || len(v) != 0 {
				return nil, ErrStoredResponseUnsupported
			}
		case "background", "parallel_tool_calls":
			if flag, ok := value.(bool); !ok || flag {
				return nil, ErrStoredResponseUnsupported
			}
		default:
			return nil, ErrStoredResponseUnsupported
		}
	}
	if err := validateStoredResponseNested(params); err != nil {
		return nil, err
	}
	result.Input, err = json.Marshal(object["input"])
	if err != nil {
		return nil, err
	}
	result.Parameters, err = json.Marshal(params)
	if err != nil {
		return nil, err
	}
	// Client tags are local resource metadata, not BPS task metadata.
	delete(object, "metadata")
	result.UpstreamBody, err = json.Marshal(object)
	return result, err
}

func storedResponseTextInput(value any) bool {
	if _, ok := value.(string); ok {
		return true
	}
	items, ok := value.([]any)
	if !ok {
		return false
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		for key := range item {
			if key != "type" && key != "role" && key != "content" {
				return false
			}
		}
		if kind, exists := item["type"]; exists && kind != "message" {
			return false
		}
		switch item["role"] {
		case "user", "assistant", "system", "developer":
		default:
			return false
		}
		if _, ok := item["content"].(string); ok {
			continue
		}
		parts, ok := item["content"].([]any)
		if !ok {
			return false
		}
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				return false
			}
			for key := range part {
				if key != "type" && key != "text" {
					return false
				}
			}
			if part["type"] != "input_text" && part["type"] != "output_text" {
				return false
			}
			if _, ok := part["text"].(string); !ok {
				return false
			}
		}
	}
	return true
}

// Reject duplicate decoded keys and malformed UTF-8 before normalization. This
// prevents different policy/storage consumers choosing different JSON values.
func decodeStoredResponseJSON(raw []byte) (any, error) {
	if !utf8.Valid(raw) {
		return nil, ErrStoredResponseInvalid
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var read func(int) (any, error)
	read = func(depth int) (any, error) {
		if depth > 64 {
			return nil, ErrStoredResponseInvalid
		}
		tok, err := dec.Token()
		if err != nil {
			return nil, ErrStoredResponseInvalid
		}
		delim, compound := tok.(json.Delim)
		if !compound {
			return tok, nil
		}
		switch delim {
		case '{':
			out := map[string]any{}
			for dec.More() {
				keyToken, e := dec.Token()
				if e != nil {
					return nil, ErrStoredResponseInvalid
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, ErrStoredResponseInvalid
				}
				if _, exists := out[key]; exists {
					return nil, ErrStoredResponseInvalid
				}
				v, e := read(depth + 1)
				if e != nil {
					return nil, e
				}
				out[key] = v
			}
			end, e := dec.Token()
			if e != nil || end != json.Delim('}') {
				return nil, ErrStoredResponseInvalid
			}
			return out, nil
		case '[':
			out := []any{}
			for dec.More() {
				v, e := read(depth + 1)
				if e != nil {
					return nil, e
				}
				out = append(out, v)
			}
			end, e := dec.Token()
			if e != nil || end != json.Delim(']') {
				return nil, ErrStoredResponseInvalid
			}
			return out, nil
		default:
			return nil, ErrStoredResponseInvalid
		}
	}
	value, err := read(0)
	if err != nil {
		return nil, err
	}
	if _, err = dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: trailing JSON", ErrStoredResponseInvalid)
	}
	return value, nil
}
