package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
)

type openAILoginCredentialRepository struct {
	db *sql.DB
}

func NewOpenAILoginCredentialRepository(db *sql.DB) service.OpenAILoginCredentialRepository {
	return &openAILoginCredentialRepository{db: db}
}

func (r *openAILoginCredentialRepository) Upsert(ctx context.Context, rec *service.OpenAILoginCredentialRecord) error {
	_, err := r.db.ExecContext(ctx, `
INSERT INTO openai_login_credentials (account_id, login_email, target, password_ciphertext, totp_ciphertext)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (account_id) DO UPDATE SET
    login_email = EXCLUDED.login_email,
    target = EXCLUDED.target,
    password_ciphertext = EXCLUDED.password_ciphertext,
    totp_ciphertext = EXCLUDED.totp_ciphertext,
    fail_count = 0,
    next_attempt_at = NULL,
    last_code = '',
    updated_at = NOW()`,
		rec.AccountID, rec.LoginEmail, rec.Target, rec.PasswordCiphertext, rec.TOTPCiphertext)
	return err
}

func (r *openAILoginCredentialRepository) Get(ctx context.Context, accountID int64) (*service.OpenAILoginCredentialRecord, error) {
	rec := &service.OpenAILoginCredentialRecord{}
	var lastAt, nextAt sql.NullTime
	err := r.db.QueryRowContext(ctx, `
SELECT account_id, login_email, target, password_ciphertext, totp_ciphertext, fail_count, last_attempt_at, next_attempt_at, last_code
FROM openai_login_credentials WHERE account_id = $1`, accountID).Scan(
		&rec.AccountID, &rec.LoginEmail, &rec.Target, &rec.PasswordCiphertext, &rec.TOTPCiphertext,
		&rec.FailCount, &lastAt, &nextAt, &rec.LastCode)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if lastAt.Valid {
		t := lastAt.Time
		rec.LastAttemptAt = &t
	}
	if nextAt.Valid {
		t := nextAt.Time
		rec.NextAttemptAt = &t
	}
	return rec, nil
}

func (r *openAILoginCredentialRepository) RecordAttempt(ctx context.Context, accountID int64, success bool, code string, nextAttemptAt *time.Time) error {
	var next any
	if nextAttemptAt != nil {
		next = *nextAttemptAt
	}
	if success {
		_, err := r.db.ExecContext(ctx, `
UPDATE openai_login_credentials
SET fail_count = 0, last_attempt_at = NOW(), next_attempt_at = NULL, last_code = '', updated_at = NOW()
WHERE account_id = $1`, accountID)
		return err
	}
	_, err := r.db.ExecContext(ctx, `
UPDATE openai_login_credentials
SET fail_count = fail_count + 1, last_attempt_at = NOW(), next_attempt_at = $2, last_code = $3, updated_at = NOW()
WHERE account_id = $1`, accountID, next, code)
	return err
}

func (r *openAILoginCredentialRepository) Delete(ctx context.Context, accountID int64) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM openai_login_credentials WHERE account_id = $1`, accountID)
	return err
}

func (r *openAILoginCredentialRepository) Exists(ctx context.Context, accountID int64) (bool, error) {
	var one int
	err := r.db.QueryRowContext(ctx, `SELECT 1 FROM openai_login_credentials WHERE account_id = $1`, accountID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
