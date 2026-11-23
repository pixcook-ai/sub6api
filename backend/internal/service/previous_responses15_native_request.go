package service

import (
	"encoding/json"
	"strings"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

// NormalizeStoredResponseNativeRequest15 is an explicit, reduced native-history
// contract. It preserves only the current delta and never expands ancestors.
// It performs no account authorization, preflight or native replay admission.
// The ordinary pure-text normalizer remains unchanged.
func NormalizeStoredResponseNativeRequest15(body []byte) (*StoredResponseRequest, error) {
	value, err := decodeStoredResponseJSON(body)
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, ErrStoredResponseInvalid
	}
	for _, field := range []string{"previous_response_id", "conversation", "additional_tools"} {
		if _, exists := object[field]; exists {
			return nil, ErrStoredResponseUnsupported
		}
	}
	store := true
	if raw, exists := object["store"]; exists {
		var valid bool
		store, valid = raw.(bool)
		if !valid {
			return nil, ErrStoredResponseInvalid
		}
	}
	if err = storedNativeRequestInput15(object["input"]); err != nil {
		return nil, err
	}
	tools, hasTools := object["tools"]
	if hasTools {
		count := 0
		if err = storedNativeRequestTools15(tools, "", false, &count, map[string]bool{}); err != nil {
			return nil, err
		}
	}
	choice, hasChoice := object["tool_choice"]
	if hasChoice {
		if err = storedNativeRequestChoice15(choice); err != nil {
			return nil, err
		}
	}
	parallel, hasParallel := object["parallel_tool_calls"]
	if hasParallel {
		if _, ok := parallel.(bool); !ok {
			return nil, ErrStoredResponseInvalid
		}
	}
	// Let the existing contract check all common request fields, even when the
	// caller's store:false body is being revalidated before private admission.
	common := make(map[string]any, len(object))
	for key, raw := range object {
		common[key] = raw
	}
	common["store"], common["input"], common["tools"] = true, "", []any{}
	if hasChoice {
		common["tool_choice"] = "auto"
	}
	if hasParallel {
		common["parallel_tool_calls"] = false
	}
	commonRaw, err := json.Marshal(common)
	if err != nil {
		return nil, err
	}
	checked, err := NormalizeStoredResponseRequest(commonRaw)
	if err != nil {
		return nil, err
	}
	parametersValue, err := decodeStoredResponseJSON(checked.Parameters)
	if err != nil {
		return nil, err
	}
	parameters := parametersValue.(map[string]any)
	if hasTools {
		parameters["tools"] = tools
	}
	if hasChoice {
		parameters["tool_choice"] = choice
	}
	if hasParallel {
		parameters["parallel_tool_calls"] = parallel
	}
	inputRaw, err := json.Marshal(object["input"])
	if err != nil {
		return nil, err
	}
	paramsRaw, err := json.Marshal(parameters)
	if err != nil {
		return nil, err
	}
	object["store"] = false
	wireRaw, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	inputRaw, err = CanonicalStoredResponseJSON15(inputRaw)
	if err != nil {
		return nil, err
	}
	paramsRaw, err = CanonicalStoredResponseJSON15(paramsRaw)
	if err != nil {
		return nil, err
	}
	wireRaw, err = CanonicalStoredResponseJSON15(wireRaw)
	if err != nil {
		return nil, err
	}
	return &StoredResponseRequest{Store: store, Model: checked.Model, Input: inputRaw, Parameters: paramsRaw, UpstreamBody: wireRaw}, nil
}

func storedNativeRequestName15(value any) bool {
	name, ok := value.(string)
	if !ok || name == "" || len(name) > 256 || strings.TrimSpace(name) != name {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func storedNativeRequestFields15(object map[string]any, fields string) error {
	allowed := map[string]bool{}
	for _, field := range strings.Fields(fields) {
		allowed[field] = true
	}
	for field := range object {
		if !allowed[field] {
			return ErrStoredResponseUnsupported
		}
	}
	return nil
}

func storedNativeRequestInput15(value any) error {
	if _, ok := value.(string); ok {
		return nil
	}
	items, ok := value.([]any)
	if !ok {
		return ErrStoredResponseUnsupported
	}
	if len(items) > 4096 {
		return ErrStoredResponseTooLarge
	}
	results := map[string]bool{}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return ErrStoredResponseInvalid
		}
		switch item["type"] {
		case nil, "message":
			if !storedNativeRequestMessage15(item) {
				return ErrStoredResponseUnsupported
			}
		case "function_call_output", "custom_tool_call_output":
			if err := storedNativeRequestFields15(item, "type id call_id output status"); err != nil {
				return err
			}
			if !storedNativeRequestName15(item["call_id"]) {
				return ErrStoredResponseInvalid
			}
			id := item["call_id"].(string)
			if results[id] {
				return ErrStoredResponseInvalid
			}
			results[id] = true
			if id, exists := item["id"]; exists && !storedNativeRequestName15(id) {
				return ErrStoredResponseInvalid
			}
			if status, exists := item["status"]; exists && status != "completed" {
				return ErrStoredResponseUnsupported
			}
			if _, ok := item["output"].(string); ok {
				continue
			}
			if !storedNativeRequestContent15(item["output"]) {
				return ErrStoredResponseUnsupported
			}
		default:
			// Calls need original native evidence. Search, reasoning and
			// discovery need their own immutable provenance contract.
			return ErrStoredResponseUnsupported
		}
	}
	return nil
}

