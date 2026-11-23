package repository

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/lib/pq"
)

var _ service.ManagedDeploymentRepository = (*accountRepository)(nil)

const managedDeploymentSelect = `SELECT deployment_id,group_id,route_revision,selected_provider,bps_binding,codex_binding FROM managed_deployments `

func (r *accountRepository) readManagedDeployment(ctx context.Context, clause string, value any) (*service.ManagedDeployment, error) {
	rows, e := r.sql.QueryContext(ctx, managedDeploymentSelect+clause, value)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	d := &service.ManagedDeployment{Contract: service.ManagedDeploymentContract}
	var b, c []byte
	if e = rows.Scan(&d.DeploymentID, &d.GroupID, &d.RouteRevision, &d.SelectedProvider, &b, &c); e != nil {
		return nil, e
	}
	if json.Unmarshal(b, &d.BPS) != nil || json.Unmarshal(c, &d.Codex) != nil || d.Validate() != nil {
		return nil, service.ErrManagedDeploymentConflict
	}
	return d, rows.Err()
}
func (r *accountRepository) GetManagedDeployment(ctx context.Context, id string) (*service.ManagedDeployment, error) {
	return r.readManagedDeployment(ctx, " WHERE deployment_id=$1", id)
}
func (r *accountRepository) GetManagedDeploymentForGroup(ctx context.Context, id int64) (*service.ManagedDeployment, error) {
	return r.readManagedDeployment(ctx, " WHERE group_id=$1", id)
}

const putManagedDeploymentSQL = `
INSERT INTO managed_deployments(deployment_id,group_id,route_revision,selected_provider,bps_binding,codex_binding)
SELECT $1,$2,$3,$4,$5::jsonb,$6::jsonb FROM groups g
WHERE g.id=$2 AND g.deleted_at IS NULL AND g.platform='openai'
 AND EXISTS(SELECT 1 FROM account_groups ag JOIN accounts a ON a.id=ag.account_id WHERE ag.group_id=g.id AND a.id=$7 AND a.deleted_at IS NULL AND a.platform='openai' AND a.type='apikey' AND a.extra->'managed_deployment_binding'=($5::jsonb-'account_id'))
 AND EXISTS(SELECT 1 FROM account_groups ag JOIN accounts a ON a.id=ag.account_id WHERE ag.group_id=g.id AND a.id=$8 AND a.deleted_at IS NULL AND a.platform='openai' AND a.type='oauth'
  AND ((NOT(COALESCE(a.extra,'{}'::jsonb) ? 'core_oauth') AND a.extra->'managed_oauth' @> (($6::jsonb-'account_id') || jsonb_build_object('contract',$10::text)))
   OR (NOT(COALESCE(a.extra,'{}'::jsonb) ? 'managed_oauth') AND NOT(COALESCE(a.credentials,'{}'::jsonb) ?| ARRAY['refresh_token','id_token'])
    AND a.extra->'core_oauth' @> jsonb_build_object('contract','sub6-core-oauth/v1','provider','codex','profile_ref',substring($6::jsonb->>'profile_ref' from 7),'subject',$6::jsonb->>'subject','workspace_id',$6::jsonb->>'workspace_id','identity_epoch',$6::jsonb->>'identity_epoch')
    AND a.extra->'managed_deployment_binding'=($6::jsonb-'account_id'))))
 AND EXISTS(SELECT 1 FROM accounts selected WHERE selected.id=CASE $4::text WHEN 'bps_excel' THEN $7::bigint ELSE $8::bigint END AND selected.deleted_at IS NULL AND selected.status='active' AND selected.schedulable=TRUE AND selected.parent_account_id IS NULL FOR SHARE)
 AND ($9::bigint=0 OR EXISTS(SELECT 1 FROM managed_deployments WHERE deployment_id=$1))
ON CONFLICT(deployment_id) DO UPDATE SET route_revision=EXCLUDED.route_revision,selected_provider=EXCLUDED.selected_provider,updated_at=NOW()
WHERE managed_deployments.group_id=EXCLUDED.group_id AND managed_deployments.bps_binding=EXCLUDED.bps_binding AND managed_deployments.codex_binding=EXCLUDED.codex_binding
 AND (managed_deployments.route_revision=$9 OR (managed_deployments.route_revision=EXCLUDED.route_revision AND managed_deployments.selected_provider=EXCLUDED.selected_provider))
RETURNING deployment_id,group_id,route_revision,selected_provider,bps_binding,codex_binding`

