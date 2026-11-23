package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
)

type fileRepository struct {
	db              *sql.DB
	remoteLifecycle service.FileRemoteLifecyclePort
}

func NewFileRepository(db *sql.DB) service.FileRepository { return &fileRepository{db: db} }

func NewFileRepositoryWithRemoteLifecycle(db *sql.DB, port service.FileRemoteLifecyclePort) service.FileRepository {
	return &fileRepository{db: db, remoteLifecycle: port}
}

const fileColumns = `id, public_id, user_id, api_key_id, group_id, storage_key, filename, purpose, mime,
 state, bytes, reserved_bytes, sha256, COALESCE(upload_token, ''), created_at,
 expires_at, retired_at, read_drain_until, local_purged_at, lifecycle_version, remote_activity_possible`

// All processes take this transaction lock before reserving disk budget. In
// progress and pending-cleanup bytes remain charged until durable cleanup.
const fileBudgetAdvisoryLock int64 = 0x66696c6573627564

func (r *fileRepository) Reserve(ctx context.Context, file *service.StoredFile, userQuota, globalQuota int64, lease time.Duration) error {
	return r.reserveUpload(ctx, file, userQuota, globalQuota, lease, 0, 0, 0)
}

func (r *fileRepository) ReserveUpload(ctx context.Context, file *service.StoredFile, userQuota, globalQuota int64, lease, wall time.Duration, userLimit, globalLimit int) error {
	if wall <= 0 || userLimit <= 0 || globalLimit < userLimit {
		return service.ErrInvalidFile
	}
	return r.reserveUpload(ctx, file, userQuota, globalQuota, lease, wall, userLimit, globalLimit)
}

