package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// This module accepts only facts minted by verifyNativeTerminal15 in the
// authenticated private transport. It cannot promote a public terminal alone.
// Signed snapshot bytes are base64 JSON strings: JSONB key reordering must not
// change the exact bytes whose digest the gateway authenticated.
type storedResponseNative15 struct {
	Version               int    `json:"version"`
	Kind                  string `json:"kind"`
	PublicID              string `json:"public_id"`
	UserID                int64  `json:"user_id"`
	APIKeyID              int64  `json:"api_key_id"`
	GroupID               int64  `json:"group_id"`
	AccountID             int64  `json:"account_id"`
	Model                 string `json:"model"`
	ResolvedModel         string `json:"resolved_model"`
	BindingSHA256         string `json:"binding_sha256"`
	SubjectDigest         string `json:"subject_digest"`
	BackendID             string `json:"backend_id"`
	CredentialGeneration  string `json:"credential_generation"`
	AdmissionID           string `json:"admission_id"`
	RequestWireSHA256     string `json:"request_wire_sha256"`
	NativeWireSHA256      string `json:"native_wire_sha256"`
	GatewayTerminalSHA256 string `json:"gateway_terminal_sha256"`
	SnapshotSHA256        string `json:"snapshot_sha256"`
	InputSHA256           string `json:"input_sha256"`
	ParametersSHA256      string `json:"parameters_sha256"`
	PublicSHA256          string `json:"public_sha256"`
	Nonce                 string `json:"nonce"`
	Snapshot              []byte `json:"snapshot"`
}

