package managedfiles

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const NativeAttachmentContract15 = "bps-native-attachments/1"
const NativeAttachmentAttemptContract15 = "bps-native-attachment-attempt/1"
const NativeAttachmentMaxSnapshotBytes15 = 1 << 20

// NativeAttachmentWireCopy15 contains stable confirmed copy facts only.
// A decoded value is never permission to access a file or send a request.
type NativeAttachmentWireCopy15 struct {
	PublicFileID         string `json:"public_file_id"`
	FileVersion          int64  `json:"file_version"`
	SHA256               string `json:"sha256"`
	Filename             string `json:"filename"`
	MIME                 string `json:"mime"`
	Bytes                int64  `json:"bytes"`
	AccountID            int64  `json:"account_id"`
	BackendID            string `json:"backend_id"`
	SubjectDigest        string `json:"subject_digest"`
	CredentialGeneration string `json:"credential_generation"`
	OperationID          string `json:"operation_id"`
	RemoteID             string `json:"remote_id"`
	InputTokens          int64  `json:"input_tokens"`
}

// NativeAttachmentPosition15 uses absolute expanded input indices. CallItem is
// the complete original call; opaque arguments/custom input remain untouched.
type NativeAttachmentPosition15 struct {
	Turn       int             `json:"turn"`
	Input      int             `json:"input"`
	Field      string          `json:"field"`
	Part       int             `json:"part"`
	SourceKind string          `json:"source_kind"`
	CallID     string          `json:"call_id"`
	CallInput  int             `json:"call_input"`
	CallItem   json.RawMessage `json:"call_item"`
	CopyIndex  int             `json:"copy_index"`
}

type NativeAttachmentSnapshot15 struct {
	Version   string                       `json:"version"`
	Copies    []NativeAttachmentWireCopy15 `json:"copies"`
	Positions []NativeAttachmentPosition15 `json:"positions"`
}

// NativeAttachmentFence15 belongs to a single fresh attempt, never recovery.
type NativeAttachmentFence15 struct {
	CopyIndex      int    `json:"copy_index"`
	BindingVersion int64  `json:"binding_version"`
	LeaseToken     string `json:"lease_token"`
	LeaseUntil     int64  `json:"lease_until"`
}

// NativeAttachmentAttempt15 is deliberately separate from durable copy facts.
// It has no authority encoder/verifier until wired into native admission.
type NativeAttachmentAttempt15 struct {
	Version              string                    `json:"version"`
	SnapshotSHA256       string                    `json:"snapshot_sha256"`
	UserID               int64                     `json:"user_id"`
	GroupID              int64                     `json:"group_id"`
	APIKeyID             int64                     `json:"api_key_id"`
	AccountID            int64                     `json:"account_id"`
	BackendID            string                    `json:"backend_id"`
	SubjectDigest        string                    `json:"subject_digest"`
	CredentialGeneration string                    `json:"credential_generation"`
	RequestWireSHA256    string                    `json:"request_wire_sha256"`
	Nonce                string                    `json:"nonce"`
	ExpiresAt            int64                     `json:"expires_at"`
	Fences               []NativeAttachmentFence15 `json:"fences"`
}

func DecodeNativeAttachmentSnapshot15(raw []byte) (NativeAttachmentSnapshot15, error) {
	fail := func() (NativeAttachmentSnapshot15, error) {
		return NativeAttachmentSnapshot15{}, errors.New("invalid_native_attachment_snapshot")
	}
	if len(raw) == 0 || len(raw) > NativeAttachmentMaxSnapshotBytes15 {
		return fail()
	}
	value, err := nativeAttachmentParse15(raw)
	if err != nil || !nativeAttachmentShape15(value) {
		return fail()
	}
	var snapshot NativeAttachmentSnapshot15
	if json.Unmarshal(raw, &snapshot) != nil || !nativeAttachmentValid15(snapshot) {
		return fail()
	}
	return snapshot, nil
}

func CanonicalNativeAttachmentSnapshot15(snapshot NativeAttachmentSnapshot15) ([]byte, error) {
	if !nativeAttachmentValid15(snapshot) {
		return nil, errors.New("invalid_native_attachment_snapshot")
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return nil, errors.New("invalid_native_attachment_snapshot")
	}
	if _, err = DecodeNativeAttachmentSnapshot15(raw); err != nil {
		return nil, err
	}
	value, err := nativeAttachmentParse15(raw)
	if err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func NativeAttachmentSnapshotSHA25615(snapshot NativeAttachmentSnapshot15) (string, error) {
	raw, err := CanonicalNativeAttachmentSnapshot15(snapshot)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(NativeAttachmentContract15+"\x00"), raw...))
	return hex.EncodeToString(sum[:]), nil
}

