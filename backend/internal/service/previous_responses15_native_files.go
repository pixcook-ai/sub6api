package service

import (
	"context"
	"encoding/json"
	"sort"

	pkghttputil "github.com/MACOS-DO/sub4api/internal/pkg/httputil"
	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

// Native history retains public identities. Every attempt independently opens
// and reserves every referenced file, including references in old turns.
func expandedNativeFileRequest15(current, parent []byte) ([]byte, []int, error) {
	value, err := decodeStoredResponseJSON(current)
	if err != nil {
		return nil, nil, err
	}
	root, ok := value.(map[string]any)
	if !ok {
		return nil, nil, ErrStoredResponseInvalid
	}
	var input []any
	var turns []int
	appendItems := func(value any, turn int) error {
		items, ok := storedNativeInput15(value).([]any)
		if !ok {
			return ErrStoredResponseInvalid
		}
		input = append(input, items...)
		for range items {
			turns = append(turns, turn)
		}
		if len(input) > 4096 {
			return ErrStoredResponseTooLarge
		}
		return nil
	}
	turnNumber := 0
	if len(parent) > 0 {
		value, err = decodeStoredResponseJSON(parent)
		if err != nil {
			return nil, nil, err
		}
		prior, ok := value.(map[string]any)
		if !ok {
			return nil, nil, ErrStoredResponseInvalid
		}
		var history []any
		switch prior["version"] {
		case json.Number("1"), json.Number("4"):
			history = []any{prior}
		case json.Number("2"):
			history, ok = prior["turns"].([]any)
			if !ok {
				return nil, nil, ErrStoredResponseInvalid
			}
		default:
			return nil, nil, ErrStoredResponseUnsupported
		}
		if len(history) == 0 || len(history) >= 32 {
			return nil, nil, ErrStoredResponseTooLarge
		}
		for _, raw := range history {
			turn, ok := raw.(map[string]any)
			if !ok {
				return nil, nil, ErrStoredResponseInvalid
			}
			for _, field := range []string{"input", "output"} {
				if err = appendItems(turn[field], turnNumber); err != nil {
					return nil, nil, err
				}
			}
			if _, exists := root["tools"]; !exists && turnNumber == len(history)-1 {
				root["tools"] = turn["tools"]
			}
			turnNumber++
		}
	}
	if err = appendItems(root["input"], turnNumber); err != nil {
		return nil, nil, err
	}
	root["input"], root["store"] = input, false
	raw, err := json.Marshal(root)
	return raw, turns, err
}

func nativeFileSnapshot15(body []byte, turns []int, copies []managedfiles.NativeAttachmentWireCopy15) (*managedfiles.NativeAttachmentSnapshot15, error) {
	value, err := decodeStoredResponseJSON(body)
	if err != nil {
		return nil, err
	}
	root, ok := value.(map[string]any)
	if !ok {
		return nil, ErrStoredResponseInvalid
	}
	input, ok := root["input"].([]any)
	if !ok || len(input) != len(turns) {
		return nil, ErrStoredResponseInvalid
	}
	sorted := append([]managedfiles.NativeAttachmentWireCopy15(nil), copies...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].PublicFileID < sorted[j].PublicFileID })
	indices := map[string]int{}
	for i, copy := range sorted {
		if _, exists := indices[copy.PublicFileID]; exists {
			return nil, ErrStoredResponseInvalid
		}
		indices[copy.PublicFileID] = i
	}
	snapshot := &managedfiles.NativeAttachmentSnapshot15{Version: managedfiles.NativeAttachmentContract15, Copies: sorted, Positions: []managedfiles.NativeAttachmentPosition15{}}
	type call struct {
		index int
		item  map[string]any
	}
	calls := map[string]call{}
	for i, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, ErrStoredResponseInvalid
		}
		kind, _ := item["type"].(string)
		if kind == "function_call" || kind == "custom_tool_call" {
			id, _ := item["call_id"].(string)
			if _, exists := calls[id]; id == "" || exists {
				return nil, ErrStoredResponseInvalid
			}
			calls[id] = call{i, item}
		}
		field, source := "content", "message"
		if kind == "function_call_output" || kind == "custom_tool_call_output" {
			field, source = "output", kind
		}
		parts, _ := item[field].([]any)
		for j, raw := range parts {
			part, ok := raw.(map[string]any)
			if !ok || part["type"] != "input_file" {
				continue
			}
			id, ok := part["file_id"].(string)
			index, exists := indices[id]
			if !ok || !exists {
				return nil, ErrStoredResponseInvalid
			}
			pos := managedfiles.NativeAttachmentPosition15{Turn: turns[i], Input: i, Field: field, Part: j, SourceKind: source, CallInput: -1, CallItem: json.RawMessage("null"), CopyIndex: index}
			if source != "message" {
				pos.CallID, _ = item["call_id"].(string)
				call, ok := calls[pos.CallID]
				if !ok {
					return nil, ErrStoredResponseInvalid
				}
				pos.CallInput = call.index
				pos.CallItem, err = json.Marshal(call.item)
				if err != nil {
					return nil, err
				}
			}
			snapshot.Positions = append(snapshot.Positions, pos)
		}
	}
	if _, err = managedfiles.CanonicalNativeAttachmentSnapshot15(*snapshot); err != nil {
		return nil, ErrStoredResponseInvalid
	}
	return snapshot, nil
}

