package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
)

// ListVisibleFiles uses the original immutable (created_at,id) cursor even
// after retirement or expiry. Cursor authorization is the same as page scope.
func (r *fileRepository) ListVisibleFiles(ctx context.Context, owner service.FileOwner, q service.FileListQuery) (service.FileListPage, error) {
	page := service.FileListPage{Files: make([]*service.StoredFile, 0)}
	if owner.UserID <= 0 || owner.APIKeyID <= 0 || owner.GroupID <= 0 || q.Limit < 1 || q.Limit > 10000 || (q.Order != "asc" && q.Order != "desc") {
		return page, service.ErrInvalidFile
	}
	deploymentID, binding := "", "{}"
	if session, managed := service.ManagedDeploymentSessionFromContext(ctx); managed {
		if session.UserID != owner.UserID || session.APIKeyID != owner.APIKeyID || session.GroupID != owner.GroupID || session.DeploymentID == "" || session.Binding.Validate() != nil {
			return page, service.ErrManagedDeploymentConflict
		}
		raw, err := json.Marshal(session.Binding)
		if err != nil {
			return page, err
		}
		deploymentID, binding = session.DeploymentID, string(raw)
	}
	var cursorTime time.Time
	var cursorID int64
	if q.After != "" {
		err := r.db.QueryRowContext(ctx, `SELECT created_at,id FROM gateway_files
 WHERE public_id=$1 AND user_id=$2 AND group_id=$3 AND ($4='' OR purpose=$4) AND api_key_id=$5
 AND ($6='' OR EXISTS(SELECT 1 FROM managed_deployment_resources r
 JOIN managed_deployment_sessions s USING(deployment_id,user_id,api_key_id,session_key)
 JOIN managed_deployments d ON d.deployment_id=s.deployment_id
 WHERE r.deployment_id=$6 AND d.group_id=gateway_files.group_id
 AND r.user_id=gateway_files.user_id AND r.api_key_id=gateway_files.api_key_id
 AND r.resource_kind='file' AND r.resource_hash=encode(sha256(convert_to(gateway_files.public_id,'UTF8')),'hex')
 AND s.binding=$7::jsonb))`,
			q.After, owner.UserID, owner.GroupID, q.Purpose, owner.APIKeyID, deploymentID, binding).Scan(&cursorTime, &cursorID)
		if errors.Is(err, sql.ErrNoRows) {
			return page, service.ErrFileInvalidCursor
		}
		if err != nil {
			return page, err
		}
	}
	// Direction is chosen only from these two constant branches, never from
	// caller-provided SQL. All expiry checks share this statement's DB clock.
	comparison, direction := "<", "DESC"
	if q.Order == "asc" {
		comparison, direction = ">", "ASC"
	}
	rows, err := r.db.QueryContext(ctx, `WITH bounds AS MATERIALIZED (SELECT statement_timestamp() AS as_of)
 SELECT `+fileColumns+` FROM gateway_files CROSS JOIN bounds
 WHERE user_id=$1 AND group_id=$2 AND api_key_id=$8 AND state='ready'
 AND ($9='' OR EXISTS(SELECT 1 FROM managed_deployment_resources r
 JOIN managed_deployment_sessions s USING(deployment_id,user_id,api_key_id,session_key)
 JOIN managed_deployments d ON d.deployment_id=s.deployment_id
 WHERE r.deployment_id=$9 AND d.group_id=gateway_files.group_id
 AND r.user_id=gateway_files.user_id AND r.api_key_id=gateway_files.api_key_id
 AND r.resource_kind='file' AND r.resource_hash=encode(sha256(convert_to(gateway_files.public_id,'UTF8')),'hex')
 AND s.binding=$10::jsonb))
 AND (expires_at IS NULL OR expires_at>bounds.as_of) AND ($3='' OR purpose=$3)
 AND ($4='' OR (created_at,id) `+comparison+` ($5::timestamptz,$6::bigint))
 ORDER BY created_at `+direction+`,id `+direction+` LIMIT $7`,
		owner.UserID, owner.GroupID, q.Purpose, q.After, cursorTime, cursorID, q.Limit+1, owner.APIKeyID, deploymentID, binding)
	if err != nil {
		return page, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		file, err := scanStoredFile(rows)
		if err != nil {
			return page, err
		}
		page.Files = append(page.Files, file)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Files) > q.Limit {
		page.HasMore = true
		page.Files = page.Files[:q.Limit]
	}
	return page, nil
}
