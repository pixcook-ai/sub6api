package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

type ManagedFileRecoveryOpener func(context.Context, string) (managedfiles.File, io.ReadCloser, error)

type ManagedFileRecoveryAdmissionInput struct {
	Plan        *ManagedFilePlan
	Backend     *BPSFilesBackend
	Manifest    managedfiles.AdmitManifest
	Reservation *ManagedFileReservation
	Limits      managedfiles.Limits
	Authorize   func(context.Context) error
	Open        ManagedFileRecoveryOpener
}

type ManagedFileRecoveryAdmission struct {
	Manifest    managedfiles.AdmitManifest
	Reservation *ManagedFileReservation
	Admission   managedfiles.AdmitResult
}

// A round belongs to one Resolve attempt. Admit is called while Plan.mu is
// held; Close cannot race the immediate replacement of its preparation permit.
type ManagedFileRecoveryRound struct{ used atomic.Bool }

func (round *ManagedFileRecoveryRound) Admit(ctx context.Context, in ManagedFileRecoveryAdmissionInput) (*ManagedFileRecoveryAdmission, error) {
	if round == nil || in.Plan == nil || in.Plan.resolver == nil || in.Plan.closed || in.Backend == nil || in.Backend.upstream == nil {
		return nil, ErrManagedFileNotReady
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := managedRecoveryManifest(in); err != nil {
		return nil, err
	}
	readers := make(map[string]io.Reader)
	for _, f := range in.Manifest.Files {
		if f.Mode == "upload" {
			if in.Plan.readers[f.FileRef] == nil {
				return nil, ErrManagedFileNotReady
			}
			readers[f.FileRef] = in.Plan.readers[f.FileRef]
		}
	}
	admission, missing, err := in.Backend.AdmitRecoverable(ctx, in.Manifest, readers)
	if err != nil {
		return nil, err
	}
	if missing == nil {
		return &ManagedFileRecoveryAdmission{Manifest: in.Manifest, Reservation: in.Reservation, Admission: admission}, nil
	}
	if !round.used.CompareAndSwap(false, true) || in.Authorize == nil || in.Reservation == nil {
		return nil, ErrManagedFileNotReady
	}
	repo, ok := in.Plan.resolver.ledger.(ManagedFileRecoveryRepository)
	if !ok {
		return nil, ErrManagedFileNotReady
	}
	targets := make(map[string]bool, len(missing.MissingRefs))
	for _, ref := range missing.MissingRefs {
		targets[ref] = true
	}
	before := managedRecoveryBindings(in.Reservation)
	for _, f := range in.Manifest.Files {
		if targets[f.FileRef] {
			b, ok := before[f.FileRef]
			if !ok || f.Mode != "existing" || b.Operation.State != "succeeded" || b.Operation.ID != f.OperationID || b.Operation.RemoteID != f.RemoteID {
				return nil, ErrManagedFileNotReady
			}
		}
	}
	replacement, err := ReserveManagedFileRecovery(ctx, repo, ManagedFileRecoveryInput{Reservation: in.Reservation, FileRefs: missing.MissingRefs}, in.Authorize)
	if err != nil {
		return nil, err
	}
	// ReserveRecovery has already replaced durable fences. Close must release
	// that returned whole permit even if any validation or reopening now fails.
	in.Plan.reservation = replacement
	if err := managedRecoveryReplacement(in.Reservation, replacement, targets); err != nil {
		return nil, err
	}
	in.Reservation = replacement
	in.Manifest.Files = append([]managedfiles.File(nil), in.Manifest.Files...)
	parts := make(map[string]bool, len(in.Manifest.Files))
	for _, f := range in.Manifest.Files {
		if f.Part != "" {
			parts[f.Part] = true
		}
	}
	for i := range in.Manifest.Files {
		f := &in.Manifest.Files[i]
		if !targets[f.FileRef] {
			continue
		}
		f.Mode, f.RemoteID, f.OperationID = "upload", "", ""
		for n := i; ; n++ {
			part := fmt.Sprintf("recovery_%d", n)
			if !parts[part] {
				f.Part, parts[part] = part, true
				break
			}
		}
	}
	if err := managedRecoveryManifest(in); err != nil {
		return nil, err
	}
	opener := in.Open
	if opener == nil {
		opener = managedRecoveryDurableOpener(in.Plan, replacement)
	}
	if opener == nil {
		return nil, ErrManagedFileNotReady
	}
	reopened := make(map[string]io.ReadCloser)
	defer func() {
		for _, reader := range reopened {
			_ = reader.Close()
		}
	}()
	readers = make(map[string]io.Reader)
	for _, f := range in.Manifest.Files {
		if f.Mode != "upload" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		opened, reader, err := opener(ctx, f.FileRef)
		if reader != nil {
			reopened[f.FileRef] = reader
		}
		if err != nil {
			return nil, err
		}
		if reader == nil || !managedRecoveryDescriptor(opened, f) {
			return nil, ErrManagedFileNotReady
		}
		readers[f.FileRef] = reader
	}
	if err := in.Authorize(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := managedRecoveryManifest(in); err != nil {
		return nil, err
	}
	for _, reader := range in.Plan.readers {
		_ = reader.Close()
	}
	in.Plan.readers, reopened = reopened, nil
	admission, missing, err = in.Backend.AdmitRecoverable(ctx, in.Manifest, readers)
	if err != nil {
		return nil, err
	}
	if missing != nil {
		return nil, ErrManagedFileNotReady
	}
	return &ManagedFileRecoveryAdmission{Manifest: in.Manifest, Reservation: replacement, Admission: admission}, nil
}

func managedRecoveryDescriptor(a, b managedfiles.File) bool {
	return a.FileRef == b.FileRef && a.Filename == b.Filename && a.MIME == b.MIME && a.Bytes == b.Bytes && a.SHA256 == b.SHA256
}

func managedRecoveryBindings(p *ManagedFileReservation) map[string]ManagedFileBinding {
	bindings := make(map[string]ManagedFileBinding)
	if p != nil {
		for _, b := range p.Files {
			if b.File != nil {
				bindings[b.File.PublicID] = b
			}
		}
	}
	return bindings
}

func managedRecoveryManifest(in ManagedFileRecoveryAdmissionInput) error {
	p, m, l := in.Plan, in.Manifest, in.Limits
	if m.Expected != in.Backend.config.Expected || m.OwnerRef != strconv.FormatInt(p.owner.UserID, 10) || m.GroupRef != strconv.FormatInt(p.owner.GroupID, 10) || p.owner.UserID <= 0 || p.owner.GroupID <= 0 || p.owner.APIKeyID <= 0 || in.Reservation != p.reservation || p.RequiredAccountID != 0 && p.RequiredAccountID != in.Backend.config.AccountID {
		return ErrManagedFileNotReady
	}
	if l.FileBytes <= 0 || l.FileBytes > managedfiles.MaxFileBytes || l.MessageBytes <= 0 || l.MessageBytes > managedfiles.MaxMessageBytes || l.MessagePositions <= 0 || l.MessagePositions > managedfiles.MaxMessagePositions || l.StagingBytes <= 0 || l.AdmissionBytes <= 0 || l.ConcurrentAdmissions <= 0 || l.LiveAdmissions <= 0 || l.AdmissionTTLSeconds <= 0 || l.ManifestBytes <= 0 || l.ManifestBytes > managedfiles.MaxJSONBytes || l.UniqueFiles <= 0 || l.UniqueFiles > managedfiles.MaxUniqueFiles || l.FreeSpaceReserveBytes < 0 || l.AdmitTimeoutSeconds <= 0 || l.EnsureTimeoutSeconds <= 0 {
		return bpsFilesError("invalid_description")
	}
	if err := managedfiles.ValidateManifest(m); err != nil {
		return err
	}
	bindings := managedRecoveryBindings(in.Reservation)
	if in.Reservation != nil {
		r := in.Reservation
		if r.Owner != p.owner || r.Selection.AccountID != in.Backend.config.AccountID || r.Selection.Identity != m.Expected || r.LeaseToken == "" || len(bindings) != len(r.Files) {
			return ErrManagedFileNotReady
		}
		for _, frozen := range p.files {
			if frozen == nil {
				return ErrManagedFileNotReady
			}
			binding, ok := bindings[frozen.PublicID]
			if !ok || binding.File.ID != frozen.ID || binding.File.Version != frozen.Version || !managedResolverReady(frozen, p.owner) || !managedRecoveryDescriptor(managedfiles.File{FileRef: frozen.PublicID, Filename: frozen.Filename, MIME: frozen.MIME, Bytes: frozen.Bytes, SHA256: frozen.SHA256}, managedfiles.File{FileRef: binding.File.PublicID, Filename: binding.File.Filename, MIME: binding.File.MIME, Bytes: binding.File.Bytes, SHA256: binding.File.SHA256}) {
				return ErrManagedFileNotReady
			}
		}
	}
	type budget struct {
		positions int
		bytes     int64
	}
	budgets := map[int]budget{}
	var total int64
	for _, f := range m.Files {
		if b, ok := bindings[f.FileRef]; ok {
			if !managedResolverReady(b.File, p.owner) || b.Selection != in.Reservation.Selection || b.LeaseToken != in.Reservation.LeaseToken || b.Version <= 0 || !time.Now().Before(b.LeaseUntil) || b.Operation.ID == "" || !managedRecoveryDescriptor(f, managedfiles.File{FileRef: b.File.PublicID, Filename: b.File.Filename, MIME: b.File.MIME, Bytes: b.File.Bytes, SHA256: b.File.SHA256}) {
				return ErrManagedFileNotReady
			}
			if f.Mode == "existing" && (b.Operation.State != "succeeded" || b.Operation.ID != f.OperationID || b.Operation.RemoteID != f.RemoteID) || f.Mode == "upload" && b.Operation.State != "reserved" {
				return ErrManagedFileNotReady
			}
			delete(bindings, f.FileRef)
		} else if f.Mode != "upload" {
			return ErrManagedFileNotReady
		}
		if f.Bytes > l.FileBytes {
			return bpsFilesError("file_too_large")
		}
		if f.Mode == "upload" {
			if f.Bytes > l.AdmissionBytes-total {
				return bpsFilesError("admission_too_large")
			}
			total += f.Bytes
		}
		for _, pos := range f.Positions {
			b := budgets[pos.Input]
			if b.positions >= l.MessagePositions || f.Bytes > l.MessageBytes-b.bytes {
				return bpsFilesError("message_too_large")
			}
			b.positions++
			b.bytes += f.Bytes
			budgets[pos.Input] = b
		}
	}
	if len(bindings) != 0 {
		return ErrManagedFileNotReady
	}
	encoded, err := json.Marshal(m)
	if err != nil || int64(len(encoded)) > l.ManifestBytes || len(m.Files) > l.UniqueFiles {
		return bpsFilesError("admission_too_large")
	}
	return nil
}

func managedRecoveryReplacement(old, next *ManagedFileReservation, targets map[string]bool) error {
	if old == nil || next == nil || old.Owner != next.Owner || old.Selection != next.Selection || old.LeaseToken != next.LeaseToken || len(old.Files) != len(next.Files) {
		return ErrManagedFileNotReady
	}
	before, after := managedRecoveryBindings(old), managedRecoveryBindings(next)
	if len(before) != len(old.Files) || len(after) != len(next.Files) {
		return ErrManagedFileNotReady
	}
	oldOps, newOps := map[string]bool{}, map[string]bool{}
	for _, b := range before {
		oldOps[b.Operation.ID] = true
	}
	for ref, a := range before {
		b, ok := after[ref]
		if !ok || !reflect.DeepEqual(a.File, b.File) || a.Selection != b.Selection || a.LeaseToken != b.LeaseToken || !a.LeaseUntil.Equal(b.LeaseUntil) {
			return ErrManagedFileNotReady
		}
		if !targets[ref] {
			if a.Version != b.Version || a.Operation != b.Operation {
				return ErrManagedFileNotReady
			}
			continue
		}
		if b.Version != a.Version+1 || b.Operation.State != "reserved" || b.Operation.ID == "" || oldOps[b.Operation.ID] || newOps[b.Operation.ID] || b.Operation.RemoteID != "" || b.Operation.InputTokens != 0 {
			return ErrManagedFileNotReady
		}
		newOps[b.Operation.ID] = true
	}
	return nil
}

func managedRecoveryDurableOpener(plan *ManagedFilePlan, permit *ManagedFileReservation) ManagedFileRecoveryOpener {
	if plan.resolver.files == nil {
		return nil
	}
	bindings := managedRecoveryBindings(permit)
	return func(ctx context.Context, ref string) (managedfiles.File, io.ReadCloser, error) {
		b, ok := bindings[ref]
		if !ok {
			return managedfiles.File{}, nil, ErrManagedFileNotReady
		}
		f, reader, err := plan.resolver.files.Open(ctx, plan.owner, ref)
		if err != nil {
			return managedfiles.File{}, reader, err
		}
		if !managedResolverReady(f, plan.owner) || f.Version != b.File.Version {
			return managedfiles.File{}, reader, ErrManagedFileNotReady
		}
		return managedfiles.File{FileRef: f.PublicID, Filename: f.Filename, MIME: f.MIME, Bytes: f.Bytes, SHA256: f.SHA256}, reader, nil
	}
}
