package service

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

// Constructed only by the separately negotiated native-preflight wrapper.
// Native preflight must bind these facts itself; the pure-text v1 admission is
// never promoted by assigning a new policy label to its existing result.
type storedResponseNativeAdmission15 struct {
	owner                                                    StoredResponseOwner
	policyVersion                                            string
	publicModel, resolvedModel                               string
	accountID                                                int64
	backendID, subjectDigest, credentialGeneration           string
	admissionID, admissionBindingSHA256, nonce               string
	clientRequestSHA256, requestWireSHA256, nativeWireSHA256 string
	inputSHA256, parametersSHA256                            string
	expiresAt                                                time.Time
	previousResponseID                                       string
	parentSnapshot                                           []byte
	attachmentSnapshot                                       *managedfiles.NativeAttachmentSnapshot15
	maxBytes                                                 int
	preflightRequest                                         managedfiles.NativeResponsesPreflight15
	preflightResult                                          managedfiles.NativeResponsesPreflightResult15
}

func (a *storedResponseNativeAdmission15) hasMatchingPolicy15() bool {
	if a == nil {
		return false
	}
	// Begin drops the transient preflight request while retaining its snapshot.
	if a.preflightRequest.Files != nil || a.attachmentSnapshot != nil {
		return a.policyVersion == managedfiles.NativeResponsesAttachmentPolicy15
	}
	return a.policyVersion == managedfiles.NativeResponsesPolicy15
}

