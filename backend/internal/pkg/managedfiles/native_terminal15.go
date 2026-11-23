package managedfiles

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

const NativeTerminalContract15 = "bps-native-terminal/1"
const NativeTerminalEvent15 = "bps.private.native_terminal"

// NativeTerminalBinding15 is derived from an authenticated main-gateway admission,
// never from public client headers. PublicID is the local capture ID, not BPS's ID.
type NativeTerminalBinding15 struct {
	PublicID               string `json:"public_id"`
	UserID                 int64  `json:"user_id"`
	APIKeyID               int64  `json:"api_key_id"`
	GroupID                int64  `json:"group_id"`
	AccountID              int64  `json:"account_id"`
	PublicModel            string `json:"public_model"`
	ResolvedModel          string `json:"resolved_model"`
	BackendID              string `json:"backend_id"`
	SubjectDigest          string `json:"subject_digest"`
	CredentialGeneration   string `json:"credential_generation"`
	AdmissionID            string `json:"admission_id"`
	AdmissionBindingSHA256 string `json:"admission_binding_sha256"`
	RequestWireSHA256      string `json:"request_wire_sha256"`
	Nonce                  string `json:"nonce"`
}

type NativeTerminalReceipt15 struct {
	Version          string                  `json:"version"`
	Binding          NativeTerminalBinding15 `json:"binding"`
	NativeWireSHA256 string                  `json:"native_wire_sha256"`
	TerminalSHA256   string                  `json:"terminal_sha256"`
	SnapshotSHA256   string                  `json:"snapshot_sha256"`
	IssuedAt         int64                   `json:"issued_at"`
	ExpiresAt        int64                   `json:"expires_at"`
}

type nativeTerminalEnvelope15 struct {
	NativeTerminalReceipt15
	Terminal string `json:"terminal_base64"`
	Snapshot string `json:"snapshot_base64"`
	MAC      string `json:"mac"`
}

func nativeTerminalLimit15(maxBytes int) error {
	if maxBytes <= 0 || maxBytes > 16<<20 {
		return reject("invalid_native_terminal", "limit")
	}
	return nil
}
func nativeTerminalKey15(key string) bool {
	return len(key) >= 32 && len(key) <= 1024 && strings.TrimSpace(key) == key
}

// ValidateNativeTerminalBinding15 checks a server-derived request binding.
// It does not convert public request values into trusted authorization.
func ValidateNativeTerminalBinding15(b NativeTerminalBinding15) error {
	return nativeTerminalBinding15(b)
}
func nativeTerminalBinding15(b NativeTerminalBinding15) error {
	if len(b.PublicID) != len("resp_local_")+32 || !strings.HasPrefix(b.PublicID, "resp_local_") {
		return reject("invalid_native_terminal", "binding")
	}
	id, err := hex.DecodeString(strings.TrimPrefix(b.PublicID, "resp_local_"))
	if err != nil || len(id) != 16 || strings.ToLower(b.PublicID) != b.PublicID || b.UserID <= 0 || b.APIKeyID <= 0 || b.GroupID < 0 || b.AccountID <= 0 {
		return reject("invalid_native_terminal", "binding")
	}
	for _, s := range []string{b.PublicModel, b.ResolvedModel, b.BackendID, b.CredentialGeneration} {
		if !safeOpaque(s, 256) || strings.TrimSpace(s) != s {
			return reject("invalid_native_terminal", "binding")
		}
	}
	for _, s := range []string{b.SubjectDigest, b.AdmissionBindingSHA256, b.RequestWireSHA256, b.Nonce} {
		if !digest(s) {
			return reject("invalid_native_terminal", "binding")
		}
	}
	if !strings.HasPrefix(b.AdmissionID, "responses-") || !digest(strings.TrimPrefix(b.AdmissionID, "responses-")) {
		return reject("invalid_native_terminal", "binding")
	}
	return nil
}
func nativeTerminalReceipt15(r NativeTerminalReceipt15, now time.Time) error {
	if r.Version != NativeTerminalContract15 || nativeTerminalBinding15(r.Binding) != nil || !digest(r.NativeWireSHA256) || !digest(r.TerminalSHA256) || !digest(r.SnapshotSHA256) || r.IssuedAt <= 0 || r.ExpiresAt <= r.IssuedAt || r.ExpiresAt-r.IssuedAt > 120 || now.Unix() < r.IssuedAt-5 || now.Unix() >= r.ExpiresAt {
		return reject("invalid_native_terminal", "receipt")
	}
	return nil
}
func nativeTerminalMAC15(key string, r NativeTerminalReceipt15) string {
	raw, _ := json.Marshal(r)
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(NativeTerminalContract15 + "\x00"))
	_, _ = mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}
func nativeTerminalPayload15(raw []byte, maxBytes int, terminal bool) error {
	if len(raw) > maxBytes {
		return reject("native_terminal_limit", "payload")
	}
	value, err := Parse(raw)
	if err != nil {
		return reject("invalid_native_terminal", "payload")
	}
	obj, ok := value.(map[string]any)
	if !ok || obj == nil {
		return reject("invalid_native_terminal", "payload")
	}
	if terminal {
		_, outputOK := obj["output"].([]any)
		if !outputOK || obj["object"] != "response" || obj["status"] != "completed" || obj["error"] != nil || obj["incomplete_details"] != nil {
			return reject("invalid_native_terminal", "terminal")
		}
	}
	return nil
}

