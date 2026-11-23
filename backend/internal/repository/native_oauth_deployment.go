package repository

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/MACOS-DO/sub4api/internal/service"
)

var _ service.CoreOAuthRuntimeInstallRepository = (*accountRepository)(nil)

const installCoreOAuthRuntimeAccountSQL = `
WITH updated AS (
 UPDATE accounts a SET credentials=$1::jsonb,extra=$2::jsonb,schedulable=CASE WHEN $8 THEN TRUE ELSE a.schedulable END,updated_at=NOW()
 WHERE a.id=$3 AND a.deleted_at IS NULL AND a.status='active' AND a.platform='openai' AND a.type=$4
   AND a.parent_account_id IS NULL
   AND COALESCE(a.credentials,'{}'::jsonb)=$5::jsonb AND COALESCE(a.extra,'{}'::jsonb)=$6::jsonb
   AND a.schedulable=$9
   AND NOT(COALESCE(a.extra,'{}'::jsonb) ? 'managed_oauth')
   AND NOT(COALESCE(a.credentials,'{}'::jsonb) ?| ARRAY['refresh_token','id_token'])
 RETURNING a.id
)
INSERT INTO scheduler_outbox(event_type,account_id,group_id,payload)
SELECT $7,updated.id,NULL,NULL FROM updated
`

func (r *accountRepository) InstallCoreOAuthRuntime(ctx context.Context, bps, codex service.CoreOAuthRuntimeAccountUpdate, deployment *service.ManagedDeploymentUpdate) (*service.ManagedDeployment, error) {
	if deployment == nil || deployment.Validate() != nil || deployment.ExpectedRouteRevision < 0 || deployment.RouteRevision != deployment.ExpectedRouteRevision+1 {
		return nil, service.ErrManagedDeploymentInvalid
	}
	db, ok := r.sql.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	if !ok {
		return nil, service.ErrCoreOAuthUnavailable
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = installCoreOAuthRuntimeAccountTx(ctx, tx, bps, true); err != nil {
		return nil, err
	}
	if err = installCoreOAuthRuntimeAccountTx(ctx, tx, codex, true); err != nil {
		return nil, err
	}
	result, err := putManagedDeploymentWithExecutor(ctx, tx, deployment)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func installCoreOAuthRuntimeAccountTx(ctx context.Context, tx *sql.Tx, in service.CoreOAuthRuntimeAccountUpdate, activate bool) error {
	if in.Expected == nil || in.Expected.ID <= 0 || in.Expected.Status != service.StatusActive || in.Expected.Platform != service.PlatformOpenAI || in.Expected.ParentAccountID != nil || service.ManagedOAuthDeclared(in.Expected) {
		return service.ErrCoreOAuthConflict
	}
	for _, name := range []string{"refresh_token", "id_token"} {
		if _, present := in.Expected.Credentials[name]; present {
			return service.ErrCoreOAuthConflict
		}
		if _, present := in.Credentials[name]; present {
			return service.ErrCoreOAuthConflict
		}
	}
	next := *in.Expected
	next.Credentials = in.Credentials
	next.Extra = in.Extra
	marker, err := service.CoreOAuthAccountBindingFromAccount(&next)
	if err != nil {
		return err
	}
	shellProvider, shellErr := service.CoreOAuthRuntimeShellProvider(in.Expected)
	if activate {
		if shellErr == nil {
			if shellProvider != marker.Provider || in.Expected.Schedulable {
				return service.ErrCoreOAuthConflict
			}
		} else if !service.CoreOAuthDeclared(in.Expected) || !in.Expected.Schedulable {
			return service.ErrCoreOAuthConflict
		}
	}
	marshal := func(value map[string]any) ([]byte, error) {
		if value == nil {
			value = map[string]any{}
		}
		return json.Marshal(value)
	}
	credentials, err := marshal(in.Credentials)
	if err != nil {
		return err
	}
	extra, err := marshal(in.Extra)
	if err != nil {
		return err
	}
	beforeCredentials, err := marshal(in.Expected.Credentials)
	if err != nil {
		return err
	}
	beforeExtra, err := marshal(in.Expected.Extra)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, installCoreOAuthRuntimeAccountSQL, string(credentials), string(extra), in.Expected.ID, in.Expected.Type, string(beforeCredentials), string(beforeExtra), service.SchedulerOutboxEventAccountChanged, activate, in.Expected.Schedulable)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return service.ErrCoreOAuthConflict
	}
	return nil
}