func storedNativeDigest15(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func storedNativeCanonical15(raw []byte, max int64) ([]byte, error) {
	if max <= 0 || int64(len(raw)) > max {
		return nil, ErrStoredResponseTooLarge
	}
	canonical, err := CanonicalStoredResponseJSON15(raw)
	if err != nil {
		return nil, err
	}
	if int64(len(canonical)) > max {
		return nil, ErrStoredResponseTooLarge
	}
	return canonical, nil
}

func storedNativeHex15(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// BuildStoredResponseNativeRecovery15 binds the authenticated, unchanged gateway
// terminal to the final public object after trusted local ID/model rewrites.
// It performs no persistence and does not authorize use of historical files.
func BuildStoredResponseNativeRecovery15(record *StoredResponse, public, gatewayTerminal, snapshot []byte, proof *verifiedNativeTerminal15, maxBytes int64) ([]byte, error) {
	if record == nil || proof == nil || record.RecoveryVersion != 2 ||
		proof.publicID != record.PublicID || proof.userID != record.UserID || proof.apiKeyID != record.APIKeyID || proof.groupID != record.GroupID || proof.accountID != record.AccountID ||
		proof.publicModel != record.Model || proof.resolvedModel != record.ResolvedModel || proof.admissionBindingSHA256 != record.Binding ||
		proof.terminalSHA256 != storedNativeDigest15(gatewayTerminal) || proof.snapshotSHA256 != storedNativeDigest15(snapshot) {
		return nil, ErrStoredResponseInvalid
	}
	input, err := storedNativeCanonical15(record.Input, maxBytes)
	if err != nil {
		return nil, err
	}
	parameters, err := storedNativeCanonical15(record.Parameters, maxBytes)
	if err != nil {
		return nil, err
	}
	payload, err := storedNativeCanonical15(public, maxBytes)
	if err != nil {
		return nil, err
	}
	upstream, err := decodeStoredResponseJSON(gatewayTerminal)
	if err != nil {
		return nil, err
	}
	upstreamObject, ok := upstream.(map[string]any)
	if !ok || upstreamObject["object"] != "response" || upstreamObject["status"] != "completed" || upstreamObject["error"] != nil || upstreamObject["incomplete_details"] != nil {
		return nil, ErrStoredResponseInvalid
	}
	localValue, err := decodeStoredResponseJSON(payload)
	if err != nil {
		return nil, err
	}
	localObject, ok := localValue.(map[string]any)
	if !ok {
		return nil, ErrStoredResponseInvalid
	}
	upstreamOutput, _ := json.Marshal(upstreamObject["output"])
	localOutput, _ := json.Marshal(localObject["output"])
	if !bytes.Equal(upstreamOutput, localOutput) {
		return nil, ErrStoredResponseInvalid
	}
	material := storedResponseNative15{Version: 2, Kind: "native", PublicID: record.PublicID, UserID: record.UserID, APIKeyID: record.APIKeyID, GroupID: record.GroupID, AccountID: record.AccountID, Model: record.Model, ResolvedModel: record.ResolvedModel,
		BindingSHA256: record.Binding, SubjectDigest: proof.subjectDigest, BackendID: proof.backendID, CredentialGeneration: proof.credentialGeneration, AdmissionID: proof.admissionID, RequestWireSHA256: proof.requestWireSHA256, NativeWireSHA256: proof.nativeWireSHA256, GatewayTerminalSHA256: proof.terminalSHA256, SnapshotSHA256: proof.snapshotSHA256,
		InputSHA256: storedNativeDigest15(input), ParametersSHA256: storedNativeDigest15(parameters), PublicSHA256: storedNativeDigest15(payload), Nonce: proof.nonce, Snapshot: append([]byte(nil), snapshot...)}
	encoded, err := json.Marshal(material)
	if err != nil {
		return nil, err
	}
	if err = ValidateStoredResponseNativeRecovery15(record, payload, encoded, maxBytes); err != nil {
		return nil, err
	}
	return encoded, nil
}

// ValidateStoredResponseNativeRecovery15 is also called inside the repository's
// row lock before its existing atomic public_response/private_recovery UPDATE.
// It checks durable self-consistency; only the transport verifier authenticates
// original gateway provenance before Build is called.
func ValidateStoredResponseNativeRecovery15(record *StoredResponse, public, recovery []byte, maxBytes int64) error {
	_, err := readStoredNative15(record, public, recovery, maxBytes)
	return err
}

func readStoredNative15(record *StoredResponse, public, recovery []byte, maxBytes int64) (*storedResponseNative15, error) {
	if record == nil || record.RecoveryVersion != 2 || !previousResponseID15(record.PublicID) || record.UserID <= 0 || record.APIKeyID <= 0 || record.GroupID <= 0 || record.AccountID <= 0 {
		return nil, ErrStoredResponseInvalid
	}
	if _, err := storedNativeCanonical15(recovery, maxBytes); err != nil {
		return nil, err
	}
	var material storedResponseNative15
	dec := json.NewDecoder(bytes.NewReader(recovery))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&material); err != nil {
		return nil, ErrStoredResponseInvalid
	}
	if material.Version != 2 || material.Kind != "native" || material.PublicID != record.PublicID || material.UserID != record.UserID || material.APIKeyID != record.APIKeyID || material.GroupID != record.GroupID || material.AccountID != record.AccountID || material.Model != record.Model || material.ResolvedModel != record.ResolvedModel || material.BindingSHA256 != record.Binding {
		return nil, ErrStoredResponseInvalid
	}
	for _, digest := range []string{material.BindingSHA256, material.SubjectDigest, material.RequestWireSHA256, material.NativeWireSHA256, material.GatewayTerminalSHA256, material.SnapshotSHA256, material.InputSHA256, material.ParametersSHA256, material.PublicSHA256, material.Nonce} {
		if !storedNativeHex15(digest) {
			return nil, ErrStoredResponseInvalid
		}
	}
	if material.BackendID == "" || material.CredentialGeneration == "" || !strings.HasPrefix(material.AdmissionID, "responses-") || !storedNativeHex15(strings.TrimPrefix(material.AdmissionID, "responses-")) {
		return nil, ErrStoredResponseInvalid
	}
	input, err := storedNativeCanonical15(record.Input, maxBytes)
	if err != nil {
		return nil, err
	}
	parameters, err := storedNativeCanonical15(record.Parameters, maxBytes)
	if err != nil {
		return nil, err
	}
	payload, err := storedNativeCanonical15(public, maxBytes)
	if err != nil {
		return nil, err
	}
	if material.InputSHA256 != storedNativeDigest15(input) || material.ParametersSHA256 != storedNativeDigest15(parameters) || material.PublicSHA256 != storedNativeDigest15(payload) || material.SnapshotSHA256 != storedNativeDigest15(material.Snapshot) {
		return nil, ErrStoredResponseInvalid
	}
	publicValue, err := decodeStoredResponseJSON(payload)
	if err != nil {
		return nil, err
	}
	response, ok := publicValue.(map[string]any)
	if !ok || response["id"] != record.PublicID || response["object"] != "response" || response["status"] != "completed" || response["model"] != record.Model || response["store"] != true || response["error"] != nil || response["incomplete_details"] != nil {
		return nil, ErrStoredResponseInvalid
	}
	if previous, exists := response["previous_response_id"]; exists && previous != nil && previous != "" && previous != record.PreviousResponseID {
		return nil, ErrStoredResponseInvalid
	}
	if err = validateStoredNativePublic15(response); err != nil {
		return nil, err
	}
	created, ok := response["created_at"].(json.Number)
	if !ok {
		return nil, ErrStoredResponseInvalid
	}
	n, e := created.Int64()
	if e != nil || n != record.CreatedAt.Unix() {
		return nil, ErrStoredResponseInvalid
	}
	turns, err := storedNativeTurns15(material.Snapshot, record, material.SubjectDigest, maxBytes)
	if err != nil {
		return nil, err
	}
	for index := range turns {
		if err = validateStoredNativeAttachmentTurn15(turns[:index+1], record, material.SubjectDigest, material.BackendID, material.CredentialGeneration); err != nil {
			return nil, err
		}
	}
	last := turns[len(turns)-1]
	wantedInput, err := decodeStoredResponseJSON(input)
	if err != nil {
		return nil, err
	}
	wantedInput = storedNativeInput15(wantedInput)
	left, _ := json.Marshal(last["input"])
	right, _ := json.Marshal(wantedInput)
	if !bytes.Equal(left, right) {
		return nil, ErrStoredResponseInvalid
	}
	left, _ = json.Marshal(last["output"])
	right, _ = json.Marshal(response["output"])
	if !bytes.Equal(left, right) {
		return nil, ErrStoredResponseInvalid
	}
	return &material, nil
}

// The authenticated gateway validates native/public mapping under each turn's
// frozen catalog. Main independently checks outer shape, identity, bounded turns
// and its canonical current delta/output; it never infers native calls itself.
func storedNativeTurns15(snapshot []byte, record *StoredResponse, subject string, maxBytes int64) ([]map[string]any, error) {
	if int64(len(snapshot)) > maxBytes {
		return nil, ErrStoredResponseTooLarge
	}
	value, err := decodeStoredResponseJSON(snapshot)
	if err != nil {
		return nil, err
	}
	root, ok := value.(map[string]any)
	if !ok {
		return nil, ErrStoredResponseInvalid
	}
	var rawTurns []any
	switch root["version"] {
	case json.Number("1"), json.Number("4"):
		rawTurns = []any{root}
	case json.Number("2"):
		if len(root) != 3 {
			return nil, ErrStoredResponseInvalid
		}
		rawTurns, ok = root["turns"].([]any)
		if !ok {
			return nil, ErrStoredResponseInvalid
		}
	default:
		return nil, ErrStoredResponseUnsupported
	}
	if len(rawTurns) == 0 || len(rawTurns) > 32 {
		return nil, ErrStoredResponseTooLarge
	}
	checkBinding := func(raw any) bool {
		b, ok := raw.(map[string]any)
		if !ok || len(b) != 5 {
			return false
		}
		return b["owner_id"] == json.Number(jsonInt15(record.UserID)) && b["group_id"] == json.Number(jsonInt15(record.GroupID)) && b["account_id"] == json.Number(jsonInt15(record.AccountID)) && b["model"] == record.ResolvedModel && b["subject_digest"] == subject
	}
	if !checkBinding(root["binding"]) {
		return nil, ErrStoredResponseInvalid
	}
	turns := make([]map[string]any, 0, len(rawTurns))
	items, pairs := 0, 0
	for _, raw := range rawTurns {
		turn, ok := raw.(map[string]any)
		if !ok || !checkBinding(turn["binding"]) {
			return nil, ErrStoredResponseInvalid
		}
		switch turn["version"] {
		case json.Number("1"):
			if len(turn) != 6 {
				return nil, ErrStoredResponseInvalid
			}
		case json.Number("4"):
			if len(turn) != 7 {
				return nil, ErrStoredResponseInvalid
			}
		default:
			return nil, ErrStoredResponseUnsupported
		}
		for _, field := range []string{"input", "output", "tools", "replays"} {
			list, ok := turn[field].([]any)
			if !ok {
				return nil, ErrStoredResponseInvalid
			}
			if field == "input" || field == "output" {
				items += len(list)
			} else if len(list) > 1024 {
				return nil, ErrStoredResponseTooLarge
			}
			if field == "replays" {
				pairs += len(list)
			}
		}
		if items > 4096 || pairs > 1024 {
			return nil, ErrStoredResponseTooLarge
		}
		turns = append(turns, turn)
		if err = validateStoredNativeAttachmentTurn15(turns, record, subject, "", ""); err != nil {
			return nil, err
		}
	}
	return turns, nil
}

func jsonInt15(value int64) string { encoded, _ := json.Marshal(value); return string(encoded) }

func storedNativeInput15(value any) any {
	if text, ok := value.(string); ok {
		return []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}}
	}
	return value
}

