package managedfiles

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	ResponsesContract         = "bps-responses/1"
	ResponsesPrefix           = "/internal/bps-responses/v1/"
	ResponsesAdmissionHeader  = "X-Bps-Responses-Admission"
	ResponsesWireHeader       = "X-Bps-Responses-Wire-SHA256"
	ResponsesBindingHeader    = "X-Bps-Responses-Binding-SHA256"
	ResponsesAdmittedHeader   = "X-Bps-Responses-Admitted"
	maxResponsesEnvelopeBytes = 2*MaxJSONBytes + (16 << 10)
)

type ResponsesPreflightRequest struct {
	Version       string          `json:"version"`
	Expected      Identity        `json:"expected"`
	OwnerRef      string          `json:"owner_ref"`
	GroupRef      string          `json:"group_ref"`
	ClientRequest json.RawMessage `json:"client_request"`
	WireRequest   json.RawMessage `json:"wire_request"`
}

type ResponsesPreflightResult struct {
	Version       string   `json:"version"`
	Identity      Identity `json:"identity"`
	PolicyVersion string   `json:"policy_version"`
	AdmissionID   string   `json:"admission_id"`
	ExpiresAt     int64    `json:"expires_at"`
	WireSHA256    string   `json:"wire_sha256"`
	BindingSHA256 string   `json:"binding_sha256"`
}

// ResponsesPreflightError contains only the gateway's fixed public policy
// contract. Transport diagnostics and raw upstream messages are never retained.
type ResponsesPreflightError struct {
	Status                     int
	Type, Code, Param, Message string
}

func (e *ResponsesPreflightError) Error() string {
	return "responses preflight: request policy rejected"
}

const responsesPolicyMessage = "The requested capability is not supported by this gateway."

func decodeResponsesPolicyError(status int, raw []byte) *ResponsesPreflightError {
	if status != 400 {
		return nil
	}
	value, err := parseResponsesEnvelope(raw, 16<<10)
	if err != nil {
		return nil
	}
	root, ok := requiredResponsesFields(value, "error")
	if !ok {
		return nil
	}
	fields, ok := requiredResponsesFields(root["error"], "type", "code", "param", "message")
	if !ok || !responsesStringFields(fields, "type", "code", "param", "message") || fields["type"] != "invalid_request_error" || fields["message"] != responsesPolicyMessage {
		return nil
	}
	code := fields["code"].(string)
	switch code {
	case "unsupported_parameter", "unsupported_tool", "unsupported_tool_choice", "invalid_parameter":
	default:
		return nil
	}
	param := fields["param"].(string)
	if len(param) == 0 || len(param) > 128 {
		return nil
	}
	for _, c := range param {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '[' || c == ']') {
			return nil
		}
	}
	return &ResponsesPreflightError{Status: status, Type: "invalid_request_error", Code: code, Param: param, Message: responsesPolicyMessage}
}

// EncodeResponsesPreflight embeds both request objects verbatim. RawMessage's
// normal MarshalJSON path compacts whitespace and therefore changes wire hashes.
func EncodeResponsesPreflight(request ResponsesPreflightRequest) ([]byte, error) {
	if err := validateResponsesPreflight(request); err != nil {
		return nil, err
	}
	metadata := struct {
		Version  string   `json:"version"`
		Expected Identity `json:"expected"`
		OwnerRef string   `json:"owner_ref"`
		GroupRef string   `json:"group_ref"`
	}{request.Version, request.Expected, request.OwnerRef, request.GroupRef}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return nil, reject("invalid_responses_preflight", "body")
	}
	encoded = append(encoded[:len(encoded)-1], []byte(`,"client_request":`)...)
	encoded = append(encoded, request.ClientRequest...)
	encoded = append(encoded, []byte(`,"wire_request":`)...)
	encoded = append(encoded, request.WireRequest...)
	encoded = append(encoded, '}')
	if len(encoded) > maxResponsesEnvelopeBytes {
		return nil, reject("responses_preflight_limit", "body")
	}
	return encoded, nil
}

// DecodeResponsesPreflight checks ambiguous JSON before ordinary DTO decoding,
// then decodes the original bytes so the nested requests are never canonicalized.
func DecodeResponsesPreflight(raw []byte) (ResponsesPreflightRequest, error) {
	value, err := parseResponsesEnvelope(raw, maxResponsesEnvelopeBytes)
	if err != nil {
		return ResponsesPreflightRequest{}, err
	}
	root, ok := requiredResponsesFields(value, "version", "expected", "owner_ref", "group_ref", "client_request", "wire_request")
	if !ok || !responsesStringFields(root, "version", "owner_ref", "group_ref") || !responsesIdentityFields(root["expected"]) {
		return ResponsesPreflightRequest{}, reject("invalid_responses_preflight", "body")
	}
	var result ResponsesPreflightRequest
	if json.Unmarshal(raw, &result) != nil {
		return ResponsesPreflightRequest{}, reject("invalid_responses_preflight", "body")
	}
	if err := validateResponsesPreflight(result); err != nil {
		return ResponsesPreflightRequest{}, err
	}
	return result, nil
}

