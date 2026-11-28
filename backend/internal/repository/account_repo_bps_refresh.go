package repository

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/MACOS-DO/sub4api/internal/service"
)

var _ service.OpenAIBPSOAuthRefreshRepository = (*accountRepository)(nil)
var _ service.OpenAIBPSOAuthRefreshFailureRepository = (*accountRepository)(nil)

// The complete grant/proxy/state snapshot and scheduler notification commit atomically.
func (r *accountRepository) UpdateOpenAIBPSOAuthCredentialsIfUnchanged(ctx context.Context, expected *service.Account, credentials map[string]any, clearDiagnosis, recoverError bool) (bool, error) {
	if r == nil || r.sql == nil {
		return false, errors.New("BPS refresh SQL executor is not configured")
	}
	if expected == nil || expected.Platform != service.PlatformOpenAIBPS || expected.Type != service.AccountTypeOAuth {
		return false, nil
	}
	expectedJSON, err := json.Marshal(normalizeJSONMap(expected.Credentials))
	if err != nil {
		return false, err
	}
	credentialsJSON, err := json.Marshal(normalizeJSONMap(credentials))
	if err != nil {
		return false, err
	}
	diagnosisJSON, err := json.Marshal(expected.Extra[service.OpenAIBPSCredentialStateExtraKey])
	if err != nil {
		return false, err
	}
	result, err := r.sql.ExecContext(ctx, `
 WITH updated AS (
  UPDATE accounts AS a
  SET credentials=$1::jsonb,
      extra=CASE WHEN $2 THEN COALESCE(a.extra,'{}'::jsonb)-($3::text) ELSE a.extra END,
      status=CASE WHEN $4 THEN $5 ELSE a.status END,
      error_message=CASE WHEN $4 THEN '' ELSE a.error_message END,
      updated_at=NOW()
  WHERE a.id=$6 AND a.deleted_at IS NULL
    AND a.platform=$7 AND a.type=$8
    AND a.credentials=$9::jsonb
    AND a.proxy_id IS NOT DISTINCT FROM $10
    AND a.status=$11 AND COALESCE(a.error_message,'')=$12
    AND COALESCE(a.extra->($3::text),'null'::jsonb)=$13::jsonb
  RETURNING a.id
 )
 INSERT INTO scheduler_outbox(event_type,account_id,group_id,payload)
 SELECT $14,updated.id,NULL,NULL FROM updated
 `, string(credentialsJSON), clearDiagnosis, service.OpenAIBPSCredentialStateExtraKey, recoverError, service.StatusActive, expected.ID, service.PlatformOpenAIBPS, service.AccountTypeOAuth, string(expectedJSON), expected.ProxyID, expected.Status, expected.ErrorMessage, string(diagnosisJSON), service.SchedulerOutboxEventAccountChanged)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected == 0 {
		return false, err
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, expected.ID)
	return true, nil
}

func (r *accountRepository) SetOpenAIBPSOAuthRefreshErrorIfUnchanged(ctx context.Context, expected *service.Account, state service.OpenAIBPSCredentialState) (bool, error) {
	if r == nil || r.sql == nil {
		return false, errors.New("BPS refresh SQL executor is not configured")
	}
	if expected == nil || expected.Platform != service.PlatformOpenAIBPS || expected.Type != service.AccountTypeOAuth || expected.Status != service.StatusActive {
		return false, nil
	}
	credentialsJSON, err := json.Marshal(normalizeJSONMap(expected.Credentials))
	if err != nil {
		return false, err
	}
	payload, err := json.Marshal(struct {
		service.OpenAIBPSCredentialState
		CredentialIdentity string `json:"credential_identity"`
		ManagedStatusError bool   `json:"managed_status_error"`
	}{state, service.OpenAIBPSCredentialIdentity(service.OpenAIBPSCredentialSnapshotFromAccount(expected)), true})
	if err != nil {
		return false, err
	}
	result, err := r.sql.ExecContext(ctx, `
 WITH updated AS (
  UPDATE accounts AS a
  SET extra=jsonb_set(COALESCE(a.extra,'{}'::jsonb),ARRAY[$1]::text[],$2::jsonb,true),
      status=$3,error_message=$4,updated_at=NOW()
  WHERE a.id=$5 AND a.deleted_at IS NULL
    AND a.platform=$6 AND a.type=$7 AND a.status=$8
    AND a.credentials=$9::jsonb AND a.proxy_id IS NOT DISTINCT FROM $10
  RETURNING a.id
 )
 INSERT INTO scheduler_outbox(event_type,account_id,group_id,payload)
 SELECT $11,updated.id,NULL,NULL FROM updated
 `, service.OpenAIBPSCredentialStateExtraKey, payload, service.StatusError, service.OpenAIBPSCredentialErrorMessage(state), expected.ID, service.PlatformOpenAIBPS, service.AccountTypeOAuth, service.StatusActive, string(credentialsJSON), expected.ProxyID, service.SchedulerOutboxEventAccountChanged)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected == 0 {
		return false, err
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, expected.ID)
	return true, nil
}
