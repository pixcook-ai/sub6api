package managedfiles

import (
	"fmt"
	"strings"
)

// ToolFileSource is derived from the immutable request template. Together with
// Position it binds a file to the original call and input order without changing
// the versioned position JSON representation.
type ToolFileSource struct {
	Kind      string
	CallID    string
	CallInput int
}

// TypedFileContent selects only protocol-owned content; arguments, custom input
// and string outputs remain opaque.
func TypedFileContent(item map[string]any) ([]any, string) {
	field := "content"
	kind, _ := item["type"].(string)
	_, typed := item["type"]
	switch kind {
	case "message":
	case "":
		if _, ok := item["role"].(string); typed || !ok {
			return nil, ""
		}
	case "function_call_output", "custom_tool_call_output":
		field = "output"
	default:
		return nil, ""
	}
	parts, _ := item[field].([]any)
	return parts, field
}

// ValidateToolFileSources requires complete earlier calls when typed tool
// outputs contain files. Text-only and image-only histories keep their replay path.
func ValidateToolFileSources(input []any) (map[int]ToolFileSource, error) {
	sources := make(map[int]ToolFileSource)
	hasFile := false
	for _, raw := range input {
		item, _ := raw.(map[string]any)
		if item["type"] != "function_call_output" && item["type"] != "custom_tool_call_output" {
			continue
		}
		parts, _ := TypedFileContent(item)
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			hasFile = hasFile || part["type"] == "input_file"
		}
	}
	if !hasFile {
		return sources, nil
	}
	type call struct {
		kind  string
		index int
	}
	calls := make(map[string]call)
	callCount, outputCount := make(map[string]int), make(map[string]int)
	aliases := make(map[string]bool)
	for _, raw := range input {
		item, _ := raw.(map[string]any)
		kind, _ := item["type"].(string)
		id, _ := item["call_id"].(string)
		// Include aliased identity keys before canonical-kind filtering. A later
		// malformed duplicate must invalidate the same media call too.
		toolShaped, hasAlias := toolFileSourceAliases(item)
		if toolShaped && hasAlias {
			for key, rawID := range item {
				if strings.EqualFold(key, "call_id") {
					if sourceID, ok := rawID.(string); ok {
						aliases[sourceID] = true
					}
				}
			}
		}
		if kind != "function_call" && kind != "custom_tool_call" && kind != "function_call_output" && kind != "custom_tool_call_output" {
			continue
		}
		if kind == "function_call" || kind == "custom_tool_call" {
			callCount[id]++
		} else {
			outputCount[id]++
		}
	}
	for i, raw := range input {
		item, _ := raw.(map[string]any)
		kind, _ := item["type"].(string)
		id, _ := item["call_id"].(string)
		if kind == "function_call" || kind == "custom_tool_call" {
			if _, exists := calls[id]; !exists {
				calls[id] = call{kind, i}
			}
			continue
		}
		if kind != "function_call_output" && kind != "custom_tool_call_output" {
			continue
		}
		parts, _ := item["output"].([]any)
		mediaPart := -1
		for j, raw := range parts {
			part, _ := raw.(map[string]any)
			if part["type"] == "input_file" || part["type"] == "input_image" {
				mediaPart = j
				break
			}
		}
		if mediaPart < 0 {
			continue
		}
		original, exists := calls[id]
		expected := "function_call"
		if kind == "custom_tool_call_output" {
			expected = "custom_tool_call"
		}
		if !safeOpaque(id, 256) || !exists || original.kind != expected || callCount[id] != 1 || outputCount[id] != 1 || aliases[id] {
			return nil, reject("unsupported_managed_input", fmt.Sprintf("request_template.input[%d].output[%d]", i, mediaPart))
		}
		if _, exists := item["role"]; exists {
			return nil, reject("unsupported_managed_input", fmt.Sprintf("request_template.input[%d]", i))
		}
		if _, exists := item["content"]; exists {
			return nil, reject("unsupported_managed_input", fmt.Sprintf("request_template.input[%d].content", i))
		}
		sources[i] = ToolFileSource{Kind: kind, CallID: id, CallInput: original.index}
	}
	return sources, nil
}

// toolFileSourceAliases inspects only protocol identity keys, never opaque tool
// arguments, custom input or string results.
func toolFileSourceAliases(item map[string]any) (toolShaped, hasAlias bool) {
	for key, value := range item {
		if strings.EqualFold(key, "type") {
			kind, _ := value.(string)
			switch strings.ToLower(kind) {
			case "function_call", "custom_tool_call", "function_call_output", "custom_tool_call_output":
				toolShaped = true
				hasAlias = hasAlias || kind != strings.ToLower(kind)
			}
		}
		for _, field := range []string{"type", "call_id", "role", "content", "output", "name"} {
			if key != field && strings.EqualFold(key, field) {
				hasAlias = true
			}
		}
	}
	return toolShaped, hasAlias
}

// FilePartAt accesses a validated typed location without guessing its container.
func FilePartAt(root map[string]any, position Position) (map[string]any, error) {
	input, _ := root["input"].([]any)
	if position.Input < 0 || position.Input >= len(input) || position.Part < 0 {
		return nil, reject("invalid_managed_position", "input")
	}
	item, _ := input[position.Input].(map[string]any)
	parts, _ := TypedFileContent(item)
	if position.Part >= len(parts) {
		return nil, reject("invalid_managed_position", "input")
	}
	part, _ := parts[position.Part].(map[string]any)
	if part["type"] != "input_file" {
		return nil, reject("invalid_managed_position", "input")
	}
	return part, nil
}