// This first native persistence slice deliberately accepts ordinary function
// and custom call outputs, including strictly validated echoed catalogs.
// Discovery and files need their separately validated provenance contract first.
func validateStoredNativePublic15(response map[string]any) error {
	output, ok := response["output"].([]any)
	if !ok || len(output) > 4096 {
		return ErrStoredResponseUnsupported
	}
	for _, raw := range output {
		item, ok := raw.(map[string]any)
		if !ok {
			return ErrStoredResponseInvalid
		}
		switch item["type"] {
		case "message":
			if err := validateStoredResponseOutput([]any{item}); err != nil {
				return err
			}
		case "function_call", "custom_tool_call":
			for field := range item {
				if field != "type" && field != "id" && field != "call_id" && field != "name" && field != "namespace" && field != "arguments" && field != "input" && field != "status" && field != "encrypted_function_args" {
					return ErrStoredResponseUnsupported
				}
			}
			if encrypted, exists := item["encrypted_function_args"]; exists {
				list, ok := encrypted.([]any)
				if !ok || len(list) != 0 {
					return ErrStoredResponseUnsupported
				}
			}
			for _, field := range []string{"call_id", "name"} {
				value, ok := item[field].(string)
				if !ok || value == "" || strings.TrimSpace(value) != value {
					return ErrStoredResponseInvalid
				}
			}
			for _, field := range []string{"id", "namespace"} {
				if raw, exists := item[field]; exists {
					value, ok := raw.(string)
					if !ok || value == "" || strings.TrimSpace(value) != value {
						return ErrStoredResponseInvalid
					}
				}
			}
			if status, exists := item["status"]; exists && (status != "completed" && (response["status"] == "completed" || status != "in_progress" && status != "incomplete")) {
				return ErrStoredResponseInvalid
			}
			if item["type"] == "function_call" {
				arguments, ok := item["arguments"].(string)
				if !ok {
					return ErrStoredResponseInvalid
				}
				if response["status"] == "completed" || item["status"] == "completed" {
					value, err := decodeStoredResponseJSON([]byte(arguments))
					if err != nil {
						return err
					}
					if _, ok := value.(map[string]any); !ok {
						return ErrStoredResponseInvalid
					}
				}
				if _, exists := item["input"]; exists {
					return ErrStoredResponseUnsupported
				}
			} else {
				if _, ok := item["input"].(string); !ok {
					return ErrStoredResponseInvalid
				}
				if _, exists := item["arguments"]; exists {
					return ErrStoredResponseUnsupported
				}
			}
		default:
			return ErrStoredResponseUnsupported
		}
	}
	copy := make(map[string]any, len(response))
	for k, v := range response {
		copy[k] = v
	}
	copy["output"] = []any{}
	if tools, exists := response["tools"]; exists {
		count := 0
		if err := storedNativeRequestTools15(tools, "", false, &count, map[string]bool{}); err != nil {
			return err
		}
		delete(copy, "tools")
	}
	if choice, exists := response["tool_choice"]; exists {
		if err := storedNativeRequestChoice15(choice); err != nil {
			return err
		}
		delete(copy, "tool_choice")
	}
	if previous, exists := response["previous_response_id"]; exists && previous != nil && previous != "" {
		id, ok := previous.(string)
		if !ok || !previousResponseID15(id) {
			return ErrStoredResponseInvalid
		}
		delete(copy, "previous_response_id")
	}
	return validateStoredPublicResponse(copy)
}

