package service

import (
	"context"
	"io"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

// ManagedFilePreparationError identifies a local admission/authorization failure
// before generation. It must not count as an upstream account health failure.
type ManagedFilePreparationError struct{ Err error }

func (e *ManagedFilePreparationError) Error() string { return "managed file preparation failed" }
func (e *ManagedFilePreparationError) Unwrap() error { return e.Err }

type managedFilePreparationAuthorizationKey struct{}

// Only the selected server route installs this check. A local resource ID or
// a successful earlier upload never authorizes a later upload or generation.
func withManagedFilePreparationAuthorization(ctx context.Context, check func(context.Context) error) context.Context {
	return context.WithValue(ctx, managedFilePreparationAuthorizationKey{}, check)
}

func checkManagedFilePreparationAuthorization(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	check, _ := ctx.Value(managedFilePreparationAuthorizationKey{}).(func(context.Context) error)
	if check == nil {
		return ErrBPSRequestAuthorizationUnavailable
	}
	return check(ctx)
}

// The first rejected admission may have consumed every upload reader. Inline
// sources use their original staged index; durable files renew their own grant.
func managedRecoveryPlanOpener(plan *ManagedFilePlan, permit *ManagedFileReservation) ManagedFileRecoveryOpener {
	durable := managedRecoveryDurableOpener(plan, permit)
	return func(ctx context.Context, ref string) (managedfiles.File, io.ReadCloser, error) {
		if err := ctx.Err(); err != nil {
			return managedfiles.File{}, nil, err
		}
		if plan.staging != nil {
			for index, file := range plan.staging.Files() {
				if file.PublicID != "" || plan.inlineRefs[file.Position] != ref {
					continue
				}
				reader, err := plan.staging.Open(index)
				return managedfiles.File{FileRef: ref, Filename: file.Filename, MIME: file.MIME, Bytes: file.Bytes, SHA256: file.SHA256}, reader, err
			}
		}
		if durable == nil {
			return managedfiles.File{}, nil, ErrManagedFileNotReady
		}
		return durable(ctx, ref)
	}
}
