package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// StoredResponseBinding is provided only after trusted server-side BPS account
// admission. This module does not infer BPS identity from client headers or URLs.
type StoredResponseBinding struct {
	AccountID            int64
	ResolvedModel, Scope string
}

type StoredResponseService struct {
	repo StoredResponseRepository
	cfg  StoredResponseConfig
}

func (c StoredResponseConfig) Validate() error {
	l := c.Limits
	if c.RetentionDays < 1 || c.RetentionDays > 30 || c.LeaseSeconds < 6 || c.LeaseSeconds > 3600 ||
		l.MaxResponseBytes < 1 || l.MaxResponseBytes > 64<<20 || l.UserMaxRows < 1 || l.GlobalMaxRows < l.UserMaxRows ||
		l.UserMaxBytes < l.MaxResponseBytes || l.GlobalMaxBytes < l.UserMaxBytes {
		return ErrStoredResponseInvalid
	}
	return nil
}

func NewStoredResponseService(repo StoredResponseRepository, cfg StoredResponseConfig) (*StoredResponseService, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &StoredResponseService{repo: repo, cfg: cfg}, nil
}

// StoredResponseMinimumSnapshot builds the smallest display/recovery object from
// local client parameters. Persistence uses the same shape for admission budgets.
func StoredResponseMinimumSnapshot(record *StoredResponse, interrupted bool) ([]byte, error) {
	if record == nil {
		return nil, ErrStoredResponseInvalid
	}
	response := map[string]any{"id": record.PublicID, "object": "response", "created_at": record.CreatedAt.Unix(), "model": record.Model, "store": true, "status": "in_progress", "output": []any{}, "error": nil, "usage": nil}
	if interrupted {
		response["status"] = "failed"
		response["error"] = map[string]any{"code": "response_interrupted", "message": "Response execution was interrupted; upstream execution may have occurred."}
	}
	if err := applyStoredResponseClientParameters(response, record.Parameters); err != nil {
		return nil, err
	}
	return json.Marshal(response)
}

func applyStoredResponseClientParameters(response map[string]any, raw json.RawMessage) error {
	var parameters map[string]any
	if err := json.Unmarshal(raw, &parameters); err != nil || parameters == nil {
		return ErrStoredResponseInvalid
	}
	if instructions, exists := parameters["instructions"]; exists {
		if _, ok := instructions.(string); !ok {
			return ErrStoredResponseInvalid
		}
		response["instructions"] = instructions
	} else {
		delete(response, "instructions")
	}
	delete(response, "prompt_cache_key")
	if value, exists := parameters["metadata"]; exists {
		metadata, err := storedResponseMetadata(value)
		if err != nil {
			return err
		}
		response["metadata"] = metadata
	} else if value, exists := response["metadata"]; exists && value != nil {
		metadata, ok := value.(map[string]any)
		if !ok || len(metadata) != 0 {
			return ErrStoredResponseUnsupported
		}
	}
	return nil
}

// StoredResponseCapture has one owner goroutine. KeepLease coordinates its own
// renewal goroutine through Stop; payload capture itself is never concurrent.
type StoredResponseCapture struct {
	service       *StoredResponseService
	record        *StoredResponse
	store         bool
	finished      bool
	finalPayload  json.RawMessage
	partialOutput []any
	observedUsage any
}

func storedResponseRandom() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func validStoredResponseOwner(o StoredResponseOwner) bool {
	return o.UserID > 0 && o.APIKeyID > 0 && o.GroupID > 0
}

