package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/ctxkey"
	pkghttputil "github.com/MACOS-DO/sub4api/internal/pkg/httputil"
	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// SetStoredResponseService attaches the same bounded storage service used by
// the resource handlers. It does not enable storage for ordinary providers.
func (s *OpenAIGatewayService) SetStoredResponseService(stored *StoredResponseService) {
	s.storedResponses = stored
}

type storedResponseGenerationKey struct{}
type storedResponseGeneration struct {
	caller    context.Context
	accountID int64
	authorize func(context.Context) error
	sent      atomic.Bool
}

func isStoredResponseGeneration(ctx context.Context) bool {
	state, _ := ctx.Value(storedResponseGenerationKey{}).(*storedResponseGeneration)
	return state != nil
}

// Preflight consumes the request after every model, instruction, header and
// compression transform. Those exact bytes are restored for the one send.
func (s *OpenAIGatewayService) admitStoredResponse(ctx context.Context, account *Account, req *http.Request, plan *StoredResponsePlan) (*http.Request, context.Context, func() error, error) {
	if s.storedResponses == nil || !s.storedResponses.Enabled() || req == nil || req.Body == nil || plan == nil || plan.Request == nil || !plan.Request.Store || ManagedFilePlanFromContext(ctx) != nil {
		return nil, ctx, nil, &StoredResponseLocalError{ErrStoredResponsesDisabled}
	}
	config, err := BPSStoredResponsesConfigForAccount(account)
	if err != nil || req.Header.Get("Content-Encoding") != "" {
		return nil, ctx, nil, &StoredResponseLocalError{ErrBPSStoredResponseRoute}
	}
	groupID := plan.Owner.GroupID
	requestedModel, ok := RequestedPublicModelFromContext(ctx)
	if !ok {
		requestedModel, _ = ctx.Value(ctxkey.Model).(string)
	}
	if requestedModel == "" {
		requestedModel = plan.Request.Model
	}
	forwardModel := plan.Request.Model
	if resolved, ok := ResolvedUpstreamModelFromContext(ctx); ok {
		forwardModel = resolved
	}
	nativeAPIKey, nativeBaseURL := account.GetCredential("api_key"), account.GetCredential("base_url")
	nativeResolvedModel := ResolveOpenAIAccountUpstreamModelForRequest(account, forwardModel, false)
	authorize := func(sendCtx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := CheckBPSRequestAuthorization(sendCtx, requestedModel); err != nil {
			return err
		}
		current, err := s.AuthorizeStoredResponseAccount(sendCtx, groupID, requestedModel, account)
		if err != nil {
			return err
		}
		if plan.native15 != nil {
			freshConfig, err := BPSStoredResponsesConfigForAccount(current)
			if err != nil || freshConfig != config || current.GetCredential("api_key") != nativeAPIKey || current.GetCredential("base_url") != nativeBaseURL || ResolveOpenAIAccountUpstreamModelForRequest(current, forwardModel, false) != nativeResolvedModel {
				return ErrBPSRequestAuthorizationRevoked
			}
		}
		if plan.native15 != nil && plan.native15.previousID != "" {
			if err := s.authorizeStoredResponseNativeParentPlan15(sendCtx, plan, account, requestedModel, forwardModel); err != nil {
				return err
			}
		}
		return ctx.Err()
	}
	if err = authorize(ctx); err != nil {
		return nil, ctx, nil, &StoredResponseLocalError{err}
	}
	validatedURL, err := validateOutboundURLWithConfig(s.cfg, config.ControlURL)
	if err != nil {
		return nil, ctx, nil, &StoredResponseLocalError{ErrBPSStoredResponseRoute}
	}
	if req.ContentLength < 0 {
		return nil, ctx, nil, &StoredResponseLocalError{ErrStoredResponseInvalid}
	}
	if req.ContentLength > managedfiles.MaxJSONBytes {
		return nil, ctx, nil, &StoredResponseLocalError{ErrStoredResponseTooLarge}
	}
	// Keep the final request's header deadline and shared ingress budget. The
	// wire outlives preflight, so this scope is released with the attempt.
	wireCtx, releaseWire := pkghttputil.WithRequestBodyMemoryScope(req.Context())
	retained := false
	defer func() {
		if !retained {
			releaseWire()
		}
	}()
	if err = pkghttputil.ReserveRequestBodyMemory(wireCtx, req.ContentLength+1); err != nil {
		return nil, ctx, nil, &StoredResponseLocalError{err}
	}
	wire := make([]byte, int(req.ContentLength)+1)
	n, readErr := io.ReadFull(req.Body, wire)
	_ = req.Body.Close()
	if (readErr != io.EOF && readErr != io.ErrUnexpectedEOF) || int64(n) != req.ContentLength {
		return nil, ctx, nil, &StoredResponseLocalError{ErrStoredResponseInvalid}
	}
	wire = wire[:n:n]
	if plan.native15 == nil && gjson.GetBytes(wire, "metadata").Exists() {
		// The pure-text plan keeps client tags locally. Bind and send the same
		// final wire without those tags after all upstream transformations.
		if err = pkghttputil.ReserveRequestBodyMemory(wireCtx, int64(len(wire))); err != nil {
			return nil, ctx, nil, &StoredResponseLocalError{err}
		}
		wire, err = sjson.DeleteBytes(wire, "metadata")
		if err != nil {
			return nil, ctx, nil, &StoredResponseLocalError{ErrStoredResponseInvalid}
		}
	}
	var nativeFiles *managedfiles.NativeAttachmentAdmission15
	var nativeFilePlan *ManagedFilePlan
	var nativeFilesAuthorize func(context.Context) error
	if plan.native15 != nil {
		nativeFiles, nativeFilePlan, nativeFilesAuthorize, err = s.prepareNativeFiles15(wireCtx, account, plan, wire, authorize)
		if err != nil {
			return nil, ctx, nil, &StoredResponseLocalError{err}
		}
	}
	filesRetained := false
	defer func() {
		if !filesRetained && nativeFilePlan != nil {
			_ = nativeFilePlan.Close()
		}
	}()
	baseAuthorize := authorize
	authorize = func(check context.Context) error {
		if err := baseAuthorize(check); err != nil {
			return err
		}
		if nativeFilesAuthorize != nil {
			return nativeFilesAuthorize(check)
		}
		return check.Err()
	}
	owner, group := strconv.FormatInt(plan.Owner.UserID, 10), strconv.FormatInt(plan.Owner.GroupID, 10)
	var preflight managedfiles.ResponsesPreflightResult
	var native *storedResponseNativeAdmission15
	var capture *StoredResponseCapture
	var expiresAt time.Time
	if plan.native15 != nil {
		// Retained native context outlives the bounded preflight HTTP scope.
		if err = pkghttputil.ReserveRequestBodyMemory(wireCtx, 4*int64(len(plan.native15.parentSnapshot))+3*int64(len(plan.ClientRequest))+(32<<10)); err != nil {
			return nil, ctx, nil, &StoredResponseLocalError{err}
		}
		native, err = s.preflightStoredResponseNative15(WithHTTPUpstreamRedirectsDisabled(wireCtx), config, plan.Owner, plan.Request, wire, plan.native15.parentSnapshot, plan.native15.previousID, nativeFiles)
		if err == nil {
			expiresAt = native.expiresAt
		}
	} else {
		client, createErr := managedfiles.NewResponsesPreflightClient(managedfiles.ResponsesClientConfig{ControlURL: validatedURL, ServiceKey: config.ServiceKey, ProxyURL: config.ProxyURL, Expected: config.Expected, AccountID: config.AccountID, Concurrency: config.Concurrency}, s.httpUpstream)
		if createErr != nil {
			return nil, ctx, nil, &StoredResponseLocalError{ErrBPSStoredResponseRoute}
		}
		preflight, err = client.Preflight(WithHTTPUpstreamRedirectsDisabled(wireCtx), owner, group, plan.ClientRequest, wire)
		if err == nil {
			expiresAt = time.Unix(preflight.ExpiresAt, 0)
		}
	}
	if err != nil {
		return nil, ctx, nil, &StoredResponseLocalError{err}
	}
	if err = authorize(wireCtx); err != nil {
		return nil, ctx, nil, &StoredResponseLocalError{err}
	}
	if native != nil {
		capture, err = s.storedResponses.BeginNative15(wireCtx, plan.Request, native, authorize)
	} else {
		capture, err = s.storedResponses.Begin(wireCtx, plan.Owner, plan.Request, StoredResponseBinding{AccountID: account.ID, ResolvedModel: gjson.GetBytes(wire, "model").String(), Scope: preflight.BindingSHA256})
	}
	if err != nil {
		return nil, ctx, nil, &StoredResponseLocalError{err}
	}
	captureParent := ctx
	var nativeActivation string
	if native != nil {
		captureParent, nativeActivation, err = prepareNativeStoredResponseAttempt15(ctx, native, capture, config.ServiceKey)
		if err != nil {
			return nil, ctx, nil, &StoredResponseLocalError{err}
		}
	}
	// Closing a completed HTTP body releases its header guard. That normal
	// network cleanup must not cancel the database capture that follows it.
	attemptCtx, cleanup := withStoredResponseCapture(captureParent, capture)
	leaseCtx := attemptCtx
	state := &storedResponseGeneration{caller: ctx, accountID: account.ID, authorize: func(sendCtx context.Context) error {
		if err := leaseCtx.Err(); err != nil {
			return err
		}
		if err := wireCtx.Err(); err != nil {
			return err
		}
		if !time.Now().Before(expiresAt) {
			return ErrBPSStoredResponseRoute
		}
		if err := authorize(sendCtx); err != nil {
			return err
		}
		// Fresh parent/account authorization can wait. Recheck the signed
		// admission deadline and every attempt context after that wait.
		for _, checked := range []context.Context{leaseCtx, wireCtx, sendCtx} {
			if err := checked.Err(); err != nil {
				return err
			}
		}
		if !time.Now().Before(expiresAt) {
			return ErrBPSStoredResponseRoute
		}
		return nil
	}}
	attemptCtx = context.WithValue(attemptCtx, storedResponseGenerationKey{}, state)
	// Preserve the existing header deadline on the HTTP request and bridge lease
	// cancellation toward the network without reversing body-close cancellation.
	networkCtx, cancelNetwork := context.WithCancel(wireCtx)
	stopLeaseCancellation := context.AfterFunc(leaseCtx, cancelNetwork)
	requestCtx := context.WithValue(networkCtx, storedResponseGenerationKey{}, state)
	if native != nil {
		requestCtx = context.WithValue(requestCtx, nativeTerminalAttemptKey15{}, nativeTerminalAttemptFrom15(attemptCtx))
		requestCtx = context.WithValue(requestCtx, storedResponseNativeAckKey15{}, attemptCtx.Value(storedResponseNativeAckKey15{}))
	}
	requestCtx = context.WithValue(requestCtx, storedResponseCaptureKey{}, storedResponseAttemptFrom(attemptCtx))
	requestCtx = WithHTTPUpstreamRedirectsDisabled(requestCtx)
	req = req.WithContext(requestCtx)
	req.Body = io.NopCloser(bytes.NewReader(wire))
	req.ContentLength = int64(len(wire))
	req.TransferEncoding = nil
	req.GetBody = nil
	stripStoredResponsePrivateHeaders(req.Header)
	for _, key := range []string{managedfiles.AdmissionHeader, managedfiles.BindingHeader, "Idempotency-Key", "X-Idempotency-Key"} {
		req.Header.Del(key)
	}
	if native != nil {
		stripNativeResponsePrivateHeaders15(req.Header)
	}
	req.Header.Set(managedfiles.ServiceKeyHeader, config.ServiceKey)
	if native != nil {
		req.Header.Set(managedfiles.NativeResponsesActivationHeader15, nativeActivation)
	} else {
		req.Header.Set(managedfiles.OwnerRefHeader, owner)
		req.Header.Set(managedfiles.GroupRefHeader, group)
		req.Header.Set(managedfiles.ResponsesAdmissionHeader, preflight.AdmissionID)
		req.Header.Set(managedfiles.ResponsesWireHeader, preflight.WireSHA256)
		req.Header.Set(managedfiles.ResponsesBindingHeader, preflight.BindingSHA256)
		// The HMAC expectation stays on a server context, never a client header.
		attemptCtx = context.WithValue(attemptCtx, storedResponseAckKey{}, storedResponseAck{config.ServiceKey, preflight})
		req = req.WithContext(context.WithValue(requestCtx, storedResponseAckKey{}, storedResponseAck{config.ServiceKey, preflight}))
	}
	retained = true
	filesRetained = true
	return req, attemptCtx, func() error {
		if nativeFilePlan != nil {
			defer nativeFilePlan.Close()
		}
		defer releaseWire()
		defer cancelNetwork()
		stopLeaseCancellation()
		syncNativeTerminalCaptureUsage15(attemptCtx)
		return cleanup()
	}, nil
}