func (r *fileRepository) reserveUpload(ctx context.Context, file *service.StoredFile, userQuota, globalQuota int64, lease, wall time.Duration, userLimit, globalLimit int) error {
	if file == nil || file.ReservedBytes <= 0 || userQuota <= 0 || globalQuota <= 0 || lease <= 0 {
		return fmt.Errorf("invalid Files reservation")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = reserveUploadTx(ctx, tx, file, userQuota, globalQuota, lease, wall, userLimit, globalLimit); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	file.State = "staging"
	return nil
}

// reserveUploadTx holds the global Files budget lock until reservation commits.
func reserveUploadTx(ctx context.Context, tx *sql.Tx, file *service.StoredFile, userQuota, globalQuota int64, lease, wall time.Duration, userLimit, globalLimit int) error {
	var err error
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, fileBudgetAdvisoryLock); err != nil {
		return err
	}
	var total, owned int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(reserved_bytes),0),
 COALESCE(SUM(reserved_bytes) FILTER (WHERE user_id=$1),0)
 FROM gateway_files WHERE state <> 'deleted'`, file.UserID).Scan(&total, &owned); err != nil {
		return err
	}
	// Subtraction avoids overflow even with a malformed caller-supplied size.
	if file.ReservedBytes > globalQuota || total > globalQuota-file.ReservedBytes ||
		file.ReservedBytes > userQuota || owned > userQuota-file.ReservedBytes {
		return service.ErrFileQuotaExceeded
	}
	if userLimit > 0 {
		var uploading, ownedUploading int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COUNT(*) FILTER (WHERE user_id=$1)
 FROM gateway_files WHERE state IN ('staging','tombstone')`, file.UserID).Scan(&uploading, &ownedUploading); err != nil {
			return err
		}
		if uploading >= globalLimit || ownedUploading >= userLimit {
			return service.ErrFileUploadConcurrency
		}
	}
	var now time.Time
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	leaseUntil := now.Add(lease)
	var deadline *time.Time
	if wall > 0 {
		d := now.Add(wall)
		deadline = &d
		if d.Before(leaseUntil) {
			leaseUntil = d
		}
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO gateway_files
 (public_id,user_id,api_key_id,group_id,storage_key,filename,purpose,mime,state,reserved_bytes,upload_token,upload_lease_until,upload_deadline_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'staging',$9,$10,$11,$12)
 RETURNING id,created_at,lifecycle_version`, file.PublicID, file.UserID, file.APIKeyID, file.GroupID, file.StorageKey, file.Filename, file.Purpose, file.MIME,
		file.ReservedBytes, file.UploadToken, leaseUntil, deadline).Scan(&file.ID, &file.CreatedAt, &file.Version)
	if err != nil {
		return err
	}
	return nil
}

func (r *fileRepository) CommitUpload(ctx context.Context, id int64, token string, size int64, sha, purpose string) error {
	_, err := r.CommitUploadWithRetention(ctx, id, token, size, sha, purpose, nil)
	return err
}

func (r *fileRepository) CommitUploadWithRetention(ctx context.Context, id int64, token string, size int64, sha, purpose string, retention *int64) (*service.StoredFile, error) {
	return r.commitUpload(ctx, id, token, size, sha, purpose, nil, nil, retention)
}

func (r *fileRepository) CommitUploadMetadata(ctx context.Context, id int64, token string, size int64, sha, purpose, filename, mime string, retention *int64) (*service.StoredFile, error) {
	return r.commitUpload(ctx, id, token, size, sha, purpose, &filename, &mime, retention)
}

func (r *fileRepository) commitUpload(ctx context.Context, id int64, token string, size int64, sha, purpose string, filename, mime *string, retention *int64) (*service.StoredFile, error) {
	if retention != nil && (*retention < 3600 || *retention > 2592000) {
		return nil, service.ErrInvalidFileRetention
	}
	file, err := scanStoredFile(r.db.QueryRowContext(ctx, `UPDATE gateway_files SET state='ready',bytes=$3,reserved_bytes=$3,
 sha256=$4,purpose=$5,filename=COALESCE($7,filename),mime=COALESCE($8,mime),expires_at=CASE WHEN $6::bigint IS NULL THEN NULL ELSE created_at+make_interval(secs=>$6::double precision) END,
 upload_token=NULL,upload_lease_until=NULL,updated_at=clock_timestamp()
 WHERE id=$1 AND state='staging' AND upload_token=$2 AND upload_lease_until>clock_timestamp()
 AND (upload_deadline_at IS NULL OR upload_deadline_at>clock_timestamp())
 AND $3>=0 AND $3<=reserved_bytes RETURNING `+fileColumns, id, token, size, sha, purpose, retention, filename, mime))
	if errors.Is(err, service.ErrFileNotFound) {
		return nil, service.ErrFileLeaseConflict
	}
	return file, err
}

func (r *fileRepository) AbortUpload(ctx context.Context, id int64, token string) (bool, error) {
	result, err := r.db.ExecContext(ctx, `UPDATE gateway_files SET state='tombstone',updated_at=clock_timestamp()
 WHERE id=$1 AND state='staging' AND upload_token=$2`, id, token)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (r *fileRepository) GetForOwner(ctx context.Context, userID, groupID int64, publicID string) (*service.StoredFile, error) {
	return scanStoredFile(r.db.QueryRowContext(ctx, `SELECT `+fileColumns+` FROM gateway_files
 WHERE public_id=$1 AND user_id=$2 AND group_id=$3 AND state='ready'
 AND (expires_at IS NULL OR expires_at>clock_timestamp())`, publicID, userID, groupID))
}

func (r *fileRepository) AcquireRead(ctx context.Context, userID, groupID int64, publicID, token string, lease time.Duration) (*service.StoredFile, error) {
	file, _, err := r.AcquireReadGrant(ctx, userID, groupID, publicID, token, lease, lease)
	return file, err
}

func (r *fileRepository) AcquireReadGrant(ctx context.Context, userID, groupID int64, publicID, token string, lease, drain time.Duration) (*service.StoredFile, service.FileReadGrant, error) {
	var grant service.FileReadGrant
	if token == "" || lease <= 0 || drain < lease {
		return nil, grant, service.ErrFileLeaseConflict
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, grant, err
	}
	defer tx.Rollback()
	file, err := scanStoredFile(tx.QueryRowContext(ctx, `SELECT `+fileColumns+` FROM gateway_files
 WHERE public_id=$1 AND user_id=$2 AND group_id=$3 AND state='ready' FOR UPDATE`, publicID, userID, groupID))
	if err != nil {
		return nil, grant, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&grant.DBNow); err != nil {
		return nil, grant, err
	}
	if file.ExpiresAt != nil && !file.ExpiresAt.After(grant.DBNow) {
		return nil, grant, service.ErrFileNotFound
	}
	grant.Until = grant.DBNow.Add(lease)
	if file.ExpiresAt != nil && file.ExpiresAt.Add(drain).Before(grant.Until) {
		grant.Until = file.ExpiresAt.Add(drain)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO gateway_file_read_leases(file_id,token,lease_until) VALUES($1,$2,$3)`, file.ID, token, grant.Until)
	if err != nil {
		return nil, grant, err
	}
	if err = tx.Commit(); err != nil {
		return nil, grant, err
	}
	return file, grant, nil
}

func (r *fileRepository) RenewUpload(ctx context.Context, id int64, token string, lease time.Duration) error {
	if token == "" || lease <= 0 {
		return service.ErrFileLeaseConflict
	}
	result, err := r.db.ExecContext(ctx, `UPDATE gateway_files
	 SET upload_lease_until=LEAST(clock_timestamp()+make_interval(secs=>$3::double precision),upload_deadline_at)
 WHERE id=$1 AND upload_token=$2 AND state='staging' AND upload_lease_until>clock_timestamp()
 AND (upload_deadline_at IS NULL OR upload_deadline_at>clock_timestamp())`, id, token, lease.Seconds())
	return fileCASResult(result, err)
}

