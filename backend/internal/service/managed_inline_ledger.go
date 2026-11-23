package service

import (
	"context"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

// ManagedInlineDescriptor describes a request-private upload, never a Files resource.
type ManagedInlineDescriptor struct {
	FileRef, Filename, MIME, SHA256 string
	Bytes                           int64
}

type ManagedInlineReserveInput struct {
	Owner                 FileOwner
	Selection             ManagedFileSelection
	RequestID, LeaseToken string
	Lease                 time.Duration
	Files                 []ManagedInlineDescriptor
}

type ManagedInlineBinding struct {
	Descriptor ManagedInlineDescriptor
	Operation  ManagedFileOperation
}

// ManagedInlineReservation is an immutable fence rechecked against durable facts.
type ManagedInlineReservation struct {
	Owner                 FileOwner
	Selection             ManagedFileSelection
	RequestID, LeaseToken string
	Files                 []ManagedInlineBinding
}

// ManagedInlineOperationRepository records each possible side effect before HTTP.
// CompleteInline records late confirmation even after release, returning whether
// this operation still has a live lease. AuthorizeInlineGeneration checks the batch.
type ManagedInlineOperationRepository interface {
	ReserveInline(context.Context, ManagedInlineReserveInput) (*ManagedInlineReservation, error)
	StartInline(context.Context, *ManagedInlineReservation, string) error
	CompleteInline(context.Context, *ManagedInlineReservation, string, managedfiles.EnsureResult) (bool, error)
	MarkInlineUncertain(context.Context, *ManagedInlineReservation, string, string) error
	AuthorizeInlineGeneration(context.Context, *ManagedInlineReservation) error
	ReleaseInline(context.Context, *ManagedInlineReservation) error
}