func (r *accountRepository) PutManagedDeployment(ctx context.Context, in *service.ManagedDeploymentUpdate) (*service.ManagedDeployment, error) {
	return putManagedDeploymentWithExecutor(ctx, r.sql, in)
}

func putManagedDeploymentWithExecutor(ctx context.Context, executor sqlExecutor, in *service.ManagedDeploymentUpdate) (*service.ManagedDeployment, error) {
	b, e := json.Marshal(in.BPS)
	if e != nil {
		return nil, e
	}
	c, e := json.Marshal(in.Codex)
	if e != nil {
		return nil, e
	}

	rows, e := executor.QueryContext(ctx, putManagedDeploymentSQL, in.DeploymentID, in.GroupID, in.RouteRevision, in.SelectedProvider, string(b), string(c), in.BPS.AccountID, in.Codex.AccountID, in.ExpectedRouteRevision, service.ManagedOAuthContract)
	if e != nil {
		var pe *pq.Error
		if errors.As(e, &pe) && pe.Code == "23505" {
			return nil, service.ErrManagedDeploymentConflict
		}
		return nil, e
	}
	defer rows.Close()
	if !rows.Next() {
		if e = rows.Err(); e != nil {
			return nil, e
		}
		return nil, service.ErrManagedDeploymentConflict
	}
	d := &service.ManagedDeployment{Contract: service.ManagedDeploymentContract}
	var bJSON, cJSON []byte
	if e = rows.Scan(&d.DeploymentID, &d.GroupID, &d.RouteRevision, &d.SelectedProvider, &bJSON, &cJSON); e != nil {
		return nil, e
	}
	if json.Unmarshal(bJSON, &d.BPS) != nil || json.Unmarshal(cJSON, &d.Codex) != nil || d.Validate() != nil {
		return nil, service.ErrManagedDeploymentConflict
	}
	return d, rows.Err()
}

const bindManagedDeploymentSessionSQL = `
WITH d AS MATERIALIZED(SELECT * FROM managed_deployments WHERE group_id=$1 FOR SHARE),
 inserted AS (
 INSERT INTO managed_deployment_sessions(deployment_id,user_id,api_key_id,session_key,route_revision,binding)
 SELECT d.deployment_id,$2,$3,$4,d.route_revision,CASE d.selected_provider WHEN 'bps_excel' THEN d.bps_binding ELSE d.codex_binding END FROM d
 WHERE NOT $5::boolean
 ON CONFLICT(deployment_id,user_id,api_key_id,session_key) DO UPDATE SET session_key=EXCLUDED.session_key
 RETURNING deployment_id,route_revision,binding
 )
 SELECT deployment_id,route_revision,binding FROM inserted
 UNION ALL
 SELECT s.deployment_id,s.route_revision,s.binding FROM managed_deployment_sessions s JOIN d ON d.deployment_id=s.deployment_id
 WHERE $5::boolean AND s.user_id=$2 AND s.api_key_id=$3 AND s.session_key=$4`

func (r *accountRepository) BindManagedDeploymentSession(ctx context.Context, group, user, key int64, session string, existingOnly bool) (*service.ManagedDeploymentSession, error) {
	rows, e := r.sql.QueryContext(ctx, bindManagedDeploymentSessionSQL, group, user, key, session, existingOnly)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	if !rows.Next() {
		if e = rows.Err(); e != nil {
			return nil, e
		}
		return nil, service.ErrManagedDeploymentContext
	}
	s := &service.ManagedDeploymentSession{GroupID: group, UserID: user, APIKeyID: key, SessionKey: session}
	var raw []byte
	if e = rows.Scan(&s.DeploymentID, &s.RouteRevision, &raw); e != nil {
		return nil, e
	}
	if json.Unmarshal(raw, &s.Binding) != nil || s.Binding.Validate() != nil {
		return nil, service.ErrManagedDeploymentConflict
	}
	return s, rows.Err()
}

