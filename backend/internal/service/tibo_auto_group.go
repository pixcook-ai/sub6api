package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// ErrOpenAITiboProbeIdentityChanged means the observation was not applied at
// all. Callers must discard the old identity's in-memory sample and must not
// advance persistence watermarks as if the repository had accepted it.
var ErrOpenAITiboProbeIdentityChanged = errors.New("tibo probe identity changed before persistence")

// TiboAutoGroupSample is issued only by a newly completed active Tibo probe.
// StartedAt orders overlapping probes across processes; completion order must
// not let an older request undo a newer result.
type TiboAutoGroupSample struct {
	Verdict       string
	ProbeIdentity string
	StartedAt     time.Time
	CompletedAt   time.Time
}

// TiboAutoGroupRepository persists the observation and its bounded membership
// change atomically. Generic UpdateExtra and passive model observations cannot
// invoke this capability implicitly.
type TiboAutoGroupRepository interface {
	PersistTiboProbeResult(context.Context, int64, map[string]any, TiboAutoGroupSample) error
}

// OpenAITiboProbeIdentity binds a synthetic probe to its actual credential and
// proxy binding. Mutable observations and ordinary configuration extra are not
// identity, so routine usage writes cannot invalidate every in-flight probe.
// Only the digest travels with the sample; credentials are never logged.
func OpenAITiboProbeIdentity(account *Account) string {
	if account == nil {
		return ""
	}
	identity := struct {
		Platform    string         `json:"platform"`
		Type        string         `json:"type"`
		Credentials map[string]any `json:"credentials"`
		ProxyID     *int64         `json:"proxy_id"`
	}{Platform: account.Platform, Type: account.Type, Credentials: account.Credentials, ProxyID: account.ProxyID}
	raw, err := json.Marshal(identity)
	if err != nil {
		// A malformed in-memory map must not silently opt a production probe out of
		// identity checking. Persisted JSON credentials cannot produce this value.
		return "invalid"
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

// TiboObservationRepository persists passive history with an upstream identity
// fence and never grants automatic action or active-sample watermark authority.
type TiboObservationRepository interface {
	PersistTiboObservation(context.Context, int64, map[string]any, string) error
}