func (s *OpenAIGatewayService) prepareNativeFiles15(ctx context.Context, account *Account, plan *StoredResponsePlan, wire []byte, authorize func(context.Context) error) (*managedfiles.NativeAttachmentAdmission15, *ManagedFilePlan, func(context.Context) error, error) {
	if s == nil || plan == nil || plan.native15 == nil {
		return nil, nil, nil, ErrStoredResponseInvalid
	}
	if len(wire) > managedfiles.NativeResponsesMaxBytes15 || len(plan.native15.parentSnapshot) > managedfiles.NativeResponsesMaxBytes15 {
		return nil, nil, nil, ErrStoredResponseTooLarge
	}
	if err := pkghttputil.ReserveRequestBodyMemory(ctx, 16*int64(len(wire)+len(plan.native15.parentSnapshot))+(1<<20)); err != nil {
		return nil, nil, nil, err
	}
	expanded, turns, err := expandedNativeFileRequest15(wire, plan.native15.parentSnapshot)
	if err != nil {
		return nil, nil, nil, err
	}
	if !HasManagedResponseFiles(expanded) {
		return nil, nil, nil, nil
	}
	if s == nil || !s.managedFiles.Enabled() {
		return nil, nil, nil, ErrFilesDisabled
	}
	// Inline payloads lack a durable public identity and cannot enter history.
	preparation, err := PreflightManagedInlineFiles(ctx, expanded, DefaultManagedInlineLimits())
	if err != nil {
		return nil, nil, nil, err
	}
	for _, file := range preparation.Files() {
		if file.PublicID == "" {
			return nil, nil, nil, ErrStoredResponseUnsupported
		}
	}
	backend, err := BPSFilesConfigForAccount(account)
	stored, storedErr := BPSStoredResponsesConfigForAccount(account)
	if err != nil || storedErr != nil || backend.ControlURL != stored.ControlURL || backend.ServiceKey != stored.ServiceKey || backend.Expected != stored.Expected {
		return nil, nil, nil, ErrBPSStoredResponseRoute
	}
	// Both independent resource contracts remain revocable during every wait.
	fileAuthorize := func(check context.Context) error {
		if err := authorize(check); err != nil {
			return err
		}
		return s.authorizeNativeFileAccount15(check, plan.Owner, account, backend, plan.Request.Model)
	}
	if err := fileAuthorize(ctx); err != nil {
		return nil, nil, nil, err
	}
	fileCtx := withManagedFilePreparationAuthorization(ctx, fileAuthorize)
	files, err := s.managedFiles.Prepare(fileCtx, FileOwner{UserID: plan.Owner.UserID, APIKeyID: plan.Owner.APIKeyID, GroupID: plan.Owner.GroupID}, expanded)
	if err != nil {
		return nil, nil, nil, err
	}
	fail := func(err error) (*managedfiles.NativeAttachmentAdmission15, *ManagedFilePlan, func(context.Context) error, error) {
		if files != nil {
			_ = files.Close()
		}
		return nil, nil, nil, err
	}
	if files == nil {
		return fail(ErrManagedFileNotReady)
	}
	resolved, err := s.managedFiles.Resolve(fileCtx, files, account, expanded)
	if err != nil {
		return fail(err)
	}
	snapshot, err := nativeFileSnapshot15(expanded, turns, resolved.nativeCopies)
	if err != nil {
		return fail(err)
	}
	proof := &managedfiles.NativeAttachmentAdmission15{AdmissionID: resolved.AdmissionID, OwnerRef: resolved.OwnerRef, GroupRef: resolved.GroupRef, BindingSHA256: resolved.BindingSHA256, Snapshot: *snapshot}
	fresh := func(check context.Context) error {
		return resolved.Authorize(withManagedFilePreparationAuthorization(check, fileAuthorize))
	}
	return proof, files, fresh, nil
}

func (s *OpenAIGatewayService) authorizeNativeFileAccount15(ctx context.Context, owner StoredResponseOwner, selected *Account, expected BPSFilesBackendConfig, publicModel string) error {
	current, err := s.AuthorizeManagedFileAccount(ctx, owner.GroupID, publicModel, selected)
	if err != nil {
		return err
	}
	actual, err := BPSFilesConfigForAccount(current)
	if err != nil || actual != expected {
		return ErrBPSRequestAuthorizationRevoked
	}
	return ctx.Err()
}
