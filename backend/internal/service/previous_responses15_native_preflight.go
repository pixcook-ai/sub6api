package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"time"

	pkghttputil "github.com/MACOS-DO/sub4api/internal/pkg/httputil"
	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

// preflightStoredResponseNative15 is a separate internal control request. It
// cannot accept a pure-text result and does not reserve a row or send inference.
// The caller keeps the original request body scope and the fresh authorize
// closure; activation later adds the actual locally reserved response ID.
func (s *OpenAIGatewayService) preflightStoredResponseNative15(ctx context.Context, config BPSStoredResponsesBackendConfig, owner StoredResponseOwner, request *StoredResponseRequest, wire, parentSnapshot []byte, previousID string, files ...*managedfiles.NativeAttachmentAdmission15) (*storedResponseNativeAdmission15, error) {
	if s == nil || s.httpUpstream == nil || s.storedResponses == nil || request == nil || !request.Store || !validStoredResponseOwner(owner) || config.AccountID <= 0 || config.Concurrency <= 0 || managedfiles.ValidateIdentity(config.Expected) != nil {
		return nil, ErrStoredResponseInvalid
	}
	if managedfiles.ValidateNativeResponsesServiceKey15(config.ServiceKey) != nil {
		return nil, ErrStoredResponseInvalid
	}
	maxBytes := s.storedResponses.cfg.Limits.MaxResponseBytes
	if maxBytes > managedfiles.NativeResponsesMaxBytes15 {
		maxBytes = managedfiles.NativeResponsesMaxBytes15
	}
	if maxBytes <= 0 || int64(len(wire)) > maxBytes || int64(len(request.UpstreamBody)) > maxBytes || int64(len(parentSnapshot)) > maxBytes {
		return nil, ErrStoredResponseTooLarge
	}
	if (previousID == "") != (len(parentSnapshot) == 0) || previousID != "" && !previousResponseID15(previousID) {
		return nil, ErrStoredResponseInvalid
	}
	validatedURL, err := validateOutboundURLWithConfig(s.cfg, config.ControlURL)
	if err != nil {
		return nil, ErrBPSStoredResponseRoute
	}
	wireValue, err := decodeStoredResponseJSON(wire)
	if err != nil {
		return nil, err
	}
	wireObject, ok := wireValue.(map[string]any)
	if !ok || wireObject["store"] != false {
		return nil, ErrStoredResponseInvalid
	}
	resolvedModel, ok := wireObject["model"].(string)
	if !ok || resolvedModel == "" {
		return nil, ErrStoredResponseInvalid
	}
	var nonceBytes [32]byte
	if _, err = rand.Read(nonceBytes[:]); err != nil {
		return nil, err
	}
	nonce := hex.EncodeToString(nonceBytes[:])
	preflightRequest := managedfiles.NativeResponsesPreflight15{Version: managedfiles.NativeResponsesContract15, MaxBytes: int(maxBytes), Expected: config.Expected, Context: managedfiles.NativeResponsesContext15{UserID: owner.UserID, APIKeyID: owner.APIKeyID, GroupID: owner.GroupID, AccountID: config.AccountID, PublicModel: request.Model, ResolvedModel: resolvedModel, Nonce: nonce}, ClientRequest: request.UpstreamBody, WireRequest: wire, PreviousSnapshotBase64: base64.StdEncoding.EncodeToString(parentSnapshot)}
	if len(files) > 1 {
		return nil, ErrStoredResponseInvalid
	}
	if len(files) == 1 {
		preflightRequest.Files = files[0]
	}
	controlCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	controlCtx, release := pkghttputil.WithRequestBodyMemoryScope(controlCtx)
	defer release()
	if err = pkghttputil.ReserveRequestBodyMemory(controlCtx, 3*(int64(len(request.UpstreamBody))+int64(len(wire))+int64(len(parentSnapshot))*2+(16<<10))); err != nil {
		return nil, err
	}
	encoded, err := managedfiles.EncodeNativeResponsesPreflight15(preflightRequest)
	if err != nil {
		return nil, ErrStoredResponseInvalid
	}
	req, err := http.NewRequestWithContext(WithHTTPUpstreamRedirectsDisabled(controlCtx), http.MethodPost, strings.TrimRight(validatedURL, "/")+managedfiles.NativeResponsesPrefix15+"preflight", bytes.NewReader(encoded))
	if err != nil {
		return nil, ErrBPSStoredResponseRoute
	}
	req.GetBody = nil
	req.Header.Set(managedfiles.ServiceKeyHeader, config.ServiceKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := s.httpUpstream.Do(req, config.ProxyURL, config.AccountID, config.Concurrency)
	if err != nil {
		return nil, ErrBPSStoredResponseRoute
	}
	if resp == nil || resp.Body == nil {
		return nil, ErrBPSStoredResponseRoute
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (16<<10)+1))
	if err != nil || len(raw) > 16<<10 {
		return nil, ErrBPSStoredResponseRoute
	}
	if resp.StatusCode != http.StatusOK {
		values := resp.Header.Values("X-Bps-Request-Policy")
		if len(values) == 1 && values[0] == "rejected-v1" {
			if policy := managedfiles.DecodeNativeResponsesPolicyError15(resp.StatusCode, raw); policy != nil {
				return nil, policy
			}
			if resp.StatusCode == http.StatusBadRequest {
				return nil, ErrStoredResponseUnsupported
			}
		}
		return nil, ErrBPSStoredResponseRoute
	}
	result, err := managedfiles.DecodeNativeResponsesPreflightResult15(raw)
	if err != nil {
		return nil, ErrStoredResponseInvalid
	}
	if err = managedfiles.ValidateNativeResponsesPreflightResult15(preflightRequest, result, time.Now()); err != nil {
		return nil, ErrStoredResponseInvalid
	}
	admission := &storedResponseNativeAdmission15{owner: owner, maxBytes: result.MaxBytes, preflightRequest: preflightRequest, preflightResult: result, policyVersion: result.PolicyVersion, publicModel: request.Model, resolvedModel: resolvedModel, accountID: config.AccountID, backendID: config.Expected.BackendID, subjectDigest: managedfiles.NativeResponsesSubjectDigest15(config.Expected), credentialGeneration: config.Expected.CredentialGeneration, admissionID: result.AdmissionID, admissionBindingSHA256: result.BindingSHA256, nonce: nonce, clientRequestSHA256: storedNativeDigest15(request.UpstreamBody), inputSHA256: storedNativeDigest15(request.Input), parametersSHA256: storedNativeDigest15(request.Parameters), requestWireSHA256: result.RequestWireSHA256, nativeWireSHA256: result.NativeWireSHA256, expiresAt: time.Unix(result.ExpiresAt, 0), previousResponseID: previousID, parentSnapshot: append([]byte(nil), parentSnapshot...)}
	if preflightRequest.Files != nil {
		snapshot := preflightRequest.Files.Snapshot
		admission.attachmentSnapshot = &snapshot
	}
	return admission, nil
}

func (a *storedResponseNativeAdmission15) terminalBinding15(publicID string) (managedfiles.NativeTerminalBinding15, error) {
	if a == nil || !previousResponseID15(publicID) || !a.hasMatchingPolicy15() || !time.Now().Before(a.expiresAt) {
		return managedfiles.NativeTerminalBinding15{}, ErrStoredResponseLease
	}
	return managedfiles.NativeTerminalBinding15{PublicID: publicID, UserID: a.owner.UserID, APIKeyID: a.owner.APIKeyID, GroupID: a.owner.GroupID, AccountID: a.accountID, PublicModel: a.publicModel, ResolvedModel: a.resolvedModel, BackendID: a.backendID, SubjectDigest: a.subjectDigest, CredentialGeneration: a.credentialGeneration, AdmissionID: a.admissionID, AdmissionBindingSHA256: a.admissionBindingSHA256, RequestWireSHA256: a.requestWireSHA256, Nonce: a.nonce}, nil
}