const bindManagedDeploymentResourceSQL = `
INSERT INTO managed_deployment_resources(deployment_id,user_id,api_key_id,resource_kind,resource_hash,session_key) VALUES($1,$2,$3,$4,$5,$6)
ON CONFLICT(deployment_id,user_id,api_key_id,resource_kind,resource_hash)
DO UPDATE SET session_key=managed_deployment_resources.session_key
WHERE EXISTS(SELECT 1 FROM managed_deployment_sessions original
 JOIN managed_deployment_sessions requested ON original.deployment_id=requested.deployment_id AND original.user_id=requested.user_id AND original.api_key_id=requested.api_key_id AND original.binding=requested.binding
 WHERE original.deployment_id=EXCLUDED.deployment_id AND original.user_id=EXCLUDED.user_id AND original.api_key_id=EXCLUDED.api_key_id AND original.session_key=managed_deployment_resources.session_key AND requested.session_key=EXCLUDED.session_key)
`

func (r *accountRepository) BindManagedDeploymentResource(ctx context.Context, s *service.ManagedDeploymentSession, kind, id string) error {
	if s == nil || id == "" {
		return service.ErrManagedDeploymentInvalid
	}
	result, e := r.sql.ExecContext(ctx, bindManagedDeploymentResourceSQL, s.DeploymentID, s.UserID, s.APIKeyID, kind, service.ManagedDeploymentOwnerHash(id), s.SessionKey)
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return service.ErrManagedDeploymentConflict
	}
	return nil
}
func (r *accountRepository) CheckManagedDeploymentResource(ctx context.Context, s *service.ManagedDeploymentSession, kind, id string) (bool, error) {
	if s == nil {
		return false, service.ErrManagedDeploymentInvalid
	}
	owner, e := r.ResolveManagedDeploymentResource(ctx, s.GroupID, s.UserID, s.APIKeyID, kind, id)
	if e != nil {
		return false, e
	}
	return owner != nil && owner.DeploymentID == s.DeploymentID && owner.Binding == s.Binding, nil
}

const resolveManagedDeploymentResourceSQL = `
 SELECT s.deployment_id,s.route_revision,s.session_key,s.binding
 FROM managed_deployment_resources r
 JOIN managed_deployment_sessions s USING(deployment_id,user_id,api_key_id,session_key)
 JOIN managed_deployments d ON d.deployment_id=s.deployment_id
 WHERE d.group_id=$1 AND r.user_id=$2 AND r.api_key_id=$3 AND r.resource_kind=$4 AND r.resource_hash=$5`

func (r *accountRepository) ResolveManagedDeploymentResource(ctx context.Context, group, user, key int64, kind, id string) (*service.ManagedDeploymentSession, error) {
	rows, e := r.sql.QueryContext(ctx, resolveManagedDeploymentResourceSQL, group, user, key, kind, service.ManagedDeploymentOwnerHash(id))
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	if !rows.Next() {
		if e = rows.Err(); e != nil {
			return nil, e
		}
		return nil, service.ErrManagedDeploymentContext
	}
	s := &service.ManagedDeploymentSession{GroupID: group, UserID: user, APIKeyID: key}
	var raw []byte
	if e = rows.Scan(&s.DeploymentID, &s.RouteRevision, &s.SessionKey, &raw); e != nil {
		return nil, e
	}
	if json.Unmarshal(raw, &s.Binding) != nil || s.Binding.Validate() != nil {
		return nil, service.ErrManagedDeploymentConflict
	}
	return s, rows.Err()
}
