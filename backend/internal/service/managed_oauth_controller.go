package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/google/uuid"
)

// Unknown is distinct from revoked: a revoked persisted key must never fall back to an env scope.
var ErrManagedOAuthControllerKeyUnknown = errors.New("unknown OAuth controller key")

type ManagedOAuthControllerKeyState struct {
	ManagedOAuthIdentity
	AccountID   int64  `json:"account_id"`
	OperationID string `json:"operation_id"`
	Active      bool   `json:"active"`
}

type ManagedOAuthControllerKeyRepository interface {
	PutManagedOAuthControllerKey(context.Context, int64, ManagedOAuthState, string, string) (*ManagedOAuthControllerKeyState, error)
	RevokeManagedOAuthControllerKey(context.Context, int64, string) (*ManagedOAuthControllerKeyState, error)
	FindManagedOAuthControllerKey(context.Context, string) (*ManagedOAuthControllerKeyState, error)
}

type ManagedOAuthControllerService interface {
	RegisterManagedOAuthControllerKey(context.Context, int64, string, string) (*ManagedOAuthControllerKeyState, error)
	RevokeManagedOAuthControllerKey(context.Context, int64, string) (*ManagedOAuthControllerKeyState, error)
	AuthenticateManagedOAuthControllerKey(context.Context, string) (*ManagedOAuthControllerKeyState, error)
}

func validManagedOAuthControllerOperation(operation string) bool {
	id, err := uuid.Parse(operation)
	return err == nil && id != uuid.Nil && id.String() == operation
}

func ManagedOAuthControllerKeyHash(key string) (string, error) {
	if len(key) < 32 || len(key) > 4096 {
		return "", ErrManagedOAuthInvalid
	}
	for _, b := range []byte(key) {
		if b <= 32 || b > 126 {
			return "", ErrManagedOAuthInvalid
		}
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]), nil
}

func (s *adminServiceImpl) RegisterManagedOAuthControllerKey(ctx context.Context, accountID int64, operationID, key string) (*ManagedOAuthControllerKeyState, error) {
	if accountID <= 0 || !validManagedOAuthControllerOperation(operationID) {
		return nil, ErrManagedOAuthInvalid
	}
	hash, err := ManagedOAuthControllerKeyHash(key)
	if err != nil {
		return nil, err
	}
	repo, ok := s.accountRepo.(ManagedOAuthControllerKeyRepository)
	if !ok {
		return nil, ErrManagedOAuthUnavailable
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	state, err := managedOAuthStateFromAccount(account)
	if err != nil {
		return nil, err
	}
	if state.PublicationFenceOperationID != "" {
		return nil, ErrManagedOAuthConflict
	}
	return repo.PutManagedOAuthControllerKey(ctx, accountID, *state, operationID, hash)
}

func (s *adminServiceImpl) RevokeManagedOAuthControllerKey(ctx context.Context, accountID int64, operationID string) (*ManagedOAuthControllerKeyState, error) {
	if accountID <= 0 || !validManagedOAuthControllerOperation(operationID) {
		return nil, ErrManagedOAuthInvalid
	}
	repo, ok := s.accountRepo.(ManagedOAuthControllerKeyRepository)
	if !ok {
		return nil, ErrManagedOAuthUnavailable
	}
	return repo.RevokeManagedOAuthControllerKey(ctx, accountID, operationID)
}

func (s *adminServiceImpl) AuthenticateManagedOAuthControllerKey(ctx context.Context, key string) (*ManagedOAuthControllerKeyState, error) {
	hash, err := ManagedOAuthControllerKeyHash(key)
	if err != nil {
		return nil, ErrManagedOAuthInvalid
	}
	repo, ok := s.accountRepo.(ManagedOAuthControllerKeyRepository)
	if !ok {
		return nil, ErrManagedOAuthUnavailable
	}
	record, err := repo.FindManagedOAuthControllerKey(ctx, hash)
	if err != nil {
		return nil, err
	}
	if record == nil || !record.Active || record.AccountID <= 0 || !validManagedOAuthControllerOperation(record.OperationID) || record.ManagedOAuthIdentity.Validate() != nil {
		return nil, ErrManagedOAuthInvalid
	}
	current, err := s.GetManagedOAuthState(ctx, record.AccountID)
	if err != nil || current == nil || current.PublicationFenceOperationID != "" || current.AccountID != record.AccountID || current.ManagedOAuthIdentity != record.ManagedOAuthIdentity {
		return nil, ErrManagedOAuthInvalid
	}
	return record, nil
}
