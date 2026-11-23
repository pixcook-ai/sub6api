package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
)

type storedResponseRepository struct{ db *sql.DB }

func NewStoredResponseRepository(db *sql.DB) service.StoredResponseRepository {
	return &storedResponseRepository{db: db}
}

const storedResponseColumns = `public_id,user_id,api_key_id,group_id,model,resolved_model,account_id,binding,
 created_at,expires_at,state,execution_state,normalized_input,effective_parameters,public_response,
 content_bytes,reserved_bytes,COALESCE(lease_token,''),lease_until,version,terminal_hash,deleted_at,recovery_version,private_recovery,COALESCE(previous_response_id,'')`

type storedResponseScanner interface{ Scan(...any) error }

func scanStoredResponse(row storedResponseScanner) (*service.StoredResponse, error) {
	r := &service.StoredResponse{}
	var lease sql.NullTime
	var input, parameters, response, recovery []byte
	err := row.Scan(&r.PublicID, &r.UserID, &r.APIKeyID, &r.GroupID, &r.Model, &r.ResolvedModel, &r.AccountID, &r.Binding,
		&r.CreatedAt, &r.ExpiresAt, &r.State, &r.ExecutionState, &input, &parameters, &response,
		&r.ContentBytes, &r.ReservedBytes, &r.LeaseToken, &lease, &r.Version, &r.TerminalHash, &r.DeletedAt, &r.RecoveryVersion, &recovery, &r.PreviousResponseID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrStoredResponseNotFound
	}
	if err != nil {
		return nil, err
	}
	r.Input, r.Parameters, r.PublicResponse, r.Recovery = input, parameters, response, recovery
	for _, field := range []*json.RawMessage{&r.Input, &r.Parameters, &r.PublicResponse, &r.Recovery} {
		if len(*field) > 0 {
			value, e := canonicalStoredJSON(*field)
			if e != nil {
				return nil, e
			}
			*field = value
		}
	}
	if lease.Valid {
		r.LeaseUntil = lease.Time
	}
	return r, nil
}

func storedResponseIDValid(id string) bool {
	if len(id) != 43 || !strings.HasPrefix(id, "resp_local_") {
		return false
	}
	for _, c := range id[11:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func validStoredLimits(v service.StoredResponseLimits) bool {
	return v.MaxResponseBytes > 0 && v.MaxResponseBytes <= 64<<20 && v.UserMaxRows > 0 && v.GlobalMaxRows >= v.UserMaxRows &&
		v.UserMaxBytes >= v.MaxResponseBytes && v.GlobalMaxBytes >= v.UserMaxBytes
}

// Canonicalization makes terminal hashes stable across JSONB round trips. Reject
// decoded duplicate keys before map decoding, including escaped key spellings.
func canonicalStoredJSON(raw []byte) ([]byte, error) {
	return service.CanonicalStoredResponseJSON15(raw)
}

func storedHash(state, execution string, payload []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(state))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(execution))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))
}
func storedTokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func lockStoredGlobal(ctx context.Context, tx *sql.Tx) error {
	var owner int64
	return tx.QueryRowContext(ctx, `SELECT owner_id FROM gateway_stored_response_budgets WHERE owner_id=0 FOR UPDATE`).Scan(&owner)
}
func lockStoredOwner(ctx context.Context, tx *sql.Tx, userID int64) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_stored_response_budgets(owner_id) VALUES($1) ON CONFLICT DO NOTHING`, userID); err != nil {
		return err
	}
	var owner int64
	return tx.QueryRowContext(ctx, `SELECT owner_id FROM gateway_stored_response_budgets WHERE owner_id=$1 FOR UPDATE`, userID).Scan(&owner)
}
func storedBudgetDelta(ctx context.Context, tx *sql.Tx, userID, rowDelta, byteDelta int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE gateway_stored_response_budgets SET rows_count=rows_count+$2,reserved_bytes=reserved_bytes+$3 WHERE owner_id IN (0,$1)`, userID, rowDelta, byteDelta)
	if err != nil {
		return err
	}
	if rowDelta < 0 {
		_, err = tx.ExecContext(ctx, `DELETE FROM gateway_stored_response_budgets WHERE owner_id=$1 AND rows_count=0 AND reserved_bytes=0`, userID)
	}
	return err
}
func storedTx(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	if db == nil {
		return nil, service.ErrStoredResponseInvalid
	}
	return db.BeginTx(ctx, nil)
}