// EncodeNativeTerminal15 is called only after terminal/native capture succeeds.
// Payloads use canonical base64 so JSON marshaling cannot rewrite exact bytes.
func EncodeNativeTerminal15(key string, b NativeTerminalBinding15, nativeWire, terminal, snapshot []byte, now time.Time, ttl time.Duration, maxBytes int) ([]byte, error) {
	if nativeTerminalLimit15(maxBytes) != nil || !nativeTerminalKey15(key) || nativeTerminalBinding15(b) != nil || ttl < time.Second || ttl > 120*time.Second {
		return nil, reject("invalid_native_terminal", "binding")
	}
	for _, p := range []struct {
		raw      []byte
		terminal bool
	}{{nativeWire, false}, {terminal, true}, {snapshot, false}} {
		if err := nativeTerminalPayload15(p.raw, maxBytes, p.terminal); err != nil {
			return nil, err
		}
	}
	receipt := NativeTerminalReceipt15{Version: NativeTerminalContract15, Binding: b, NativeWireSHA256: ResponsesWireSHA256(nativeWire), TerminalSHA256: ResponsesWireSHA256(terminal), SnapshotSHA256: ResponsesWireSHA256(snapshot), IssuedAt: now.Unix(), ExpiresAt: now.Add(ttl).Unix()}
	if err := nativeTerminalReceipt15(receipt, now); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(nativeTerminalEnvelope15{NativeTerminalReceipt15: receipt, Terminal: base64.StdEncoding.EncodeToString(terminal), Snapshot: base64.StdEncoding.EncodeToString(snapshot), MAC: nativeTerminalMAC15(key, receipt)})
	if err != nil || len(encoded) > 2*base64.StdEncoding.EncodedLen(maxBytes)+(16<<10) {
		return nil, reject("native_terminal_limit", "body")
	}
	return encoded, nil
}

// VerifyNativeTerminal15 authenticates exact gateway bytes before main rewrites
// ID/model/usage. It does not authorize historical resources or validate the
// native snapshot schema. The caller must do both before durable Finalize.
func VerifyNativeTerminal15(raw []byte, key string, expected NativeTerminalBinding15, now time.Time, maxBytes int) (NativeTerminalReceipt15, []byte, []byte, error) {
	fail := func() (NativeTerminalReceipt15, []byte, []byte, error) {
		return NativeTerminalReceipt15{}, nil, nil, reject("invalid_native_terminal", "body")
	}
	if nativeTerminalLimit15(maxBytes) != nil || !nativeTerminalKey15(key) || nativeTerminalBinding15(expected) != nil || len(raw) > 2*base64.StdEncoding.EncodedLen(maxBytes)+(16<<10) {
		return fail()
	}
	value, err := Parse(raw)
	if err != nil {
		return fail()
	}
	root, ok := requiredResponsesFields(value, "version", "binding", "native_wire_sha256", "terminal_sha256", "snapshot_sha256", "issued_at", "expires_at", "terminal_base64", "snapshot_base64", "mac")
	if !ok {
		return fail()
	}
	binding, ok := requiredResponsesFields(root["binding"], "public_id", "user_id", "api_key_id", "group_id", "account_id", "public_model", "resolved_model", "backend_id", "subject_digest", "credential_generation", "admission_id", "admission_binding_sha256", "request_wire_sha256", "nonce")
	if !ok || len(binding) != 14 {
		return fail()
	}
	for _, name := range []string{"user_id", "api_key_id", "group_id", "account_id"} {
		if _, ok := binding[name].(json.Number); !ok {
			return fail()
		}
	}
	// Exact field sets above protect required zero-valued group_id too.
	var envelope nativeTerminalEnvelope15
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&envelope) != nil || envelope.Binding != expected || nativeTerminalReceipt15(envelope.NativeTerminalReceipt15, now) != nil || !digest(envelope.MAC) || !hmac.Equal([]byte(envelope.MAC), []byte(nativeTerminalMAC15(key, envelope.NativeTerminalReceipt15))) {
		return fail()
	}
	decode := func(s string) ([]byte, error) {
		if len(s) > base64.StdEncoding.EncodedLen(maxBytes) {
			return nil, reject("native_terminal_limit", "payload")
		}
		p, e := base64.StdEncoding.Strict().DecodeString(s)
		if e != nil || base64.StdEncoding.EncodeToString(p) != s || len(p) > maxBytes {
			return nil, reject("invalid_native_terminal", "payload")
		}
		return p, nil
	}
	terminal, err := decode(envelope.Terminal)
	if err != nil || ResponsesWireSHA256(terminal) != envelope.TerminalSHA256 || nativeTerminalPayload15(terminal, maxBytes, true) != nil {
		return fail()
	}
	snapshot, err := decode(envelope.Snapshot)
	if err != nil || ResponsesWireSHA256(snapshot) != envelope.SnapshotSHA256 || nativeTerminalPayload15(snapshot, maxBytes, false) != nil {
		return fail()
	}
	return envelope.NativeTerminalReceipt15, terminal, snapshot, nil
}