func (r *fileRepository) RenewRead(ctx context.Context, id int64, token string, lease time.Duration) error {
	_, err := r.RenewReadGrant(ctx, id, token, lease, lease)
	return err
}

func (r *fileRepository) RenewReadGrant(ctx context.Context, id int64, token string, lease, drain time.Duration) (service.FileReadGrant, error) {
	var grant service.FileReadGrant
	if token == "" || lease <= 0 || drain < lease {
		return grant, service.ErrFileLeaseConflict
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return grant, err
	}
	defer tx.Rollback()
	file, err := scanStoredFile(tx.QueryRowContext(ctx, `SELECT `+fileColumns+` FROM gateway_files WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, service.ErrFileNotFound) {
		return grant, service.ErrFileLeaseConflict
	}
	if err != nil {
		return grant, err
	}
	if file.State != "ready" && file.State != "retired" {
		return grant, service.ErrFileLeaseConflict
	}
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&grant.DBNow); err != nil {
		return grant, err
	}
	grant.Until = grant.DBNow.Add(lease)
	if file.ExpiresAt != nil && file.ExpiresAt.Add(drain).Before(grant.Until) {
		grant.Until = file.ExpiresAt.Add(drain)
	}
	if file.ReadDrainUntil != nil && file.ReadDrainUntil.Before(grant.Until) {
		grant.Until = *file.ReadDrainUntil
	}
	if !grant.Until.After(grant.DBNow) {
		return grant, service.ErrFileLeaseConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE gateway_file_read_leases SET lease_until=$3 WHERE file_id=$1 AND token=$2 AND lease_until>$4`, id, token, grant.Until, grant.DBNow)
	if err = fileCASResult(result, err); err != nil {
		return grant, err
	}
	if err = tx.Commit(); err != nil {
		return grant, err
	}
	return grant, nil
}

func (r *fileRepository) ReleaseRead(ctx context.Context, id int64, token string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM gateway_file_read_leases WHERE file_id=$1 AND token=$2`, id, token)
	return err
}

func (r *fileRepository) ClaimStaleUploads(ctx context.Context, token string, lease time.Duration, limit int) ([]*service.StoredFile, error) {
	if token == "" || lease <= 0 || limit <= 0 {
		return nil, service.ErrFileLeaseConflict
	}
	rows, err := r.db.QueryContext(ctx, `WITH stale AS (
 SELECT id FROM gateway_files f WHERE state IN ('staging','tombstone') AND upload_lease_until<clock_timestamp()
 AND NOT EXISTS(SELECT 1 FROM gateway_file_read_leases l WHERE l.file_id=f.id AND l.lease_until>clock_timestamp())
 ORDER BY upload_lease_until,id FOR UPDATE SKIP LOCKED LIMIT $3
 ) UPDATE gateway_files SET state='tombstone',upload_token=$1,
 upload_lease_until=clock_timestamp()+make_interval(secs=>$2::double precision),updated_at=clock_timestamp()
 WHERE id IN (SELECT id FROM stale) RETURNING `+fileColumns, token, lease.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var files []*service.StoredFile
	for rows.Next() {
		file, err := scanStoredFile(rows)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

func (r *fileRepository) FinishCleanup(ctx context.Context, id int64, token string) error {
	result, err := r.db.ExecContext(ctx, `UPDATE gateway_files SET state='deleted',reserved_bytes=0,bytes=0,
 upload_token=NULL,upload_lease_until=NULL,updated_at=clock_timestamp()
 WHERE id=$1 AND state='tombstone' AND upload_token=$2
 AND NOT EXISTS(SELECT 1 FROM gateway_file_read_leases WHERE file_id=$1 AND lease_until>clock_timestamp())`, id, token)
	return fileCASResult(result, err)
}

func fileCASResult(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return service.ErrFileLeaseConflict
	}
	return nil
}

func scanStoredFile(row interface{ Scan(...any) error }) (*service.StoredFile, error) {
	file := &service.StoredFile{}
	err := row.Scan(&file.ID, &file.PublicID, &file.UserID, &file.APIKeyID, &file.GroupID, &file.StorageKey,
		&file.Filename, &file.Purpose, &file.MIME, &file.State, &file.Bytes, &file.ReservedBytes, &file.SHA256, &file.UploadToken, &file.CreatedAt,
		&file.ExpiresAt, &file.RetiredAt, &file.ReadDrainUntil, &file.LocalPurgedAt, &file.Version, &file.RemoteActivityPossible)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrFileNotFound
	}
	if err != nil {
		return nil, err
	}
	return file, nil
}
