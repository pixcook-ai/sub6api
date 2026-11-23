package service

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
)

const coreOAuthRefreshTimeout = 35 * time.Second

// Refresh rotates a profile's grant. The operation claim is persisted before
// the provider is contacted, so independent service instances share one
// provider call through the repository CAS fence.
func (s *CoreOAuthService) Refresh(ctx context.Context, ref string, expectedRevision int64) (*CoreOAuthProfile, error) {
	record, err := s.load(ctx, ref)
	if err != nil {
		return nil, err
	}
	if expectedRevision < 0 || record.TokenRevision != expectedRevision {
		view := s.view(record)
		return &view, ErrCoreOAuthConflict
	}
	return s.refreshRecord(ctx, record)
}

func (s *CoreOAuthService) refreshRecord(ctx context.Context, record *coreOAuthRecord) (*CoreOAuthProfile, error) {
	if err := s.refreshFence(ctx, record); err != nil {
		return nil, err
	}
	if attempt := record.Attempt; attempt != nil && attempt.Status == "pending" && attempt.ExpiresAt.After(time.Now()) {
		return nil, ErrCoreOAuthBusy
	}
	protocol := s.protocols[record.Provider]
	if protocol == nil {
		return nil, ErrCoreOAuthUnavailable
	}
	transport, err := s.transport(ctx, record)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	oldGrant := coreOAuthGrantValue(record.Grant)
	record.Operation = &coreOAuthOperation{ID: uuid.NewString(), Kind: "refresh", ExpiresAt: time.Now().UTC().Add(45 * time.Second)}
	if err = s.save(ctx, record); err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, coreOAuthRefreshTimeout)
	grant, refreshErr := protocol.Refresh(requestCtx, oldGrant, transport)
	requestErr := requestCtx.Err()
	cancel()

	if refreshErr != nil {
		if requestErr != nil || errors.Is(refreshErr, context.Canceled) || errors.Is(refreshErr, context.DeadlineExceeded) {
			return nil, s.persistRefreshUncertain(ctx, record)
		}
		var typed *CoreOAuthProtocolError
		if !errors.As(refreshErr, &typed) || typed == nil {
			return nil, s.persistRefreshUncertain(ctx, record)
		}
		if typed.Permanent {
			return s.persistRefreshNeedsAuthorization(ctx, record)
		}
		if typed.OutcomeUnknown {
			return nil, s.persistRefreshUncertain(ctx, record)
		}
		return s.persistRefreshRejected(ctx, record)
	}
	if !coreOAuthValidGrant(record.Provider, grant, record.Grant) {
		// The provider call succeeded at the transport layer, so an invalid
		// response may still have consumed the rotating refresh token.
		return nil, s.persistRefreshUncertain(ctx, record)
	}

	// Persist the rotated RT before returning the new AT to callers.
	record.Grant = coreOAuthStoreGrant(*grant)
	record.TokenRevision++
	record.Operation = nil
	record.NeedsAuthorization = false
	record.LastErrorCode = ""
	persistCtx, persistCancel := coreOAuthPersistContext(ctx)
	defer persistCancel()
	if err = s.save(persistCtx, record); err != nil {
		return nil, err
	}
	view := s.view(record)
	return &view, nil
}

func (s *CoreOAuthService) persistRefreshNeedsAuthorization(ctx context.Context, record *coreOAuthRecord) (*CoreOAuthProfile, error) {
	persistCtx, cancel := coreOAuthPersistContext(ctx)
	defer cancel()
	record.Operation = nil
	record.NeedsAuthorization = true
	record.LastErrorCode = ErrCoreOAuthAuthorizationRequired.Reason
	if err := s.save(persistCtx, record); err != nil {
		return nil, err
	}
	return nil, ErrCoreOAuthAuthorizationRequired
}

func (s *CoreOAuthService) persistRefreshRejected(ctx context.Context, record *coreOAuthRecord) (*CoreOAuthProfile, error) {
	persistCtx, cancel := coreOAuthPersistContext(ctx)
	defer cancel()
	record.Operation = nil
	record.LastErrorCode = ErrCoreOAuthProviderRejected.Reason
	if err := s.save(persistCtx, record); err != nil {
		return nil, err
	}
	return nil, ErrCoreOAuthProviderRejected
}

func (s *CoreOAuthService) persistRefreshUncertain(ctx context.Context, record *coreOAuthRecord) error {
	persistCtx, cancel := coreOAuthPersistContext(ctx)
	defer cancel()
	record.Operation.Kind = "refresh_uncertain"
	record.Operation.ExpiresAt = time.Now().UTC()
	record.NeedsAuthorization = true
	record.LastErrorCode = ErrCoreOAuthRefreshUncertain.Reason
	if err := s.save(persistCtx, record); err != nil {
		return err
	}
	return ErrCoreOAuthRefreshUncertain
}

func (s *CoreOAuthService) refreshFence(ctx context.Context, record *coreOAuthRecord) error {
	if op := record.Operation; op != nil {
		switch {
		case op.Kind == "refresh_uncertain":
			return ErrCoreOAuthRefreshUncertain
		case op.ExpiresAt.After(time.Now()):
			return ErrCoreOAuthBusy
		case op.Kind == "refresh":
			// An expired claim might have consumed the old RT.
			return s.persistRefreshUncertain(ctx, record)
		default:
			return ErrCoreOAuthAuthorizationRequired
		}
	}
	if record.NeedsAuthorization || record.Grant == nil {
		return ErrCoreOAuthAuthorizationRequired
	}
	return nil
}

func (s *CoreOAuthService) RecoverUnauthorized(ctx context.Context, input CoreOAuthUnauthorizedInput) (*CoreOAuthProfile, error) {
	record, err := s.load(ctx, input.ProfileRef)
	if err != nil {
		return nil, err
	}
	view := s.view(record)
	// Only an upstream OAuth 401 for the exact current identity revision can
	// trigger a refresh. All other failures are ordinary no-ops.
	if input.Origin != "upstream_oauth" || input.StatusCode != http.StatusUnauthorized || input.FailedRevision != record.TokenRevision || input.IdentityEpoch == "" || input.IdentityEpoch != record.IdentityEpoch {
		return &view, nil
	}
	return s.refreshRecord(ctx, record)
}

func (s *CoreOAuthService) CredentialSnapshot(ctx context.Context, ref string) (*CoreOAuthCredentialSnapshot, error) {
	record, err := s.load(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := s.refreshFence(ctx, record); err != nil {
		return nil, err
	}
	if !record.Grant.ExpiresAt.After(time.Now()) {
		if _, err = s.refreshRecord(ctx, record); err != nil {
			return nil, err
		}
		record, err = s.load(ctx, ref)
		if err != nil {
			return nil, err
		}
		if err := s.refreshFence(ctx, record); err != nil {
			return nil, err
		}
	}
	view := s.view(record)
	if !view.Authorized {
		return nil, ErrCoreOAuthAuthorizationRequired
	}
	return &CoreOAuthCredentialSnapshot{Profile: view, AccessToken: record.Grant.AccessToken}, nil
}
