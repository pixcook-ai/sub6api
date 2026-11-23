package service

import "encoding/json"

// These are public events emitted by the real gateway bridge. Its private
// terminal receipt is consumed before CaptureEvent and is never public progress.
func nativeStoredResponseEvent15(event string) bool {
	switch event {
	case "response.created", "response.queued", "response.in_progress", "response.completed", "response.incomplete", "response.failed",
		"response.output_text.delta", "response.output_text.done", "response.refusal.delta", "response.refusal.done",
		"response.output_item.added", "response.output_item.done", "response.content_part.added", "response.content_part.done",
		"response.function_call_arguments.delta", "response.function_call_arguments.done", "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done":
		return true
	default:
		return false
	}
}

// observeNativeProgress15 validates public shape before release. It deliberately
// does not reconstruct text from sparse output_index/content_index deltas: tool
// positions and message positions share that index space. Only completed text
// items with stable IDs and validated usage enter an interrupted public snapshot.
// No partial/done tool item is recovery evidence without a signed full terminal.
func (c *StoredResponseCapture) observeNativeProgress15(event string, object map[string]any) error {
	if c == nil || c.record == nil || c.service == nil || c.record.RecoveryVersion != 2 || !c.store || c.finished {
		return ErrStoredResponseInvalid
	}
	if !nativeStoredResponseEvent15(event) || event == "response.completed" || event == "response.incomplete" || event == "response.failed" {
		return ErrStoredResponseUnsupported
	}
	if err := nativeStoredProgressEnvelope15(event, object); err != nil {
		return err
	}
	public, err := json.Marshal(object)
	if err != nil {
		return err
	}
	if int64(len(c.record.Input)+len(c.record.Parameters)+len(public)) > c.service.cfg.Limits.MaxResponseBytes {
		return ErrStoredResponseTooLarge
	}
	// Work on clones. A later invalid/oversized event cannot corrupt previously
	// retained completed text or validated usage.
	outputRaw, _ := json.Marshal(c.partialOutput)
	if c.partialOutput == nil {
		outputRaw = []byte("[]")
	}
	value, err := decodeStoredResponseJSON(outputRaw)
	if err != nil {
		return err
	}
	output := value.([]any)
	usage := c.observedUsage
	retain := func(item map[string]any) {
		id, ok := item["id"].(string)
		if !ok || id == "" || item["type"] != "message" {
			return
		}
		for index, raw := range output {
			previous, _ := raw.(map[string]any)
			if previous["id"] == id {
				output[index] = item
				return
			}
		}
		output = append(output, item)
	}
	if raw, exists := object["response"]; exists {
		response := raw.(map[string]any) // validated by nativeStoredProgressEnvelope15
		for _, raw := range response["output"].([]any) {
			item := raw.(map[string]any)
			if item["type"] == "message" && item["status"] == "completed" {
				retain(item)
			}
		}
		if observed, ok := response["usage"].(map[string]any); ok {
			usage = mergeStoredResponseUsage(usage, observed)
		}
	}
	if event == "response.output_item.done" {
		item := object["item"].(map[string]any)
		if item["type"] == "message" {
			retain(item)
		}
	}
	if err = validateStoredResponseOutput(output); err != nil {
		return err
	}
	minimum, err := StoredResponseMinimumSnapshot(c.record, true)
	if err != nil {
		return err
	}
	minimumValue, err := decodeStoredResponseJSON(minimum)
	if err != nil {
		return err
	}
	interrupted := minimumValue.(map[string]any)
	interrupted["output"], interrupted["usage"] = output, usage
	bounded, err := json.Marshal(interrupted)
	if err != nil {
		return err
	}
	if int64(len(c.record.Input)+len(c.record.Parameters)+len(bounded)) > c.service.cfg.Limits.MaxResponseBytes {
		return ErrStoredResponseTooLarge
	}
	c.partialOutput, c.observedUsage = output, usage
	return nil
}

