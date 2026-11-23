package repository

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"mime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
	"github.com/MACOS-DO/sub4api/internal/service"
)

var _ service.ManagedInlineOperationRepository = (*ManagedFileLedgerRepository)(nil)

func inlineOpaque(s string, limit int) bool {
	return s != "" && len(s) <= limit && strings.TrimSpace(s) == s && utf8.ValidString(s) && strings.IndexFunc(s, unicode.IsControl) < 0
}

func inlineDescriptorValid(f service.ManagedInlineDescriptor) bool {
	_, hashErr := hex.DecodeString(f.SHA256)
	media, params, mimeErr := mime.ParseMediaType(f.MIME)
	return managedfiles.ValidRemoteID(f.FileRef) && inlineOpaque(f.Filename, 1024) && f.Filename != "." && f.Filename != ".." && !strings.ContainsAny(f.Filename, "/\\:") &&
		len(f.SHA256) == 64 && strings.ToLower(f.SHA256) == f.SHA256 && hashErr == nil && f.Bytes > 0 && f.Bytes <= managedfiles.MaxFileBytes &&
		inlineOpaque(f.MIME, 1024) && mimeErr == nil && len(params) == 0 && media == f.MIME && strings.Count(media, "/") == 1 && !strings.Contains(media, "*")
}

func inlinePermitValid(p *service.ManagedInlineReservation) bool {
	if p == nil || !managedOwnerValid(p.Owner) || p.Selection.AccountID <= 0 || managedfiles.ValidateIdentity(p.Selection.Identity) != nil || !inlineOpaque(p.RequestID, 256) || !inlineOpaque(p.LeaseToken, 128) || len(p.Files) == 0 || len(p.Files) > managedfiles.MaxUniqueFiles {
		return false
	}
	seen := make(map[string]bool, len(p.Files))
	for _, f := range p.Files {
		if !inlineDescriptorValid(f.Descriptor) || seen[f.Descriptor.FileRef] || !inlineOpaque(f.Operation.ID, 64) {
			return false
		}
		seen[f.Descriptor.FileRef] = true
	}
	return true
}

