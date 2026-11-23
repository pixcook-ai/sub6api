package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
)

func (r *fileRepository) DeleteLocalFile(ctx context.Context, owner service.FileOwner, id string, drain time.Duration) error {
	if owner.UserID <= 0 || owner.APIKeyID <= 0 || owner.GroupID <= 0 || id == "" || len(id) > 256 || drain <= 0 || drain > 24*time.Hour {
		return service.ErrInvalidFile
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The same file-row lock fences Reserve/Start/Complete and read leases.
	// Never select only visible rows: an owned deletion tombstone is idempotent.
	f, err := scanStoredFile(tx.QueryRowContext(ctx, `SELECT `+fileColumns+` FROM gateway_files
 WHERE public_id=$1 AND user_id=$2 AND group_id=$3 AND api_key_id=$4 FOR UPDATE`, id, owner.UserID, owner.GroupID, owner.APIKeyID))
	if err != nil {
		return err
	}
	var reason sql.NullString
	var now time.Time
	var managed bool
	err = tx.QueryRowContext(ctx, `SELECT retire_reason,clock_timestamp(),
 EXISTS(SELECT 1 FROM gateway_file_bindings WHERE file_id=$1)
 OR EXISTS(SELECT 1 FROM gateway_file_remote_operations WHERE file_id=$1)
 FROM gateway_files WHERE id=$1`, f.ID).Scan(&reason, &now, &managed)
	if err != nil {
		return err
	}
	if f.RemoteActivityPossible || managed {
		// Until the native deletion contract is verified, even a reserved
		// managed mapping is conservatively outside local-only deletion.
		return service.ErrFileRemoteDeleteUnsupported
	}
	if (f.State == "retired" || f.State == "deleted") && reason.String == "deleted" {
		return tx.Commit()
	}
	if f.State != "ready" || f.ExpiresAt != nil && !f.ExpiresAt.After(now) {
		return service.ErrFileNotFound
	}
	result, err := tx.ExecContext(ctx, `UPDATE gateway_files SET state='retired',retired_at=$2,
 retire_reason='deleted',read_drain_until=$3,lifecycle_version=lifecycle_version+1,updated_at=$2
 WHERE id=$1 AND state='ready'`, f.ID, now, now.Add(drain))
	if err = fileCASResult(result, err); err != nil {
		return err
	}
	return tx.Commit()
}
