package managedfiles

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

const (
	NativeResponsesContract15         = "bps-responses-native/1"
	NativeResponsesPolicy15           = "bps-responses-native-function-custom-2026-11-21-v1"
	NativeResponsesAttachmentPolicy15 = "bps-responses-native-attachments-2026-11-22-v1"
	NativeResponsesPrefix15           = "/internal/bps-responses-native/v1/"
	NativeResponsesActivationHeader15 = "X-Bps-Responses-Native-Activation"
	NativeResponsesAdmittedHeader15   = "X-Bps-Responses-Native-Admitted"
	NativeResponsesMaxBytes15         = 16 << 20
)

type NativeResponsesContext15 struct {
	UserID        int64  `json:"user_id"`
	APIKeyID      int64  `json:"api_key_id"`
	GroupID       int64  `json:"group_id"`
	AccountID     int64  `json:"account_id"`
	PublicModel   string `json:"public_model"`
	ResolvedModel string `json:"resolved_model"`
	Nonce         string `json:"nonce"`
}
type NativeResponsesPreflight15 struct {
	Version                string                       `json:"version"`
	Expected               Identity                     `json:"expected"`
	Context                NativeResponsesContext15     `json:"context"`
	ClientRequest          json.RawMessage              `json:"client_request"`
	WireRequest            json.RawMessage              `json:"wire_request"`
	PreviousSnapshotBase64 string                       `json:"previous_snapshot_base64"`
	MaxBytes               int                          `json:"max_bytes"`
	Files                  *NativeAttachmentAdmission15 `json:"files,omitempty"`
}

// NativeAttachmentAdmission15 carries stable copy facts, never File or lease authority.
type NativeAttachmentAdmission15 struct {
	AdmissionID   string                     `json:"admission_id"`
	OwnerRef      string                     `json:"owner_ref"`
	GroupRef      string                     `json:"group_ref"`
	BindingSHA256 string                     `json:"binding_sha256"`
	Snapshot      NativeAttachmentSnapshot15 `json:"snapshot"`
}
type NativeResponsesPreflightResult15 struct {
	Version           string   `json:"version"`
	PolicyVersion     string   `json:"policy_version"`
	Identity          Identity `json:"identity"`
	AdmissionID       string   `json:"admission_id"`
	ExpiresAt         int64    `json:"expires_at"`
	RequestWireSHA256 string   `json:"request_wire_sha256"`
	NativeWireSHA256  string   `json:"native_wire_sha256"`
	BindingSHA256     string   `json:"binding_sha256"`
	MaxBytes          int      `json:"max_bytes"`
}
type NativeResponsesActivation15 struct {
	Version          string                  `json:"version"`
	PolicyVersion    string                  `json:"policy_version"`
	Binding          NativeTerminalBinding15 `json:"binding"`
	NativeWireSHA256 string                  `json:"native_wire_sha256"`
	ExpiresAt        int64                   `json:"expires_at"`
	MaxBytes         int                     `json:"max_bytes"`
}
type nativeResponsesActivationEnvelope15 struct {
	NativeResponsesActivation15
	MAC string `json:"mac"`
}

// This rule is shared by gateway capture, main durable proof and main read.
// SubjectFingerprint is opaque; hash its exact bytes, without trimming.
func NativeResponsesSubjectDigest15(identity Identity) string {
	return ResponsesWireSHA256([]byte(identity.SubjectFingerprint))
}