func validateResponsesPreflight(request ResponsesPreflightRequest) error {
	if request.Version != ResponsesContract {
		return reject("unsupported_responses_contract", "version")
	}
	if err := ValidateIdentity(request.Expected); err != nil {
		return err
	}
	for _, field := range []struct{ name, value string }{{"owner_ref", request.OwnerRef}, {"group_ref", request.GroupRef}} {
		if !safeOpaque(field.value, 256) {
			return reject("invalid_responses_preflight", field.name)
		}
	}
	for _, field := range []struct {
		name string
		raw  []byte
	}{{"client_request", request.ClientRequest}, {"wire_request", request.WireRequest}} {
		if !bytes.Equal(bytes.TrimSpace(field.raw), field.raw) {
			return reject("invalid_responses_preflight", field.name)
		}
		value, err := Parse(field.raw)
		if err != nil {
			return err
		}
		object, ok := value.(map[string]any)
		if !ok || object["store"] != false {
			return reject("invalid_responses_preflight", field.name)
		}
	}
	return nil
}

// The envelope may contain two individually bounded requests. Reuse the shared
// strict scanner without raising the existing Files contract's 64 MiB limit.
func parseResponsesEnvelope(raw []byte, limit int) (any, error) {
	if len(raw) == 0 || len(raw) > limit || !utf8.Valid(raw) || !validUnicodeEscapes(raw) {
		return nil, reject("invalid_responses_preflight", "body")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	value, err := parseValue(dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, reject("invalid_responses_preflight", "body")
	}
	return value, nil
}

func requiredResponsesFields(value any, names ...string) (map[string]any, bool) {
	object, ok := exactFields(value, names...)
	return object, ok && len(object) == len(names)
}

func responsesStringFields(object map[string]any, names ...string) bool {
	for _, name := range names {
		if _, ok := object[name].(string); !ok {
			return false
		}
	}
	return true
}

func responsesIdentityFields(value any) bool {
	object, ok := requiredResponsesFields(value, "backend_id", "subject_fingerprint", "credential_generation")
	return ok && responsesStringFields(object, "backend_id", "subject_fingerprint", "credential_generation")
}

// ResponsesWireSHA256 hashes the exact inference body, without the Files
// canonicalization or domain prefix.
func ResponsesWireSHA256(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func DecodeResponsesPreflightResult(raw []byte) (ResponsesPreflightResult, error) {
	value, err := parseResponsesEnvelope(raw, 16<<10)
	if err != nil {
		return ResponsesPreflightResult{}, err
	}
	root, ok := requiredResponsesFields(value, "version", "identity", "policy_version", "admission_id", "expires_at", "wire_sha256", "binding_sha256")
	if !ok || !responsesStringFields(root, "version", "policy_version", "admission_id", "wire_sha256", "binding_sha256") || !responsesIdentityFields(root["identity"]) {
		return ResponsesPreflightResult{}, reject("invalid_responses_preflight_result", "body")
	}
	expiry, ok := root["expires_at"].(json.Number)
	if !ok || !unsignedInteger(expiry.String()) {
		return ResponsesPreflightResult{}, reject("invalid_responses_preflight_result", "expires_at")
	}
	var result ResponsesPreflightResult
	if json.Unmarshal(raw, &result) != nil || result.ExpiresAt <= 0 {
		return ResponsesPreflightResult{}, reject("invalid_responses_preflight_result", "body")
	}
	if result.Version != ResponsesContract {
		return ResponsesPreflightResult{}, reject("unsupported_responses_contract", "version")
	}
	if err := ValidateIdentity(result.Identity); err != nil {
		return ResponsesPreflightResult{}, err
	}
	if !safeOpaque(result.PolicyVersion, 256) || !strings.HasPrefix(result.AdmissionID, "responses-") || !digest(strings.TrimPrefix(result.AdmissionID, "responses-")) || !digest(result.WireSHA256) || !digest(result.BindingSHA256) {
		return ResponsesPreflightResult{}, reject("invalid_responses_preflight_result", "body")
	}
	return result, nil
}

func ResponsesBindingSHA256(request ResponsesPreflightRequest, policyVersion string, wireSHA256 string) (string, error) {
	if err := validateResponsesPreflight(request); err != nil {
		return "", err
	}
	if !safeOpaque(policyVersion, 256) || !digest(wireSHA256) || wireSHA256 != ResponsesWireSHA256(request.WireRequest) {
		return "", reject("invalid_responses_binding", "wire_sha256")
	}
	fields := []string{ResponsesContract, request.OwnerRef, request.GroupRef, request.Expected.BackendID, request.Expected.SubjectFingerprint, request.Expected.CredentialGeneration, policyVersion, wireSHA256}
	return ResponsesWireSHA256([]byte(strings.Join(fields, "\x00"))), nil
}

func ResponsesAck(serviceKey string, result ResponsesPreflightResult) string {
	fields := []string{result.Version, result.AdmissionID, result.Identity.BackendID, result.Identity.SubjectFingerprint, result.Identity.CredentialGeneration, result.WireSHA256, result.BindingSHA256}
	mac := hmac.New(sha256.New, []byte(serviceKey))
	_, _ = mac.Write([]byte(strings.Join(fields, "\x00")))
	return hex.EncodeToString(mac.Sum(nil))
}

func VerifyResponsesAck(serviceKey string, result ResponsesPreflightResult, actual string) bool {
	return digest(actual) && hmac.Equal([]byte(ResponsesAck(serviceKey, result)), []byte(actual))
}