func (s *StoredResponseService) Begin(ctx context.Context, owner StoredResponseOwner, req *StoredResponseRequest, binding StoredResponseBinding) (*StoredResponseCapture, error) {
	if s == nil || !s.cfg.Enabled {
		return nil, ErrStoredResponsesDisabled
	}
	if !validStoredResponseOwner(owner) || req == nil || req.Model == "" || binding.AccountID <= 0 || binding.Scope == "" {
		return nil, ErrStoredResponseInvalid
	}
	// Revalidate mutable exported request fields; callers cannot bypass the pure
	// text contract by constructing a StoredResponseRequest directly.
	canonical, err := NormalizeStoredResponseRequest(req.UpstreamBody)
	if err != nil {
		return nil, err
	}
	if canonical.Model != req.Model {
		return nil, ErrStoredResponseInvalid
	}
	if req.Store {
		value, err := decodeStoredResponseJSON(req.UpstreamBody)
		if err != nil {
			return nil, err
		}
		object := value.(map[string]any)
		object["store"] = true
		if err := restoreStoredResponseMetadata(object, req.localMetadata); err != nil {
			return nil, err
		}
		raw, _ := json.Marshal(object)
		checked, err := NormalizeStoredResponseRequest(raw)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(checked.Input, req.Input) || !bytes.Equal(checked.Parameters, req.Parameters) {
			return nil, ErrStoredResponseInvalid
		}
	}
	id, err := storedResponseRandom()
	if err != nil {
		return nil, err
	}
	token, err := storedResponseRandom()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	record := &StoredResponse{PublicID: "resp_local_" + id, UserID: owner.UserID, APIKeyID: owner.APIKeyID, GroupID: owner.GroupID,
		Model: req.Model, ResolvedModel: binding.ResolvedModel, AccountID: binding.AccountID, Binding: binding.Scope,
		CreatedAt: now, ExpiresAt: now.Add(time.Duration(s.cfg.RetentionDays) * 24 * time.Hour), State: "in_progress", ExecutionState: "not_sent", LeaseToken: token, RecoveryVersion: 1, PreviousResponseID: req.previousResponseID,
		Input: append(json.RawMessage(nil), req.Input...), Parameters: append(json.RawMessage(nil), req.Parameters...), ReservedBytes: s.cfg.Limits.MaxResponseBytes}
	if req.Store {
		if s.repo == nil {
			return nil, fmt.Errorf("%w: persistence unavailable", ErrStoredResponsesDisabled)
		}
		record.PublicResponse, err = StoredResponseMinimumSnapshot(record, false)
		if err != nil {
			return nil, err
		}
		recovery, err := StoredResponseMinimumSnapshot(record, true)
		if err != nil {
			return nil, err
		}
		if int64(len(record.Input)+len(record.Parameters)+len(record.PublicResponse)) > s.cfg.Limits.MaxResponseBytes || int64(len(record.Input)+len(record.Parameters)+len(recovery)) > s.cfg.Limits.MaxResponseBytes {
			return nil, ErrStoredResponseTooLarge
		}
		minimumCompleted := map[string]any{"object": "response", "status": "completed", "output": []any{}}
		minimumCompletedRaw, _ := json.Marshal(minimumCompleted)
		minimumPrivate, err := BuildStoredResponseTextRecovery15(record.Input, minimumCompletedRaw, s.cfg.Limits.MaxResponseBytes)
		if err != nil {
			return nil, err
		}
		if int64(len(record.Input)+len(record.Parameters)+len(record.PublicResponse)+len(minimumPrivate)) > s.cfg.Limits.MaxResponseBytes {
			return nil, ErrStoredResponseTooLarge
		}
		if err = s.repo.Reserve(ctx, record, s.cfg.Limits, time.Duration(s.cfg.RetentionDays)*24*time.Hour, time.Duration(s.cfg.LeaseSeconds)*time.Second); err != nil {
			return nil, err
		}
	} else {
		record.Input = nil
		record.Parameters = nil
		record.Binding = ""
	}
	return &StoredResponseCapture{service: s, record: record, store: req.Store}, nil
}

func (s *StoredResponseService) Get(ctx context.Context, owner StoredResponseOwner, id string) (*StoredResponse, error) {
	if s == nil || !s.cfg.Enabled {
		return nil, ErrStoredResponsesDisabled
	}
	if !validStoredResponseOwner(owner) {
		return nil, ErrStoredResponseNotFound
	}
	if s.repo == nil {
		return nil, ErrStoredResponseNotFound
	}
	return s.getForKeyOwner(ctx, owner, id)
}
func (s *StoredResponseService) Delete(ctx context.Context, owner StoredResponseOwner, id string) error {
	if s == nil || !s.cfg.Enabled {
		return ErrStoredResponsesDisabled
	}
	if !validStoredResponseOwner(owner) || s.repo == nil {
		return ErrStoredResponseNotFound
	}
	return s.deleteForKeyOwner(ctx, owner, id)
}
func (s *StoredResponseService) Sweep(ctx context.Context, limit int) (int, error) {
	if s == nil || !s.cfg.Enabled {
		return 0, nil
	}
	if s.repo == nil {
		return 0, ErrStoredResponsesDisabled
	}
	if limit < 1 || limit > 100 {
		return 0, ErrStoredResponseInvalid
	}
	return s.repo.Sweep(ctx, limit)
}