func ValidateNativeResponsesServiceKey15(key string) error {
	if !nativeTerminalKey15(key) {
		return reject("invalid_native_responses_service_key", "service_key")
	}
	return nil
}
func validNativeResponsesMaxBytes15(n int) bool { return n > 0 && n <= NativeResponsesMaxBytes15 }
func validateNativeResponsesContext15(c NativeResponsesContext15) error {
	if c.UserID <= 0 || c.APIKeyID <= 0 || c.GroupID < 0 || c.AccountID <= 0 || !safeOpaque(c.PublicModel, 256) || !safeOpaque(c.ResolvedModel, 256) || strings.TrimSpace(c.PublicModel) != c.PublicModel || strings.TrimSpace(c.ResolvedModel) != c.ResolvedModel || !digest(c.Nonce) {
		return reject("invalid_native_responses", "context")
	}
	return nil
}
func NativeResponsesPreviousSnapshot15(r NativeResponsesPreflight15) ([]byte, error) {
	if !validNativeResponsesMaxBytes15(r.MaxBytes) {
		return nil, reject("native_responses_limit", "max_bytes")
	}
	if r.PreviousSnapshotBase64 == "" {
		return nil, nil
	}
	if len(r.PreviousSnapshotBase64) > base64.StdEncoding.EncodedLen(r.MaxBytes) {
		return nil, reject("native_responses_limit", "previous_snapshot")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(r.PreviousSnapshotBase64)
	if err != nil || base64.StdEncoding.EncodeToString(raw) != r.PreviousSnapshotBase64 || len(raw) > r.MaxBytes {
		return nil, reject("invalid_native_responses", "previous_snapshot")
	}
	value, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, reject("invalid_native_responses", "previous_snapshot")
	}
	return raw, nil
}

// This tranche permits only trusted model mapping between the two deltas.
// Every other original input/tool/instruction/control field must be identical.
func ValidateNativeResponsesDeltas15(r NativeResponsesPreflight15) error {
	if !validNativeResponsesMaxBytes15(r.MaxBytes) {
		return reject("native_responses_limit", "max_bytes")
	}
	values := make([]map[string]any, 2)
	for i, raw := range [][]byte{r.ClientRequest, r.WireRequest} {
		if len(raw) == 0 || len(raw) > r.MaxBytes || !bytes.Equal(raw, bytes.TrimSpace(raw)) {
			return reject("native_responses_limit", "request")
		}
		value, err := Parse(raw)
		if err != nil {
			return err
		}
		v, ok := value.(map[string]any)
		if !ok || v["store"] != false {
			return reject("invalid_native_responses", "request")
		}
		if _, exists := v["previous_response_id"]; exists {
			return reject("invalid_native_responses", "previous_response_id")
		}
		if _, exists := v["conversation"]; exists {
			return reject("invalid_native_responses", "conversation")
		}
		values[i] = v
	}
	if values[0]["model"] != r.Context.PublicModel || values[1]["model"] != r.Context.ResolvedModel {
		return reject("invalid_native_responses", "model")
	}
	delete(values[0], "model")
	delete(values[1], "model")
	a, _ := json.Marshal(values[0])
	b, _ := json.Marshal(values[1])
	if !bytes.Equal(a, b) {
		return reject("invalid_native_responses", "delta_mismatch")
	}
	return nil
}
func validateNativeResponsesPreflight15(r NativeResponsesPreflight15) error {
	if r.Version != NativeResponsesContract15 {
		return reject("unsupported_native_responses_contract", "version")
	}
	if err := ValidateIdentity(r.Expected); err != nil {
		return err
	}
	if err := validateNativeResponsesContext15(r.Context); err != nil {
		return err
	}
	if err := ValidateNativeResponsesDeltas15(r); err != nil {
		return err
	}
	if r.Files != nil {
		f := r.Files
		if !nativeAttachmentOpaque15(f.AdmissionID, 256) || f.OwnerRef != strconv.FormatInt(r.Context.UserID, 10) || f.GroupRef != strconv.FormatInt(r.Context.GroupID, 10) || !digest(f.BindingSHA256) {
			return reject("invalid_native_attachments", "files")
		}
		if _, err := CanonicalNativeAttachmentSnapshot15(f.Snapshot); err != nil {
			return err
		}
		for _, copy := range f.Snapshot.Copies {
			if copy.AccountID != r.Context.AccountID || copy.BackendID != r.Expected.BackendID || copy.SubjectDigest != NativeResponsesSubjectDigest15(r.Expected) || copy.CredentialGeneration != r.Expected.CredentialGeneration {
				return reject("invalid_native_attachments", "identity")
			}
		}
	}
	_, err := NativeResponsesPreviousSnapshot15(r)
	return err
}

