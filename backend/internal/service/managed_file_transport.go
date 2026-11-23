package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

var ErrManagedFileGenerationReplay = errors.New("managed file generation cannot be replayed")

type managedFileGenerationKey struct{}
type managedFileGeneration struct {
	caller                                context.Context
	accountID                             int64
	key, admission, owner, group, binding string
	authorize                             func(context.Context) error
	sent                                  atomic.Bool
}

func withManagedFileGeneration(ctx context.Context, accountID int64, key, admission, owner, group, binding string, authorize func(context.Context) error) context.Context {
	return context.WithValue(ctx, managedFileGenerationKey{}, &managedFileGeneration{caller: ctx, accountID: accountID, key: key, admission: admission, owner: owner, group: group, binding: binding, authorize: authorize})
}
func managedFileGenerationFrom(ctx context.Context) *managedFileGeneration {
	state, _ := ctx.Value(managedFileGenerationKey{}).(*managedFileGeneration)
	return state
}
func IsManagedFileGeneration(ctx context.Context) bool { return managedFileGenerationFrom(ctx) != nil }

// The original caller remains authoritative before the one external send even
// when streaming forwarding detached its context to drain reported usage.
func claimManagedFileGeneration(ctx context.Context, accountID int64) error {
	state := managedFileGenerationFrom(ctx)
	if state == nil {
		return nil
	}
	if state.accountID != accountID {
		return &ManagedFilePreparationError{ErrManagedFileGenerationReplay}
	}
	if err := state.caller.Err(); err != nil {
		return &ManagedFilePreparationError{err}
	}
	if err := ctx.Err(); err != nil {
		return &ManagedFilePreparationError{err}
	}
	if !state.sent.CompareAndSwap(false, true) {
		return &ManagedFilePreparationError{ErrManagedFileGenerationReplay}
	}
	if state.authorize == nil {
		return &ManagedFilePreparationError{ErrManagedFileNotReady}
	}
	if err := state.authorize(ctx); err != nil {
		return &ManagedFilePreparationError{err}
	}
	if err := state.caller.Err(); err != nil {
		return &ManagedFilePreparationError{err}
	}
	if err := ctx.Err(); err != nil {
		return &ManagedFilePreparationError{err}
	}
	return nil
}

// Called after every public/header-override transform. Private proof can only
// come from the server-resolved context; even ordinary requests strip it.
func prepareManagedFileGenerationRequest(req *http.Request) {
	for _, key := range []string{managedfiles.ServiceKeyHeader, managedfiles.AdmissionHeader, managedfiles.OwnerRefHeader, managedfiles.GroupRefHeader, managedfiles.BindingHeader, managedfiles.RecoveryFeatureHeader} {
		req.Header.Del(key)
	}
	state := managedFileGenerationFrom(req.Context())
	if state == nil {
		return
	}
	req.Header.Set(managedfiles.ServiceKeyHeader, state.key)
	req.Header.Set(managedfiles.AdmissionHeader, state.admission)
	req.Header.Set(managedfiles.OwnerRefHeader, state.owner)
	req.Header.Set(managedfiles.GroupRefHeader, state.group)
	req.Header.Set(managedfiles.BindingHeader, state.binding)
	for _, key := range []string{"Idempotency-Key", "X-Idempotency-Key"} {
		req.Header.Del(key)
	}
	req.GetBody = nil
	*req = *req.WithContext(WithHTTPUpstreamRedirectsDisabled(req.Context()))
}

func (s *OpenAIGatewayService) SetManagedFileResolver(resolver *ManagedFileResolver) {
	s.managedFiles = resolver
}

func replaceManagedFileWire(req *http.Request, body []byte) {
	if !IsManagedFileGeneration(req.Context()) {
		return
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.TransferEncoding = nil
	req.Header.Del("Content-Encoding")
	req.GetBody = nil
}