type storedResponseAckKey struct{}
type storedResponseAck struct {
	key    string
	result managedfiles.ResponsesPreflightResult
}

func verifyStoredResponseAck(ctx context.Context, header http.Header) bool {
	if verified, native := verifyNativeStoredResponseAck15(ctx, header); native {
		return verified
	}
	proof, ok := ctx.Value(storedResponseAckKey{}).(storedResponseAck)
	if !ok {
		return true
	}
	values := header.Values(managedfiles.ResponsesAdmittedHeader)
	return len(values) == 1 && managedfiles.VerifyResponsesAck(proof.key, proof.result, values[0])
}

func stripStoredResponsePrivateHeaders(headers http.Header) {
	for key := range headers {
		if strings.HasPrefix(strings.ToLower(key), "x-bps-responses-") {
			delete(headers, key)
		}
	}
}

func claimStoredResponseGeneration(ctx context.Context, accountID int64) error {
	state, _ := ctx.Value(storedResponseGenerationKey{}).(*storedResponseGeneration)
	if state == nil {
		return nil
	}
	if state.accountID != accountID || state.authorize == nil || !state.sent.CompareAndSwap(false, true) {
		return &StoredResponseLocalError{ErrStoredResponseConflict}
	}
	if err := state.caller.Err(); err != nil {
		return &StoredResponseLocalError{err}
	}
	if err := ctx.Err(); err != nil {
		return &StoredResponseLocalError{err}
	}
	if err := state.authorize(ctx); err != nil {
		return &StoredResponseLocalError{err}
	}
	if err := state.caller.Err(); err != nil {
		return &StoredResponseLocalError{err}
	}
	if err := ctx.Err(); err != nil {
		return &StoredResponseLocalError{err}
	}
	return nil
}