func NativeAttachmentCopySHA25615(copy NativeAttachmentWireCopy15) (string, error) {
	if !nativeAttachmentCopyValid15(copy) {
		return "", errors.New("invalid_native_attachment_snapshot")
	}
	raw, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	value, err := nativeAttachmentParse15(raw)
	if err != nil {
		return "", err
	}
	raw, err = json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte("bps-native-attachment-copy/1\x00"), raw...))
	return hex.EncodeToString(sum[:]), nil
}

func nativeAttachmentObject15(value any, fields string) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	keys := strings.Fields(fields)
	if !ok || len(object) != len(keys) {
		return nil, false
	}
	for _, key := range keys {
		if _, exists := object[key]; !exists {
			return nil, false
		}
	}
	return object, true
}

func nativeAttachmentInteger15(value any, negativeOne bool) bool {
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	text := number.String()
	if negativeOne && text == "-1" {
		return true
	}
	if text == "" || len(text) > 1 && text[0] == '0' {
		return false
	}
	for _, ch := range []byte(text) {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	_, err := strconv.ParseInt(text, 10, 64)
	return err == nil
}

func nativeAttachmentShape15(value any) bool {
	root, ok := nativeAttachmentObject15(value, "version copies positions")
	if !ok {
		return false
	}
	if _, ok = root["version"].(string); !ok {
		return false
	}
	copies, ok := root["copies"].([]any)
	if !ok || len(copies) == 0 || len(copies) > 128 {
		return false
	}
	positions, ok := root["positions"].([]any)
	if !ok || len(positions) == 0 || len(positions) > 2560 {
		return false
	}
	for _, value := range copies {
		copy, ok := nativeAttachmentObject15(value, "public_file_id file_version sha256 filename mime bytes account_id backend_id subject_digest credential_generation operation_id remote_id input_tokens")
		if !ok {
			return false
		}
		for _, key := range []string{"public_file_id", "sha256", "filename", "mime", "backend_id", "subject_digest", "credential_generation", "operation_id", "remote_id"} {
			if _, ok := copy[key].(string); !ok {
				return false
			}
		}
		for _, key := range []string{"file_version", "bytes", "account_id", "input_tokens"} {
			if !nativeAttachmentInteger15(copy[key], false) {
				return false
			}
		}
	}
	for _, value := range positions {
		pos, ok := nativeAttachmentObject15(value, "turn input field part source_kind call_id call_input call_item copy_index")
		if !ok {
			return false
		}
		for _, key := range []string{"field", "source_kind", "call_id"} {
			if _, ok := pos[key].(string); !ok {
				return false
			}
		}
		for _, key := range []string{"turn", "input", "part", "copy_index"} {
			if !nativeAttachmentInteger15(pos[key], false) {
				return false
			}
		}
		if !nativeAttachmentInteger15(pos["call_input"], true) {
			return false
		}
	}
	return true
}

func nativeAttachmentOpaque15(text string, limit int) bool {
	return text != "" && len(text) <= limit && strings.TrimSpace(text) == text && utf8.ValidString(text) && strings.IndexFunc(text, unicode.IsControl) < 0
}
func nativeAttachmentDigest15(text string) bool {
	if len(text) != 64 || strings.ToLower(text) != text {
		return false
	}
	_, err := hex.DecodeString(text)
	return err == nil
}
func nativeAttachmentFileID15(text string) bool {
	if !strings.HasPrefix(text, "file-") || len(text) < 6 || len(text) > 256 {
		return false
	}
	for _, ch := range []byte(text[5:]) {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
			return false
		}
	}
	return true
}
func nativeAttachmentCopyValid15(copy NativeAttachmentWireCopy15) bool {
	if !nativeAttachmentFileID15(copy.PublicFileID) || copy.FileVersion <= 0 || !nativeAttachmentDigest15(copy.SHA256) || !nativeAttachmentDigest15(copy.SubjectDigest) || copy.Bytes <= 0 || copy.Bytes > 200<<20 || copy.AccountID <= 0 || copy.InputTokens < 0 || !nativeAttachmentFileID15(copy.RemoteID) {
		return false
	}
	if !nativeAttachmentOpaque15(copy.BackendID, 256) || !nativeAttachmentOpaque15(copy.CredentialGeneration, 256) || !nativeAttachmentOpaque15(copy.OperationID, 256) {
		return false
	}
	if !nativeAttachmentOpaque15(copy.Filename, 1024) || copy.Filename == "." || copy.Filename == ".." || strings.ContainsAny(copy.Filename, "/\\:") {
		return false
	}
	media, params, err := mime.ParseMediaType(copy.MIME)
	return err == nil && len(params) == 0 && media == copy.MIME && len(media) <= 1024 && strings.Count(media, "/") == 1 && !strings.Contains(media, "*")
}