func storedNativeRequestMessage15(item map[string]any) bool {
	if storedNativeRequestFields15(item, "type role content") != nil {
		return false
	}
	if kind, exists := item["type"]; exists && kind != "message" {
		return false
	}
	switch item["role"] {
	case "user", "assistant", "system", "developer":
	default:
		return false
	}
	return storedNativeRequestContent15(item["content"])
}

// File IDs are syntax only here. Native admission separately requires fresh
// file ownership, leases and confirmed copy facts before any external send.
func storedNativeRequestContent15(value any) bool {
	if _, ok := value.(string); ok {
		return true
	}
	parts, ok := value.([]any)
	if !ok {
		return false
	}
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		if part["type"] == "input_file" {
			id, valid := part["file_id"].(string)
			if len(part) != 2 || !valid || !managedfiles.ValidRemoteID(id) {
				return false
			}
			continue
		}
		if !storedResponseTextInput([]any{map[string]any{"role": "user", "content": []any{part}}}) {
			return false
		}
	}
	return true
}

func storedNativeRequestTools15(value any, namespace string, nested bool, count *int, seen map[string]bool) error {
	tools, ok := value.([]any)
	if !ok {
		return ErrStoredResponseUnsupported
	}
	for _, raw := range tools {
		*count++
		if *count > 1024 {
			return ErrStoredResponseTooLarge
		}
		tool, ok := raw.(map[string]any)
		if !ok {
			return ErrStoredResponseInvalid
		}
		if !storedNativeRequestName15(tool["name"]) {
			return ErrStoredResponseInvalid
		}
		name := tool["name"].(string)
		if description, exists := tool["description"]; exists {
			if _, ok := description.(string); !ok {
				return ErrStoredResponseInvalid
			}
		}
		if tool["type"] == "namespace" {
			if nested {
				return ErrStoredResponseUnsupported
			}
			if err := storedNativeRequestFields15(tool, "type name description tools"); err != nil {
				return err
			}
			if err := storedNativeRequestTools15(tool["tools"], name, true, count, seen); err != nil {
				return err
			}
			continue
		}
		key := name
		if namespace != "" {
			key = namespace + "." + name
		}
		if seen[key] {
			return ErrStoredResponseInvalid
		}
		seen[key] = true
		switch tool["type"] {
		case "function":
			if err := storedNativeRequestFields15(tool, "type name description parameters strict"); err != nil {
				return err
			}
			if raw, exists := tool["parameters"]; exists {
				if _, ok := raw.(map[string]any); !ok {
					return ErrStoredResponseInvalid
				}
			}
			if raw, exists := tool["strict"]; exists && raw != nil {
				if _, ok := raw.(bool); !ok {
					return ErrStoredResponseInvalid
				}
			}
		case "custom":
			if err := storedNativeRequestFields15(tool, "type name description format"); err != nil {
				return err
			}
			if raw, exists := tool["format"]; exists {
				format, ok := raw.(map[string]any)
				if !ok {
					return ErrStoredResponseInvalid
				}
				switch format["type"] {
				case "text":
					if len(format) != 1 {
						return ErrStoredResponseUnsupported
					}
				case "grammar":
					if len(format) != 3 {
						return ErrStoredResponseUnsupported
					}
					if format["syntax"] != "lark" && format["syntax"] != "regex" {
						return ErrStoredResponseUnsupported
					}
					if _, ok := format["definition"].(string); !ok {
						return ErrStoredResponseInvalid
					}
				default:
					return ErrStoredResponseUnsupported
				}
			}
		default:
			return ErrStoredResponseUnsupported
		}
	}
	return nil
}

func storedNativeRequestChoice15(value any) error {
	if name, ok := value.(string); ok {
		if name == "auto" || name == "none" || name == "required" {
			return nil
		}
		return ErrStoredResponseUnsupported
	}
	choice, ok := value.(map[string]any)
	if !ok {
		return ErrStoredResponseInvalid
	}
	if err := storedNativeRequestFields15(choice, "type name namespace"); err != nil {
		return err
	}
	if choice["type"] != "function" && choice["type"] != "custom" {
		return ErrStoredResponseUnsupported
	}
	if !storedNativeRequestName15(choice["name"]) {
		return ErrStoredResponseInvalid
	}
	if namespace, exists := choice["namespace"]; exists && !storedNativeRequestName15(namespace) {
		return ErrStoredResponseInvalid
	}
	return nil
}