func (c *StoredResponseCapture) observeNativeTerminal15(response map[string]any) error {
	safe := make(map[string]any, len(response))
	for key, value := range response {
		safe[key] = value
	}
	output := []any{}
	if items, ok := response["output"].([]any); ok {
		for _, raw := range items {
			item, ok := raw.(map[string]any)
			if ok && item["type"] == "message" {
				output = append(output, item)
			}
		}
	}
	safe["output"] = output
	delete(safe, "tools")
	delete(safe, "tool_choice")
	delete(safe, "previous_response_id")
	return c.observeProgress("response.in_progress", map[string]any{"response": safe})
}

// FinalizeStoredResponseNative15 is the tracer's one-write persistence seam.
// The caller has already stopped lease renewal and validated observed usage.
// Neither a public completion nor private bytes are returned on write failure.
func (s *StoredResponseService) FinalizeStoredResponseNative15(ctx context.Context, record *StoredResponse, public, gatewayTerminal, snapshot []byte, proof *verifiedNativeTerminal15) ([]byte, error) {
	if s == nil || !s.Enabled() || s.repo == nil {
		return nil, ErrStoredResponsesDisabled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	material, err := BuildStoredResponseNativeRecovery15(record, public, gatewayTerminal, snapshot, proof, s.cfg.Limits.MaxResponseBytes)
	if err != nil {
		return nil, err
	}
	payload, err := storedNativeCanonical15(public, s.cfg.Limits.MaxResponseBytes)
	if err != nil {
		return nil, err
	}
	if int64(len(record.Input)+len(record.Parameters)+len(payload)+len(material)) > s.cfg.Limits.MaxResponseBytes {
		return nil, ErrStoredResponseTooLarge
	}
	saved, err := s.repo.Finalize(ctx, record.PublicID, record.LeaseToken, record.Version, StoredResponseTerminal{State: "completed", ExecutionState: "terminal", PublicResponse: payload, Recovery: material})
	if err != nil {
		return nil, err
	}
	if saved == nil {
		return nil, ErrStoredResponseConflict
	}
	return payload, nil
}

type StoredResponseNativeReadBinding15 struct {
	PublicModel, ResolvedModel, BackendID, SubjectDigest, CredentialGeneration string
	AccountID                                                                  int64
}

type storedNativeRowCheckpoint15 struct {
	row                                 StoredResponse
	input, parameters, public, recovery string
}

func checkpointStoredNativeRow15(record *StoredResponse) storedNativeRowCheckpoint15 {
	checkpoint := storedNativeRowCheckpoint15{row: *record, input: storedNativeDigest15(record.Input), parameters: storedNativeDigest15(record.Parameters), public: storedNativeDigest15(record.PublicResponse), recovery: storedNativeDigest15(record.Recovery)}
	// Retain immutable scalar facts and digests, not shared mutable row buffers.
	checkpoint.row.Input, checkpoint.row.Parameters, checkpoint.row.PublicResponse, checkpoint.row.Recovery = nil, nil, nil, nil
	return checkpoint
}

func (c storedNativeRowCheckpoint15) matches(record *StoredResponse) bool {
	prior := &c.row
	return record != nil && record.PublicID == prior.PublicID && record.UserID == prior.UserID && record.APIKeyID == prior.APIKeyID && record.GroupID == prior.GroupID && record.AccountID == prior.AccountID &&
		record.Model == prior.Model && record.ResolvedModel == prior.ResolvedModel && record.Binding == prior.Binding && record.PreviousResponseID == prior.PreviousResponseID && record.RecoveryVersion == 2 &&
		record.State == "completed" && record.ExecutionState == "terminal" && record.DeletedAt == nil && record.Version == prior.Version && record.TerminalHash == prior.TerminalHash &&
		record.CreatedAt.Equal(prior.CreatedAt) && record.ExpiresAt.Equal(prior.ExpiresAt) && record.ExpiresAt.After(time.Now()) &&
		storedNativeDigest15(record.Input) == c.input && storedNativeDigest15(record.Parameters) == c.parameters && storedNativeDigest15(record.PublicResponse) == c.public && storedNativeDigest15(record.Recovery) == c.recovery
}

// ResolvePreviousResponseNative15 returns only exact authenticated snapshot bytes
// from durable storage after every ancestor passes a fresh authorizer. The
// caller must keep the required account pin and reauthorize immediately pre-send.
func (s *StoredResponseService) ResolvePreviousResponseNative15(ctx context.Context, owner StoredResponseOwner, id string, binding StoredResponseNativeReadBinding15, authorize PreviousResponseTextAuthorize15) ([]byte, int64, error) {
	if s == nil || !s.Enabled() || s.repo == nil {
		return nil, 0, ErrStoredResponsesDisabled
	}
	if !validStoredResponseOwner(owner) || authorize == nil || binding.AccountID <= 0 || binding.PublicModel == "" || binding.ResolvedModel == "" || binding.BackendID == "" || binding.CredentialGeneration == "" || !storedNativeHex15(binding.SubjectDigest) {
		return nil, 0, ErrBPSRequestAuthorizationUnavailable
	}
	seen := map[string]bool{}
	var leaf []byte
	var childTurns []map[string]any
	var checkpoints []storedNativeRowCheckpoint15
	remaining := s.cfg.Limits.MaxResponseBytes
	var validUntil time.Time
	for id != "" {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if !previousResponseID15(id) || seen[id] {
			return nil, 0, ErrStoredResponseNotFound
		}
		if len(seen) >= 31 {
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
		if validUntil.IsZero() || record.ExpiresAt.Before(validUntil) {
			validUntil = record.ExpiresAt
		}
		if record.State != "completed" || record.ExecutionState != "terminal" || record.RecoveryVersion != 2 {
			return nil, 0, ErrStoredResponseUnsupported
		}
		if record.AccountID != binding.AccountID || record.Model != binding.PublicModel || record.ResolvedModel != binding.ResolvedModel {
			return nil, 0, ErrBPSRequestAuthorizationRevoked
		}
		if err = authorize(ctx, owner, record); err != nil {
			return nil, 0, err
		}
		if err = ctx.Err(); err != nil {
			return nil, 0, err
		}
		// Authorization may wait on a refreshed account or policy lookup. A
		// snapshot that expires during that wait must not escape this resolver.
		if !validUntil.After(time.Now()) {
			return nil, 0, ErrStoredResponseNotFound
		}
		if int64(len(record.Recovery)) > remaining {
			return nil, 0, ErrStoredResponseTooLarge
		}
		remaining -= int64(len(record.Recovery))
		material, err := readStoredNative15(record, record.PublicResponse, record.Recovery, s.cfg.Limits.MaxResponseBytes)
		if err != nil {
			return nil, 0, err
		}
		if material.SubjectDigest != binding.SubjectDigest || material.BackendID != binding.BackendID || material.CredentialGeneration != binding.CredentialGeneration {
			return nil, 0, ErrBPSRequestAuthorizationRevoked
		}
		turns, err := storedNativeTurns15(material.Snapshot, record, binding.SubjectDigest, s.cfg.Limits.MaxResponseBytes)
		if err != nil {
			return nil, 0, err
		}
		if leaf == nil {
			leaf = append([]byte(nil), material.Snapshot...)
		}
		if childTurns != nil {
			if len(childTurns) != len(turns)+1 {
				return nil, 0, ErrStoredResponseInvalid
			}
			child, _ := json.Marshal(childTurns[:len(turns)])
			parent, _ := json.Marshal(turns)
			if !bytes.Equal(child, parent) {
				return nil, 0, ErrStoredResponseInvalid
			}
		}
		if record.PreviousResponseID == "" && len(turns) != 1 {
			return nil, 0, ErrStoredResponseInvalid
		}
		checkpoints = append(checkpoints, checkpointStoredNativeRow15(record))
		childTurns = turns
		id = record.PreviousResponseID
	}
	// A later ancestor's authority lookup can wait while a previously checked
	// row is deleted or changed. Re-read the whole durable chain after all such
	// waits; do not invoke another external authorizer beyond this boundary.
	for _, checkpoint := range checkpoints {
		current, err := s.getForKeyOwner(ctx, owner, checkpoint.row.PublicID)
		if err != nil {
			return nil, 0, err
		}
		if !checkpoint.matches(current) {
			return nil, 0, ErrStoredResponseNotFound
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if leaf == nil || !validUntil.After(time.Now()) {
		return nil, 0, ErrStoredResponseNotFound
	}
	return leaf, binding.AccountID, nil
}