func (c *StoredResponseCapture) PublicID() string { return c.record.PublicID }
func (c *StoredResponseCapture) CaptureEvent(ctx context.Context, event string, raw []byte) ([]byte, error) {
	if c.finished && event != "response.completed" && event != "response.incomplete" && event != "response.failed" {
		return nil, ErrStoredResponseConflict
	}
	switch event {
	case "response.created", "response.queued", "response.in_progress", "response.completed", "response.incomplete", "response.failed", "response.output_text.delta", "response.output_text.done", "response.output_item.added", "response.output_item.done", "response.content_part.added", "response.content_part.done", "response.refusal.delta", "response.refusal.done":
	default:
		if c.store && (c.record.RecoveryVersion != 2 || !nativeStoredResponseEvent15(event)) {
			return nil, ErrStoredResponseUnsupported
		}
	}
	if c.store && c.record.RecoveryVersion == 2 && int64(len(raw)) > c.service.cfg.Limits.MaxResponseBytes {
		return nil, ErrStoredResponseTooLarge
	}
	value, err := decodeStoredResponseJSON(raw)
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, ErrStoredResponseInvalid
	}
	if typ, ok := object["type"].(string); !ok || typ != event {
		return nil, ErrStoredResponseInvalid
	}
	// Validate envelope references before a terminal payload can be committed.
	if _, exists := object["response_id"]; exists {
		if c.store && c.record.RecoveryVersion == 2 {
			if !storedNativeRequestName15(object["response_id"]) {
				return nil, ErrStoredResponseInvalid
			}
			object["response_id"] = c.record.PublicID
		} else {
			switch event {
			case "response.output_text.delta", "response.output_text.done", "response.output_item.added", "response.output_item.done", "response.content_part.added", "response.content_part.done":
				object["response_id"] = c.record.PublicID
			default:
				if c.store {
					return nil, ErrStoredResponseUnsupported
				}
			}
		}
	}
	if response, exists := object["response"]; exists {
		r, ok := response.(map[string]any)
		if !ok {
			return nil, ErrStoredResponseInvalid
		}
		terminal := event == "response.completed" || event == "response.incomplete" || event == "response.failed"
		if terminal && r["status"] != event[len("response."):] {
			return nil, ErrStoredResponseInvalid
		}
		if err = c.prepareResponse(ctx, r, terminal, "terminal"); err != nil {
			return nil, err
		}
	} else if event == "response.completed" || event == "response.incomplete" || event == "response.failed" || event == "response.created" {
		return nil, ErrStoredResponseInvalid
	}
	if c.store && !c.finished {
		if c.record.RecoveryVersion == 2 {
			err = c.observeNativeProgress15(event, object)
		} else {
			err = c.observeProgress(event, object)
		}
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(object)
}

func (c *StoredResponseCapture) CaptureJSON(ctx context.Context, raw []byte) ([]byte, error) {
	value, err := decodeStoredResponseJSON(raw)
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, ErrStoredResponseInvalid
	}
	response := object
	if nested, exists := object["response"]; exists {
		var valid bool
		response, valid = nested.(map[string]any)
		if !valid {
			return nil, ErrStoredResponseInvalid
		}
		if response["object"] == nil {
			if err = c.expandFailureDiagnostic(object, response); err != nil {
				return nil, err
			}
		}
	}
	if err = c.prepareResponse(ctx, response, true, "terminal"); err != nil {
		return nil, err
	}
	return json.Marshal(object)
}

