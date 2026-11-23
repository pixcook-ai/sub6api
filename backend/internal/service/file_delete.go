package service

import (
	"context"
	"errors"
	"time"
)

var ErrFileRemoteDeleteUnsupported = errors.New("remote file deletion is not supported")

// LocalFileDeleteRepository is optional so an older repository fails closed.
// Success proves that no managed remote side effect can exist for this file.
type LocalFileDeleteRepository interface {
	DeleteLocalFile(context.Context, FileOwner, string, time.Duration) error
}

func (s *FileService) Delete(ctx context.Context, owner FileOwner, id string) error {
	if !s.Enabled() {
		return ErrFilesDisabled
	}
	if owner.UserID <= 0 || owner.GroupID <= 0 || owner.APIKeyID <= 0 || id == "" || len(id) > 256 {
		return ErrInvalidFile
	}
	repo, ok := s.repo.(LocalFileDeleteRepository)
	if !ok {
		return ErrFileRemoteGuardUnavailable
	}
	return repo.DeleteLocalFile(ctx, owner, id, time.Duration(s.cfg.ReadDrainSeconds)*time.Second)
}
