package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// StoredResponseInputItems projects the saved public current-request input into
// API item objects. Private recovery, backend identifiers and parent snapshots
// are deliberately outside this projection.
func StoredResponseInputItems(record *StoredResponse) ([]map[string]any, error) {
	if record == nil || record.PublicID == "" {
		return nil, ErrStoredResponseInvalid
	}
	input, err := decodeStoredResponseJSON(record.Input)
	if err != nil {
		return nil, err
	}
	if err := storedNativeRequestInput15(input); err != nil {
		return nil, err
	}
	var items []any
	if text, ok := input.(string); ok {
		items = []any{map[string]any{"role": "user", "content": text}}
	} else {
		items = input.([]any)
	}
	result := make([]map[string]any, 0, len(items))
	for index, raw := range items {
		item := raw.(map[string]any)
		kind, _ := item["type"].(string)
		prefix := "item_"
		if kind == "" || kind == "message" {
			prefix = "msg_"
			item["type"] = "message"
			parts := []any{}
			if text, ok := item["content"].(string); ok {
				parts = append(parts, map[string]any{"type": "input_text", "text": text})
			} else {
				parts = item["content"].([]any)
			}
			for _, rawPart := range parts {
				part := rawPart.(map[string]any)
				if part["type"] == "input_file" {
					continue
				}
				if item["role"] == "assistant" {
					part["type"], part["annotations"] = "output_text", []any{}
				} else {
					part["type"] = "input_text"
				}
			}
			item["content"], item["status"] = parts, "completed"
		}
		sum := sha256.Sum256([]byte(record.PublicID + "\x00" + strconv.Itoa(index)))
		item["id"] = prefix + hex.EncodeToString(sum[:16])
		result = append(result, item)
	}
	return result, nil
}

type storedResponseInputProjectionKey struct{}
type storedResponseInputProjection struct {
	owner     StoredResponseOwner
	accountID int64
	ids       map[string]bool
}

// Only the local resource handler installs this after a fresh owning-row read.
func WithStoredResponseInputItemsProjection(ctx context.Context, record *StoredResponse) context.Context {
	if record == nil {
		return ctx
	}
	items, err := StoredResponseInputItems(record)
	if err != nil {
		return ctx
	}
	projection := storedResponseInputProjection{owner: StoredResponseOwner{record.UserID, record.APIKeyID, record.GroupID}, accountID: record.AccountID, ids: map[string]bool{}}
	for _, item := range items {
		id, _ := item["id"].(string)
		if id != "" {
			projection.ids[id] = true
		}
	}
	return context.WithValue(ctx, storedResponseInputProjectionKey{}, projection)
}
func storedResponseInputProjectionOwns(ctx context.Context, session *ManagedDeploymentSession, ref ManagedDeploymentResourceRef) bool {
	projection, ok := ctx.Value(storedResponseInputProjectionKey{}).(storedResponseInputProjection)
	return ok && session != nil && ref.Kind == "item" && projection.owner.UserID == session.UserID && projection.owner.APIKeyID == session.APIKeyID && projection.owner.GroupID == session.GroupID && projection.accountID == session.Binding.AccountID && projection.ids[ref.ID]
}