func (c *StoredResponseCapture) prepareResponse(ctx context.Context, response map[string]any, terminal bool, execution string) error {
	if response["object"] != "response" {
		return ErrStoredResponseInvalid
	}
	status, ok := response["status"].(string)
	if !ok {
		return ErrStoredResponseInvalid
	}
	if terminal {
		if status != "completed" && status != "incomplete" && status != "failed" {
			return ErrStoredResponseInvalid
		}
	} else if status != "in_progress" && status != "queued" {
		return ErrStoredResponseInvalid
	}
	response["id"] = c.record.PublicID
	response["created_at"] = c.record.CreatedAt.Unix()
	response["store"] = c.store
	if c.store {
		response["model"] = c.record.Model
		if err := applyStoredResponseClientParameters(response, c.record.Parameters); err != nil {
			return err
		}
	}
	if !terminal {
		return nil
	}
	if c.store {
		var err error
		if c.record.RecoveryVersion == 2 {
			if c.record.PreviousResponseID != "" {
				response["previous_response_id"] = c.record.PreviousResponseID
			}
			err = validateStoredNativePublic15(response)
		} else {
			err = validateStoredPublicResponse(response)
		}
		if err != nil {
			return err
		}
	}
	payload, err := json.Marshal(response)
	if err != nil {
		return err
	}
	if c.finished {
		if bytes.Equal(payload, c.finalPayload) {
			return nil
		}
		return ErrStoredResponseConflict
	}
	if c.store {
		// Validated usage is an observed upstream fact even if private material
		// exceeds its reservation. Abort must retain it without rerunning inference.
		if usage, ok := response["usage"].(map[string]any); ok {
			c.observedUsage = mergeStoredResponseUsage(c.observedUsage, usage)
		}
		var recovery json.RawMessage
		if status == "completed" {
			switch c.record.RecoveryVersion {
			case 1:
				recovery, err = BuildStoredResponseTextRecovery15(c.record.Input, payload, c.service.cfg.Limits.MaxResponseBytes)
			case 2:
				delivery := nativeTerminalDeliveryFromContext15(ctx)
				if delivery == nil {
					return ErrStoredResponseInvalid
				}
				recovery, err = BuildStoredResponseNativeRecovery15(c.record, payload, delivery.gatewayTerminal, delivery.snapshot, delivery.receipt, c.service.cfg.Limits.MaxResponseBytes)
			default:
				err = ErrStoredResponseInvalid
			}
			if err != nil {
				return err
			}
		}
		if int64(len(c.record.Input)+len(c.record.Parameters)+len(payload)+len(recovery)) > c.service.cfg.Limits.MaxResponseBytes {
			return ErrStoredResponseTooLarge
		}
		// Preserve validated observed facts even if the following commit fails.
		if c.record.RecoveryVersion == 2 {
			err = c.observeNativeTerminal15(response)
		} else {
			err = c.observeProgress("response.in_progress", map[string]any{"response": response})
		}
		if err != nil {
			return err
		}
		saved, err := c.service.repo.Finalize(ctx, c.record.PublicID, c.record.LeaseToken, c.record.Version, StoredResponseTerminal{State: status, ExecutionState: execution, PublicResponse: payload, Recovery: recovery})
		if err != nil {
			return err
		}
		if saved == nil {
			return ErrStoredResponseConflict
		}
	}
	c.finished = true
	c.finalPayload = payload
	return nil
}

