package managedfiles

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
)

// RecoveryMissing is private evidence bound to the exact admitted manifest.
// It permits recovery evaluation; it never establishes file ownership.
type RecoveryMissing struct {
	Contract       string   `json:"contract"`
	Identity       Identity `json:"identity"`
	RequestID      string   `json:"request_id"`
	ManifestSHA256 string   `json:"manifest_sha256"`
	MissingRefs    []string `json:"missing_refs"`
}

func RecoveryManifestHash(m AdmitManifest) (string, error) {
	if err := ValidateManifest(m); err != nil {
		return "", err
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return "", reject("invalid_managed_recovery", "manifest")
	}
	canonical, err := Canonical(raw)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(canonical)
	return hex.EncodeToString(hash[:]), nil
}

func ParseRecoveryMissing(status int, raw []byte, m AdmitManifest) (*RecoveryMissing, error) {
	invalid := func() (*RecoveryMissing, error) {
		return nil, reject("invalid_managed_recovery", "response")
	}
	if status != http.StatusConflict {
		return invalid()
	}
	hash, err := RecoveryManifestHash(m)
	if err != nil {
		return invalid()
	}
	value, err := Parse(raw)
	if err != nil {
		return invalid()
	}
	root, ok := exactFields(value, "error")
	if !ok || len(root) != 1 {
		return invalid()
	}
	failure, ok := exactFields(root["error"], "type", "code", "param", "message", "recovery")
	if !ok || len(failure) != 5 || failure["type"] != "invalid_request_error" || failure["code"] != "managed_metadata_missing" || failure["param"] != nil || failure["message"] != "owner-scoped native metadata is unavailable" {
		return invalid()
	}
	recovery, ok := exactFields(failure["recovery"], "contract", "identity", "request_id", "manifest_sha256", "missing_refs")
	if !ok || len(recovery) != 5 || recovery["contract"] != "bps-metadata-recovery/1" || recovery["request_id"] != m.RequestID || recovery["manifest_sha256"] != hash {
		return invalid()
	}
	identity, ok := exactFields(recovery["identity"], "backend_id", "subject_fingerprint", "credential_generation")
	if !ok || len(identity) != 3 || identity["backend_id"] != m.Expected.BackendID || identity["subject_fingerprint"] != m.Expected.SubjectFingerprint || identity["credential_generation"] != m.Expected.CredentialGeneration {
		return invalid()
	}
	refs, ok := recovery["missing_refs"].([]any)
	if !ok || len(refs) == 0 || len(refs) > MaxUniqueFiles {
		return invalid()
	}
	existing := make(map[string]bool, len(m.Files))
	for _, file := range m.Files {
		if file.Mode == "existing" {
			existing[file.FileRef] = true
		}
	}
	missing := make([]string, 0, len(refs))
	for _, value := range refs {
		ref, ok := value.(string)
		if !ok || !existing[ref] || len(missing) > 0 && ref <= missing[len(missing)-1] {
			return invalid()
		}
		missing = append(missing, ref)
	}
	return &RecoveryMissing{Contract: "bps-metadata-recovery/1", Identity: m.Expected, RequestID: m.RequestID, ManifestSHA256: hash, MissingRefs: missing}, nil
}
