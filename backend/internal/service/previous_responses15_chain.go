package service

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// PreviousResponseTextAuthorize15 must recheck the current key, group, public
// model and bound account. It is called for every retained ancestor before this
// resolver returns bytes; no upload, admission or generation happens here.
type PreviousResponseTextAuthorize15 func(context.Context, StoredResponseOwner, *StoredResponse) error

// ResolvePreviousResponseText15 is the internal text tracer boundary. Public
// previous_response_id remains rejected until native material and file recovery
// share this atomic storage/admission path. The returned account must be pinned
// and reauthorized immediately before send, never replaced by a new backend.
func (s *StoredResponseService) ResolvePreviousResponseText15(ctx context.Context, owner StoredResponseOwner, id string, current []byte, authorize PreviousResponseTextAuthorize15) ([]byte, int64, error) {
	if s == nil || !s.Enabled() || s.repo == nil {
		return nil, 0, ErrStoredResponsesDisabled
	}
	if !validStoredResponseOwner(owner) || authorize == nil {
		return nil, 0, ErrBPSRequestAuthorizationUnavailable
	}
	if int64(len(current)) > s.cfg.Limits.MaxResponseBytes {
		return nil, 0, ErrStoredResponseTooLarge
	}
	request, err := normalizePreviousResponseCurrent15(current)
	if err != nil {
		return nil, 0, err
	}
	seen := map[string]bool{}
	chain := make([]*previousResponseText15, 0)
	var account int64
	var resolved string
	remaining := s.cfg.Limits.MaxResponseBytes
	for id != "" {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if !previousResponseID15(id) {
			return nil, 0, ErrStoredResponseNotFound
		}
		// Reserve one level for this request, matching repository depth32.
		if seen[id] || len(chain) >= 31 {
			return nil, 0, ErrStoredResponseTooLarge
		}
		seen[id] = true
		record, err := s.getForKeyOwner(ctx, owner, id)
		if err != nil {
			return nil, 0, err
		}
		if record == nil || record.PublicID != id || record.UserID != owner.UserID || record.APIKeyID != owner.APIKeyID || record.GroupID != owner.GroupID || record.DeletedAt != nil || !record.ExpiresAt.After(time.Now()) {
			return nil, 0, ErrStoredResponseNotFound
		}
		if record.State != "completed" || record.ExecutionState != "terminal" || record.RecoveryVersion != 1 || record.AccountID <= 0 {
			return nil, 0, ErrStoredResponseUnsupported
		}
		if record.Model != request.Model {
			return nil, 0, ErrBPSRequestAuthorizationRevoked
		}
		if account == 0 {
			account, resolved = record.AccountID, record.ResolvedModel
		} else if account != record.AccountID || resolved != record.ResolvedModel {
			return nil, 0, ErrBPSRequestAuthorizationRevoked
		}
		if err = authorize(ctx, owner, record); err != nil {
			return nil, 0, err
		}
		if err = ctx.Err(); err != nil {
			return nil, 0, err
		}
		if int64(len(record.Recovery)) > remaining {
			return nil, 0, ErrStoredResponseTooLarge
		}
		remaining -= int64(len(record.Recovery))
		material, err := readPreviousResponseText15(record.Recovery, s.cfg.Limits.MaxResponseBytes)
		if err != nil {
			return nil, 0, err
		}
		chain = append(chain, material)
		id = record.PreviousResponseID
	}
	if account == 0 {
		return nil, 0, ErrStoredResponseNotFound
	}
	history := make([]any, 0)
	for index := len(chain) - 1; index >= 0; index-- {
		for _, raw := range []json.RawMessage{chain[index].Input, chain[index].Output} {
			value, err := decodeStoredResponseJSON(raw)
			if err != nil {
				return nil, 0, err
			}
			if text, ok := value.(string); ok {
				history = append(history, map[string]any{"type": "message", "role": "user", "content": text})
			} else {
				history = append(history, value.([]any)...)
			}
			if len(history) > 4096 {
				return nil, 0, ErrStoredResponseTooLarge
			}
		}
	}
	currentValue, err := decodeStoredResponseJSON(request.UpstreamBody)
	if err != nil {
		return nil, 0, err
	}
	object := currentValue.(map[string]any)
	if !storedResponseTextInput(object["input"]) {
		return nil, 0, ErrStoredResponseUnsupported
	}
	if text, ok := object["input"].(string); ok {
		history = append(history, map[string]any{"type": "message", "role": "user", "content": text})
	} else {
		history = append(history, object["input"].([]any)...)
	}
	if len(history) > 4096 {
		return nil, 0, ErrStoredResponseTooLarge
	}
	object["input"] = history
	body, err := json.Marshal(object)
	if err != nil {
		return nil, 0, err
	}
	if int64(len(body)) > s.cfg.Limits.MaxResponseBytes {
		return nil, 0, ErrStoredResponseTooLarge
	}
	return body, account, nil
}

func previousResponseID15(id string) bool {
	if len(id) != 43 || !strings.HasPrefix(id, "resp_local_") {
		return false
	}
	for _, c := range id[11:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
