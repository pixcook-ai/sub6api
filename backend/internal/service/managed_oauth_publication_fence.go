package service

import (
	"context"
	"net/http"

	infraerrors "github.com/MACOS-DO/sub4api/internal/pkg/errors"
)

var ErrManagedOAuthFenceMissing = infraerrors.New(http.StatusNotFound, "MANAGED_OAUTH_FENCE_NOT_FOUND", "no permanent OAuth publication fence exists")

type ManagedOAuthPublicationFenceInput struct {
	OperationID   string               `json:"operation_id"`
	Identity      ManagedOAuthIdentity `json:"identity"`
	TokenRevision int64                `json:"token_revision"`
}

// This receipt proves permanent publisher revocation, not receiver activation.
type ManagedOAuthPublicationFence struct {
	ManagedOAuthPublicationFenceInput
	AccountID int64 `json:"account_id"`
	Fenced    bool  `json:"fenced"`
}

type ManagedOAuthPublicationFenceRepository interface {
	FenceManagedOAuthPublicationIfMatch(context.Context, int64, ManagedOAuthState, string) (bool, error)
}

type ManagedOAuthPublicationFenceService interface {
	FenceManagedOAuthPublication(context.Context, int64, *ManagedOAuthPublicationFenceInput) (*ManagedOAuthPublicationFence, error)
	GetManagedOAuthPublicationFence(context.Context, int64) (*ManagedOAuthPublicationFence, error)
}

func (s *adminServiceImpl) GetManagedOAuthPublicationFence(ctx context.Context, accountID int64) (*ManagedOAuthPublicationFence, error) {
	if accountID <= 0 {
		return nil, ErrManagedOAuthInvalid
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	state, err := managedOAuthStateFromAccount(account)
	if err != nil {
		return nil, err
	}
	if state.PublicationFenceOperationID == "" {
		return nil, ErrManagedOAuthFenceMissing
	}
	return managedOAuthFenceReceipt(accountID, state), nil
}

func managedOAuthFenceReceipt(accountID int64, state *ManagedOAuthState) *ManagedOAuthPublicationFence {
	return &ManagedOAuthPublicationFence{ManagedOAuthPublicationFenceInput: ManagedOAuthPublicationFenceInput{
		OperationID: state.PublicationFenceOperationID, Identity: state.ManagedOAuthIdentity, TokenRevision: state.TokenRevision,
	}, AccountID: accountID, Fenced: true}
}

func (s *adminServiceImpl) FenceManagedOAuthPublication(ctx context.Context, accountID int64, input *ManagedOAuthPublicationFenceInput) (*ManagedOAuthPublicationFence, error) {
	if input == nil || accountID <= 0 || !validManagedOAuthControllerOperation(input.OperationID) || input.Identity.Validate() != nil || input.TokenRevision < 0 || input.TokenRevision > managedOAuthMaxRevision {
		return nil, ErrManagedOAuthInvalid
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	state, err := managedOAuthStateFromAccount(account)
	if err != nil {
		return nil, err
	}
	if state.ManagedOAuthIdentity != input.Identity || state.TokenRevision != input.TokenRevision {
		return nil, ErrManagedOAuthConflict
	}
	if state.PublicationFenceOperationID != "" {
		if state.PublicationFenceOperationID != input.OperationID {
			return nil, ErrManagedOAuthConflict
		}
		return managedOAuthFenceReceipt(accountID, state), nil
	}
	repo, ok := s.accountRepo.(ManagedOAuthPublicationFenceRepository)
	if !ok {
		return nil, ErrManagedOAuthUnavailable
	}
	applied, err := repo.FenceManagedOAuthPublicationIfMatch(ctx, accountID, *state, input.OperationID)
	if err != nil {
		return nil, err
	}
	if !applied {
		return nil, ErrManagedOAuthConflict
	}
	next := *state
	next.PublicationFenceOperationID = input.OperationID
	return managedOAuthFenceReceipt(accountID, &next), nil
}
