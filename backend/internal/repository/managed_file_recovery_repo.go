package repository

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
	"github.com/MACOS-DO/sub4api/internal/service"
)

func (r *ManagedFileLedgerRepository) ReserveRecovery(ctx context.Context, in service.ManagedFileRecoveryInput) (*service.ManagedFileReservation, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	permit := in.Reservation
	refs := make([]string, 0, len(permit.Files))
	expected := make(map[string]service.ManagedFileBinding, len(permit.Files))
	for _, binding := range permit.Files {
		if binding.File == nil || expected[binding.File.PublicID].File != nil {
			return nil, service.ErrInvalidFile
		}
		refs = append(refs, binding.File.PublicID)
		expected[binding.File.PublicID] = binding
	}
	targets := make(map[string]bool, len(in.FileRefs))
	for _, ref := range in.FileRefs {
		targets[ref] = true
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	files, err := r.lockFiles(ctx, tx, permit.Owner, refs, true)
	if err != nil {
		return nil, err
	}
	rows := make([]managedLedgerRow, 0, len(files))
	for _, file := range files {
		want := expected[file.PublicID]
		if file.ID != want.File.ID || file.Version != want.File.Version {
			return nil, service.ErrFileLeaseConflict
		}
		if file.UserID != want.File.UserID || file.GroupID != want.File.GroupID || file.APIKeyID != want.File.APIKeyID || file.SHA256 != want.File.SHA256 || file.Bytes != want.File.Bytes || file.Filename != want.File.Filename || file.MIME != want.File.MIME {
			return nil, service.ErrInvalidFile
		}
		row, err := readManagedBinding(ctx, tx, file)
		if err != nil {
			return nil, err
		}
		if row.migrationID.Valid {
			return nil, service.ErrManagedFileBusy
		}
		if !row.hasBinding {
			return nil, service.ErrFileLeaseConflict
		}
		if row.binding.Selection != permit.Selection {
			return nil, service.ErrManagedFileBindingConflict
		}
		if row.binding.Version != want.Version || row.binding.LeaseToken != permit.LeaseToken || !row.binding.LeaseUntil.Equal(want.LeaseUntil) || row.binding.Operation.ID != want.Operation.ID {
			return nil, service.ErrFileLeaseConflict
		}
		if row.binding.Operation.State == "started" || row.binding.Operation.State == "uncertain" {
			return nil, service.ErrManagedFileUncertain
		}
		if row.binding.Operation.State != "reserved" && row.binding.Operation.State != "succeeded" {
			return nil, service.ErrManagedFileNotReady
		}
		if row.binding.Operation.State == "succeeded" && row.active != row.binding.Operation.ID || row.binding.Operation.State == "reserved" && row.active != "" {
			return nil, service.ErrManagedFileNotReady
		}
		if err := managedRecoveryOperationIdentity(ctx, tx, file, permit.Selection, row.binding.Operation.ID); err != nil {
			return nil, err
		}
		if targets[file.PublicID] {
			if row.binding.Operation.State != "succeeded" || !managedfiles.ValidRemoteID(row.binding.Operation.RemoteID) {
				return nil, service.ErrManagedFileNotReady
			}
			if row.binding.Operation.RemoteID != want.Operation.RemoteID || row.binding.Operation.InputTokens != want.Operation.InputTokens {
				return nil, service.ErrManagedFileBindingConflict
			}
		}
		rows = append(rows, row)
	}
	// Read the clock after every lock. A file or lease can expire while a
	// later member of the batch is waiting; validate all before any write.
	var now time.Time
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return nil, err
	}
	for _, row := range rows {
		file := row.binding.File
		if file.ExpiresAt != nil && !file.ExpiresAt.After(now) {
			return nil, service.ErrFileNotFound
		}
		if !row.binding.LeaseUntil.After(now) {
			return nil, service.ErrFileLeaseConflict
		}
		if !targets[file.PublicID] {
			continue
		}
		var attempts int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM gateway_file_remote_operations WHERE file_id=$1 AND kind='recovery' AND created_at>$2`, file.ID, now.Add(-service.ManagedFileRecoveryWindow)).Scan(&attempts); err != nil {
			return nil, err
		}
		if attempts >= service.ManagedFileRecoveryMaxAttempts {
			return nil, service.ErrManagedFileRecoveryBudget
		}
	}
	out := &service.ManagedFileReservation{Owner: permit.Owner, Selection: permit.Selection, LeaseToken: permit.LeaseToken}
	for _, row := range rows {
		binding := row.binding
		file := binding.File
		if targets[file.PublicID] {
			var random [16]byte
			if _, err = rand.Read(random[:]); err != nil {
				return nil, err
			}
			operation := hex.EncodeToString(random[:])
			// Preserve historical timestamp ordering for existing operators. The
			// active mapping is selected only by the explicit current pointer.
			_, err = tx.ExecContext(ctx, `INSERT INTO gateway_file_remote_operations(operation_id,file_id,file_version,main_account_id,backend_id,subject_fingerprint,credential_generation,kind,state,sha256,bytes,filename,mime,created_at)
 SELECT $1,$2,$3,$4,$5,$6,$7,'recovery','reserved',$8,$9,$10,$11,
 GREATEST(clock_timestamp(),COALESCE(MAX(created_at)+interval '1 microsecond','-infinity'::timestamptz))
 FROM gateway_file_remote_operations WHERE file_id=$2`, operation, file.ID, file.Version, permit.Selection.AccountID, permit.Selection.Identity.BackendID, permit.Selection.Identity.SubjectFingerprint, permit.Selection.Identity.CredentialGeneration, file.SHA256, file.Bytes, file.Filename, file.MIME)
			if err != nil {
				return nil, err
			}
			err = tx.QueryRowContext(ctx, `UPDATE gateway_file_bindings SET version=version+1,active_operation_id=NULL,current_operation_id=$6,updated_at=clock_timestamp() WHERE file_id=$1 AND version=$2 AND lease_token=$3 AND active_operation_id=$4 AND current_operation_id=$4 AND lease_until=$5 AND lease_until>clock_timestamp() RETURNING version`, file.ID, binding.Version, permit.LeaseToken, binding.Operation.ID, binding.LeaseUntil, operation).Scan(&binding.Version)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, service.ErrFileLeaseConflict
			}
			if err != nil {
				return nil, err
			}
			binding.Operation = service.ManagedFileOperation{ID: operation, State: "reserved"}
		}
		out.Files = append(out.Files, binding)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func managedRecoveryOperationIdentity(ctx context.Context, tx *sql.Tx, file *service.StoredFile, selection service.ManagedFileSelection, operation string) error {
	var version, account, bytes int64
	var identity managedfiles.Identity
	var sha, name, mime string
	err := tx.QueryRowContext(ctx, `SELECT file_version,main_account_id,backend_id,subject_fingerprint,credential_generation,sha256,bytes,filename,mime FROM gateway_file_remote_operations WHERE operation_id=$1 AND file_id=$2 FOR UPDATE`, operation, file.ID).Scan(&version, &account, &identity.BackendID, &identity.SubjectFingerprint, &identity.CredentialGeneration, &sha, &bytes, &name, &mime)
	if err != nil {
		return err
	}
	if version != file.Version || account != selection.AccountID || identity != selection.Identity || sha != file.SHA256 || bytes != file.Bytes || name != file.Filename || mime != file.MIME {
		return service.ErrManagedFileBindingConflict
	}
	return nil
}
