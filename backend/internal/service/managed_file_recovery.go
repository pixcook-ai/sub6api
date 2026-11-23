package service

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

const (
	ManagedFileRecoveryMaxAttempts = 3
	ManagedFileRecoveryWindow      = time.Hour
)

var ErrManagedFileRecoveryBudget = errors.New("managed file recovery budget exhausted")

// The caller must match FileRefs to a precise missing-metadata response from
// the selected backend and its just-sent manifest before requesting recovery.
type ManagedFileRecoveryInput struct {
	Reservation *ManagedFileReservation
	FileRefs    []string
}

// ReserveRecovery appends recovery operations under the existing preparation
// lease. It never changes the frozen backend identity or remote copy facts.
type ManagedFileRecoveryRepository interface {
	ReserveRecovery(context.Context, ManagedFileRecoveryInput) (*ManagedFileReservation, error)
}

func (in ManagedFileRecoveryInput) Validate() error {
	if in.Reservation == nil || len(in.Reservation.Files) == 0 || in.Reservation.Owner.UserID <= 0 || in.Reservation.Owner.GroupID <= 0 || in.Reservation.Owner.APIKeyID <= 0 || in.Reservation.Selection.AccountID <= 0 || managedfiles.ValidateIdentity(in.Reservation.Selection.Identity) != nil || in.Reservation.LeaseToken == "" {
		return ErrManagedFileNotReady
	}
	if len(in.FileRefs) == 0 || len(in.FileRefs) > managedfiles.MaxUniqueFiles {
		return ErrManagedFileNotReady
	}
	refs := append([]string(nil), in.FileRefs...)
	if !sort.StringsAreSorted(refs) {
		return ErrManagedFileNotReady
	}
	for i, ref := range refs {
		if strings.TrimSpace(ref) != ref || ref == "" || (i > 0 && refs[i-1] == ref) || !managedfiles.ValidRemoteID(ref) {
			return ErrManagedFileNotReady
		}
	}
	allowed := make(map[string]struct{}, len(in.Reservation.Files))
	for _, binding := range in.Reservation.Files {
		if binding.File == nil || binding.Selection != in.Reservation.Selection || binding.LeaseToken != in.Reservation.LeaseToken || binding.Version <= 0 || binding.Operation.ID == "" {
			return ErrManagedFileNotReady
		}
		if _, exists := allowed[binding.File.PublicID]; exists {
			return ErrManagedFileNotReady
		}
		allowed[binding.File.PublicID] = struct{}{}
	}
	for _, ref := range refs {
		if _, ok := allowed[ref]; !ok {
			return ErrManagedFileNotReady
		}
	}
	return nil
}

func ReserveManagedFileRecovery(ctx context.Context, repo ManagedFileRecoveryRepository, in ManagedFileRecoveryInput, authorize func(context.Context) error) (*ManagedFileReservation, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	if repo == nil || authorize == nil {
		return nil, ErrManagedFileNotReady
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := authorize(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return repo.ReserveRecovery(ctx, in)
}
