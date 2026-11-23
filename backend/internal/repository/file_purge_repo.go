package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
)

func (r *fileRepository) LifecycleCandidates(ctx context.Context, after int64, limit int) ([]int64, error) {
	if limit < 1 || limit > 1000 {
		return nil, service.ErrInvalidFile
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM gateway_files WHERE id>$1 AND
 ((state='ready' AND expires_at<=clock_timestamp()) OR (state='retired' AND local_purged_at IS NULL)) ORDER BY id LIMIT $2`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (r *fileRepository) lifecycleFile(ctx context.Context, tx *sql.Tx, id int64) (*service.StoredFile, error) {
	return scanStoredFile(tx.QueryRowContext(ctx, `SELECT `+fileColumns+` FROM gateway_files WHERE id=$1 FOR UPDATE SKIP LOCKED`, id))
}
func lifecycleSnapshot(f *service.StoredFile) service.FileLifecycleSnapshot {
	return service.FileLifecycleSnapshot{ID: f.ID, UserID: f.UserID, GroupID: f.GroupID, Version: f.Version, PublicID: f.PublicID, State: f.State, RemoteActivityPossible: f.RemoteActivityPossible, ExpiresAt: f.ExpiresAt, RetiredAt: f.RetiredAt, LocalPurgedAt: f.LocalPurgedAt}
}
func (r *fileRepository) inspectRemote(ctx context.Context, tx *sql.Tx, f *service.StoredFile, retire bool) error {
	if !f.RemoteActivityPossible {
		return nil
	}
	if r.remoteLifecycle == nil {
		return service.ErrFileRemoteGuardUnavailable
	}
	var d service.FileRemoteDisposition
	var err error
	if retire {
		d, err = r.remoteLifecycle.FenceForRetirement(ctx, tx, lifecycleSnapshot(f))
	} else {
		d, err = r.remoteLifecycle.InspectForLocalPurge(ctx, tx, lifecycleSnapshot(f))
	}
	if err != nil {
		return err
	}
	switch d.Kind {
	case service.FileRemoteNone, service.FileRemoteConfirmed, service.FileRemotePending, service.FileRemoteUncertain:
		return nil
	}
	return service.ErrFileRemoteGuardUnavailable
}

func (r *fileRepository) RetireExpired(ctx context.Context, id int64, drain time.Duration) (bool, error) {
	if drain <= 0 {
		return false, service.ErrInvalidFile
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	f, err := r.lifecycleFile(ctx, tx, id)
	if errors.Is(err, service.ErrFileNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if f.State != "ready" {
		return false, nil
	}
	var now time.Time
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return false, err
	}
	if f.ExpiresAt == nil || f.ExpiresAt.After(now) {
		return false, nil
	}
	if err = r.inspectRemote(ctx, tx, f, true); err != nil {
		return false, err
	}
	until := f.ExpiresAt.Add(drain)
	if until.Before(now) {
		until = now
	}
	result, err := tx.ExecContext(ctx, `UPDATE gateway_files SET state='retired',retired_at=$2,retire_reason='expired',read_drain_until=$3,lifecycle_version=lifecycle_version+1,updated_at=$2 WHERE id=$1 AND state='ready'`, id, now, until)
	if err = fileCASResult(result, err); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (r *fileRepository) ClaimLocalPurge(ctx context.Context, id int64, token string, lease time.Duration) (*service.FilePurgeTicket, error) {
	if token == "" || lease <= 0 {
		return nil, service.ErrFileLeaseConflict
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	f, err := r.lifecycleFile(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if f.State != "retired" || f.LocalPurgedAt != nil {
		return nil, service.ErrFileLeaseConflict
	}
	if err = r.inspectRemote(ctx, tx, f, false); err != nil {
		return nil, err
	}
	var blocked bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM gateway_file_read_leases WHERE file_id=$1 AND lease_until>clock_timestamp())`, id).Scan(&blocked)
	if err != nil {
		return nil, err
	}
	if blocked {
		return nil, service.ErrFileLeaseConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE gateway_files SET cleanup_token=$2,cleanup_lease_until=clock_timestamp()+make_interval(secs=>$3::double precision) WHERE id=$1 AND (cleanup_token IS NULL OR cleanup_lease_until<=clock_timestamp())`, id, token, lease.Seconds())
	if err = fileCASResult(result, err); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &service.FilePurgeTicket{ID: id, Version: f.Version, StorageKey: f.StorageKey, Token: token}, nil
}

func (r *fileRepository) ConfirmLocalPurge(ctx context.Context, t service.FilePurgeTicket) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	f, err := r.lifecycleFile(ctx, tx, t.ID)
	if err != nil {
		return err
	}
	if f.State != "retired" || f.Version != t.Version || f.StorageKey != t.StorageKey {
		return service.ErrFileLeaseConflict
	}
	var token sql.NullString
	var until sql.NullTime
	if err = tx.QueryRowContext(ctx, `SELECT cleanup_token,cleanup_lease_until FROM gateway_files WHERE id=$1`, t.ID).Scan(&token, &until); err != nil {
		return err
	}
	if !token.Valid || token.String != t.Token {
		return service.ErrFileLeaseConflict
	}
	if err = r.inspectRemote(ctx, tx, f, false); err != nil {
		return err
	}
	if f.LocalPurgedAt != nil {
		return tx.Commit()
	}
	result, err := tx.ExecContext(ctx, `UPDATE gateway_files SET local_purged_at=clock_timestamp(),reserved_bytes=0,updated_at=clock_timestamp()
 WHERE id=$1 AND cleanup_token=$2 AND cleanup_lease_until>clock_timestamp()
 AND NOT EXISTS(SELECT 1 FROM gateway_file_read_leases WHERE file_id=$1 AND lease_until>clock_timestamp())`, t.ID, t.Token)
	if err = fileCASResult(result, err); err != nil {
		return err
	}
	return tx.Commit()
}
