package managedfiles

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// DecodeManifest validates duplicate keys and required positional integers
// before ordinary DTO decoding can erase their spelling or presence.
func DecodeManifest(raw []byte) (AdmitManifest, error) {
	value, err := Parse(raw)
	if err != nil {
		return AdmitManifest{}, err
	}
	if !manifestExactFields(value) {
		return AdmitManifest{}, reject("invalid_managed_manifest", "manifest")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return AdmitManifest{}, reject("invalid_managed_manifest", "manifest")
	}
	var result AdmitManifest
	dec := json.NewDecoder(bytes.NewReader(canonical))
	dec.DisallowUnknownFields()
	if dec.Decode(&result) != nil {
		return AdmitManifest{}, reject("invalid_managed_manifest", "manifest")
	}
	if err := ValidateManifest(result); err != nil {
		return AdmitManifest{}, err
	}
	return result, nil
}

// encoding/json accepts case-insensitive field aliases and null strings. Both
// would disagree with the exact wire schema, even with DisallowUnknownFields.
func manifestExactFields(value any) bool {
	root, ok := exactFields(value, "contract", "json_profile", "request_id", "expected", "owner_ref", "group_ref", "request_template", "files", "session_binding_contract", "conversation_ref")
	if !ok {
		return false
	}
	contract, hasContract := root["session_binding_contract"]
	conversation, hasConversation := root["conversation_ref"]
	if hasContract != hasConversation {
		return false
	}
	if hasContract {
		contractText, contractOK := contract.(string)
		conversationText, conversationOK := conversation.(string)
		if !contractOK || !conversationOK || contractText != SessionBindingContract || !safeOpaque(conversationText, 256) {
			return false
		}
	}
	if _, ok := exactFields(root["expected"], "backend_id", "subject_fingerprint", "credential_generation"); !ok {
		return false
	}
	files, ok := root["files"].([]any)
	if !ok {
		return false
	}
	for _, value := range files {
		file, ok := exactFields(value, "file_ref", "mode", "part", "filename", "mime", "bytes", "sha256", "remote_id", "operation_id", "positions")
		if !ok {
			return false
		}
		for _, key := range []string{"part", "remote_id", "operation_id"} {
			if v, exists := file[key]; exists {
				if _, ok := v.(string); !ok {
					return false
				}
			}
		}
	}
	return true
}

func exactFields(value any, names ...string) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	for key := range object {
		known := false
		for _, name := range names {
			if key == name {
				known = true
				break
			}
		}
		if !known {
			return nil, false
		}
	}
	return object, true
}

func (p *Position) UnmarshalJSON(raw []byte) error {
	v, err := Parse(raw)
	if err != nil {
		return err
	}
	object, ok := v.(map[string]any)
	if !ok || len(object) != 2 {
		return reject("invalid_managed_position", "positions")
	}
	values := make([]int, 2)
	for i, key := range []string{"input", "part"} {
		n, ok := object[key].(json.Number)
		if !ok || !unsignedInteger(n.String()) {
			return reject("invalid_managed_position", "positions")
		}
		values[i], err = strconv.Atoi(n.String())
		if err != nil {
			return reject("invalid_managed_position", "positions")
		}
	}
	p.Input, p.Part = values[0], values[1]
	return nil
}

