package service

import (
	"encoding/json"
	"unicode/utf8"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

func storedResponseMetadata(value any) (map[string]any, error) {
	if value == nil {
		return nil, nil
	}
	metadata, ok := value.(map[string]any)
	if !ok || len(metadata) > 16 {
		return nil, storedResponseMetadataError()
	}
	for key, raw := range metadata {
		text, ok := raw.(string)
		if !ok || utf8.RuneCountInString(key) > 64 || utf8.RuneCountInString(text) > 512 {
			return nil, storedResponseMetadataError()
		}
	}
	return metadata, nil
}

func storedResponseMetadataError() error {
	return &managedfiles.ResponsesPreflightError{Status: 400, Type: "invalid_request_error", Code: "invalid_metadata", Param: "metadata", Message: "Metadata must contain at most 16 string pairs with keys up to 64 characters and values up to 512 characters."}
}

// localMetadata is captured before removal from the upstream request. Restoring
// it only for admission revalidation keeps mutable exported Parameters honest.
func restoreStoredResponseMetadata(object map[string]any, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	value, err := decodeStoredResponseJSON(raw)
	if err != nil {
		return err
	}
	if _, err := storedResponseMetadata(value); err != nil {
		return err
	}
	object["metadata"] = value
	return nil
}