func nativeResponsesPolicyForRequest15(r NativeResponsesPreflight15) string {
	if r.Files != nil {
		return NativeResponsesAttachmentPolicy15
	}
	return NativeResponsesPolicy15
}

func canonicalNativeAttachmentAdmission15(f *NativeAttachmentAdmission15) ([]byte, error) {
	raw, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	return Canonical(raw)
}
func EncodeNativeResponsesPreflight15(r NativeResponsesPreflight15) ([]byte, error) {
	if err := validateNativeResponsesPreflight15(r); err != nil {
		return nil, err
	}
	meta := struct {
		Version  string                   `json:"version"`
		Expected Identity                 `json:"expected"`
		Context  NativeResponsesContext15 `json:"context"`
		Previous string                   `json:"previous_snapshot_base64"`
		MaxBytes int                      `json:"max_bytes"`
	}{r.Version, r.Expected, r.Context, r.PreviousSnapshotBase64, r.MaxBytes}
	raw, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	if r.Files != nil {
		files, err := canonicalNativeAttachmentAdmission15(r.Files)
		if err != nil {
			return nil, err
		}
		raw = append(raw[:len(raw)-1], []byte(`,"files":`)...)
		raw = append(raw, files...)
		raw = append(raw, '}')
	}
	raw = append(raw[:len(raw)-1], []byte(`,"client_request":`)...)
	raw = append(raw, r.ClientRequest...)
	raw = append(raw, []byte(`,"wire_request":`)...)
	raw = append(raw, r.WireRequest...)
	raw = append(raw, '}')
	if len(raw) > 2*NativeResponsesMaxBytes15+base64.StdEncoding.EncodedLen(NativeResponsesMaxBytes15)+NativeAttachmentMaxSnapshotBytes15+(32<<10) {
		return nil, reject("native_responses_limit", "body")
	}
	return raw, nil
}
func DecodeNativeResponsesPreflight15(raw []byte) (NativeResponsesPreflight15, error) {
	fail := func() (NativeResponsesPreflight15, error) {
		return NativeResponsesPreflight15{}, reject("invalid_native_responses", "body")
	}
	value, err := parseResponsesEnvelope(raw, 2*NativeResponsesMaxBytes15+base64.StdEncoding.EncodedLen(NativeResponsesMaxBytes15)+NativeAttachmentMaxSnapshotBytes15+(32<<10))
	if err != nil {
		return fail()
	}
	object, ok := value.(map[string]any)
	if !ok {
		return fail()
	}
	fields := []string{"version", "expected", "context", "client_request", "wire_request", "previous_snapshot_base64", "max_bytes"}
	if _, hasFiles := object["files"]; hasFiles {
		fields = append(fields, "files")
		files, valid := requiredResponsesFields(object["files"], "admission_id", "owner_ref", "group_ref", "binding_sha256", "snapshot")
		if !valid || !responsesStringFields(files, "admission_id", "owner_ref", "group_ref", "binding_sha256") {
			return fail()
		}
		snapshot, err := json.Marshal(files["snapshot"])
		if err != nil {
			return fail()
		}
		if _, err = DecodeNativeAttachmentSnapshot15(snapshot); err != nil {
			return fail()
		}
	}
	root, ok := requiredResponsesFields(value, fields...)
	if !ok || !responsesStringFields(root, "version", "previous_snapshot_base64") || !responsesIdentityFields(root["expected"]) {
		return fail()
	}
	if _, ok := root["max_bytes"].(json.Number); !ok {
		return fail()
	}
	context, ok := requiredResponsesFields(root["context"], "user_id", "api_key_id", "group_id", "account_id", "public_model", "resolved_model", "nonce")
	if !ok {
		return fail()
	}
	for _, key := range []string{"user_id", "api_key_id", "group_id", "account_id"} {
		if _, ok := context[key].(json.Number); !ok {
			return fail()
		}
	}
	var r NativeResponsesPreflight15
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&r) != nil {
		return fail()
	}
	if err := validateNativeResponsesPreflight15(r); err != nil {
		return NativeResponsesPreflight15{}, err
	}
	return r, nil
}
func NativeResponsesBindingSHA25615(r NativeResponsesPreflight15, nativeWireSHA256 string) (string, error) {
	if err := validateNativeResponsesPreflight15(r); err != nil {
		return "", err
	}
	if !digest(nativeWireSHA256) {
		return "", reject("invalid_native_responses", "native_wire_sha256")
	}
	previous, err := NativeResponsesPreviousSnapshot15(r)
	if err != nil {
		return "", err
	}
	binding := struct {
		Version, Policy                string
		Expected                       Identity
		Context                        NativeResponsesContext15
		Client, Wire, Previous, Native string
		MaxBytes                       int
	}{r.Version, nativeResponsesPolicyForRequest15(r), r.Expected, r.Context, ResponsesWireSHA256(r.ClientRequest), ResponsesWireSHA256(r.WireRequest), ResponsesWireSHA256(previous), nativeWireSHA256, r.MaxBytes}
	raw, err := json.Marshal(binding)
	if err != nil {
		return "", err
	}
	if r.Files != nil {
		files, err := canonicalNativeAttachmentAdmission15(r.Files)
		if err != nil {
			return "", err
		}
		raw = append(raw, 0)
		raw = append(raw, files...)
	}
	return ResponsesWireSHA256(append([]byte("bps-native-admission-binding/1\x00"), raw...)), nil
}
func DecodeNativeResponsesPreflightResult15(raw []byte) (NativeResponsesPreflightResult15, error) {
	fail := func() (NativeResponsesPreflightResult15, error) {
		return NativeResponsesPreflightResult15{}, reject("invalid_native_responses_result", "body")
	}
	value, err := parseResponsesEnvelope(raw, 16<<10)
	if err != nil {
		return fail()
	}
	root, ok := requiredResponsesFields(value, "version", "policy_version", "identity", "admission_id", "expires_at", "request_wire_sha256", "native_wire_sha256", "binding_sha256", "max_bytes")
	if !ok || !responsesIdentityFields(root["identity"]) {
		return fail()
	}
	if _, ok := root["expires_at"].(json.Number); !ok {
		return fail()
	}
	if _, ok := root["max_bytes"].(json.Number); !ok {
		return fail()
	}
	var r NativeResponsesPreflightResult15
	if json.Unmarshal(raw, &r) != nil || r.Version != NativeResponsesContract15 || (r.PolicyVersion != NativeResponsesPolicy15 && r.PolicyVersion != NativeResponsesAttachmentPolicy15) || ValidateIdentity(r.Identity) != nil || r.ExpiresAt <= 0 || !validNativeResponsesMaxBytes15(r.MaxBytes) || !strings.HasPrefix(r.AdmissionID, "responses-") || !digest(strings.TrimPrefix(r.AdmissionID, "responses-")) || !digest(r.RequestWireSHA256) || !digest(r.NativeWireSHA256) || !digest(r.BindingSHA256) {
		return fail()
	}
	return r, nil
}
func ValidateNativeResponsesPreflightResult15(request NativeResponsesPreflight15, result NativeResponsesPreflightResult15, now time.Time) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if _, err = DecodeNativeResponsesPreflightResult15(encoded); err != nil {
		return err
	}
	expected, err := NativeResponsesBindingSHA25615(request, result.NativeWireSHA256)
	if err != nil || result.PolicyVersion != nativeResponsesPolicyForRequest15(request) || result.Identity != request.Expected || result.MaxBytes != request.MaxBytes || result.RequestWireSHA256 != ResponsesWireSHA256(request.WireRequest) || result.BindingSHA256 != expected || result.ExpiresAt <= now.Unix() || result.ExpiresAt > now.Add(30*time.Second).Unix() {
		return reject("invalid_native_responses_result", "binding")
	}
	return nil
}
func NativeResponsesTerminalBinding15(r NativeResponsesPreflight15, result NativeResponsesPreflightResult15, publicID string) NativeTerminalBinding15 {
	c := r.Context
	return NativeTerminalBinding15{PublicID: publicID, UserID: c.UserID, APIKeyID: c.APIKeyID, GroupID: c.GroupID, AccountID: c.AccountID, PublicModel: c.PublicModel, ResolvedModel: c.ResolvedModel, BackendID: r.Expected.BackendID, SubjectDigest: NativeResponsesSubjectDigest15(r.Expected), CredentialGeneration: r.Expected.CredentialGeneration, AdmissionID: result.AdmissionID, AdmissionBindingSHA256: result.BindingSHA256, RequestWireSHA256: result.RequestWireSHA256, Nonce: c.Nonce}
}
func nativeResponsesActivationMAC15(key string, a NativeResponsesActivation15) string {
	raw, _ := json.Marshal(a)
	m := hmac.New(sha256.New, []byte(key))
	_, _ = m.Write([]byte("bps-native-activation/1\x00"))
	_, _ = m.Write(raw)
	return hex.EncodeToString(m.Sum(nil))
}
func EncodeNativeResponsesActivation15(key string, r NativeResponsesPreflight15, result NativeResponsesPreflightResult15, publicID string, now time.Time) (string, error) {
	if !nativeTerminalKey15(key) || ValidateNativeResponsesPreflightResult15(r, result, now) != nil {
		return "", reject("invalid_native_responses_activation", "binding")
	}
	a := NativeResponsesActivation15{Version: NativeResponsesContract15, PolicyVersion: nativeResponsesPolicyForRequest15(r), Binding: NativeResponsesTerminalBinding15(r, result, publicID), NativeWireSHA256: result.NativeWireSHA256, ExpiresAt: result.ExpiresAt, MaxBytes: result.MaxBytes}
	if ValidateNativeTerminalBinding15(a.Binding) != nil {
		return "", reject("invalid_native_responses_activation", "binding")
	}
	raw, err := json.Marshal(nativeResponsesActivationEnvelope15{a, nativeResponsesActivationMAC15(key, a)})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
func VerifyNativeResponsesActivation15(header, key string, r NativeResponsesPreflight15, result NativeResponsesPreflightResult15, now time.Time) (NativeResponsesActivation15, error) {
	fail := func() (NativeResponsesActivation15, error) {
		return NativeResponsesActivation15{}, reject("invalid_native_responses_activation", "binding")
	}
	if !nativeTerminalKey15(key) || len(header) == 0 || len(header) > 16<<10 || ValidateNativeResponsesPreflightResult15(r, result, now) != nil {
		return fail()
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(header)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != header {
		return fail()
	}
	value, err := parseResponsesEnvelope(raw, 16<<10)
	if err != nil {
		return fail()
	}
	root, ok := requiredResponsesFields(value, "version", "policy_version", "binding", "native_wire_sha256", "expires_at", "max_bytes", "mac")
	if !ok {
		return fail()
	}
	binding, ok := requiredResponsesFields(root["binding"], "public_id", "user_id", "api_key_id", "group_id", "account_id", "public_model", "resolved_model", "backend_id", "subject_digest", "credential_generation", "admission_id", "admission_binding_sha256", "request_wire_sha256", "nonce")
	if !ok {
		return fail()
	}
	for _, key := range []string{"user_id", "api_key_id", "group_id", "account_id"} {
		if _, ok := binding[key].(json.Number); !ok {
			return fail()
		}
	}
	var envelope nativeResponsesActivationEnvelope15
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&envelope) != nil {
		return fail()
	}
	a := envelope.NativeResponsesActivation15
	expected := NativeResponsesTerminalBinding15(r, result, a.Binding.PublicID)
	if a.Version != NativeResponsesContract15 || a.PolicyVersion != nativeResponsesPolicyForRequest15(r) || ValidateNativeTerminalBinding15(a.Binding) != nil || a.Binding != expected || a.NativeWireSHA256 != result.NativeWireSHA256 || a.ExpiresAt != result.ExpiresAt || a.MaxBytes != result.MaxBytes || !digest(envelope.MAC) || !hmac.Equal([]byte(envelope.MAC), []byte(nativeResponsesActivationMAC15(key, a))) {
		return fail()
	}
	return a, nil
}

// NativeResponsesActivationAdmission15 extracts a bounded lookup hint only.
// Callers MUST VerifyNativeResponsesActivation15 before using any authority.
func NativeResponsesActivationAdmission15(header string) (string, error) {
	if len(header) == 0 || len(header) > 16<<10 {
		return "", reject("invalid_native_responses_activation", "body")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(header)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != header {
		return "", reject("invalid_native_responses_activation", "body")
	}
	value, err := parseResponsesEnvelope(raw, 16<<10)
	if err != nil {
		return "", err
	}
	root, ok := value.(map[string]any)
	if !ok {
		return "", reject("invalid_native_responses_activation", "body")
	}
	binding, ok := root["binding"].(map[string]any)
	if !ok {
		return "", reject("invalid_native_responses_activation", "binding")
	}
	id, ok := binding["admission_id"].(string)
	if !ok || !strings.HasPrefix(id, "responses-") || !digest(strings.TrimPrefix(id, "responses-")) {
		return "", reject("invalid_native_responses_activation", "admission")
	}
	return id, nil
}

// NativeResponsesAck15 authenticates the exact native result and the public ID
// assigned after Begin. Main verifies it before releasing any streamed output.
func NativeResponsesAck15(key string, result NativeResponsesPreflightResult15, publicID string) (string, error) {
	if ValidateNativeResponsesServiceKey15(key) != nil || len(publicID) != len("resp_local_")+32 || !strings.HasPrefix(publicID, "resp_local_") || strings.ToLower(publicID) != publicID {
		return "", reject("invalid_native_responses_ack", "binding")
	}
	id, err := hex.DecodeString(strings.TrimPrefix(publicID, "resp_local_"))
	if err != nil || len(id) != 16 {
		return "", reject("invalid_native_responses_ack", "binding")
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	if _, err = DecodeNativeResponsesPreflightResult15(raw); err != nil {
		return "", err
	}
	raw, err = json.Marshal(struct {
		Result   NativeResponsesPreflightResult15 `json:"result"`
		PublicID string                           `json:"public_id"`
	}{result, publicID})
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte("bps-native-admitted/1\x00"))
	_, _ = mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil)), nil
}
func VerifyNativeResponsesAck15(key string, result NativeResponsesPreflightResult15, publicID, ack string) error {
	expected, err := NativeResponsesAck15(key, result, publicID)
	if err != nil {
		return err
	}
	if !digest(ack) || !hmac.Equal([]byte(expected), []byte(ack)) {
		return reject("invalid_native_responses_ack", "binding")
	}
	return nil
}

// DecodeNativeResponsesPolicyError15 admits only the fixed public policy envelope.
// Private diagnostics and arbitrary upstream messages never cross this boundary.
func DecodeNativeResponsesPolicyError15(status int, raw []byte) *ResponsesPreflightError {
	return decodeResponsesPolicyError(status, raw)
}
