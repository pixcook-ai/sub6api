package service

import (
	"context"
	"encoding/json"
	"sync/atomic"
)

// StoredResponsePlan is created from the original caller body only after the
// server has fixed a trusted local-storage route. It is never read from headers.
type StoredResponsePlan struct {
	Owner         StoredResponseOwner
	Request       *StoredResponseRequest
	ClientRequest json.RawMessage
	started       atomic.Bool
	native15      *storedResponseNativePlan15
}

type storedResponsePlanKey struct{}

type storedResponseAccountPinKey struct{}

// WithStoredResponseAccountPin carries only a server-authorized owning account.
func WithStoredResponseAccountPin(ctx context.Context, accountID int64) context.Context {
	return context.WithValue(ctx, storedResponseAccountPinKey{}, accountID)
}

func StoredResponseAccountPinFromContext(ctx context.Context) (int64, bool) {
	if ctx == nil {
		return 0, false
	}
	id, ok := ctx.Value(storedResponseAccountPinKey{}).(int64)
	return id, ok && id > 0
}

func NewStoredResponsePlan(owner StoredResponseOwner, body []byte) (*StoredResponsePlan, error) {
	if !validStoredResponseOwner(owner) {
		return nil, ErrStoredResponseInvalid
	}
	request, err := NormalizeStoredResponseRequest(body)
	if err != nil {
		return nil, err
	}
	return &StoredResponsePlan{Owner: owner, Request: request, ClientRequest: append(json.RawMessage(nil), request.UpstreamBody...)}, nil
}

func WithStoredResponsePlan(ctx context.Context, plan *StoredResponsePlan) context.Context {
	return context.WithValue(ctx, storedResponsePlanKey{}, plan)
}

func StoredResponsePlanFromContext(ctx context.Context) *StoredResponsePlan {
	if ctx == nil {
		return nil
	}
	plan, _ := ctx.Value(storedResponsePlanKey{}).(*StoredResponsePlan)
	return plan
}
