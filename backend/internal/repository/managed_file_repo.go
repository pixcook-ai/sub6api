package repository

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/lib/pq"
)

type ManagedFileLedgerRepository struct{ db *sql.DB }

func NewManagedFileRepository(db *sql.DB) *ManagedFileLedgerRepository {
	return &ManagedFileLedgerRepository{db: db}
}

type managedLedgerRow struct {
	binding     service.ManagedFileBinding
	active      string
	migrationID sql.NullString
	hasBinding  bool
}

func managedOwnerValid(owner service.FileOwner) bool {
	return owner.UserID > 0 && owner.GroupID > 0 && owner.APIKeyID > 0
}

func (r *ManagedFileLedgerRepository) lockFiles(ctx context.Context, tx *sql.Tx, owner service.FileOwner, ids []string, ready bool) ([]*service.StoredFile, error) {
	if !managedOwnerValid(owner) || len(ids) == 0 || len(ids) > managedfiles.MaxUniqueFiles {
		return nil, service.ErrInvalidFile
	}
	seen := make(map[string]bool, len(ids))
	files := make([]*service.StoredFile, 0, len(ids))
	// Discover IDs first; lock all files by immutable numeric ID before any binding.
	for _, ref := range ids {
		if ref == "" || seen[ref] {
			return nil, service.ErrInvalidFile
		}
		seen[ref] = true
		f, e := scanStoredFile(tx.QueryRowContext(ctx, `SELECT `+fileColumns+` FROM gateway_files WHERE public_id=$1 AND user_id=$2 AND group_id=$3 AND api_key_id=$4`, ref, owner.UserID, owner.GroupID, owner.APIKeyID))
		if e != nil {
			return nil, e
		}
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].ID < files[j].ID })
	for i, f := range files {
		f, e := scanStoredFile(tx.QueryRowContext(ctx, `SELECT `+fileColumns+` FROM gateway_files WHERE id=$1 AND user_id=$2 AND group_id=$3 AND api_key_id=$4 FOR UPDATE`, f.ID, owner.UserID, owner.GroupID, owner.APIKeyID))
		if e != nil {
			return nil, e
		}
		files[i] = f
	}
	if ready {
		// Take the clock only after every row lock: an earlier file may expire
		// while this transaction waits for a later row in the batch.
		var valid bool
		numericIDs := make([]int64, len(files))
		for i, f := range files {
			numericIDs[i] = f.ID
		}
		if err := tx.QueryRowContext(ctx, `WITH bounds AS MATERIALIZED (SELECT clock_timestamp() AS at)
 SELECT bool_and(state='ready' AND (expires_at IS NULL OR expires_at>bounds.at)) FROM gateway_files CROSS JOIN bounds WHERE id=ANY($1)`, pq.Array(numericIDs)).Scan(&valid); err != nil {
			return nil, err
		}
		if !valid {
			return nil, service.ErrFileNotFound
		}
	}
	return files, nil
}

func readManagedBinding(ctx context.Context, tx *sql.Tx, f *service.StoredFile) (managedLedgerRow, error) {
	row := managedLedgerRow{binding: service.ManagedFileBinding{File: f}}
	var lease sql.NullTime
	var token, active, current sql.NullString
	e := tx.QueryRowContext(ctx, `SELECT main_account_id,backend_id,subject_fingerprint,credential_generation,version,lease_token,lease_until,active_operation_id,current_operation_id,migration_id FROM gateway_file_bindings WHERE file_id=$1 FOR UPDATE`, f.ID).Scan(&row.binding.Selection.AccountID, &row.binding.Selection.Identity.BackendID, &row.binding.Selection.Identity.SubjectFingerprint, &row.binding.Selection.Identity.CredentialGeneration, &row.binding.Version, &token, &lease, &active, &current, &row.migrationID)
	if errors.Is(e, sql.ErrNoRows) {
		return row, nil
	}
	if e != nil {
		return row, e
	}
	row.hasBinding = true
	row.binding.LeaseToken = token.String
	row.binding.LeaseUntil = lease.Time
	row.active = active.String
	e = tx.QueryRowContext(ctx, `SELECT operation_id,state,COALESCE(remote_id,''),COALESCE(input_tokens,0) FROM gateway_file_remote_operations WHERE file_id=$1 AND operation_id=$2 AND main_account_id=$3 AND backend_id=$4 AND subject_fingerprint=$5 AND credential_generation=$6 FOR UPDATE`, f.ID, current.String, row.binding.Selection.AccountID, row.binding.Selection.Identity.BackendID, row.binding.Selection.Identity.SubjectFingerprint, row.binding.Selection.Identity.CredentialGeneration).Scan(&row.binding.Operation.ID, &row.binding.Operation.State, &row.binding.Operation.RemoteID, &row.binding.Operation.InputTokens)
	if errors.Is(e, sql.ErrNoRows) {
		return row, service.ErrFileRemoteGuardUnavailable
	}
	return row, e
}

