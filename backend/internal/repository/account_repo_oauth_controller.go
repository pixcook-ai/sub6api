package repository

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

var _ service.ManagedOAuthControllerKeyRepository = (*accountRepository)(nil)

// The account lock serializes registration and revocation; retained revoked rows
// prevent an old operation from restoring a controller credential.
const putManagedOAuthControllerKeySQL = `
WITH eligible_account AS MATERIALIZED (
 SELECT a.id FROM accounts a
 WHERE a.id=$1 AND a.deleted_at IS NULL AND a.platform='openai' AND a.type='oauth'
   AND COALESCE(a.credentials->>'refresh_token','')=''
   AND a.extra->'managed_oauth'=$2::jsonb
   AND COALESCE(a.extra->'managed_oauth'->>'publication_fence_operation_id','')=''
 FOR NO KEY UPDATE
)
INSERT INTO managed_oauth_controller_keys AS k
 (account_id,operation_id,key_hash,identity)
SELECT id,$3::uuid,$4,$5::jsonb FROM eligible_account
ON CONFLICT (account_id,operation_id) DO UPDATE
 SET operation_id=EXCLUDED.operation_id
 WHERE k.key_hash=EXCLUDED.key_hash AND k.identity=EXCLUDED.identity AND k.revoked_at IS NULL
RETURNING k.account_id,k.operation_id::text,k.identity,k.revoked_at IS NULL
`

const revokeManagedOAuthControllerKeySQL = `
WITH locked_account AS MATERIALIZED (
 SELECT id FROM accounts WHERE id=$1 FOR NO KEY UPDATE
)
UPDATE managed_oauth_controller_keys k
 SET revoked_at=COALESCE(k.revoked_at,NOW())
 FROM locked_account a
 WHERE k.account_id=a.id AND k.operation_id=$2::uuid
RETURNING k.account_id,k.operation_id::text,k.identity,k.revoked_at IS NULL
`

const findManagedOAuthControllerKeySQL = `
SELECT account_id,operation_id::text,identity,revoked_at IS NULL
FROM managed_oauth_controller_keys WHERE key_hash=$1
`

func validOAuthControllerRepositoryOperation(operation string) bool {
	id, err := uuid.Parse(operation)
	return err == nil && id != uuid.Nil && id.String() == operation
}

func validOAuthControllerRepositoryHash(hash string) bool {
	raw, err := hex.DecodeString(hash)
	return err == nil && len(raw) == 32 && hex.EncodeToString(raw) == hash
}

func (r *accountRepository) scanManagedOAuthControllerKey(ctx context.Context, query string, args []any, missing error) (*service.ManagedOAuthControllerKeyState, error) {
	var state service.ManagedOAuthControllerKeyState
	var identity []byte
	err := scanSingleRow(ctx, r.sql, query, args, &state.AccountID, &state.OperationID, &identity, &state.Active)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, missing
	}
	var postgresErr *pq.Error
	if errors.As(err, &postgresErr) && postgresErr != nil && postgresErr.Code == "23505" {
		return nil, service.ErrManagedOAuthConflict
	}
	if err != nil {
		return nil, err
	}
	if state.AccountID <= 0 || !validOAuthControllerRepositoryOperation(state.OperationID) || json.Unmarshal(identity, &state.ManagedOAuthIdentity) != nil || state.ManagedOAuthIdentity.Validate() != nil {
		return nil, service.ErrManagedOAuthInvalid
	}
	return &state, nil
}

func (r *accountRepository) PutManagedOAuthControllerKey(ctx context.Context, accountID int64, expected service.ManagedOAuthState, operationID, keyHash string) (*service.ManagedOAuthControllerKeyState, error) {
	if accountID <= 0 || expected.ManagedOAuthIdentity.Validate() != nil || expected.PublicationFenceOperationID != "" || !validOAuthControllerRepositoryOperation(operationID) || !validOAuthControllerRepositoryHash(keyHash) {
		return nil, service.ErrManagedOAuthInvalid
	}
	before, err := json.Marshal(expected)
	if err != nil {
		return nil, err
	}
	identity, err := json.Marshal(expected.ManagedOAuthIdentity)
	if err != nil {
		return nil, err
	}
	return r.scanManagedOAuthControllerKey(ctx, putManagedOAuthControllerKeySQL, []any{accountID, string(before), operationID, keyHash, string(identity)}, service.ErrManagedOAuthConflict)
}

func (r *accountRepository) RevokeManagedOAuthControllerKey(ctx context.Context, accountID int64, operationID string) (*service.ManagedOAuthControllerKeyState, error) {
	if accountID <= 0 || !validOAuthControllerRepositoryOperation(operationID) {
		return nil, service.ErrManagedOAuthInvalid
	}
	return r.scanManagedOAuthControllerKey(ctx, revokeManagedOAuthControllerKeySQL, []any{accountID, operationID}, service.ErrManagedOAuthConflict)
}

func (r *accountRepository) FindManagedOAuthControllerKey(ctx context.Context, keyHash string) (*service.ManagedOAuthControllerKeyState, error) {
	if !validOAuthControllerRepositoryHash(keyHash) {
		return nil, service.ErrManagedOAuthInvalid
	}
	return r.scanManagedOAuthControllerKey(ctx, findManagedOAuthControllerKeySQL, []any{keyHash}, service.ErrManagedOAuthControllerKeyUnknown)
}