func validateStoredResponseOutput(value any) error {
	items, ok := value.([]any)
	if !ok {
		return ErrStoredResponseUnsupported
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok || item["type"] != "message" || item["role"] != "assistant" {
			return ErrStoredResponseUnsupported
		}
		for key, value := range item {
			switch key {
			case "type", "role", "content":
			case "id":
				if _, ok := value.(string); !ok {
					return ErrStoredResponseInvalid
				}
			case "status":
				if value != "in_progress" && value != "completed" && value != "incomplete" {
					return ErrStoredResponseInvalid
				}
			default:
				return ErrStoredResponseUnsupported
			}
		}
		parts, ok := item["content"].([]any)
		if !ok {
			return ErrStoredResponseUnsupported
		}
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				return ErrStoredResponseUnsupported
			}
			switch part["type"] {
			case "output_text":
				if _, ok := part["text"].(string); !ok {
					return ErrStoredResponseInvalid
				}
				for key, value := range part {
					switch key {
					case "type", "text":
					case "annotations", "logprobs":
						if key == "logprobs" && value == nil {
							continue
						}
						list, ok := value.([]any)
						if !ok || len(list) > 0 {
							return ErrStoredResponseUnsupported
						}
					default:
						return ErrStoredResponseUnsupported
					}
				}
			case "refusal":
				if _, ok := part["refusal"].(string); !ok {
					return ErrStoredResponseInvalid
				}
				for key := range part {
					if key != "type" && key != "refusal" {
						return ErrStoredResponseUnsupported
					}
				}
			default:
				return ErrStoredResponseUnsupported
			}
		}
	}
	return nil
}

// Abort records an interrupted local attempt. It never retries inference or
// asserts remote cancellation, refunds, or zero usage. Normalized partial output
// is deliberately not fabricated when no authoritative terminal was observed.
func (c *StoredResponseCapture) Abort(ctx context.Context) error {
	if c.finished || !c.store {
		return nil
	}
	output := c.partialOutput
	if output == nil {
		output = []any{}
	}
	raw, err := StoredResponseMinimumSnapshot(c.record, true)
	if err != nil {
		return err
	}
	value, err := decodeStoredResponseJSON(raw)
	if err != nil {
		return err
	}
	response := value.(map[string]any)
	response["output"], response["usage"] = output, c.observedUsage
	return c.prepareResponse(ctx, response, true, "unknown")
}

func (c *StoredResponseCapture) Renew(ctx context.Context) error {
	if !c.store || c.finished {
		return nil
	}
	return c.service.repo.Renew(ctx, c.record.PublicID, c.record.LeaseToken, c.record.Version, time.Duration(c.service.cfg.LeaseSeconds)*time.Second)
}

// KeepLease cancels the attempt on renewal failure. Call stop before final
// capture; its join prevents a renewal racing against a successful Finalize.
func (c *StoredResponseCapture) KeepLease(parent context.Context) (context.Context, func() error) {
	if !c.store {
		return parent, func() error { return nil }
	}
	ctx, cancel := context.WithCancel(parent)
	done, stop := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var leaseErr error
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Duration(c.service.cfg.LeaseSeconds) * time.Second / 3)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				renewCtx, finish := context.WithTimeout(ctx, time.Duration(c.service.cfg.LeaseSeconds)*time.Second/6)
				leaseErr = c.service.repo.Renew(renewCtx, c.record.PublicID, c.record.LeaseToken, c.record.Version, time.Duration(c.service.cfg.LeaseSeconds)*time.Second)
				finish()
				if leaseErr != nil {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, func() error { once.Do(func() { close(stop); <-done }); return leaseErr }
}