func (r *storedResponseRepository) Reserve(ctx context.Context, record *service.StoredResponse, limits service.StoredResponseLimits, retention, lease time.Duration) error {
	if record == nil || !storedResponseIDValid(record.PublicID) || record.UserID <= 0 || record.APIKeyID <= 0 || record.GroupID <= 0 || record.AccountID <= 0 || record.Model == "" || len(record.Model) > 255 || len(record.ResolvedModel) > 255 || record.Binding == "" || len(record.Binding) > 1024 || (record.PreviousResponseID != "" && !storedResponseIDValid(record.PreviousResponseID)) || record.RecoveryVersion < 0 || record.RecoveryVersion > 2 || len(record.Recovery) != 0 || record.LeaseToken == "" || len(record.LeaseToken) > 128 || !validStoredLimits(limits) || retention < 24*time.Hour || retention > 30*24*time.Hour || retention%(24*time.Hour) != 0 || lease <= 0 || lease > time.Hour {
		return service.ErrStoredResponseInvalid
	}
	if len(record.Input) > int(limits.MaxResponseBytes) || len(record.Parameters) > int(limits.MaxResponseBytes) {
		return service.ErrStoredResponseTooLarge
	}
	input, err := canonicalStoredJSON(record.Input)
	if err != nil {
		return err
	}
	params, err := canonicalStoredJSON(record.Parameters)
	if err != nil {
		return err
	}
	var parameters map[string]json.RawMessage
	if json.Unmarshal(params, &parameters) != nil || parameters == nil {
		return service.ErrStoredResponseInvalid
	}
	for key := range parameters {
		if key == "model" || key == "input" || key == "store" {
			return service.ErrStoredResponseInvalid
		}
	}
	parameters["model"], _ = json.Marshal(record.Model)
	parameters["input"] = input
	parameters["store"] = json.RawMessage("true")
	normalizedRequest, err := json.Marshal(parameters)
	if err != nil {
		return service.ErrStoredResponseInvalid
	}
	if record.RecoveryVersion == 2 {
		_, err = service.NormalizeStoredResponseNativeRequest15(normalizedRequest)
	} else {
		_, err = service.NormalizeStoredResponseRequest(normalizedRequest)
	}
	if err != nil {
		return err
	}

	if len(input)+len(params) > int(limits.MaxResponseBytes) {
		return service.ErrStoredResponseTooLarge
	}
	tx, err := storedTx(ctx, r.db)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = lockStoredGlobal(ctx, tx); err != nil {
		return err
	}
	if err = lockStoredOwner(ctx, tx, record.UserID); err != nil {
		return err
	}
	if record.PreviousResponseID != "" {
		if record.RecoveryVersion != 1 && record.RecoveryVersion != 2 {
			return service.ErrStoredResponseInvalid
		}
		id := record.PreviousResponseID
		seen := make(map[string]bool)
		for depth := 1; id != ""; depth++ {
			if depth >= 32 || seen[id] {
				return service.ErrStoredResponseTooLarge
			}
			seen[id] = true
			var parent string
			err = tx.QueryRowContext(ctx, `SELECT COALESCE(previous_response_id,'') FROM gateway_stored_responses WHERE public_id=$1 AND user_id=$2 AND group_id=$3 AND model=$4 AND resolved_model=$5 AND account_id=$6 AND state='completed' AND execution_state='terminal' AND recovery_version=$7 AND api_key_id=$8 AND private_recovery IS NOT NULL AND expires_at>clock_timestamp()`, id, record.UserID, record.GroupID, record.Model, record.ResolvedModel, record.AccountID, record.RecoveryVersion, record.APIKeyID).Scan(&parent)
			if errors.Is(err, sql.ErrNoRows) {
				return service.ErrStoredResponseNotFound
			}
			if err != nil {
				return err
			}
			id = parent
		}
	}
	if record.RecoveryVersion == 2 {
		var parent *service.StoredResponse
		if record.PreviousResponseID != "" {
			parent, err = scanStoredResponse(tx.QueryRowContext(ctx, `SELECT `+storedResponseColumns+` FROM gateway_stored_responses WHERE public_id=$1`, record.PreviousResponseID))
			if err != nil {
				return err
			}
		}
		parentNow, err := storedRecordTime(ctx, tx)
		if err != nil {
			return err
		}
		if err = service.ValidateStoredResponseNativeReservationParent15(record, parent, parentNow, limits.MaxResponseBytes); err != nil {
			return err
		}
	}
	var globalRows, globalBytes, userRows, userBytes int64
	if err = tx.QueryRowContext(ctx, `SELECT rows_count,reserved_bytes FROM gateway_stored_response_budgets WHERE owner_id=0`).Scan(&globalRows, &globalBytes); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, `SELECT rows_count,reserved_bytes FROM gateway_stored_response_budgets WHERE owner_id=$1`, record.UserID).Scan(&userRows, &userBytes); err != nil {
		return err
	}
	if globalRows >= limits.GlobalMaxRows || userRows >= limits.UserMaxRows || globalBytes > limits.GlobalMaxBytes-limits.MaxResponseBytes || userBytes > limits.UserMaxBytes-limits.MaxResponseBytes {
		return service.ErrStoredResponseQuota
	}
	var now time.Time
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	snapshotRecord := *record
	snapshotRecord.CreatedAt, snapshotRecord.Parameters = now, params
	skeleton, err := service.StoredResponseMinimumSnapshot(&snapshotRecord, false)
	if err != nil {
		return err
	}
	size := int64(len(input) + len(params) + len(skeleton))
	recovery, err := service.StoredResponseMinimumSnapshot(&snapshotRecord, true)
	if err != nil {
		return err
	}
	if size > limits.MaxResponseBytes || int64(len(input)+len(params)+len(recovery)) > limits.MaxResponseBytes {
		return service.ErrStoredResponseTooLarge
	}
	if record.RecoveryVersion == 1 {
		minimumPrivate, err := service.BuildStoredResponseTextRecovery15(input, []byte(`{"object":"response","status":"completed","output":[]}`), limits.MaxResponseBytes)
		if err != nil {
			return err
		}
		if size+int64(len(minimumPrivate)) > limits.MaxResponseBytes {
			return service.ErrStoredResponseTooLarge
		}
	}
	if record.RecoveryVersion == 2 {
		minimumRecord := *record
		minimumRecord.Input, minimumRecord.Parameters = input, params
		minimumPrivate, err := service.ValidateStoredResponseNativeReservation15(&minimumRecord, now, limits.MaxResponseBytes)
		if err != nil {
			return err
		}
		if size+int64(len(minimumPrivate)) > limits.MaxResponseBytes {
			return service.ErrStoredResponseTooLarge
		}
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO gateway_stored_responses(public_id,user_id,api_key_id,group_id,model,resolved_model,account_id,binding,created_at,expires_at,state,execution_state,normalized_input,effective_parameters,public_response,content_bytes,reserved_bytes,lease_token,lease_until,version,recovery_version,previous_response_id)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::timestamptz,$9::timestamptz+make_interval(secs=>$10::double precision),'in_progress','not_sent',$11::jsonb,$12::jsonb,$13::jsonb,$14,$15,$16,$9::timestamptz+make_interval(secs=>$17::double precision),1,$18,$19) RETURNING created_at,expires_at,lease_until,version`, record.PublicID, record.UserID, record.APIKeyID, record.GroupID, record.Model, record.ResolvedModel, record.AccountID, record.Binding, now, retention.Seconds(), string(input), string(params), string(skeleton), size, limits.MaxResponseBytes, record.LeaseToken, lease.Seconds(), record.RecoveryVersion, nullableRecovery15([]byte(record.PreviousResponseID))).Scan(&record.CreatedAt, &record.ExpiresAt, &record.LeaseUntil, &record.Version)
	if err != nil {
		return err
	}
	if err = storedBudgetDelta(ctx, tx, record.UserID, 1, limits.MaxResponseBytes); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	record.Input, record.Parameters, record.PublicResponse = input, params, skeleton
	record.State = "in_progress"
	record.ExecutionState = "not_sent"
	record.ContentBytes = size
	record.ReservedBytes = limits.MaxResponseBytes
	return nil
}

func (r *storedResponseRepository) GetForOwner(ctx context.Context, userID, groupID int64, id string) (*service.StoredResponse, error) {
	if r.db == nil {
		return nil, service.ErrStoredResponseInvalid
	}
	if userID <= 0 || groupID <= 0 || !storedResponseIDValid(id) {
		return nil, service.ErrStoredResponseNotFound
	}
	return scanStoredResponse(r.db.QueryRowContext(ctx, `SELECT `+storedResponseColumns+` FROM gateway_stored_responses WHERE public_id=$1 AND user_id=$2 AND group_id=$3 AND state<>'deleted' AND expires_at>clock_timestamp()`, id, userID, groupID))
}

func (r *storedResponseRepository) GetForKeyOwner(ctx context.Context, userID, apiKeyID, groupID int64, id string) (*service.StoredResponse, error) {
	if userID <= 0 || apiKeyID <= 0 || groupID <= 0 || !storedResponseIDValid(id) {
		return nil, service.ErrStoredResponseNotFound
	}
	if r == nil || r.db == nil {
		return nil, errors.New("stored response persistence unavailable")
	}
	return scanStoredResponse(r.db.QueryRowContext(ctx, `SELECT `+storedResponseColumns+` FROM gateway_stored_responses WHERE public_id=$1 AND user_id=$2 AND api_key_id=$3 AND group_id=$4 AND state<>'deleted' AND expires_at>clock_timestamp()`, id, userID, apiKeyID, groupID))
}

// GetManagementModel deliberately includes deleted tombstones. It retains the
// authorization boundary for idempotent DELETE without exposing response content.
func (r *storedResponseRepository) GetManagementModel(ctx context.Context, userID, groupID int64, id string) (string, error) {
	if userID <= 0 || groupID <= 0 || !storedResponseIDValid(id) {
		return "", service.ErrStoredResponseNotFound
	}
	if r == nil || r.db == nil {
		return "", errors.New("stored response persistence unavailable")
	}
	var model string
	err := r.db.QueryRowContext(ctx, `SELECT model FROM gateway_stored_responses WHERE public_id=$1 AND user_id=$2 AND group_id=$3 AND expires_at>clock_timestamp()`, id, userID, groupID).Scan(&model)
	if errors.Is(err, sql.ErrNoRows) {
		return "", service.ErrStoredResponseNotFound
	}
	if err != nil {
		return "", err
	}
	return model, nil
}

// Deleted tombstones keep the original key boundary for repeated DELETE.
func (r *storedResponseRepository) GetManagementModelForKeyOwner(ctx context.Context, userID, apiKeyID, groupID int64, id string) (string, error) {
	if userID <= 0 || apiKeyID <= 0 || groupID <= 0 || !storedResponseIDValid(id) {
		return "", service.ErrStoredResponseNotFound
	}
	if r == nil || r.db == nil {
		return "", errors.New("stored response persistence unavailable")
	}
	var model string
	err := r.db.QueryRowContext(ctx, `SELECT model FROM gateway_stored_responses WHERE public_id=$1 AND user_id=$2 AND api_key_id=$3 AND group_id=$4 AND expires_at>clock_timestamp()`, id, userID, apiKeyID, groupID).Scan(&model)
	if errors.Is(err, sql.ErrNoRows) {
		return "", service.ErrStoredResponseNotFound
	}
	return model, err
}

// lockRecord takes the global budget before an owner or response row, matching
// Reserve/Delete/Sweep. The unlocked owner lookup is safe while global is held.
func lockStoredRecord(ctx context.Context, tx *sql.Tx, id string) (*service.StoredResponse, error) {
	if err := lockStoredGlobal(ctx, tx); err != nil {
		return nil, err
	}
	var userID int64
	if err := tx.QueryRowContext(ctx, `SELECT user_id FROM gateway_stored_responses WHERE public_id=$1`, id).Scan(&userID); errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrStoredResponseNotFound
	} else if err != nil {
		return nil, err
	}
	if err := lockStoredOwner(ctx, tx, userID); err != nil {
		return nil, err
	}
	return scanStoredResponse(tx.QueryRowContext(ctx, `SELECT `+storedResponseColumns+` FROM gateway_stored_responses WHERE public_id=$1 FOR UPDATE`, id))
}

func storedRecordTime(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	var now time.Time
	err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now)
	return now, err
}

func (r *storedResponseRepository) Finalize(ctx context.Context, id, token string, version int64, terminal service.StoredResponseTerminal) (*service.StoredResponse, error) {
	if !storedResponseIDValid(id) || token == "" || version <= 0 {
		return nil, service.ErrStoredResponseLease
	}
	if terminal.State != "completed" && terminal.State != "incomplete" && terminal.State != "failed" {
		return nil, service.ErrStoredResponseInvalid
	}
	if terminal.ExecutionState != "terminal" && terminal.ExecutionState != "unknown" {
		return nil, service.ErrStoredResponseInvalid
	}
	if len(terminal.PublicResponse) > 64<<20 || len(terminal.Recovery) > 64<<20 {
		return nil, service.ErrStoredResponseTooLarge
	}
	payload, err := canonicalStoredJSON(terminal.PublicResponse)
	if err != nil {
		return nil, err
	}
	var obj map[string]json.RawMessage
	if err = json.Unmarshal(payload, &obj); err != nil || obj == nil {
		return nil, service.ErrStoredResponseInvalid
	}
	var pid, state, kind string
	var store bool
	if json.Unmarshal(obj["id"], &pid) != nil || json.Unmarshal(obj["status"], &state) != nil || json.Unmarshal(obj["object"], &kind) != nil || json.Unmarshal(obj["store"], &store) != nil || pid != id || state != terminal.State || kind != "response" || !store {
		return nil, service.ErrStoredResponseInvalid
	}
	recovery := []byte(nil)
	if len(terminal.Recovery) != 0 {
		recovery, err = canonicalStoredJSON(terminal.Recovery)
		if err != nil {
			return nil, err
		}
	}
	hashPayload := payload
	if len(recovery) != 0 {
		hashPayload = append(append(append([]byte(nil), payload...), 0), recovery...)
	}
	hash := storedHash(terminal.State, terminal.ExecutionState, hashPayload)
	if terminal.TerminalHash != "" && terminal.TerminalHash != hash {
		return nil, service.ErrStoredResponseInvalid
	}
	tx, err := storedTx(ctx, r.db)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	record, err := lockStoredRecord(ctx, tx, id)
	if errors.Is(err, service.ErrStoredResponseNotFound) {
		return nil, service.ErrStoredResponseLease
	}
	if err != nil {
		return nil, err
	}
	now, err := storedRecordTime(ctx, tx)
	if err != nil {
		return nil, err
	}
	var responseModel string
	var responseCreated int64
	if json.Unmarshal(obj["model"], &responseModel) != nil || json.Unmarshal(obj["created_at"], &responseCreated) != nil || responseModel != record.Model || responseCreated != record.CreatedAt.Unix() {
		return nil, service.ErrStoredResponseInvalid
	}
	if record.RecoveryVersion != 0 && terminal.State == "completed" {
		if terminal.ExecutionState != "terminal" {
			return nil, service.ErrStoredResponseInvalid
		}
		switch record.RecoveryVersion {
		case 1:
			err = service.ValidateStoredResponseTextRecovery15(record.Input, payload, recovery, record.ReservedBytes)
		case 2:
			err = service.ValidateStoredResponseNativeRecovery15(record, payload, recovery, record.ReservedBytes)
		default:
			err = service.ErrStoredResponseInvalid
		}
		if err != nil {
			return nil, err
		}
	} else if len(recovery) != 0 {
		return nil, service.ErrStoredResponseInvalid
	}
	if !record.ExpiresAt.After(now) || record.State == "deleted" {
		return nil, service.ErrStoredResponseLease
	}
	if record.State != "in_progress" {
		var digest string
		if err = tx.QueryRowContext(ctx, `SELECT finalized_token_hash FROM gateway_stored_responses WHERE public_id=$1`, id).Scan(&digest); err != nil {
			return nil, err
		}
		if record.Version == version+1 && record.TerminalHash == hash && digest == storedTokenHash(token) {
			return record, tx.Commit()
		}
		return nil, service.ErrStoredResponseConflict
	}
	if record.Version != version || record.LeaseToken != token || !record.LeaseUntil.After(now) {
		return nil, service.ErrStoredResponseLease
	}
	if record.RecoveryVersion == 2 && terminal.State == "completed" {
		var parent *service.StoredResponse
		if record.PreviousResponseID != "" {
			parent, err = scanStoredResponse(tx.QueryRowContext(ctx, `SELECT `+storedResponseColumns+` FROM gateway_stored_responses WHERE public_id=$1`, record.PreviousResponseID))
			if err != nil {
				return nil, err
			}
		}
		if err = service.ValidateStoredResponseNativeRecoveryParent15(record, payload, recovery, parent, now, record.ReservedBytes); err != nil {
			return nil, err
		}
	}
	size := int64(len(record.Input) + len(record.Parameters) + len(payload) + len(recovery))
	if size > record.ReservedBytes {
		return nil, service.ErrStoredResponseTooLarge
	}
	updated, err := tx.ExecContext(ctx, `UPDATE gateway_stored_responses SET state=$2,execution_state=$3,public_response=$4::jsonb,content_bytes=$5,reserved_bytes=$5,lease_token=NULL,lease_until=NULL,version=version+1,terminal_hash=$6,finalized_token_hash=$7,private_recovery=$10::jsonb WHERE public_id=$1 AND state='in_progress' AND lease_token=$8 AND version=$9 AND lease_until>clock_timestamp() AND expires_at>clock_timestamp() AND ($2<>'completed' OR previous_response_id IS NULL OR EXISTS (SELECT 1 FROM gateway_stored_responses AS parent WHERE parent.public_id=gateway_stored_responses.previous_response_id AND parent.user_id=gateway_stored_responses.user_id AND parent.api_key_id=gateway_stored_responses.api_key_id AND parent.group_id=gateway_stored_responses.group_id AND parent.account_id=gateway_stored_responses.account_id AND parent.model=gateway_stored_responses.model AND parent.resolved_model=gateway_stored_responses.resolved_model AND parent.recovery_version=gateway_stored_responses.recovery_version AND parent.state='completed' AND parent.execution_state='terminal' AND parent.private_recovery IS NOT NULL AND parent.expires_at>clock_timestamp()))`, id, terminal.State, terminal.ExecutionState, string(payload), size, hash, storedTokenHash(token), token, version, nullableRecovery15(recovery))
	if err != nil {
		return nil, err
	}
	affected, err := updated.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected != 1 {
		return nil, service.ErrStoredResponseLease
	}
	if err = storedBudgetDelta(ctx, tx, record.UserID, 0, size-record.ReservedBytes); err != nil {
		return nil, err
	}
	result, err := scanStoredResponse(tx.QueryRowContext(ctx, `SELECT `+storedResponseColumns+` FROM gateway_stored_responses WHERE public_id=$1`, id))
	if err != nil {
		return nil, err
	}
	return result, tx.Commit()
}

func (r *storedResponseRepository) Renew(ctx context.Context, id, token string, version int64, lease time.Duration) error {
	if r.db == nil || !storedResponseIDValid(id) || token == "" || version <= 0 || lease <= 0 || lease > time.Hour {
		return service.ErrStoredResponseLease
	}
	// Renewal changes no budget and only holds a response lock, never waits for a
	// budget after it. It cannot form a reverse global->owner->response lock cycle.
	result, err := r.db.ExecContext(ctx, `UPDATE gateway_stored_responses SET lease_until=LEAST(expires_at,clock_timestamp()+make_interval(secs=>$4::double precision)) WHERE public_id=$1 AND state='in_progress' AND lease_token=$2 AND version=$3 AND lease_until>clock_timestamp() AND expires_at>clock_timestamp()`, id, token, version, lease.Seconds())
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return service.ErrStoredResponseLease
	}
	return nil
}

func (r *storedResponseRepository) Delete(ctx context.Context, userID, groupID int64, id string) error {
	return r.deleteForOwner(ctx, userID, 0, groupID, id)
}

func (r *storedResponseRepository) DeleteForKeyOwner(ctx context.Context, userID, apiKeyID, groupID int64, id string) error {
	if apiKeyID <= 0 {
		return service.ErrStoredResponseNotFound
	}
	return r.deleteForOwner(ctx, userID, apiKeyID, groupID, id)
}

func (r *storedResponseRepository) deleteForOwner(ctx context.Context, userID, apiKeyID, groupID int64, id string) error {
	if userID <= 0 || groupID <= 0 || !storedResponseIDValid(id) {
		return service.ErrStoredResponseNotFound
	}
	tx, err := storedTx(ctx, r.db)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	record, err := lockStoredRecord(ctx, tx, id)
	if err != nil {
		return err
	}
	now, err := storedRecordTime(ctx, tx)
	if err != nil {
		return err
	}
	if record.UserID != userID || record.GroupID != groupID || (apiKeyID > 0 && record.APIKeyID != apiKeyID) || !record.ExpiresAt.After(now) {
		return service.ErrStoredResponseNotFound
	}
	if record.State == "deleted" {
		return tx.Commit()
	}
	if record.State == "in_progress" {
		return service.ErrStoredResponseConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE gateway_stored_responses SET state='deleted',private_recovery=NULL,previous_response_id=NULL,normalized_input=NULL,effective_parameters=NULL,public_response=NULL,binding='',account_id=0,content_bytes=0,reserved_bytes=0,lease_token=NULL,lease_until=NULL,terminal_hash='',finalized_token_hash='',deleted_at=clock_timestamp(),version=version+1 WHERE public_id=$1`, id); err != nil {
		return err
	}
	if err = storedBudgetDelta(ctx, tx, record.UserID, 0, -record.ReservedBytes); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *storedResponseRepository) Sweep(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, service.ErrStoredResponseInvalid
	}
	tx, err := storedTx(ctx, r.db)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = lockStoredGlobal(ctx, tx); err != nil {
		return 0, err
	}
	// Other mutators require global first. No response lock is acquired while
	// waiting for it; owners are then locked before their response rows.
	rows, err := tx.QueryContext(ctx, `SELECT public_id,user_id FROM gateway_stored_responses WHERE expires_at<=clock_timestamp() OR (state='in_progress' AND lease_until<=clock_timestamp()) ORDER BY expires_at,public_id LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	type candidate struct {
		id   string
		user int64
	}
	var batch []candidate
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.id, &c.user); err != nil {
			_ = rows.Close()
			return 0, err
		}
		batch = append(batch, c)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()
	processed := 0
	for _, c := range batch {
		if err = lockStoredOwner(ctx, tx, c.user); err != nil {
			return 0, err
		}
		record, e := scanStoredResponse(tx.QueryRowContext(ctx, `SELECT `+storedResponseColumns+` FROM gateway_stored_responses WHERE public_id=$1 FOR UPDATE SKIP LOCKED`, c.id))
		if errors.Is(e, service.ErrStoredResponseNotFound) {
			continue
		}
		if e != nil {
			return 0, e
		}
		now, e := storedRecordTime(ctx, tx)
		if e != nil {
			return 0, e
		}
		if !record.ExpiresAt.After(now) {
			if _, e = tx.ExecContext(ctx, `DELETE FROM gateway_stored_responses WHERE public_id=$1`, c.id); e != nil {
				return 0, e
			}
			if e = storedBudgetDelta(ctx, tx, c.user, -1, -record.ReservedBytes); e != nil {
				return 0, e
			}
			processed++
			continue
		}
		if record.State != "in_progress" || record.LeaseUntil.After(now) {
			continue
		}
		payload, e := service.StoredResponseMinimumSnapshot(record, true)
		if e != nil {
			return 0, e
		}
		size := int64(len(record.Input) + len(record.Parameters) + len(payload))
		input, params := record.Input, record.Parameters
		if size > record.ReservedBytes {
			return 0, service.ErrStoredResponseTooLarge
		}
		if _, e = tx.ExecContext(ctx, `UPDATE gateway_stored_responses SET state='failed',execution_state='unknown',normalized_input=$2::jsonb,effective_parameters=$3::jsonb,public_response=$4::jsonb,content_bytes=$5,reserved_bytes=$5,lease_token=NULL,lease_until=NULL,version=version+1,terminal_hash=$6,finalized_token_hash='' WHERE public_id=$1`, c.id, string(input), string(params), string(payload), size, storedHash("failed", "unknown", payload)); e != nil {
			return 0, e
		}
		if e = storedBudgetDelta(ctx, tx, c.user, 0, size-record.ReservedBytes); e != nil {
			return 0, e
		}
		processed++
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return processed, nil
}

func nullableRecovery15(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}
