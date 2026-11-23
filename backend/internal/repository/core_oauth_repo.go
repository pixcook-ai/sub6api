package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/google/uuid"
)

const coreOAuthProfilePrefix = "core_oauth_profile:"
const coreOAuthMaxCiphertextBytes = 4 * 1024 * 1024

type coreOAuthRepository struct{ db *sql.DB }

func NewCoreOAuthRepository(db *sql.DB) service.CoreOAuthRepository {
	return &coreOAuthRepository{db: db}
}

type coreOAuthSettingsValue struct {
	Version    int64  `json:"version"`
	Ciphertext string `json:"ciphertext"`
}

func validCoreOAuthProfileRef(ref string) bool {
	id, err := uuid.Parse(ref)
	return err == nil && id != uuid.Nil && id.String() == ref
}

func validCoreOAuthSealedProfile(profile service.CoreOAuthSealedProfile) bool {
	return validCoreOAuthProfileRef(profile.ProfileRef) && profile.Version > 0 && profile.Ciphertext != "" &&
		len(profile.Ciphertext) <= coreOAuthMaxCiphertextBytes &&
		strings.IndexFunc(profile.Ciphertext, func(r rune) bool { return r < 33 || r > 126 }) < 0
}

func coreOAuthProfileFromSetting(key, raw string) (*service.CoreOAuthSealedProfile, error) {
	ref, found := strings.CutPrefix(key, coreOAuthProfilePrefix)
	if !found || !validCoreOAuthProfileRef(ref) || len(raw) > coreOAuthMaxCiphertextBytes+128 {
		return nil, service.ErrCoreOAuthStorage
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var value coreOAuthSettingsValue
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, service.ErrCoreOAuthStorage
	}
	profile := &service.CoreOAuthSealedProfile{ProfileRef: ref, Version: value.Version, Ciphertext: value.Ciphertext}
	if !validCoreOAuthSealedProfile(*profile) {
		return nil, service.ErrCoreOAuthStorage
	}
	return profile, nil
}

func (r *coreOAuthRepository) readProfile(ctx context.Context, ref string) (*service.CoreOAuthSealedProfile, string, error) {
	if r == nil || r.db == nil || !validCoreOAuthProfileRef(ref) {
		return nil, "", service.ErrCoreOAuthStorage
	}
	var key, raw string
	err := r.db.QueryRowContext(ctx, "SELECT key, value FROM settings WHERE key = $1", coreOAuthProfilePrefix+ref).Scan(&key, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", service.ErrCoreOAuthProfileNotFound
	}
	if err != nil {
		return nil, "", service.ErrCoreOAuthStorage
	}
	profile, err := coreOAuthProfileFromSetting(key, raw)
	if err != nil || profile.ProfileRef != ref {
		return nil, "", service.ErrCoreOAuthStorage
	}
	return profile, raw, nil
}

func (r *coreOAuthRepository) ReadProfile(ctx context.Context, ref string) (*service.CoreOAuthSealedProfile, error) {
	profile, _, err := r.readProfile(ctx, ref)
	return profile, err
}

func (r *coreOAuthRepository) ListProfiles(ctx context.Context) ([]service.CoreOAuthSealedProfile, error) {
	if r == nil || r.db == nil {
		return nil, service.ErrCoreOAuthStorage
	}
	// The underscore escapes are required: neighbouring settings are not profiles.
	rows, err := r.db.QueryContext(ctx, "SELECT key, value FROM settings WHERE key LIKE $1 ESCAPE '!' ORDER BY key", "core!_oauth!_profile:%")
	if err != nil {
		return nil, service.ErrCoreOAuthStorage
	}
	defer rows.Close()
	profiles := make([]service.CoreOAuthSealedProfile, 0)
	for rows.Next() {
		var key, raw string
		if rows.Scan(&key, &raw) != nil {
			return nil, service.ErrCoreOAuthStorage
		}
		profile, err := coreOAuthProfileFromSetting(key, raw)
		if err != nil {
			return nil, service.ErrCoreOAuthStorage
		}
		profiles = append(profiles, *profile)
	}
	if rows.Err() != nil {
		return nil, service.ErrCoreOAuthStorage
	}
	return profiles, nil
}

func (r *coreOAuthRepository) CreateProfile(ctx context.Context, profile service.CoreOAuthSealedProfile) error {
	if r == nil || r.db == nil || !validCoreOAuthSealedProfile(profile) || profile.Version != 1 {
		return service.ErrCoreOAuthStorage
	}
	value, err := json.Marshal(coreOAuthSettingsValue{Version: profile.Version, Ciphertext: profile.Ciphertext})
	if err != nil {
		return service.ErrCoreOAuthStorage
	}
	result, err := r.db.ExecContext(ctx, "INSERT INTO settings (key, value, updated_at) VALUES ($1, $2, CURRENT_TIMESTAMP) ON CONFLICT (key) DO NOTHING", coreOAuthProfilePrefix+profile.ProfileRef, string(value))
	if err != nil {
		return service.ErrCoreOAuthStorage
	}
	count, err := result.RowsAffected()
	if err != nil {
		return service.ErrCoreOAuthStorage
	}
	if count == 0 {
		return service.ErrCoreOAuthConflict
	}
	if count != 1 {
		return service.ErrCoreOAuthStorage
	}
	return nil
}

func (r *coreOAuthRepository) CompareAndSwapProfile(ctx context.Context, ref string, expectedVersion int64, next service.CoreOAuthSealedProfile) (bool, error) {
	if r == nil || r.db == nil || !validCoreOAuthSealedProfile(next) || next.ProfileRef != ref ||
		expectedVersion <= 0 || expectedVersion >= (1<<63)-1 || next.Version != expectedVersion+1 {
		return false, service.ErrCoreOAuthStorage
	}
	current, previousValue, err := r.readProfile(ctx, ref)
	if errors.Is(err, service.ErrCoreOAuthProfileNotFound) {
		return false, nil
	}
	if err != nil {
		return false, service.ErrCoreOAuthStorage
	}
	if current.Version != expectedVersion {
		return false, nil
	}
	value, err := json.Marshal(coreOAuthSettingsValue{Version: next.Version, Ciphertext: next.Ciphertext})
	if err != nil {
		return false, service.ErrCoreOAuthStorage
	}
	// Compare the exact sealed value, not just the earlier in-process read.
	// Concurrent refresh claims can observe the same version; only one UPDATE wins.
	result, err := r.db.ExecContext(ctx, "UPDATE settings SET value = $1, updated_at = CURRENT_TIMESTAMP WHERE key = $2 AND value = $3", string(value), coreOAuthProfilePrefix+ref, previousValue)
	if err != nil {
		return false, service.ErrCoreOAuthStorage
	}
	count, err := result.RowsAffected()
	if err != nil || count < 0 || count > 1 {
		return false, service.ErrCoreOAuthStorage
	}
	return count == 1, nil
}

var _ service.CoreOAuthRepository = (*coreOAuthRepository)(nil)