// Only the public text Response schema may enter the durable content store.
// Unknown native/proprietary fields fail closed instead of leaking on retrieve.
func validateStoredPublicResponse(response map[string]any) error {
	for key, value := range response {
		switch key {
		case "id", "object", "status", "model":
			if _, ok := value.(string); !ok {
				return ErrStoredResponseInvalid
			}
		case "created_at", "max_output_tokens", "max_tool_calls", "completed_at":
			if value != nil && !storedNonnegativeInteger(value) {
				return ErrStoredResponseInvalid
			}
		case "store", "parallel_tool_calls":
			if _, ok := value.(bool); !ok {
				return ErrStoredResponseInvalid
			}
		case "background":
			if flag, ok := value.(bool); !ok || flag {
				return ErrStoredResponseUnsupported
			}
		case "instructions", "truncation", "service_tier", "prompt_cache_key":
			if value != nil {
				if _, ok := value.(string); !ok {
					return ErrStoredResponseInvalid
				}
			}
		case "tool_choice":
			if value != "auto" && value != "none" {
				return ErrStoredResponseUnsupported
			}
		case "temperature", "top_p":
			if value != nil {
				number, ok := value.(json.Number)
				if !ok {
					return ErrStoredResponseInvalid
				}
				if _, err := strconv.ParseFloat(string(number), 64); err != nil {
					return ErrStoredResponseInvalid
				}
			}
		case "output", "error", "usage", "incomplete_details", "metadata", "text", "reasoning", "tools", "previous_response_id", "user", "safety_identifier":
		default:
			return ErrStoredResponseUnsupported
		}
	}
	if err := validateStoredResponseOutput(response["output"]); err != nil {
		return err
	}
	if err := validateStoredResponseNested(response); err != nil {
		return err
	}
	if tools, exists := response["tools"]; exists {
		list, ok := tools.([]any)
		if !ok || len(list) > 0 {
			return ErrStoredResponseUnsupported
		}
	}
	for _, field := range []string{"previous_response_id", "user", "safety_identifier"} {
		if v, exists := response[field]; exists && v != nil && v != "" {
			return ErrStoredResponseUnsupported
		}
	}
	if e, exists := response["error"]; exists && e != nil {
		object, ok := e.(map[string]any)
		if !ok {
			return ErrStoredResponseInvalid
		}
		for key, v := range object {
			switch key {
			case "code", "type", "message", "param":
				if v != nil {
					if _, ok := v.(string); !ok {
						return ErrStoredResponseInvalid
					}
				}
			default:
				return ErrStoredResponseUnsupported
			}
		}
	}
	return nil
}

func validateStoredResponseNested(response map[string]any) error {
	for _, field := range []string{"metadata"} {
		if value, exists := response[field]; exists && value != nil {
			if _, err := storedResponseMetadata(value); err != nil {
				return err
			}
		}
	}
	if value, exists := response["usage"]; exists && value != nil {
		object, ok := value.(map[string]any)
		if !ok {
			return ErrStoredResponseInvalid
		}
		for key, value := range object {
			switch key {
			case "input_tokens", "output_tokens", "total_tokens":
				if !storedNonnegativeInteger(value) {
					return ErrStoredResponseInvalid
				}
			case "input_tokens_details", "output_tokens_details":
				details, ok := value.(map[string]any)
				if !ok {
					return ErrStoredResponseInvalid
				}
				for k, v := range details {
					if (key == "input_tokens_details" && k != "cached_tokens") || (key == "output_tokens_details" && k != "reasoning_tokens") {
						return ErrStoredResponseUnsupported
					}
					if !storedNonnegativeInteger(v) {
						return ErrStoredResponseInvalid
					}
				}
			default:
				return ErrStoredResponseUnsupported
			}
		}
	}
	if value, exists := response["reasoning"]; exists && value != nil {
		object, ok := value.(map[string]any)
		if !ok {
			return ErrStoredResponseInvalid
		}
		for key, v := range object {
			switch key {
			case "effort", "summary":
				if v != nil {
					if _, ok := v.(string); !ok {
						return ErrStoredResponseInvalid
					}
				}
			default:
				return ErrStoredResponseUnsupported
			}
		}
	}
	if value, exists := response["incomplete_details"]; exists && value != nil {
		object, ok := value.(map[string]any)
		if !ok || len(object) != 1 {
			return ErrStoredResponseInvalid
		}
		if reason, ok := object["reason"].(string); !ok || (reason != "max_output_tokens" && reason != "content_filter") {
			return ErrStoredResponseUnsupported
		}
	}
	if value, exists := response["text"]; exists && value != nil {
		object, ok := value.(map[string]any)
		if !ok {
			return ErrStoredResponseInvalid
		}
		for key, v := range object {
			switch key {
			case "verbosity":
				if _, ok := v.(string); !ok {
					return ErrStoredResponseInvalid
				}
			case "format":
				format, ok := v.(map[string]any)
				if !ok || len(format) != 1 || format["type"] != "text" {
					return ErrStoredResponseUnsupported
				}
			default:
				return ErrStoredResponseUnsupported
			}
		}
	}
	return nil
}

func storedNonnegativeInteger(value any) bool {
	if number, ok := value.(int64); ok {
		return number >= 0
	}
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	n, err := strconv.ParseInt(string(number), 10, 64)
	return err == nil && n >= 0
}