func nativeStoredProgressEnvelope15(event string, object map[string]any) error {
	fields := "type sequence_number response_id"
	switch event {
	case "response.created", "response.queued", "response.in_progress":
		fields += " response"
	case "response.output_item.added", "response.output_item.done":
		fields += " output_index item"
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		fields += " output_index item_id delta"
	case "response.function_call_arguments.done":
		fields += " output_index item_id arguments"
	case "response.custom_tool_call_input.done":
		fields += " output_index item_id input"
	case "response.output_text.delta", "response.refusal.delta":
		fields += " output_index content_index item_id delta logprobs"
	case "response.output_text.done":
		fields += " output_index content_index item_id text logprobs"
	case "response.refusal.done":
		fields += " output_index content_index item_id refusal"
	case "response.content_part.added", "response.content_part.done":
		fields += " output_index content_index item_id part"
	default:
		return ErrStoredResponseUnsupported
	}
	if err := storedNativeRequestFields15(object, fields); err != nil {
		return err
	}
	if object["type"] != event {
		return ErrStoredResponseInvalid
	}
	if sequence, exists := object["sequence_number"]; exists && !storedNonnegativeInteger(sequence) {
		return ErrStoredResponseInvalid
	}
	if responseID, exists := object["response_id"]; exists && !storedNativeRequestName15(responseID) {
		return ErrStoredResponseInvalid
	}
	if raw, exists := object["response"]; exists {
		response, ok := raw.(map[string]any)
		if !ok {
			return ErrStoredResponseInvalid
		}
		if err := nativeStoredProgressResponse15(response); err != nil {
			return err
		}
		return nil
	}
	if event == "response.created" || event == "response.queued" || event == "response.in_progress" {
		return ErrStoredResponseInvalid
	}
	if err := nativeStoredProgressIndex15(object["output_index"]); err != nil {
		return err
	}
	if item, exists := object["item"]; exists {
		return nativeStoredProgressItem15(item, event == "response.output_item.done")
	}
	if !storedNativeRequestName15(object["item_id"]) {
		return ErrStoredResponseInvalid
	}
	if raw, exists := object["content_index"]; exists {
		if _, err := storedProgressIndex(raw); err != nil {
			return err
		}
	}
	if event == "response.content_part.added" || event == "response.content_part.done" {
		if _, exists := object["content_index"]; !exists {
			return ErrStoredResponseInvalid
		}
		return validateStoredResponseOutput([]any{map[string]any{"type": "message", "role": "assistant", "content": []any{object["part"]}}})
	}
	field := "delta"
	switch event {
	case "response.function_call_arguments.done":
		field = "arguments"
	case "response.custom_tool_call_input.done":
		field = "input"
	case "response.output_text.done":
		field = "text"
	case "response.refusal.done":
		field = "refusal"
	}
	text, ok := object[field].(string)
	if !ok {
		return ErrStoredResponseInvalid
	}
	if event == "response.function_call_arguments.done" {
		value, err := decodeStoredResponseJSON([]byte(text))
		if err != nil {
			return err
		}
		if _, ok := value.(map[string]any); !ok {
			return ErrStoredResponseInvalid
		}
	}
	if event == "response.output_text.delta" || event == "response.output_text.done" || event == "response.refusal.delta" || event == "response.refusal.done" {
		if _, exists := object["content_index"]; !exists {
			return ErrStoredResponseInvalid
		}
	}
	if raw, exists := object["logprobs"]; exists && raw != nil {
		list, ok := raw.([]any)
		if !ok || len(list) != 0 {
			return ErrStoredResponseUnsupported
		}
	}
	return nil
}

func nativeStoredProgressIndex15(raw any) error {
	index, ok := raw.(json.Number)
	if !ok {
		return ErrStoredResponseInvalid
	}
	value, err := index.Int64()
	if err != nil || value < 0 || value >= 4096 {
		return ErrStoredResponseInvalid
	}
	return nil
}

func nativeStoredProgressItem15(raw any, done bool) error {
	item, ok := raw.(map[string]any)
	if !ok {
		return ErrStoredResponseInvalid
	}
	if item["type"] == "message" {
		if err := validateStoredResponseOutput([]any{item}); err != nil {
			return err
		}
		if done {
			if status, exists := item["status"]; exists && status != "completed" {
				return ErrStoredResponseInvalid
			}
		}
		return nil
	}
	if item["type"] != "function_call" && item["type"] != "custom_tool_call" {
		return ErrStoredResponseUnsupported
	}
	if err := storedNativeRequestFields15(item, "type id call_id name namespace arguments input status encrypted_function_args"); err != nil {
		return err
	}
	for _, key := range []string{"id", "call_id", "name"} {
		if !storedNativeRequestName15(item[key]) {
			return ErrStoredResponseInvalid
		}
	}
	if namespace, exists := item["namespace"]; exists && !storedNativeRequestName15(namespace) {
		return ErrStoredResponseInvalid
	}
	status := "in_progress"
	if done {
		status = "completed"
	}
	if item["status"] != status {
		return ErrStoredResponseInvalid
	}
	if encrypted, exists := item["encrypted_function_args"]; exists {
		list, ok := encrypted.([]any)
		if !ok || len(list) != 0 {
			return ErrStoredResponseUnsupported
		}
	}
	if item["type"] == "function_call" {
		arguments, ok := item["arguments"].(string)
		if !ok {
			return ErrStoredResponseInvalid
		}
		if _, exists := item["input"]; exists {
			return ErrStoredResponseUnsupported
		}
		if done {
			value, err := decodeStoredResponseJSON([]byte(arguments))
			if err != nil {
				return err
			}
			if _, ok := value.(map[string]any); !ok {
				return ErrStoredResponseInvalid
			}
		}
	} else {
		if _, ok := item["input"].(string); !ok {
			return ErrStoredResponseInvalid
		}
		if _, exists := item["arguments"]; exists {
			return ErrStoredResponseUnsupported
		}
	}
	return nil
}

func nativeStoredProgressResponse15(response map[string]any) error {
	if response["object"] != "response" || (response["status"] != "in_progress" && response["status"] != "queued") {
		return ErrStoredResponseInvalid
	}
	output, ok := response["output"].([]any)
	if !ok || len(output) > 4096 {
		return ErrStoredResponseUnsupported
	}
	for _, raw := range output {
		item, ok := raw.(map[string]any)
		if !ok {
			return ErrStoredResponseInvalid
		}
		if err := nativeStoredProgressItem15(item, item["status"] == "completed"); err != nil {
			return err
		}
	}
	if tools, exists := response["tools"]; exists {
		count := 0
		if err := storedNativeRequestTools15(tools, "", false, &count, map[string]bool{}); err != nil {
			return err
		}
	}
	if choice, exists := response["tool_choice"]; exists {
		if err := storedNativeRequestChoice15(choice); err != nil {
			return err
		}
	}
	copy := make(map[string]any, len(response))
	for key, value := range response {
		copy[key] = value
	}
	copy["output"] = []any{}
	delete(copy, "tools")
	delete(copy, "tool_choice")
	return validateStoredPublicResponse(copy)
}
