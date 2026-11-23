package service

import "encoding/json"

// expandFailureDiagnostic only accepts the existing gateway's bounded diagnostic
// envelope. Identity/model defaults come from this local admitted request.
func (c *StoredResponseCapture) expandFailureDiagnostic(envelope, response map[string]any) error {
	if len(envelope) != 2 || response["status"] != "failed" {
		return ErrStoredResponseInvalid
	}
	failure, ok := envelope["error"].(map[string]any)
	if !ok {
		return ErrStoredResponseInvalid
	}
	for key, value := range failure {
		switch key {
		case "type", "code", "message", "param":
			if value != nil {
				if _, ok := value.(string); !ok {
					return ErrStoredResponseInvalid
				}
			}
		default:
			return ErrStoredResponseUnsupported
		}
	}
	if _, ok := failure["message"].(string); !ok {
		return ErrStoredResponseInvalid
	}
	for key := range response {
		switch key {
		case "id", "status", "usage":
		default:
			return ErrStoredResponseUnsupported
		}
	}
	response["object"] = "response"
	response["model"] = c.record.Model
	response["output"] = []any{}
	response["error"] = failure
	return nil
}

// observeProgress retains only validated public text and usage in bounded memory.
// No original native envelope, tool state or arbitrary extension is captured.
func (c *StoredResponseCapture) observeProgress(event string, object map[string]any) error {
	output := c.partialOutput
	usage := c.observedUsage
	if raw, exists := object["response"]; exists {
		response, ok := raw.(map[string]any)
		if !ok {
			return ErrStoredResponseInvalid
		}
		if err := validateStoredPublicResponse(response); err != nil {
			return err
		}
		if items, ok := response["output"].([]any); ok && len(items) > 0 {
			output = items
		}
		if v, ok := response["usage"].(map[string]any); ok {
			usage = mergeStoredResponseUsage(usage, v)
		}
	}
	switch event {
	case "response.output_text.delta", "response.output_text.done", "response.refusal.delta", "response.refusal.done":
		index, err := storedProgressIndex(object["output_index"])
		if err != nil {
			return err
		}
		partIndex, err := storedProgressIndex(object["content_index"])
		if err != nil {
			return err
		}
		// Copy the public structure before mutation so an invalid/oversize event
		// cannot alter the last validated partial snapshot.
		raw, _ := json.Marshal(output)
		if output == nil {
			raw = []byte("[]")
		}
		value, err := decodeStoredResponseJSON(raw)
		if err != nil {
			return err
		}
		output = value.([]any)
		for len(output) <= index {
			output = append(output, map[string]any{"type": "message", "role": "assistant", "content": []any{}})
		}
		item, ok := output[index].(map[string]any)
		if !ok {
			return ErrStoredResponseInvalid
		}
		if id, ok := object["item_id"].(string); ok {
			item["id"] = id
		}
		parts, ok := item["content"].([]any)
		if !ok {
			return ErrStoredResponseInvalid
		}
		for len(parts) <= partIndex {
			parts = append(parts, map[string]any{"type": "output_text", "text": "", "annotations": []any{}})
		}
		part, ok := parts[partIndex].(map[string]any)
		if !ok {
			return ErrStoredResponseInvalid
		}
		field := "text"
		kind := "output_text"
		source := "delta"
		done := event == "response.output_text.done" || event == "response.refusal.done"
		if event == "response.refusal.delta" || event == "response.refusal.done" {
			field = "refusal"
			kind = "refusal"
		}
		if done {
			source = field
		}
		text, ok := object[source].(string)
		if !ok {
			return ErrStoredResponseInvalid
		}
		if !done {
			prior, _ := part[field].(string)
			text = prior + text
		}
		replacement := map[string]any{"type": kind, field: text}
		if kind == "output_text" {
			replacement["annotations"] = []any{}
		}
		parts[partIndex] = replacement
		item["content"] = parts
	case "response.output_item.added", "response.output_item.done":
		index, err := storedProgressIndex(object["output_index"])
		if err != nil {
			return err
		}
		item, ok := object["item"].(map[string]any)
		if !ok {
			return ErrStoredResponseInvalid
		}
		if err := validateStoredResponseOutput([]any{item}); err != nil {
			return err
		}
		// item.added typically has empty content. Do not discard deltas already seen.
		if event == "response.output_item.done" || index >= len(output) {
			raw, _ := json.Marshal(output)
			if output == nil {
				raw = []byte("[]")
			}
			value, err := decodeStoredResponseJSON(raw)
			if err != nil {
				return err
			}
			output = value.([]any)
			for len(output) <= index {
				output = append(output, map[string]any{"type": "message", "role": "assistant", "content": []any{}})
			}
			output[index] = item
		}
	}
	if output == nil {
		output = []any{}
	}
	if err := validateStoredResponseOutput(output); err != nil {
		return err
	}
	minimum, err := StoredResponseMinimumSnapshot(c.record, true)
	if err != nil {
		return err
	}
	value, err := decodeStoredResponseJSON(minimum)
	if err != nil {
		return err
	}
	snapshot := value.(map[string]any)
	snapshot["output"], snapshot["usage"] = output, usage
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if int64(len(c.record.Input)+len(c.record.Parameters)+len(raw)) > c.service.cfg.Limits.MaxResponseBytes {
		return ErrStoredResponseTooLarge
	}
	c.partialOutput = output
	c.observedUsage = usage
	return nil
}

// Missing counters remain unknown or retain their last observed value. Clone
// both levels before merging so rejected oversized snapshots cannot mutate facts.
func mergeStoredResponseUsage(previous any, update map[string]any) any {
	merged := make(map[string]any)
	if prior, ok := previous.(map[string]any); ok {
		for key, value := range prior {
			merged[key] = value
		}
	}
	for key, value := range update {
		if details, ok := value.(map[string]any); ok {
			if len(details) == 0 {
				continue
			}
			nested := make(map[string]any)
			if prior, ok := merged[key].(map[string]any); ok {
				for field, count := range prior {
					nested[field] = count
				}
			}
			for field, count := range details {
				nested[field] = count
			}
			merged[key] = nested
		} else {
			merged[key] = value
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

func storedProgressIndex(value any) (int, error) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, ErrStoredResponseInvalid
	}
	index, err := number.Int64()
	if err != nil || index < 0 || index >= 128 {
		return 0, ErrStoredResponseInvalid
	}
	return int(index), nil
}
