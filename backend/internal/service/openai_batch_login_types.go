package service

import (
	"net/mail"
	"regexp"
	"strings"

	infraerrors "github.com/MACOS-DO/sub4api/internal/pkg/errors"
)

const (
	BatchLoginTargetBPS   = "bps"
	BatchLoginTargetCodex = "codex"

	BatchLoginStatusRunning  = "running"
	BatchLoginStatusDone     = "done"
	BatchLoginStatusCanceled = "canceled"

	BatchLoginItemPending  = "pending"
	BatchLoginItemRunning  = "running"
	BatchLoginItemCreated  = "created"
	BatchLoginItemSkipped  = "skipped"
	BatchLoginItemFailed   = "failed"
	BatchLoginItemCanceled = "canceled"
)

var batchLoginTOTPPattern = regexp.MustCompile(`^[A-Z2-7]+=*$`)

// BatchLoginEntry is one "email----password----2FA" line. It only lives in memory
// for the duration of the job and is never part of a job snapshot.
type BatchLoginEntry struct {
	Email      string
	Password   string
	TOTPSecret string
}

// BatchLoginInput is the validated request for a batch login job.
type BatchLoginInput struct {
	Entries            []BatchLoginEntry
	Targets            []string
	ProxyID            *int64
	AccountConcurrency int
	Priority           int
	BPSGroupIDs        []int64
	CodexGroupIDs      []int64
	SaveCredentials    bool
}

// BatchLoginItem is the public per (email, target) progress row.
type BatchLoginItem struct {
	Email     string `json:"email"`
	Target    string `json:"target"`
	Status    string `json:"status"`
	Code      string `json:"code,omitempty"`
	AccountID int64  `json:"account_id,omitempty"`
}

// BatchLoginJob is the public job snapshot (no credentials).
type BatchLoginJob struct {
	ID     string           `json:"id"`
	Status string           `json:"status"`
	Items  []BatchLoginItem `json:"items"`
}

// ValidateBatchLoginInput normalizes and validates the input in place.
func ValidateBatchLoginInput(in *BatchLoginInput, maxRows int) error {
	if maxRows <= 0 {
		maxRows = 100
	}
	if in == nil || len(in.Entries) == 0 || len(in.Entries) > maxRows {
		return infraerrors.BadRequest("BATCH_LOGIN_INVALID_ROWS", "batch must contain 1 to the configured maximum number of rows")
	}
	seen := make(map[string]struct{}, len(in.Entries))
	for i := range in.Entries {
		e := &in.Entries[i]
		email := strings.TrimSpace(e.Email)
		addr, err := mail.ParseAddress(email)
		if err != nil || addr.Address != email || len(email) > 320 {
			return infraerrors.BadRequest("BATCH_LOGIN_INVALID_ROW", "a row has an invalid email, password or 2FA secret")
		}
		email = strings.ToLower(email)
		if _, dup := seen[email]; dup {
			return infraerrors.BadRequest("BATCH_LOGIN_DUPLICATE_EMAIL", "duplicate email in batch")
		}
		seen[email] = struct{}{}
		totp := strings.ToUpper(strings.Join(strings.Fields(e.TOTPSecret), ""))
		if strings.TrimSpace(e.Password) == "" || len(e.Password) > 4096 ||
			strings.ContainsAny(e.Password, "\r\n\x00") ||
			totp == "" || len(totp) > 4096 || !batchLoginTOTPPattern.MatchString(totp) {
			return infraerrors.BadRequest("BATCH_LOGIN_INVALID_ROW", "a row has an invalid email, password or 2FA secret")
		}
		e.Email, e.TOTPSecret = email, totp
	}
	targets := make([]string, 0, 2)
	for _, t := range in.Targets {
		if t != BatchLoginTargetBPS && t != BatchLoginTargetCodex {
			return infraerrors.BadRequest("BATCH_LOGIN_INVALID_TARGET", "targets must be bps and/or codex")
		}
		dup := false
		for _, existing := range targets {
			dup = dup || existing == t
		}
		if !dup {
			targets = append(targets, t)
		}
	}
	if len(targets) == 0 {
		return infraerrors.BadRequest("BATCH_LOGIN_INVALID_TARGET", "select at least one target")
	}
	in.Targets = targets
	return nil
}