// BeginNative15 is deliberately distinct from Begin's existing text contract.
// The trusted native preflight has already prepared this exact current request;
// fresh authorization is required immediately before this single reservation.
func (s *StoredResponseService) BeginNative15(ctx context.Context, req *StoredResponseRequest, admission *storedResponseNativeAdmission15, authorize func(context.Context) error) (*StoredResponseCapture, error) {
	if s == nil || !s.Enabled() || s.repo == nil {
		return nil, ErrStoredResponsesDisabled
	}
	if req == nil || !req.Store || admission == nil || authorize == nil || !validStoredResponseOwner(admission.owner) || admission.accountID <= 0 || req.Model != admission.publicModel || admission.resolvedModel == "" || !admission.hasMatchingPolicy15() {
		return nil, ErrStoredResponseInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if admission.maxBytes <= 0 || admission.maxBytes > managedfiles.NativeResponsesMaxBytes15 || int64(admission.maxBytes) > s.cfg.Limits.MaxResponseBytes {
		return nil, ErrStoredResponseTooLarge
	}
	if !time.Now().Before(admission.expiresAt) || admission.expiresAt.After(time.Now().Add(31*time.Second)) {
		return nil, ErrStoredResponseLease
	}
	for _, digest := range []string{admission.subjectDigest, admission.admissionBindingSHA256, admission.clientRequestSHA256, admission.requestWireSHA256, admission.nativeWireSHA256, admission.nonce, admission.inputSHA256, admission.parametersSHA256} {
		if !storedNativeHex15(digest) {
			return nil, ErrStoredResponseInvalid
		}
	}
	if admission.backendID == "" || admission.credentialGeneration == "" || admission.admissionID == "" || admission.clientRequestSHA256 != storedNativeDigest15(req.UpstreamBody) {
		return nil, ErrStoredResponseInvalid
	}
	if (admission.previousResponseID == "") != (len(admission.parentSnapshot) == 0) || admission.previousResponseID != "" && !previousResponseID15(admission.previousResponseID) {
		return nil, ErrStoredResponseInvalid
	}
	canonical, err := NormalizeStoredResponseNativeRequest15(req.UpstreamBody)
	if err != nil {
		return nil, err
	}
	if canonical.Model != req.Model || !bytes.Equal(canonical.Input, req.Input) || !bytes.Equal(canonical.Parameters, req.Parameters) {
		return nil, ErrStoredResponseInvalid
	}
	if storedNativeDigest15(canonical.Input) != admission.inputSHA256 || storedNativeDigest15(canonical.Parameters) != admission.parametersSHA256 {
		return nil, ErrStoredResponseInvalid
	}
	if err = authorize(ctx); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
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
	record := &StoredResponse{PublicID: "resp_local_" + id, UserID: admission.owner.UserID, APIKeyID: admission.owner.APIKeyID, GroupID: admission.owner.GroupID, Model: req.Model, ResolvedModel: admission.resolvedModel, AccountID: admission.accountID, Binding: admission.admissionBindingSHA256, CreatedAt: now, ExpiresAt: now.Add(time.Duration(s.cfg.RetentionDays) * 24 * time.Hour), State: "in_progress", ExecutionState: "not_sent", LeaseToken: token, RecoveryVersion: 2, PreviousResponseID: admission.previousResponseID, Input: append(json.RawMessage(nil), req.Input...), Parameters: append(json.RawMessage(nil), req.Parameters...), ReservedBytes: s.cfg.Limits.MaxResponseBytes}
	admitted := *admission
	admitted.parentSnapshot = append([]byte(nil), admission.parentSnapshot...)
	admitted.preflightRequest = managedfiles.NativeResponsesPreflight15{}
	admitted.preflightResult = managedfiles.NativeResponsesPreflightResult15{}
	record.nativeAdmission15 = &admitted
	record.PublicResponse, err = StoredResponseMinimumSnapshot(record, false)
	if err != nil {
		return nil, err
	}
	interrupted, err := StoredResponseMinimumSnapshot(record, true)
	if err != nil {
		return nil, err
	}
	minimum, err := ValidateStoredResponseNativeReservation15(record, now, s.cfg.Limits.MaxResponseBytes)
	if err != nil {
		return nil, err
	}
	if int64(len(record.Input)+len(record.Parameters)+len(record.PublicResponse)+len(minimum)) > s.cfg.Limits.MaxResponseBytes || int64(len(record.Input)+len(record.Parameters)+len(interrupted)) > s.cfg.Limits.MaxResponseBytes {
		return nil, ErrStoredResponseTooLarge
	}
	if !time.Now().Before(admission.expiresAt) {
		return nil, ErrStoredResponseLease
	}
	if err = authorize(ctx); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if !time.Now().Before(admission.expiresAt) {
		return nil, ErrStoredResponseLease
	}
	if err = s.repo.Reserve(ctx, record, s.cfg.Limits, time.Duration(s.cfg.RetentionDays)*24*time.Hour, time.Duration(s.cfg.LeaseSeconds)*time.Second); err != nil {
		return nil, err
	}
	record.nativeAdmission15 = nil
	return &StoredResponseCapture{service: s, record: record, store: true}, nil
}

// ValidateStoredResponseNativeReservation15 is called again under repository
// locks using database time. A fabricated exported StoredResponse cannot opt
// into version2 without the private native-admission capability from Begin.
func ValidateStoredResponseNativeReservation15(record *StoredResponse, now time.Time, maxBytes int64) ([]byte, error) {
	if record == nil || record.RecoveryVersion != 2 || record.nativeAdmission15 == nil {
		return nil, ErrStoredResponseInvalid
	}
	a := record.nativeAdmission15
	if !a.hasMatchingPolicy15() || a.owner.UserID != record.UserID || a.owner.APIKeyID != record.APIKeyID || a.owner.GroupID != record.GroupID || a.publicModel != record.Model || a.resolvedModel != record.ResolvedModel || a.accountID != record.AccountID || a.admissionBindingSHA256 != record.Binding || a.previousResponseID != record.PreviousResponseID {
		return nil, ErrStoredResponseInvalid
	}
	if !now.Before(a.expiresAt) {
		return nil, ErrStoredResponseLease
	}
	input, err := storedNativeCanonical15(record.Input, maxBytes)
	if err != nil {
		return nil, err
	}
	parameters, err := storedNativeCanonical15(record.Parameters, maxBytes)
	if err != nil {
		return nil, err
	}
	if storedNativeDigest15(input) != a.inputSHA256 || storedNativeDigest15(parameters) != a.parametersSHA256 {
		return nil, ErrStoredResponseInvalid
	}
	return storedNativeMinimum15(record, a, maxBytes)
}

// This is a byte-budget lower bound only, not authenticated recovery material.
// Every identity/hash string has its actual final length; the frozen original
// prefix and current input/effective catalog are already included before send.
func storedNativeMinimum15(record *StoredResponse, admission *storedResponseNativeAdmission15, maxBytes int64) ([]byte, error) {
	input, err := decodeStoredResponseJSON(record.Input)
	if err != nil {
		return nil, err
	}
	input = storedNativeInput15(input)
	parametersValue, err := decodeStoredResponseJSON(record.Parameters)
	if err != nil {
		return nil, err
	}
	parameters, ok := parametersValue.(map[string]any)
	if !ok {
		return nil, ErrStoredResponseInvalid
	}
	tools := any([]any{})
	var turns []map[string]any
	if len(admission.parentSnapshot) != 0 {
		turns, err = storedNativeTurns15(admission.parentSnapshot, record, admission.subjectDigest, maxBytes)
		if err != nil {
			return nil, err
		}
		if len(turns) >= 32 {
			return nil, ErrStoredResponseTooLarge
		}
		tools = turns[len(turns)-1]["tools"]
	}
	if declared, exists := parameters["tools"]; exists {
		tools = declared
	}
	binding := map[string]any{"owner_id": record.UserID, "group_id": record.GroupID, "account_id": record.AccountID, "model": record.ResolvedModel, "subject_digest": admission.subjectDigest}
	turn := map[string]any{"version": json.Number("1"), "binding": binding, "input": input, "tools": tools, "output": []any{}, "replays": []any{}}
	if admission.attachmentSnapshot != nil {
		raw, err := managedfiles.CanonicalNativeAttachmentSnapshot15(*admission.attachmentSnapshot)
		if err != nil {
			return nil, ErrStoredResponseInvalid
		}
		attachments, err := decodeStoredResponseJSON(raw)
		if err != nil {
			return nil, err
		}
		turn["version"], turn["attachments"] = json.Number("4"), attachments
	}
	if err = validateStoredNativeAttachmentTurn15(append(turns, turn), record, admission.subjectDigest, admission.backendID, admission.credentialGeneration); err != nil {
		return nil, err
	}
	var snapshot []byte
	if len(turns) == 0 {
		snapshot, err = json.Marshal(turn)
	} else {
		turns = append(turns, turn)
		snapshot, err = json.Marshal(map[string]any{"version": 2, "binding": binding, "turns": turns})
	}
	if err != nil {
		return nil, err
	}
	zeroHash := "0000000000000000000000000000000000000000000000000000000000000000"
	material := storedResponseNative15{Version: 2, Kind: "native", PublicID: record.PublicID, UserID: record.UserID, APIKeyID: record.APIKeyID, GroupID: record.GroupID, AccountID: record.AccountID, Model: record.Model, ResolvedModel: record.ResolvedModel, BindingSHA256: admission.admissionBindingSHA256, SubjectDigest: admission.subjectDigest, BackendID: admission.backendID, CredentialGeneration: admission.credentialGeneration, AdmissionID: admission.admissionID, RequestWireSHA256: admission.requestWireSHA256, NativeWireSHA256: admission.nativeWireSHA256, GatewayTerminalSHA256: zeroHash, SnapshotSHA256: storedNativeDigest15(snapshot), InputSHA256: zeroHash, ParametersSHA256: zeroHash, PublicSHA256: zeroHash, Nonce: admission.nonce, Snapshot: snapshot}
	encoded, err := json.Marshal(material)
	if err != nil {
		return nil, err
	}
	if int64(len(encoded)) > maxBytes {
		return nil, ErrStoredResponseTooLarge
	}
	return encoded, nil
}