// An invalid activation ACK must never release content or trigger replay. Read
// only bounded, structurally valid usage facts for the existing settlement path.
func (s *OpenAIGatewayService) readUnacknowledgedStoredResponseUsage(ctx context.Context, account *Account, resp *http.Response) (OpenAIUsage, bool) {
	var usage OpenAIUsage
	if resp == nil || resp.Body == nil {
		return usage, false
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stop := context.AfterFunc(bounded, func() { _ = resp.Body.Close() })
	defer stop()
	defer resp.Body.Close()
	limit := int64(1 << 20)
	if s.storedResponses != nil {
		limit = s.storedResponses.cfg.Limits.MaxResponseBytes
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	oversized := int64(len(raw)) > limit
	if oversized {
		raw = raw[:limit]
	}
	reported := false
	observe := func(payload []byte, path string) {
		if _, ok := decodeBPSFailureObject(payload); !ok {
			return
		}
		if safe := safeOpenAIFailureUsage(gjson.GetBytes(payload, path)); safe != nil {
			parsed := storedResponseOpenAIUsage(safe)
			mergeOpenAIUsageNonZero(&usage, *parsed)
			reported = true
		}
	}
	if isEventStreamResponse(resp.Header) {
		forEachOpenAISSEFrame(string(raw), func(event string, payload []byte) {
			if _, ok := decodeBPSFailureObject(payload); !ok || gjson.GetBytes(payload, "type").String() != event {
				return
			}
			switch event {
			case "response.created", "response.in_progress", "response.completed", "response.incomplete":
				if gjson.GetBytes(payload, "response.object").String() != "response" {
					return
				}
				status := gjson.GetBytes(payload, "response.status").String()
				expected := map[string]string{"response.created": "in_progress", "response.in_progress": "in_progress", "response.completed": "completed", "response.incomplete": "incomplete"}[event]
				if status != expected {
					return
				}
			case "response.failed", "response.cancelled", "error":
				if !bpsStreamingFailure(account, resp, payload) {
					return
				}
			default:
				return
			}
			observe(payload, "response.usage")
		})
		return usage, reported
	}
	if readErr != nil || oversized {
		return usage, false
	}
	if resp.StatusCode >= 400 {
		safe, valid := bpsNonStreamingFailure(account, resp, raw)
		if valid {
			observe(safe, "response.usage")
		}
		return usage, reported
	}
	if gjson.GetBytes(raw, "object").String() != "response" {
		return usage, false
	}
	switch gjson.GetBytes(raw, "status").String() {
	case "completed", "incomplete", "in_progress":
		observe(raw, "usage")
	}
	return usage, reported
}
