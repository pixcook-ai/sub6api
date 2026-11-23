package service

import (
	"context"
	"errors"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

var (
	ErrManagedFileBindingConflict = errors.New("managed file binding conflict")
	ErrManagedFileBusy            = errors.New("managed file is busy")
	ErrManagedFileUncertain       = errors.New("managed file upload outcome is uncertain")
	ErrManagedFileNotReady        = errors.New("managed file is not ready")
)

type ManagedFileSelection struct {
	AccountID int64
	Identity  managedfiles.Identity
}

type ManagedFileVersion struct {
	PublicID string
	Version  int64
}

type ManagedFileOperation struct {
	ID, State, RemoteID string
	InputTokens         int64
}

type ManagedFileBinding struct {
	File       *StoredFile
	Selection  ManagedFileSelection
	Version    int64
	LeaseToken string
	LeaseUntil time.Time
	Operation  ManagedFileOperation
}

type ManagedFileLookup struct {
	AccountID int64
	Bindings  []ManagedFileBinding
}

type ManagedFileReserveInput struct {
	Owner      FileOwner
	Files      []ManagedFileVersion
	Selection  ManagedFileSelection
	LeaseToken string
	Lease      time.Duration
}

// ManagedFileReservation is an internal fence. No field is accepted from a
// public request. The repository rechecks its immutable facts under file locks.
type ManagedFileReservation struct {
	Owner      FileOwner
	Selection  ManagedFileSelection
	LeaseToken string
	Files      []ManagedFileBinding
}

// ManagedFileRepository persists possible remote side effects before HTTP and
// retains late confirmed copies independently of active local file lifetime.
type ManagedFileRepository interface {
	LookupBindings(context.Context, FileOwner, []string) (ManagedFileLookup, error)
	Reserve(context.Context, ManagedFileReserveInput) (*ManagedFileReservation, error)
	StartOperation(context.Context, *ManagedFileReservation, string) error
	CompleteOperation(context.Context, *ManagedFileReservation, string, managedfiles.EnsureResult) (bool, error)
	MarkUncertain(context.Context, *ManagedFileReservation, string, string) error
	AuthorizeGeneration(context.Context, *ManagedFileReservation) error
	ReleaseReservation(context.Context, *ManagedFileReservation) error
}
