package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

type storedResponseNativeAckKey15 struct{}
type storedResponseNativeAck15 struct {
	key      string
	result   managedfiles.NativeResponsesPreflightResult15
	publicID string
}

func stripNativeResponsePrivateHeaders15(header http.Header) {
	for name := range header {
		if strings.HasPrefix(strings.ToLower(name), "x-bps-") {
			delete(header, name)
		}
	}
}

func verifyNativeStoredResponseAck15(ctx context.Context, header http.Header) (bool, bool) {
	proof, ok := ctx.Value(storedResponseNativeAckKey15{}).(storedResponseNativeAck15)
	if !ok {
		return false, false
	}
	values := header.Values(managedfiles.NativeResponsesAdmittedHeader15)
	return len(values) == 1 && managedfiles.VerifyNativeResponsesAck15(proof.key, proof.result, proof.publicID, values[0]) == nil, true
}

func prepareNativeStoredResponseAttempt15(ctx context.Context, admission *storedResponseNativeAdmission15, capture *StoredResponseCapture, key string) (context.Context, string, error) {
	fail := func(err error) (context.Context, string, error) {
		abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if capture != nil {
			err = errors.Join(err, capture.Abort(abortCtx))
		}
		return nil, "", err
	}
	if admission == nil || capture == nil {
		return fail(ErrStoredResponseInvalid)
	}
	binding, err := admission.terminalBinding15(capture.PublicID())
	if err != nil {
		return fail(err)
	}
	activation, err := managedfiles.EncodeNativeResponsesActivation15(key, admission.preflightRequest, admission.preflightResult, capture.PublicID(), time.Now())
	if err != nil {
		return fail(ErrStoredResponseInvalid)
	}
	gate, err := newNativeTerminalGate15(key, binding, admission.nativeWireSHA256, admission.maxBytes)
	if err != nil {
		return fail(err)
	}
	bound, err := withNativeTerminalAttempt15(ctx, gate)
	if err != nil {
		return fail(err)
	}
	bound = context.WithValue(bound, storedResponseNativeAckKey15{}, storedResponseNativeAck15{key: key, result: admission.preflightResult, publicID: capture.PublicID()})
	return bound, activation, nil
}
