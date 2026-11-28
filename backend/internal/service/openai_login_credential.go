package service

import (
	"context"
	"time"
)

// OpenAILoginCredentialRecord is the stored (encrypted) login credential of one account.
// Ciphertexts never leave the service layer.
type OpenAILoginCredentialRecord struct {
	AccountID          int64
	LoginEmail         string
	Target             string
	PasswordCiphertext string
	TOTPCiphertext     string
	FailCount          int
	LastAttemptAt      *time.Time
	NextAttemptAt      *time.Time
	LastCode           string
}

// OpenAILoginCredentialRepository persists encrypted login credentials per account.
type OpenAILoginCredentialRepository interface {
	Upsert(ctx context.Context, rec *OpenAILoginCredentialRecord) error
	Get(ctx context.Context, accountID int64) (*OpenAILoginCredentialRecord, error)
	// RecordAttempt stores the outcome of a re-authorization attempt.
	// success resets the failure counter; otherwise it is incremented.
	RecordAttempt(ctx context.Context, accountID int64, success bool, code string, nextAttemptAt *time.Time) error
	Delete(ctx context.Context, accountID int64) error
	Exists(ctx context.Context, accountID int64) (bool, error)
}
