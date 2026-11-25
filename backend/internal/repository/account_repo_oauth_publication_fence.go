package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	dbent "github.com/MACOS-DO/sub4api/ent"
	"github.com/MACOS-DO/sub4api/internal/service"
)

var _ service.ManagedOAuthPublicationFenceRepository = (*accountRepository)(nil)

const lockManagedOAuthPublicationSQL = `
SELECT id FROM accounts a
WHERE a.id=$1 AND a.deleted_at IS NULL AND a.platform='openai' AND a.type='oauth'
 AND COALESCE(a.credentials->>'refresh_token','')=''
 AND a.extra->'managed_oauth'=$2::jsonb
 AND COALESCE(a.extra->'managed_oauth'->>'publication_fence_operation_id','')=''
FOR NO KEY UPDATE
`

const fenceManagedOAuthPublicationSQL = `
UPDATE accounts SET
 extra=jsonb_set(extra,ARRAY['managed_oauth']::text[],$2::jsonb,true),
 schedulable=false,updated_at=NOW()
WHERE id=$1
`

const revokeAllManagedOAuthControllerKeysSQL = `
UPDATE managed_oauth_controller_keys SET revoked_at=COALESCE(revoked_at,NOW())
WHERE account_id=$1
`

const enqueueManagedOAuthPublicationFenceSQL = `
INSERT INTO scheduler_outbox(event_type,account_id,group_id,payload)
VALUES($2,$1,NULL,NULL)
`

// Lock before issuing the revocation statement. A single CTE using one snapshot
// can miss a registration committed while it waited for the account row lock.
// Registration uses the same row lock and rechecks the whole managed state.
func (r *accountRepository) FenceManagedOAuthPublicationIfMatch(ctx context.Context, id int64, expected service.ManagedOAuthState, operation string) (bool, error) {
	if id <= 0 || expected.ManagedOAuthIdentity.Validate() != nil || expected.PublicationFenceOperationID != "" || !validOAuthControllerRepositoryOperation(operation) {
		return false, service.ErrManagedOAuthInvalid
	}
	if r.client == nil || dbent.TxFromContext(ctx) != nil {
		// A public receipt must only be returned after our own transaction commits.
		return false, service.ErrManagedOAuthUnavailable
	}
	before, err := json.Marshal(expected)
	if err != nil {
		return false, err
	}
	next := expected
	next.PublicationFenceOperationID = operation
	after, err := json.Marshal(next)
	if err != nil {
		return false, err
	}
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	client := tx.Client()
	var locked int64
	err = scanSingleRow(ctx, client, lockManagedOAuthPublicationSQL, []any{id, string(before)}, &locked)
	if errors.Is(err, sql.ErrNoRows) {
		return false, service.ErrManagedOAuthConflict
	}
	if err != nil {
		return false, err
	}
	if locked != id {
		return false, service.ErrManagedOAuthConflict
	}
	if _, err = client.ExecContext(ctx, fenceManagedOAuthPublicationSQL, id, string(after)); err != nil {
		return false, err
	}
	if _, err = client.ExecContext(ctx, revokeAllManagedOAuthControllerKeysSQL, id); err != nil {
		return false, err
	}
	if _, err = client.ExecContext(ctx, enqueueManagedOAuthPublicationFenceSQL, id, service.SchedulerOutboxEventAccountChanged); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, id)
	return true, nil
}
