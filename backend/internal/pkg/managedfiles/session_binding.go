package managedfiles

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

const SessionBindingContract = "bps-session-binding/1"
const SessionBindingHeader = "X-Bps-Session-Binding"
const SessionBindingFeatureHeader = "X-Bps-Session-Binding-Feature"
const MaxSessionBindingSeconds = 120
const maxSessionBindingBytes = 4096

// SessionBindingClaims authorizes one exact ordinary inference body to use the
// same trusted owner domain as a managed file admission. It grants no file access.
type SessionBindingClaims struct {
	Version         string   `json:"version"`
	OwnerRef        string   `json:"owner_ref"`
	GroupRef        string   `json:"group_ref"`
	ConversationRef string   `json:"conversation_ref"`
	Expected        Identity `json:"expected"`
	Method          string   `json:"method"`
	Path            string   `json:"path"`
	WireSHA256      string   `json:"wire_sha256"`
	ExpiresAt       int64    `json:"expires_at"`
}

func validSessionBindingClaims(c SessionBindingClaims) bool {
	return c.Version == SessionBindingContract && safeOpaque(c.OwnerRef, 256) && safeOpaque(c.GroupRef, 256) && safeOpaque(c.ConversationRef, 256) && ValidateIdentity(c.Expected) == nil && c.Method == "POST" && (c.Path == "/v1/responses" || c.Path == "/responses") && digest(c.WireSHA256) && c.ExpiresAt > 0
}

func sessionBindingMAC(key string, raw []byte) []byte {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(SessionBindingContract + "\x00"))
	mac.Write(raw)
	return mac.Sum(nil)
}

func SignSessionBinding(key string, c SessionBindingClaims) (string, error) {
	if len(key) < 32 || !validSessionBindingClaims(c) {
		return "", reject("invalid_session_binding", "session_binding")
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return "", reject("invalid_session_binding", "session_binding")
	}
	header := base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(sessionBindingMAC(key, raw))
	if len(header) > maxSessionBindingBytes {
		return "", reject("invalid_session_binding", "session_binding")
	}
	return header, nil
}

// VerifySessionBinding authenticates the original inference bytes before any
// sanitization, the current credential identity, and the intended HTTP endpoint.
func VerifySessionBinding(key, header string, raw []byte, method, path string, current Identity, now time.Time) (SessionBindingClaims, error) {
	invalid := reject("invalid_session_binding", "session_binding")
	var zero SessionBindingClaims
	if len(key) < 32 || len(header) == 0 || len(header) > maxSessionBindingBytes {
		return zero, invalid
	}
	payload, signature, ok := strings.Cut(header, ".")
	if !ok || strings.Contains(signature, ".") {
		return zero, invalid
	}
	encoded, err := base64.RawURLEncoding.Strict().DecodeString(payload)
	if err != nil || base64.RawURLEncoding.EncodeToString(encoded) != payload {
		return zero, invalid
	}
	tag, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	if err != nil || base64.RawURLEncoding.EncodeToString(tag) != signature || !hmac.Equal(tag, sessionBindingMAC(key, encoded)) {
		return zero, invalid
	}
	parsed, err := Parse(encoded)
	if err != nil {
		return zero, invalid
	}
	root, ok := requiredResponsesFields(parsed, "version", "owner_ref", "group_ref", "conversation_ref", "expected", "method", "path", "wire_sha256", "expires_at")
	if !ok || !responsesStringFields(root, "version", "owner_ref", "group_ref", "conversation_ref", "method", "path", "wire_sha256") || !responsesIdentityFields(root["expected"]) {
		return zero, invalid
	}
	var c SessionBindingClaims
	if json.Unmarshal(encoded, &c) != nil || !validSessionBindingClaims(c) || c.Expected != current || c.Method != method || c.Path != path || c.WireSHA256 != ResponsesWireSHA256(raw) || c.ExpiresAt <= now.Unix() || c.ExpiresAt > now.Unix()+MaxSessionBindingSeconds {
		return zero, invalid
	}
	canonical, err := json.Marshal(c)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return zero, invalid
	}
	return c, nil
}
