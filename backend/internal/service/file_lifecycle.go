package service

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	ErrFileInvalidCursor          = errors.New("invalid file cursor")
	ErrFileRemoteGuardUnavailable = errors.New("file remote lifecycle guard unavailable")
	ErrFileUploadConcurrency      = errors.New("file upload concurrency limit reached")
	ErrFileUploadDeadline         = errors.New("file upload deadline exceeded")
)

type FileListQuery struct {
	After, Purpose, Order string
	Limit                 int
}
type FileListPage struct {
	Files   []*StoredFile
	HasMore bool
}

// FileReadGrant carries the actual database lease, including any fixed drain
// cap. Callers conservatively anchor its remaining duration before the SQL call.
type FileReadGrant struct {
	DBNow, Until time.Time
}

type FilePurgeTicket struct {
	ID, Version       int64
	StorageKey, Token string
}

// FileLifecycleSnapshot is trusted state read while its file row is locked.
// It is never accepted from a public request or returned as a File object.
type FileLifecycleSnapshot struct {
	ID, UserID, GroupID, Version        int64
	PublicID, State                     string
	RemoteActivityPossible              bool
	ExpiresAt, RetiredAt, LocalPurgedAt *time.Time
}

type FileRemoteDispositionKind string

const (
	FileRemoteNone      FileRemoteDispositionKind = "none"
	FileRemoteConfirmed FileRemoteDispositionKind = "confirmed"
	FileRemotePending   FileRemoteDispositionKind = "pending"
	FileRemoteUncertain FileRemoteDispositionKind = "uncertain"
)

type FileRemoteDisposition struct {
	Kind     FileRemoteDispositionKind
	Revision string
}

// FileRemoteLifecyclePort belongs to managed-file ledger integration. The
// supplied transaction already holds file locks; methods do SQL only, in
// file -> binding/operation -> read-lease order. An error is never None.
// A nil port is safe only for a row whose write-ahead flag is still false.
type FileRemoteLifecyclePort interface {
	FenceForRetirement(context.Context, *sql.Tx, FileLifecycleSnapshot) (FileRemoteDisposition, error)
	InspectForLocalPurge(context.Context, *sql.Tx, FileLifecycleSnapshot) (FileRemoteDisposition, error)
}

func (s *FileService) List(ctx context.Context, owner FileOwner, q FileListQuery) (FileListPage, error) {
	if !s.Enabled() {
		return FileListPage{}, ErrFilesDisabled
	}
	if owner.UserID <= 0 || owner.APIKeyID <= 0 || owner.GroupID <= 0 {
		return FileListPage{}, ErrInvalidFile
	}
	if q.Order == "" {
		q.Order = "desc"
	}
	if q.Limit < 1 || q.Limit > 10000 || (q.Order != "asc" && q.Order != "desc") || len(q.After) > 64 || len(q.Purpose) > 32 || !utf8.ValidString(q.Purpose) || strings.IndexFunc(q.Purpose, unicode.IsControl) >= 0 {
		return FileListPage{}, ErrInvalidFile
	}
	return s.repo.ListVisibleFiles(ctx, owner, q)
}