func (r *ManagedFileLedgerRepository) ReserveInline(ctx context.Context, in service.ManagedInlineReserveInput) (*service.ManagedInlineReservation, error) {
	p := &service.ManagedInlineReservation{Owner: in.Owner, Selection: in.Selection, RequestID: in.RequestID, LeaseToken: in.LeaseToken}
	if in.Lease < time.Microsecond || in.Lease > 30*time.Minute || len(in.Files) == 0 || len(in.Files) > managedfiles.MaxUniqueFiles {
		return nil, service.ErrInvalidFile
	}
	for _, f := range in.Files {
		var random [24]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, err
		}
		p.Files = append(p.Files, service.ManagedInlineBinding{Descriptor: f, Operation: service.ManagedFileOperation{ID: "inline-" + hex.EncodeToString(random[:]), State: "reserved"}})
	}
	if !inlinePermitValid(p) {
		return nil, service.ErrInvalidFile
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO gateway_inline_requests(request_id,user_id,api_key_id,group_id,main_account_id,backend_id,subject_fingerprint,credential_generation,lease_token,lease_until)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,clock_timestamp()+($10*INTERVAL '1 microsecond')) ON CONFLICT(request_id) DO NOTHING`, p.RequestID, p.Owner.UserID, p.Owner.APIKeyID, p.Owner.GroupID, p.Selection.AccountID, p.Selection.Identity.BackendID, p.Selection.Identity.SubjectFingerprint, p.Selection.Identity.CredentialGeneration, p.LeaseToken, in.Lease.Microseconds())
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n != 1 {
		return nil, service.ErrManagedFileBusy
	}
	for _, f := range p.Files {
		_, err = tx.ExecContext(ctx, `INSERT INTO gateway_inline_remote_operations(operation_id,request_id,file_ref,sha256,bytes,filename,mime,state) VALUES($1,$2,$3,$4,$5,$6,$7,'reserved')`, f.Operation.ID, p.RequestID, f.Descriptor.FileRef, f.Descriptor.SHA256, f.Descriptor.Bytes, f.Descriptor.Filename, f.Descriptor.MIME)
		if err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return p, nil
}

// Lock request first, then all its operations. Every operation validates the
// complete immutable batch, including callers which supplied a truncated permit.
func lockInline(ctx context.Context, tx *sql.Tx, p *service.ManagedInlineReservation) ([]service.ManagedInlineBinding, error) {
	if !inlinePermitValid(p) {
		return nil, service.ErrInvalidFile
	}
	var owner service.FileOwner
	var selection service.ManagedFileSelection
	var token string
	err := tx.QueryRowContext(ctx, `SELECT user_id,api_key_id,group_id,main_account_id,backend_id,subject_fingerprint,credential_generation,lease_token FROM gateway_inline_requests WHERE request_id=$1 FOR UPDATE`, p.RequestID).Scan(&owner.UserID, &owner.APIKeyID, &owner.GroupID, &selection.AccountID, &selection.Identity.BackendID, &selection.Identity.SubjectFingerprint, &selection.Identity.CredentialGeneration, &token)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrFileLeaseConflict
	}
	if err != nil {
		return nil, err
	}
	if owner != p.Owner || selection != p.Selection || token != p.LeaseToken {
		return nil, service.ErrManagedFileBindingConflict
	}
	rows, err := tx.QueryContext(ctx, `SELECT file_ref,filename,mime,sha256,bytes,operation_id,state,COALESCE(remote_id,''),COALESCE(input_tokens,0) FROM gateway_inline_remote_operations WHERE request_id=$1 ORDER BY file_ref FOR UPDATE`, p.RequestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	expected := make(map[string]service.ManagedInlineBinding, len(p.Files))
	for _, f := range p.Files {
		expected[f.Descriptor.FileRef] = f
	}
	var actual []service.ManagedInlineBinding
	for rows.Next() {
		var f service.ManagedInlineBinding
		if err = rows.Scan(&f.Descriptor.FileRef, &f.Descriptor.Filename, &f.Descriptor.MIME, &f.Descriptor.SHA256, &f.Descriptor.Bytes, &f.Operation.ID, &f.Operation.State, &f.Operation.RemoteID, &f.Operation.InputTokens); err != nil {
			return nil, err
		}
		want, ok := expected[f.Descriptor.FileRef]
		if !ok || want.Descriptor != f.Descriptor || want.Operation.ID != f.Operation.ID {
			return nil, service.ErrManagedFileBindingConflict
		}
		actual = append(actual, f)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(actual) != len(p.Files) {
		return nil, service.ErrManagedFileBindingConflict
	}
	return actual, nil
}

func inlineLive(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var live bool
	err := tx.QueryRowContext(ctx, `SELECT NOT released AND lease_until>clock_timestamp() FROM gateway_inline_requests WHERE request_id=$1`, id).Scan(&live)
	return live, err
}

func inlineMember(files []service.ManagedInlineBinding, ref string) (service.ManagedInlineBinding, error) {
	for _, f := range files {
		if f.Descriptor.FileRef == ref {
			return f, nil
		}
	}
	return service.ManagedInlineBinding{}, service.ErrInvalidFile
}

func (r *ManagedFileLedgerRepository) StartInline(ctx context.Context, p *service.ManagedInlineReservation, ref string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	files, err := lockInline(ctx, tx, p)
	if err != nil {
		return err
	}
	f, err := inlineMember(files, ref)
	if err != nil {
		return err
	}
	live, err := inlineLive(ctx, tx, p.RequestID)
	if err != nil {
		return err
	}
	if !live {
		return service.ErrFileLeaseConflict
	}
	if f.Operation.State == "started" || f.Operation.State == "uncertain" {
		return service.ErrManagedFileUncertain
	}
	if f.Operation.State != "reserved" {
		return service.ErrManagedFileNotReady
	}
	if _, err = tx.ExecContext(ctx, `UPDATE gateway_inline_remote_operations SET state='started',started_at=clock_timestamp() WHERE operation_id=$1`, f.Operation.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *ManagedFileLedgerRepository) CompleteInline(ctx context.Context, p *service.ManagedInlineReservation, ref string, result managedfiles.EnsureResult) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	files, err := lockInline(ctx, tx, p)
	if err != nil {
		return false, err
	}
	f, err := inlineMember(files, ref)
	if err != nil {
		return false, err
	}
	if result.Contract != managedfiles.Contract || result.Identity != p.Selection.Identity || result.OperationID != f.Operation.ID || result.FileRef != ref || !managedfiles.ValidRemoteID(result.RemoteID) || result.Bytes != f.Descriptor.Bytes || result.SHA256 != f.Descriptor.SHA256 || result.Filename != f.Descriptor.Filename || result.MIME != f.Descriptor.MIME || result.InputTokens < 0 {
		return false, service.ErrInvalidFile
	}
	if f.Operation.State == "succeeded" {
		if f.Operation.RemoteID != result.RemoteID || f.Operation.InputTokens != result.InputTokens {
			return false, service.ErrManagedFileBindingConflict
		}
	} else {
		if f.Operation.State != "started" && f.Operation.State != "uncertain" {
			return false, service.ErrManagedFileNotReady
		}
		if _, err = tx.ExecContext(ctx, `UPDATE gateway_inline_remote_operations SET state='succeeded',remote_id=$2,input_tokens=$3,completed_at=clock_timestamp(),safe_error_code=NULL WHERE operation_id=$1`, f.Operation.ID, result.RemoteID, result.InputTokens); err != nil {
			return false, err
		}
	}
	live, err := inlineLive(ctx, tx, p.RequestID)
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return live, nil
}

func (r *ManagedFileLedgerRepository) MarkInlineUncertain(ctx context.Context, p *service.ManagedInlineReservation, ref, code string) error {
	if len(code) == 0 || len(code) > 64 || strings.IndexFunc(code, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') }) >= 0 {
		return service.ErrInvalidFile
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	files, err := lockInline(ctx, tx, p)
	if err != nil {
		return err
	}
	f, err := inlineMember(files, ref)
	if err != nil {
		return err
	}
	if f.Operation.State == "reserved" {
		return service.ErrManagedFileNotReady
	}
	if f.Operation.State == "started" {
		if _, err = tx.ExecContext(ctx, `UPDATE gateway_inline_remote_operations SET state='uncertain',safe_error_code=$2 WHERE operation_id=$1`, f.Operation.ID, code); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *ManagedFileLedgerRepository) AuthorizeInlineGeneration(ctx context.Context, p *service.ManagedInlineReservation) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	files, err := lockInline(ctx, tx, p)
	if err != nil {
		return err
	}
	live, err := inlineLive(ctx, tx, p.RequestID)
	if err != nil {
		return err
	}
	if !live {
		return service.ErrFileLeaseConflict
	}
	for _, f := range files {
		if f.Operation.State != "succeeded" {
			return service.ErrManagedFileNotReady
		}
	}
	return tx.Commit()
}

func (r *ManagedFileLedgerRepository) ReleaseInline(ctx context.Context, p *service.ManagedInlineReservation) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = lockInline(ctx, tx, p); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE gateway_inline_requests SET released=true WHERE request_id=$1`, p.RequestID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE gateway_inline_remote_operations SET state='uncertain',safe_error_code='request_released' WHERE request_id=$1 AND state='started'`, p.RequestID); err != nil {
		return err
	}
	return tx.Commit()
}
