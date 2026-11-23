package service

import (
	"context"
	"errors"
	"strings"
)

func (s *StoredResponseService) Enabled() bool {
	return s != nil && s.cfg.Enabled
}

// GetManagementModel includes retained deletion tombstones so repeated DELETE
// requests can still enforce the current group's original-client-model policy.
func (s *StoredResponseService) GetManagementModel(ctx context.Context, owner StoredResponseOwner, id string) (string, error) {
	if !s.Enabled() {
		return "", ErrStoredResponsesDisabled
	}
	if !validStoredResponseOwner(owner) || s.repo == nil {
		return "", ErrStoredResponseNotFound
	}
	model, err := s.managementModelForKeyOwner(ctx, owner, id)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(model) == "" {
		return "", errors.New("stored response management model unavailable")
	}
	return model, nil
}

func (s *StoredResponseService) getForKeyOwner(ctx context.Context, owner StoredResponseOwner, id string) (*StoredResponse, error) {
	repo, ok := s.repo.(StoredResponseKeyOwnerRepository)
	if !ok {
		return nil, ErrBPSRequestAuthorizationUnavailable
	}
	row, err := repo.GetForKeyOwner(ctx, owner.UserID, owner.APIKeyID, owner.GroupID, id)
	if err != nil {
		return nil, err
	}
	if row == nil || row.PublicID != id || row.UserID != owner.UserID || row.APIKeyID != owner.APIKeyID || row.GroupID != owner.GroupID {
		return nil, ErrStoredResponseNotFound
	}
	return row, nil
}

func (s *StoredResponseService) managementModelForKeyOwner(ctx context.Context, owner StoredResponseOwner, id string) (string, error) {
	repo, ok := s.repo.(StoredResponseKeyOwnerRepository)
	if !ok {
		return "", ErrBPSRequestAuthorizationUnavailable
	}
	return repo.GetManagementModelForKeyOwner(ctx, owner.UserID, owner.APIKeyID, owner.GroupID, id)
}

func (s *StoredResponseService) deleteForKeyOwner(ctx context.Context, owner StoredResponseOwner, id string) error {
	repo, ok := s.repo.(StoredResponseKeyOwnerRepository)
	if !ok {
		return ErrBPSRequestAuthorizationUnavailable
	}
	return repo.DeleteForKeyOwner(ctx, owner.UserID, owner.APIKeyID, owner.GroupID, id)
}
