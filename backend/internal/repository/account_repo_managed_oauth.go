package repository

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/openai"
	"github.com/MACOS-DO/sub4api/internal/service"
)

var _ service.ManagedOAuthRepository = (*accountRepository)(nil)

const installManagedOAuthSQL = `
WITH updated AS (
 UPDATE accounts a
 SET credentials = (COALESCE(a.credentials,'{}'::jsonb) - 'refresh_token' - 'id_token') ||
       jsonb_build_object('access_token',$1::text,'expires_at',$2::text,'chatgpt_account_id',$3::text,'client_id',$4::text),
     extra = jsonb_set(COALESCE(a.extra,'{}'::jsonb),ARRAY[$5]::text[],$6::jsonb,true),
     updated_at = NOW()
 WHERE a.id=$7 AND a.deleted_at IS NULL AND a.platform='openai' AND a.type='oauth'
   AND a.extra->$5 = $8::jsonb
   AND COALESCE(a.credentials->>'refresh_token','')=''
 RETURNING a.id
)
INSERT INTO scheduler_outbox(event_type,account_id,group_id,payload)
SELECT $9,updated.id,NULL,NULL FROM updated
`

func (r *accountRepository) InstallManagedOAuthCredentialsIfMatch(ctx context.Context, id int64, expected, next service.ManagedOAuthState, token, expiresAt string) (bool, error) {
	if expected.ManagedOAuthIdentity != next.ManagedOAuthIdentity || next.ManagedOAuthIdentity.Validate() != nil || next.TokenRevision <= expected.TokenRevision || next.AuthBlocked || token == "" {
		return false, service.ErrManagedOAuthInvalid
	}
	expiry, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return false, service.ErrManagedOAuthInvalid
	}
	before, err := json.Marshal(expected)
	if err != nil {
		return false, err
	}
	after, err := json.Marshal(next)
	if err != nil {
		return false, err
	}
	result, err := r.sql.ExecContext(ctx, installManagedOAuthSQL, token, strconv.FormatInt(expiry.Unix(), 10), next.WorkspaceID, openai.ClientID, service.ManagedOAuthExtraKey, string(after), id, string(before), service.SchedulerOutboxEventAccountChanged)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected == 0 {
		return false, err
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, id)
	return true, nil
}

const recordManagedOAuthEventSQL = `
WITH updated AS (
 UPDATE accounts a
 SET extra=jsonb_set(COALESCE(a.extra,'{}'::jsonb),ARRAY[$1]::text[],$2::jsonb,true),updated_at=NOW()
 WHERE a.id=$3 AND a.deleted_at IS NULL AND a.platform='openai' AND a.type='oauth'
   AND a.extra->$1=$4::jsonb
   AND a.credentials->>'access_token'=$5
   AND COALESCE(a.credentials->>'refresh_token','')=''
 RETURNING a.id
)
INSERT INTO scheduler_outbox(event_type,account_id,group_id,payload)
SELECT $6,updated.id,NULL,NULL FROM updated
`

func (r *accountRepository) RecordManagedOAuthEventIfMatch(ctx context.Context, id int64, expected, next service.ManagedOAuthState, rejectedToken string) (bool, error) {
	if next.ManagedOAuthIdentity != expected.ManagedOAuthIdentity || next.TokenRevision != expected.TokenRevision || !next.AuthBlocked || next.EventCursor != expected.EventCursor+1 || rejectedToken == "" {
		return false, service.ErrManagedOAuthInvalid
	}
	before, err := json.Marshal(expected)
	if err != nil {
		return false, err
	}
	after, err := json.Marshal(next)
	if err != nil {
		return false, err
	}
	result, err := r.sql.ExecContext(ctx, recordManagedOAuthEventSQL, service.ManagedOAuthExtraKey, string(after), id, string(before), rejectedToken, service.SchedulerOutboxEventAccountChanged)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected == 0 {
		return false, err
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, id)
	return true, nil
}
