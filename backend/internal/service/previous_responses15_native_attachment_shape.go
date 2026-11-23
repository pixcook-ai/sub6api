package service

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

// Attachment facts describe the whole expanded input at this turn. They never
// grant permission; each send still needs a fresh managed file admission.
func validateStoredNativeAttachmentTurn15(turns []map[string]any, record *StoredResponse, subject, backend, generation string) error {
	if len(turns) == 0 || record == nil {
		return ErrStoredResponseInvalid
	}
	var expanded []any
	var sourceTurns []int
	for index, turn := range turns {
		for _, field := range []string{"input", "output"} {
			if index == len(turns)-1 && field == "output" {
				break
			}
			items, ok := turn[field].([]any)
			if !ok || len(expanded)+len(items) > 4096 {
				return ErrStoredResponseInvalid
			}
			expanded = append(expanded, items...)
			for range items {
				sourceTurns = append(sourceTurns, index)
			}
		}
	}
	type coordinate struct{ input, part int }
	wanted := map[coordinate]string{}
	for i, raw := range expanded {
		item, ok := raw.(map[string]any)
		if !ok {
			return ErrStoredResponseInvalid
		}
		parts, _ := managedfiles.TypedFileContent(item)
		for j, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			for key, value := range part {
				if strings.EqualFold(key, "file_id") && key != "file_id" || strings.EqualFold(key, "type") && key != "type" {
					return ErrStoredResponseInvalid
				}
				if key == "type" {
					kind, _ := value.(string)
					if strings.EqualFold(kind, "input_file") && kind != "input_file" {
						return ErrStoredResponseInvalid
					}
				}
			}
			if part["type"] == "input_image" {
				return ErrStoredResponseUnsupported
			}
			if part["type"] != "input_file" {
				continue
			}
			id, ok := part["file_id"].(string)
			if len(part) != 2 || !ok || !managedfiles.ValidRemoteID(id) {
				return ErrStoredResponseInvalid
			}
			wanted[coordinate{i, j}] = id
		}
	}
	last := turns[len(turns)-1]
	if len(wanted) == 0 {
		if last["version"] != json.Number("1") {
			return ErrStoredResponseInvalid
		}
		return nil
	}
	if last["version"] != json.Number("4") {
		return ErrStoredResponseInvalid
	}
	raw, err := json.Marshal(last["attachments"])
	if err != nil {
		return ErrStoredResponseInvalid
	}
	snapshot, err := managedfiles.DecodeNativeAttachmentSnapshot15(raw)
	if err != nil || len(snapshot.Positions) != len(wanted) {
		return ErrStoredResponseInvalid
	}
	toolSources, err := managedfiles.ValidateToolFileSources(expanded)
	if err != nil {
		return ErrStoredResponseInvalid
	}
	priorCopies := map[string]managedfiles.NativeAttachmentWireCopy15{}
	for _, prior := range turns[:len(turns)-1] {
		if prior["version"] != json.Number("4") {
			continue
		}
		priorRaw, _ := json.Marshal(prior["attachments"])
		before, priorErr := managedfiles.DecodeNativeAttachmentSnapshot15(priorRaw)
		if priorErr != nil {
			return ErrStoredResponseInvalid
		}
		for _, copy := range before.Copies {
			priorCopies[copy.PublicFileID] = copy
		}
	}
	for _, copy := range snapshot.Copies {
		if copy.AccountID != record.AccountID || copy.SubjectDigest != subject || backend != "" && copy.BackendID != backend || generation != "" && copy.CredentialGeneration != generation {
			return ErrStoredResponseInvalid
		}
		if old, exists := priorCopies[copy.PublicFileID]; exists && !sameStoredNativeFileFacts15(old, copy) {
			return ErrStoredResponseInvalid
		}
	}
	for _, position := range snapshot.Positions {
		id, ok := wanted[coordinate{position.Input, position.Part}]
		if !ok || position.Turn != sourceTurns[position.Input] || snapshot.Copies[position.CopyIndex].PublicFileID != id {
			return ErrStoredResponseInvalid
		}
		item := expanded[position.Input].(map[string]any)
		_, field := managedfiles.TypedFileContent(item)
		kind, _ := item["type"].(string)
		if kind == "" {
			kind = "message"
		}
		if position.SourceKind != kind || position.Field != field {
			return ErrStoredResponseInvalid
		}
		if kind == "message" {
			continue
		}
		if position.CallID != item["call_id"] || position.CallInput < 0 || position.CallInput >= position.Input {
			return ErrStoredResponseInvalid
		}
		source, exists := toolSources[position.Input]
		if !exists || source.CallInput != position.CallInput || source.CallID != position.CallID || source.Kind != position.SourceKind {
			return ErrStoredResponseInvalid
		}
		call := expanded[position.CallInput]
		callRaw, _ := json.Marshal(call)
		actual, parseErr := CanonicalStoredResponseJSON15(position.CallItem)
		if parseErr != nil || !bytes.Equal(callRaw, actual) {
			return ErrStoredResponseInvalid
		}
	}
	return nil
}

func sameStoredNativeFileFacts15(a, b managedfiles.NativeAttachmentWireCopy15) bool {
	return a.PublicFileID == b.PublicFileID && a.FileVersion == b.FileVersion && a.SHA256 == b.SHA256 && a.Filename == b.Filename && a.MIME == b.MIME && a.Bytes == b.Bytes && a.AccountID == b.AccountID && a.BackendID == b.BackendID && a.SubjectDigest == b.SubjectDigest && a.CredentialGeneration == b.CredentialGeneration
}