func nativeAttachmentCallValid15(position NativeAttachmentPosition15) bool {
	if position.SourceKind == "message" {
		return position.Field == "content" && position.CallID == "" && position.CallInput == -1 && bytes.Equal(bytes.TrimSpace(position.CallItem), []byte("null"))
	}
	kind, opaqueField := "function_call", "arguments"
	if position.SourceKind == "custom_tool_call_output" {
		kind, opaqueField = "custom_tool_call", "input"
	} else if position.SourceKind != "function_call_output" {
		return false
	}
	if position.Field != "output" || position.CallInput < 0 || position.CallInput >= position.Input || !nativeAttachmentOpaque15(position.CallID, 256) {
		return false
	}
	value, err := nativeAttachmentParse15(position.CallItem)
	if err != nil {
		return false
	}
	call, ok := value.(map[string]any)
	if !ok || len(call) < 4 || call["type"] != kind || call["call_id"] != position.CallID {
		return false
	}
	for key, value := range call {
		switch key {
		case "encrypted_function_args":
			values, ok := value.([]any)
			if !ok || len(values) != 0 {
				return false
			}
		case "type", "call_id", "name", opaqueField, "id", "namespace", "status":
			if _, ok := value.(string); !ok {
				return false
			}
		default:
			return false
		}
	}
	if name, ok := call["name"].(string); !ok || !nativeAttachmentOpaque15(name, 256) {
		return false
	}
	if _, ok := call[opaqueField].(string); !ok {
		return false
	}
	if status, exists := call["status"]; exists && status != "completed" {
		return false
	}
	for _, key := range []string{"id", "namespace"} {
		if value, exists := call[key]; exists && !nativeAttachmentOpaque15(value.(string), 256) {
			return false
		}
	}
	return true
}

func nativeAttachmentValid15(snapshot NativeAttachmentSnapshot15) bool {
	if snapshot.Version != NativeAttachmentContract15 || len(snapshot.Copies) == 0 || len(snapshot.Copies) > 128 || len(snapshot.Positions) == 0 || len(snapshot.Positions) > 2560 {
		return false
	}
	operations := map[string]bool{}
	for i, copy := range snapshot.Copies {
		if !nativeAttachmentCopyValid15(copy) || i > 0 && snapshot.Copies[i-1].PublicFileID >= copy.PublicFileID || operations[copy.OperationID] {
			return false
		}
		operations[copy.OperationID] = true
		first := snapshot.Copies[0]
		if copy.AccountID != first.AccountID || copy.BackendID != first.BackendID || copy.SubjectDigest != first.SubjectDigest || copy.CredentialGeneration != first.CredentialGeneration {
			return false
		}
	}
	used := make([]bool, len(snapshot.Copies))
	counts := map[int]int{}
	totals := map[int]int64{}
	for i, pos := range snapshot.Positions {
		if pos.Turn < 0 || pos.Input < 0 || pos.Part < 0 || pos.CopyIndex < 0 || pos.CopyIndex >= len(snapshot.Copies) || !nativeAttachmentCallValid15(pos) {
			return false
		}
		if i > 0 {
			previous := snapshot.Positions[i-1]
			if previous.Input > pos.Input || previous.Input == pos.Input && previous.Part >= pos.Part {
				return false
			}
		}
		counts[pos.Input]++
		size := snapshot.Copies[pos.CopyIndex].Bytes
		if counts[pos.Input] > 20 || totals[pos.Input] > (500<<20)-size {
			return false
		}
		totals[pos.Input] += size
		used[pos.CopyIndex] = true
	}
	for _, exists := range used {
		if !exists {
			return false
		}
	}
	return true
}

func nativeAttachmentParse15(raw []byte) (any, error) { return Parse(raw) }