func (r *ManagedFileLedgerRepository) LookupBindings(ctx context.Context, owner service.FileOwner, refs []string) (service.ManagedFileLookup, error) {
	out := service.ManagedFileLookup{}
	tx, e := r.db.BeginTx(ctx, nil)
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	files, e := r.lockFiles(ctx, tx, owner, refs, true)
	if e != nil {
		return out, e
	}
	for _, f := range files {
		row, e := readManagedBinding(ctx, tx, f)
		if e != nil {
			return out, e
		}
		if row.hasBinding {
			if out.AccountID != 0 && out.AccountID != row.binding.Selection.AccountID {
				return out, service.ErrManagedFileBindingConflict
			}
			out.AccountID = row.binding.Selection.AccountID
		}
		out.Bindings = append(out.Bindings, row.binding)
	}
	return out, tx.Commit()
}

func (r *ManagedFileLedgerRepository) Reserve(ctx context.Context, in service.ManagedFileReserveInput) (*service.ManagedFileReservation, error) {
	if in.Selection.AccountID <= 0 || managedfiles.ValidateIdentity(in.Selection.Identity) != nil || in.Lease <= 0 || in.Lease > 30*time.Minute || in.LeaseToken == "" || len(in.LeaseToken) > 128 {
		return nil, service.ErrInvalidFile
	}
	refs := make([]string, 0, len(in.Files))
	versions := make(map[string]int64, len(in.Files))
	for _, v := range in.Files {
		refs = append(refs, v.PublicID)
		versions[v.PublicID] = v.Version
	}
	tx, e := r.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	files, e := r.lockFiles(ctx, tx, in.Owner, refs, true)
	if e != nil {
		return nil, e
	}
	// Validate the entire batch before permanently freezing any unbound file.
	// Only conversion of an already-started attempt may commit on rejection.
	var uncertain []string
	rows := make([]managedLedgerRow, 0, len(files))
	for _, f := range files {
		if versions[f.PublicID] != f.Version {
			return nil, service.ErrFileLeaseConflict
		}
		row, err := readManagedBinding(ctx, tx, f)
		if err != nil {
			return nil, err
		}
		if row.migrationID.Valid {
			return nil, service.ErrManagedFileBusy
		}
		rows = append(rows, row)
		if !row.hasBinding {
			continue
		}
		if row.binding.Selection != in.Selection {
			return nil, service.ErrManagedFileBindingConflict
		}
		var live bool
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(lease_until>clock_timestamp(),false) FROM gateway_file_bindings WHERE file_id=$1`, f.ID).Scan(&live); err != nil {
			return nil, err
		}
		if live && row.binding.LeaseToken != in.LeaseToken {
			return nil, service.ErrManagedFileBusy
		}
		switch row.binding.Operation.State {
		case "started", "uncertain":
			uncertain = append(uncertain, row.binding.Operation.ID)
		case "reserved", "succeeded":
		default:
			return nil, service.ErrManagedFileNotReady
		}
	}
	if len(uncertain) > 0 {
		for _, op := range uncertain {
			if _, e = tx.ExecContext(ctx, `UPDATE gateway_file_remote_operations SET state='uncertain',safe_error_code='lease_expired',updated_at=clock_timestamp() WHERE operation_id=$1 AND state='started'`, op); e != nil {
				return nil, e
			}
		}
		if e = tx.Commit(); e != nil {
			return nil, e
		}
		return nil, service.ErrManagedFileUncertain
	}
	out := &service.ManagedFileReservation{Owner: in.Owner, Selection: in.Selection, LeaseToken: in.LeaseToken}
	for i, f := range files {
		row := rows[i]
		if row.hasBinding {
			if e = tx.QueryRowContext(ctx, `UPDATE gateway_file_bindings SET lease_token=$2,lease_until=clock_timestamp()+make_interval(secs=>$3::double precision),version=version+1,active_operation_id=CASE WHEN $4::boolean THEN $5 ELSE active_operation_id END,updated_at=clock_timestamp() WHERE file_id=$1 RETURNING version,lease_until`, f.ID, in.LeaseToken, in.Lease.Seconds(), row.binding.Operation.State == "succeeded", row.binding.Operation.ID).Scan(&row.binding.Version, &row.binding.LeaseUntil); e != nil {
				return nil, e
			}
		} else {
			var bytes [16]byte
			if _, e = rand.Read(bytes[:]); e != nil {
				return nil, e
			}
			op := hex.EncodeToString(bytes[:])
			if e = tx.QueryRowContext(ctx, `INSERT INTO gateway_file_bindings(file_id,main_account_id,backend_id,subject_fingerprint,credential_generation,lease_token,lease_until) VALUES($1,$2,$3,$4,$5,$6,clock_timestamp()+make_interval(secs=>$7::double precision)) RETURNING version,lease_until`, f.ID, in.Selection.AccountID, in.Selection.Identity.BackendID, in.Selection.Identity.SubjectFingerprint, in.Selection.Identity.CredentialGeneration, in.LeaseToken, in.Lease.Seconds()).Scan(&row.binding.Version, &row.binding.LeaseUntil); e != nil {
				return nil, e
			}
			_, e = tx.ExecContext(ctx, `INSERT INTO gateway_file_remote_operations(operation_id,file_id,file_version,main_account_id,backend_id,subject_fingerprint,credential_generation,kind,state,sha256,bytes,filename,mime) VALUES($1,$2,$3,$4,$5,$6,$7,'upload','reserved',$8,$9,$10,$11)`, op, f.ID, f.Version, in.Selection.AccountID, in.Selection.Identity.BackendID, in.Selection.Identity.SubjectFingerprint, in.Selection.Identity.CredentialGeneration, f.SHA256, f.Bytes, f.Filename, f.MIME)
			if e != nil {
				return nil, e
			}
			if _, e = tx.ExecContext(ctx, `UPDATE gateway_file_bindings SET current_operation_id=$2 WHERE file_id=$1`, f.ID, op); e != nil {
				return nil, e
			}
			row.binding.Selection = in.Selection
			row.binding.Operation = service.ManagedFileOperation{ID: op, State: "reserved"}
		}
		row.binding.LeaseToken = in.LeaseToken
		out.Files = append(out.Files, row.binding)
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return out, nil
}

func managedPermitFile(permit *service.ManagedFileReservation, ref string) (service.ManagedFileBinding, error) {
	if permit == nil || !managedOwnerValid(permit.Owner) || permit.Selection.AccountID <= 0 || managedfiles.ValidateIdentity(permit.Selection.Identity) != nil || permit.LeaseToken == "" {
		return service.ManagedFileBinding{}, service.ErrInvalidFile
	}
	for _, f := range permit.Files {
		if f.File != nil && f.File.PublicID == ref && f.Operation.ID != "" {
			return f, nil
		}
	}
	return service.ManagedFileBinding{}, service.ErrInvalidFile
}

func managedLeaseValid(ctx context.Context, tx *sql.Tx, permit *service.ManagedFileReservation, expected service.ManagedFileBinding, current managedLedgerRow) (bool, error) {
	if !current.hasBinding || current.binding.Selection != permit.Selection || current.binding.Version != expected.Version || current.binding.LeaseToken != permit.LeaseToken || current.binding.Operation.ID != expected.Operation.ID || current.binding.File.Version != expected.File.Version {
		return false, nil
	}
	var live bool
	e := tx.QueryRowContext(ctx, `SELECT COALESCE(lease_until>clock_timestamp(),false) FROM gateway_file_bindings WHERE file_id=$1`, expected.File.ID).Scan(&live)
	return live, e
}

func (r *ManagedFileLedgerRepository) StartOperation(ctx context.Context, permit *service.ManagedFileReservation, ref string) error {
	expected, e := managedPermitFile(permit, ref)
	if e != nil {
		return e
	}
	tx, e := r.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	files, e := r.lockFiles(ctx, tx, permit.Owner, []string{ref}, true)
	if e != nil {
		return e
	}
	row, e := readManagedBinding(ctx, tx, files[0])
	if e != nil {
		return e
	}
	if row.migrationID.Valid {
		return service.ErrManagedFileBusy
	}
	live, e := managedLeaseValid(ctx, tx, permit, expected, row)
	if e != nil {
		return e
	}
	if !live {
		return service.ErrFileLeaseConflict
	}
	if row.binding.Operation.State == "started" || row.binding.Operation.State == "uncertain" {
		return service.ErrManagedFileUncertain
	}
	if row.binding.Operation.State != "reserved" {
		return service.ErrManagedFileNotReady
	}
	if _, e = tx.ExecContext(ctx, `UPDATE gateway_files SET remote_activity_possible=true WHERE id=$1`, files[0].ID); e != nil {
		return e
	}
	result, e := tx.ExecContext(ctx, `UPDATE gateway_file_remote_operations SET state='started',started_at=clock_timestamp(),updated_at=clock_timestamp() WHERE operation_id=$1 AND file_id=$2 AND file_version=$3 AND state='reserved'`, expected.Operation.ID, files[0].ID, files[0].Version)
	if e = fileCASResult(result, e); e != nil {
		return e
	}
	return tx.Commit()
}

func (r *ManagedFileLedgerRepository) CompleteOperation(ctx context.Context, permit *service.ManagedFileReservation, ref string, result managedfiles.EnsureResult) (bool, error) {
	expected, e := managedPermitFile(permit, ref)
	if e != nil {
		return false, e
	}
	if result.Contract != managedfiles.Contract || result.Identity != permit.Selection.Identity || result.OperationID != expected.Operation.ID || result.FileRef != ref || !managedfiles.ValidRemoteID(result.RemoteID) || result.Bytes != expected.File.Bytes || result.SHA256 != expected.File.SHA256 || result.Filename != expected.File.Filename || result.MIME != expected.File.MIME || result.InputTokens < 0 {
		return false, service.ErrInvalidFile
	}
	tx, e := r.db.BeginTx(ctx, nil)
	if e != nil {
		return false, e
	}
	defer tx.Rollback()
	files, e := r.lockFiles(ctx, tx, permit.Owner, []string{ref}, false)
	if e != nil {
		return false, e
	}
	row, e := readManagedBinding(ctx, tx, files[0])
	if e != nil {
		return false, e
	}
	if !row.hasBinding || row.binding.Selection != permit.Selection || row.binding.Operation.ID != expected.Operation.ID {
		return false, service.ErrManagedFileBindingConflict
	}
	var fileVersion, accountID, bytes int64
	var identity managedfiles.Identity
	var sha, name, mime, state, remote string
	var tokens sql.NullInt64
	e = tx.QueryRowContext(ctx, `SELECT file_version,main_account_id,backend_id,subject_fingerprint,credential_generation,sha256,bytes,filename,mime,state,COALESCE(remote_id,''),input_tokens FROM gateway_file_remote_operations WHERE operation_id=$1 FOR UPDATE`, expected.Operation.ID).Scan(&fileVersion, &accountID, &identity.BackendID, &identity.SubjectFingerprint, &identity.CredentialGeneration, &sha, &bytes, &name, &mime, &state, &remote, &tokens)
	if e != nil {
		return false, e
	}
	if fileVersion != expected.File.Version || accountID != permit.Selection.AccountID || identity != permit.Selection.Identity || sha != result.SHA256 || bytes != result.Bytes || name != result.Filename || mime != result.MIME {
		return false, service.ErrInvalidFile
	}
	if state == "succeeded" {
		if remote != result.RemoteID || !tokens.Valid || tokens.Int64 != result.InputTokens {
			return false, service.ErrManagedFileBindingConflict
		}
	} else {
		if state != "started" && state != "uncertain" {
			return false, service.ErrManagedFileNotReady
		}
		if _, e = tx.ExecContext(ctx, `UPDATE gateway_file_remote_operations SET state='succeeded',remote_id=$2,input_tokens=$3,completed_at=clock_timestamp(),safe_error_code=NULL,updated_at=clock_timestamp() WHERE operation_id=$1`, expected.Operation.ID, result.RemoteID, result.InputTokens); e != nil {
			return false, e
		}
	}
	// Recording success never depends on activation. A retired or expired file,
	// or a lost preparation lease, retains its copy without becoming generatable.
	activated := false
	live, e := managedLeaseValid(ctx, tx, permit, expected, row)
	if e != nil {
		return false, e
	}
	if live && !row.migrationID.Valid {
		var ready bool
		e = tx.QueryRowContext(ctx, `SELECT state='ready' AND lifecycle_version=$2 AND (expires_at IS NULL OR expires_at>clock_timestamp()) FROM gateway_files WHERE id=$1`, files[0].ID, expected.File.Version).Scan(&ready)
		if e != nil {
			return false, e
		}
		if ready {
			_, e = tx.ExecContext(ctx, `UPDATE gateway_file_bindings SET active_operation_id=$2,updated_at=clock_timestamp() WHERE file_id=$1`, files[0].ID, expected.Operation.ID)
			if e != nil {
				return false, e
			}
			activated = true
		}
	}
	if e = tx.Commit(); e != nil {
		return false, e
	}
	return activated, nil
}

func (r *ManagedFileLedgerRepository) MarkUncertain(ctx context.Context, permit *service.ManagedFileReservation, ref, code string) error {
	expected, e := managedPermitFile(permit, ref)
	if e != nil {
		return e
	}
	if len(code) == 0 || len(code) > 64 || strings.IndexFunc(code, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') }) >= 0 {
		return service.ErrInvalidFile
	}
	tx, e := r.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	files, e := r.lockFiles(ctx, tx, permit.Owner, []string{ref}, false)
	if e != nil {
		return e
	}
	row, e := readManagedBinding(ctx, tx, files[0])
	if e != nil {
		return e
	}
	if !row.hasBinding || row.binding.Selection != permit.Selection || row.binding.Operation.ID != expected.Operation.ID {
		return service.ErrManagedFileBindingConflict
	}
	if row.binding.Operation.State == "reserved" {
		return service.ErrManagedFileNotReady
	}
	if row.binding.Operation.State == "started" {
		if _, e = tx.ExecContext(ctx, `UPDATE gateway_file_remote_operations SET state='uncertain',safe_error_code=$2,updated_at=clock_timestamp() WHERE operation_id=$1`, expected.Operation.ID, code); e != nil {
			return e
		}
	}
	return tx.Commit()
}

func (r *ManagedFileLedgerRepository) AuthorizeGeneration(ctx context.Context, permit *service.ManagedFileReservation) error {
	if permit == nil || len(permit.Files) == 0 {
		return service.ErrInvalidFile
	}
	refs := make([]string, 0, len(permit.Files))
	byRef := make(map[string]service.ManagedFileBinding, len(permit.Files))
	for _, f := range permit.Files {
		if f.File == nil {
			return service.ErrInvalidFile
		}
		refs = append(refs, f.File.PublicID)
		byRef[f.File.PublicID] = f
	}
	tx, e := r.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	files, e := r.lockFiles(ctx, tx, permit.Owner, refs, true)
	if e != nil {
		return e
	}
	for _, f := range files {
		row, e := readManagedBinding(ctx, tx, f)
		if e != nil {
			return e
		}
		if row.migrationID.Valid {
			return service.ErrManagedFileBusy
		}
		expected := byRef[f.PublicID]
		live, e := managedLeaseValid(ctx, tx, permit, expected, row)
		if e != nil {
			return e
		}
		if !live {
			return service.ErrFileLeaseConflict
		}
		if row.binding.Operation.State != "succeeded" || row.active != expected.Operation.ID {
			return service.ErrManagedFileNotReady
		}
	}
	return tx.Commit()
}

func (r *ManagedFileLedgerRepository) FenceForRetirement(ctx context.Context, tx *sql.Tx, snapshot service.FileLifecycleSnapshot) (service.FileRemoteDisposition, error) {
	return inspectManagedRemote(ctx, tx, snapshot, true)
}
func (r *ManagedFileLedgerRepository) InspectForLocalPurge(ctx context.Context, tx *sql.Tx, snapshot service.FileLifecycleSnapshot) (service.FileRemoteDisposition, error) {
	return inspectManagedRemote(ctx, tx, snapshot, false)
}

// All historical operations participate, including copies that were never active.
// Re-read the file under its lock rather than trusting a caller's flag snapshot.
func inspectManagedRemote(ctx context.Context, tx *sql.Tx, snapshot service.FileLifecycleSnapshot, fence bool) (service.FileRemoteDisposition, error) {
	out := service.FileRemoteDisposition{Kind: service.FileRemoteNone}
	if tx == nil || snapshot.ID <= 0 {
		return out, service.ErrFileRemoteGuardUnavailable
	}
	var possible bool
	if err := tx.QueryRowContext(ctx, `SELECT remote_activity_possible FROM gateway_files WHERE id=$1 FOR UPDATE`, snapshot.ID).Scan(&possible); err != nil {
		return out, err
	}
	var bindingVersion int64
	err := tx.QueryRowContext(ctx, `SELECT version FROM gateway_file_bindings WHERE file_id=$1 FOR UPDATE`, snapshot.ID).Scan(&bindingVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	if fence && bindingVersion != 0 {
		if err = tx.QueryRowContext(ctx, `UPDATE gateway_file_bindings SET lease_token=NULL,lease_until=NULL,version=version+1,updated_at=clock_timestamp() WHERE file_id=$1 RETURNING version`, snapshot.ID).Scan(&bindingVersion); err != nil {
			return out, err
		}
	}
	if fence {
		if _, err = tx.ExecContext(ctx, `UPDATE gateway_file_remote_operations SET state='uncertain',safe_error_code='file_retired',updated_at=clock_timestamp() WHERE file_id=$1 AND state='started'`, snapshot.ID); err != nil {
			return out, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT state,remote_id,input_tokens,(to_jsonb(o)-'created_at'-'updated_at'-'started_at'-'completed_at')::text FROM gateway_file_remote_operations o WHERE file_id=$1 ORDER BY operation_id FOR UPDATE`, snapshot.ID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	digest := sha256.New()
	fmt.Fprintf(digest, "bps-file-ledger/1:%d:%d:%t\n", snapshot.ID, bindingVersion, possible)
	evidence := false
	for rows.Next() {
		var state, encoded string
		var remote sql.NullString
		var tokens sql.NullInt64
		if err = rows.Scan(&state, &remote, &tokens, &encoded); err != nil {
			return out, err
		}
		fmt.Fprintln(digest, encoded)
		switch state {
		case "uncertain":
			out.Kind = service.FileRemoteUncertain
			evidence = true
		case "started":
			if out.Kind != service.FileRemoteUncertain {
				out.Kind = service.FileRemotePending
			}
			evidence = true
		case "reserved":
			if out.Kind == service.FileRemoteNone || out.Kind == service.FileRemoteConfirmed {
				out.Kind = service.FileRemotePending
			}
		case "succeeded":
			if !remote.Valid || !managedfiles.ValidRemoteID(remote.String) || !tokens.Valid || tokens.Int64 < 0 {
				return out, service.ErrFileRemoteGuardUnavailable
			}
			if out.Kind == service.FileRemoteNone {
				out.Kind = service.FileRemoteConfirmed
			}
			evidence = true
		case "failed":
			// A failed attempt is still possible unless absence was independently proven.
			out.Kind = service.FileRemoteUncertain
			evidence = true
		default:
			return out, service.ErrFileRemoteGuardUnavailable
		}
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if possible && !evidence {
		return out, service.ErrFileRemoteGuardUnavailable
	}
	if evidence && !possible {
		return out, service.ErrFileRemoteGuardUnavailable
	}
	out.Revision = hex.EncodeToString(digest.Sum(nil))
	return out, nil
}

func (r *ManagedFileLedgerRepository) ReleaseReservation(ctx context.Context, permit *service.ManagedFileReservation) error {
	if permit == nil || len(permit.Files) == 0 {
		return service.ErrInvalidFile
	}
	refs := make([]string, 0, len(permit.Files))
	byRef := make(map[string]service.ManagedFileBinding, len(permit.Files))
	for _, binding := range permit.Files {
		if binding.File == nil {
			return service.ErrInvalidFile
		}
		if _, err := managedPermitFile(permit, binding.File.PublicID); err != nil {
			return err
		}
		refs = append(refs, binding.File.PublicID)
		byRef[binding.File.PublicID] = binding
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	files, err := r.lockFiles(ctx, tx, permit.Owner, refs, false)
	if err != nil {
		return err
	}
	for _, f := range files {
		row, err := readManagedBinding(ctx, tx, f)
		if err != nil {
			return err
		}
		expected := byRef[f.PublicID]
		if !row.migrationID.Valid && row.hasBinding && row.binding.Selection == permit.Selection && row.binding.Version == expected.Version && row.binding.LeaseToken == permit.LeaseToken && row.binding.Operation.ID == expected.Operation.ID {
			if _, err = tx.ExecContext(ctx, `UPDATE gateway_file_bindings SET lease_token=NULL,lease_until=NULL,version=version+1,updated_at=clock_timestamp() WHERE file_id=$1`, f.ID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