func unsignedInteger(s string) bool {
	if s == "" || len(s) > 1 && s[0] == '0' {
		return false
	}
	for _, b := range []byte(s) {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}

func safeOpaque(s string, limit int) bool {
	return s != "" && len(s) <= limit && strings.TrimSpace(s) == s && utf8.ValidString(s) && strings.IndexFunc(s, unicode.IsControl) < 0
}

func digest(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func ValidateIdentity(identity Identity) error {
	if !safeOpaque(identity.BackendID, 256) {
		return reject("invalid_managed_identity", "expected.backend_id")
	}
	if !strings.HasPrefix(identity.SubjectFingerprint, "sha256:") || !digest(strings.TrimPrefix(identity.SubjectFingerprint, "sha256:")) {
		return reject("invalid_managed_identity", "expected.subject_fingerprint")
	}
	if !safeOpaque(identity.CredentialGeneration, 256) {
		return reject("invalid_managed_identity", "expected.credential_generation")
	}
	return nil
}

// ValidRemoteID is the existing BPS attachment identifier shape, not ownership.
func ValidRemoteID(id string) bool {
	if !strings.HasPrefix(id, "file-") || len(id) < 6 || len(id) > 256 {
		return false
	}
	for _, b := range []byte(id[5:]) {
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-' || b == '_') {
			return false
		}
	}
	return true
}

func ValidateManifest(m AdmitManifest) error {
	_, err := manifestTemplate(m)
	return err
}

func manifestTemplate(m AdmitManifest) (map[string]any, error) {
	if m.Contract != Contract || m.JSONProfile != JSONProfile {
		return nil, reject("unsupported_managed_contract", "contract")
	}
	if err := ValidateIdentity(m.Expected); err != nil {
		return nil, err
	}
	for _, field := range []struct{ name, value string }{{"request_id", m.RequestID}, {"owner_ref", m.OwnerRef}, {"group_ref", m.GroupRef}} {
		if !safeOpaque(field.value, 256) {
			return nil, reject("invalid_managed_manifest", field.name)
		}
	}
	if (m.SessionBindingContract == "") != (m.ConversationRef == "") || m.SessionBindingContract != "" && (m.SessionBindingContract != SessionBindingContract || !safeOpaque(m.ConversationRef, 256)) {
		return nil, reject("invalid_session_binding", "conversation_ref")
	}
	if len(m.Files) == 0 || len(m.Files) > MaxUniqueFiles {
		return nil, reject("invalid_managed_manifest", "files")
	}
	v, err := Parse(m.RequestTemplate)
	if err != nil {
		return nil, err
	}
	root, ok := v.(map[string]any)
	if !ok {
		return nil, reject("invalid_managed_manifest", "request_template")
	}
	input, ok := root["input"].([]any)
	if !ok {
		return nil, reject("invalid_managed_manifest", "request_template.input")
	}
	toolSources, err := ValidateToolFileSources(input)
	if err != nil {
		return nil, err
	}
	actual := make(map[Position]string)
	for i, item := range input {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := object["type"].(string)
		_, role := object["role"].(string)
		if kind != "message" && !(kind == "" && role) {
			if kind == "input_file" || kind == "input_image" {
				return nil, reject("unsupported_managed_input", fmt.Sprintf("request_template.input[%d]", i))
			}
			if _, ok := toolSources[i]; !ok {
				continue
			}
		}

		parts, field := TypedFileContent(object)
		if field == "" {
			continue
		}
		for j, value := range parts {
			part, ok := value.(map[string]any)
			if !ok {
				continue
			}
			if part["type"] == "input_image" {
				for key := range part {
					if strings.EqualFold(key, "file_id") {
						return nil, reject("unsupported_managed_input", fmt.Sprintf("request_template.input[%d].%s[%d]", i, field, j))
					}
				}
				continue
			}
			if part["type"] != "input_file" {
				continue
			}
			if _, exists := part["file_data"]; exists {
				return nil, reject("unsupported_managed_input", fmt.Sprintf("request_template.input[%d].%s[%d].file_data", i, field, j))
			}
			if _, exists := part["file_url"]; exists {
				return nil, reject("unsupported_managed_input", fmt.Sprintf("request_template.input[%d].%s[%d].file_url", i, field, j))
			}
			id, ok := part["file_id"].(string)
			if !ok || !ValidRemoteID(id) {
				return nil, reject("invalid_managed_position", fmt.Sprintf("request_template.input[%d].%s[%d].file_id", i, field, j))
			}
			actual[Position{Input: i, Part: j}] = id
		}
	}
	seenRefs, seenParts := make(map[string]bool), make(map[string]bool)
	seenPositions := make(map[Position]bool)
	type budget struct {
		count int
		bytes int64
	}
	budgets := make(map[int]budget)
	for i, file := range m.Files {
		param := fmt.Sprintf("files[%d]", i)
		if !ValidRemoteID(file.FileRef) || seenRefs[file.FileRef] {
			return nil, reject("invalid_managed_manifest", param+".file_ref")
		}
		seenRefs[file.FileRef] = true
		if !safeOpaque(file.Filename, 1024) || file.Filename == "." || file.Filename == ".." || strings.ContainsAny(file.Filename, "/\\:") {
			return nil, reject("invalid_managed_manifest", param+".filename")
		}
		media, params, err := mime.ParseMediaType(file.MIME)
		if err != nil || len(params) > 0 || media != file.MIME || strings.Contains(media, "*") || len(media) > 1024 || strings.Count(media, "/") != 1 {
			return nil, reject("invalid_managed_manifest", param+".mime")
		}
		if file.Bytes <= 0 || file.Bytes > MaxFileBytes {
			return nil, reject("invalid_managed_manifest", param+".bytes")
		}
		if !digest(file.SHA256) {
			return nil, reject("invalid_managed_manifest", param+".sha256")
		}
		switch file.Mode {
		case "upload":
			if !safeOpaque(file.Part, 64) || file.Part == "manifest" || seenParts[file.Part] || file.RemoteID != "" || file.OperationID != "" {
				return nil, reject("invalid_managed_manifest", param)
			}
			seenParts[file.Part] = true
		case "existing":
			if file.Part != "" || !ValidRemoteID(file.RemoteID) || !safeOpaque(file.OperationID, 256) {
				return nil, reject("invalid_managed_manifest", param)
			}
		default:
			return nil, reject("invalid_managed_manifest", param+".mode")
		}
		if len(file.Positions) == 0 {
			return nil, reject("invalid_managed_position", param+".positions")
		}
		for _, pos := range file.Positions {
			if pos.Input < 0 || pos.Part < 0 || seenPositions[pos] || actual[pos] != file.FileRef {
				return nil, reject("invalid_managed_position", param+".positions")
			}
			seenPositions[pos] = true
			b := budgets[pos.Input]
			if b.count >= MaxMessagePositions || file.Bytes > MaxMessageBytes-b.bytes {
				return nil, reject("managed_file_budget", param+".positions")
			}
			b.count++
			b.bytes += file.Bytes
			budgets[pos.Input] = b
		}
	}
	if len(actual) != len(seenPositions) {
		return nil, reject("invalid_managed_position", "files")
	}
	return root, nil
}

// ReplacedWire returns caller-independent canonical bytes. Only exact typed
// locations may change; arguments and all other JSON remain untouched.
func ReplacedWire(m AdmitManifest, remoteIDs map[string]string) ([]byte, error) {
	root, err := manifestTemplate(m)
	if err != nil {
		return nil, err
	}
	if len(remoteIDs) != len(m.Files) {
		return nil, reject("invalid_managed_binding", "files")
	}
	for _, file := range m.Files {
		remote := remoteIDs[file.FileRef]
		if !ValidRemoteID(remote) || file.Mode == "existing" && remote != file.RemoteID {
			return nil, reject("invalid_managed_binding", "files")
		}
		for _, pos := range file.Positions {
			part, err := FilePartAt(root, pos)
			if err != nil {
				return nil, err
			}
			part["file_id"] = remote
		}
	}
	result, err := json.Marshal(root)
	if err != nil {
		return nil, reject("invalid_managed_json", "request_template")
	}
	return result, nil
}
