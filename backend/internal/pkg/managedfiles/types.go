// Package managedfiles defines the private, versioned main-to-gateway file contract.
// It never treats public client metadata as proof of resource ownership.
package managedfiles

import "encoding/json"

const (
	Contract                  = "bps-files/1"
	JSONProfile               = "bps-files-json/1"
	Prefix                    = "/internal/bps-files/v1/"
	MaxJSONBytes              = 64 << 20
	MaxJSONDepth              = 128
	MaxFileBytes        int64 = 200 << 20
	MaxMessageBytes     int64 = 500 << 20
	MaxMessagePositions       = 20
	MaxUniqueFiles            = 128
	ServiceKeyHeader          = "X-Bps-Service-Key"
	AdmissionHeader           = "X-Bps-File-Admission"
	OwnerRefHeader            = "X-Bps-Owner-Ref"
	GroupRefHeader            = "X-Bps-Group-Ref"
	BindingHeader             = "X-Bps-File-Binding"
)

// RecoveryFeatureHeader is negotiated only on the authenticated Describe route.
const RecoveryFeatureHeader = "X-Bps-Files-Recovery"

type Identity struct {
	BackendID            string `json:"backend_id"`
	SubjectFingerprint   string `json:"subject_fingerprint"`
	CredentialGeneration string `json:"credential_generation"`
}

type Limits struct {
	FileBytes             int64 `json:"file_bytes"`
	MessageBytes          int64 `json:"message_bytes"`
	MessagePositions      int   `json:"message_positions"`
	StagingBytes          int64 `json:"staging_bytes"`
	AdmissionBytes        int64 `json:"admission_bytes"`
	ConcurrentAdmissions  int   `json:"concurrent_admissions"`
	LiveAdmissions        int   `json:"live_admissions"`
	AdmissionTTLSeconds   int   `json:"admission_ttl_seconds"`
	ManifestBytes         int64 `json:"manifest_bytes"`
	UniqueFiles           int   `json:"unique_files"`
	FreeSpaceReserveBytes int64 `json:"free_space_reserve_bytes"`
	AdmitTimeoutSeconds   int   `json:"admit_timeout_seconds"`
	EnsureTimeoutSeconds  int   `json:"ensure_timeout_seconds"`
}

type Capabilities struct {
	BatchAdmit       bool `json:"batch_admit"`
	EnsureUpload     bool `json:"ensure_upload"`
	MetadataRecovery bool `json:"metadata_recovery"`
}

type DescribeResult struct {
	Contract string `json:"contract"`
	Identity
	JSONProfile  string       `json:"json_profile"`
	Limits       Limits       `json:"limits"`
	Capabilities Capabilities `json:"capabilities"`
}

type Position struct {
	Input int `json:"input"`
	Part  int `json:"part"`
}

type File struct {
	FileRef     string     `json:"file_ref"`
	Mode        string     `json:"mode"`
	Part        string     `json:"part,omitempty"`
	Filename    string     `json:"filename"`
	MIME        string     `json:"mime"`
	Bytes       int64      `json:"bytes"`
	SHA256      string     `json:"sha256"`
	RemoteID    string     `json:"remote_id,omitempty"`
	OperationID string     `json:"operation_id,omitempty"`
	Positions   []Position `json:"positions"`
}

type AdmitManifest struct {
	Contract               string          `json:"contract"`
	JSONProfile            string          `json:"json_profile"`
	RequestID              string          `json:"request_id"`
	Expected               Identity        `json:"expected"`
	OwnerRef               string          `json:"owner_ref"`
	GroupRef               string          `json:"group_ref"`
	SessionBindingContract string          `json:"session_binding_contract,omitempty"`
	ConversationRef        string          `json:"conversation_ref,omitempty"`
	RequestTemplate        json.RawMessage `json:"request_template"`
	Files                  []File          `json:"files"`
}

type AdmittedFile struct {
	FileRef string `json:"file_ref"`
	State   string `json:"state"`
}

type AdmitResult struct {
	Contract       string         `json:"contract"`
	JSONProfile    string         `json:"json_profile"`
	Identity       Identity       `json:"identity"`
	AdmissionID    string         `json:"admission_id"`
	ExpiresAt      int64          `json:"expires_at"`
	TemplateSHA256 string         `json:"template_sha256"`
	Ready          bool           `json:"ready"`
	WireSHA256     string         `json:"wire_sha256,omitempty"`
	BindingSHA256  string         `json:"binding_sha256,omitempty"`
	Files          []AdmittedFile `json:"files"`
}

type EnsureRequest struct {
	Contract    string   `json:"contract"`
	Expected    Identity `json:"expected"`
	AdmissionID string   `json:"admission_id"`
	FileRef     string   `json:"file_ref"`
	OperationID string   `json:"operation_id"`
}

type EnsureResult struct {
	Contract      string   `json:"contract"`
	Identity      Identity `json:"identity"`
	OperationID   string   `json:"operation_id"`
	FileRef       string   `json:"file_ref"`
	RemoteID      string   `json:"remote_id"`
	Bytes         int64    `json:"bytes"`
	SHA256        string   `json:"sha256"`
	Filename      string   `json:"filename"`
	MIME          string   `json:"mime"`
	InputTokens   int64    `json:"input_tokens"`
	Ready         bool     `json:"ready"`
	WireSHA256    string   `json:"wire_sha256,omitempty"`
	BindingSHA256 string   `json:"binding_sha256,omitempty"`
}

// Error contains only fixed codes and structural paths, never request values.
type Error struct {
	Code  string `json:"code"`
	Param string `json:"param,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Param }

func reject(code, param string) error { return &Error{Code: code, Param: param} }
