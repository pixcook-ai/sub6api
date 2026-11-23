package managedfiles

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

type BoundRequest struct {
	Wire           json.RawMessage `json:"wire"`
	TemplateSHA256 string          `json:"template_sha256"`
	WireSHA256     string          `json:"wire_sha256"`
	BindingSHA256  string          `json:"binding_sha256"`
}

func domainHash(domain string, raw []byte) (string, error) {
	canonical, err := Canonical(raw)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = h.Write([]byte(domain))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func TemplateHash(raw []byte) (string, error) { return domainHash("bps-files-template/1", raw) }
func WireHash(raw []byte) (string, error)     { return domainHash("bps-files-wire/1", raw) }

type boundFile struct {
	FileRef     string     `json:"file_ref"`
	Filename    string     `json:"filename"`
	MIME        string     `json:"mime"`
	Bytes       int64      `json:"bytes"`
	SHA256      string     `json:"sha256"`
	Positions   []Position `json:"positions"`
	RemoteID    string     `json:"remote_id"`
	OperationID string     `json:"operation_id"`
}

// Bind validates successful upload metadata and combines it with confirmed
// existing copies. It recomputes all hashes rather than trusting response claims.
// Existing files need no Ensure request; their confirmed operation is in File.
func Bind(m AdmitManifest, results []EnsureResult) (BoundRequest, error) {
	if err := ValidateManifest(m); err != nil {
		return BoundRequest{}, err
	}
	byRef := make(map[string]File, len(m.Files))
	for _, file := range m.Files {
		byRef[file.FileRef] = file
	}
	completed := make(map[string]EnsureResult, len(results))
	for _, result := range results {
		file, known := byRef[result.FileRef]
		_, duplicate := completed[result.FileRef]
		if !known || duplicate || result.Contract != Contract || result.Identity != m.Expected || !ValidRemoteID(result.RemoteID) || !safeOpaque(result.OperationID, 256) ||
			result.Bytes != file.Bytes || result.SHA256 != file.SHA256 || result.Filename != file.Filename || result.MIME != file.MIME || result.InputTokens < 0 {
			return BoundRequest{}, reject("invalid_managed_binding", "ensure_results")
		}
		if file.Mode == "existing" && (result.RemoteID != file.RemoteID || result.OperationID != file.OperationID) {
			return BoundRequest{}, reject("invalid_managed_binding", "ensure_results")
		}
		completed[result.FileRef] = result
	}
	remoteIDs := make(map[string]string, len(m.Files))
	files := make([]boundFile, 0, len(m.Files))
	operations := make(map[string]bool, len(m.Files))
	for _, file := range m.Files {
		remote, operation := file.RemoteID, file.OperationID
		if file.Mode == "upload" {
			result, ok := completed[file.FileRef]
			if !ok {
				return BoundRequest{}, reject("managed_files_incomplete", "files")
			}
			remote, operation = result.RemoteID, result.OperationID
		}
		if operations[operation] {
			return BoundRequest{}, reject("invalid_managed_binding", "ensure_results.operation_id")
		}
		operations[operation] = true
		remoteIDs[file.FileRef] = remote
		positions := append([]Position(nil), file.Positions...)
		sort.Slice(positions, func(i, j int) bool {
			if positions[i].Input == positions[j].Input {
				return positions[i].Part < positions[j].Part
			}
			return positions[i].Input < positions[j].Input
		})
		files = append(files, boundFile{file.FileRef, file.Filename, file.MIME, file.Bytes, file.SHA256, positions, remote, operation})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].FileRef < files[j].FileRef })
	wire, err := ReplacedWire(m, remoteIDs)
	if err != nil {
		return BoundRequest{}, err
	}
	templateHash, err := TemplateHash(m.RequestTemplate)
	if err != nil {
		return BoundRequest{}, err
	}
	wireHash, err := WireHash(wire)
	if err != nil {
		return BoundRequest{}, err
	}
	binding := struct {
		Contract               string      `json:"contract"`
		JSONProfile            string      `json:"json_profile"`
		OwnerRef               string      `json:"owner_ref"`
		GroupRef               string      `json:"group_ref"`
		SessionBindingContract string      `json:"session_binding_contract,omitempty"`
		ConversationRef        string      `json:"conversation_ref,omitempty"`
		ExpectedIdentity       Identity    `json:"expected_identity"`
		TemplateSHA256         string      `json:"template_sha256"`
		WireSHA256             string      `json:"wire_sha256"`
		Files                  []boundFile `json:"files"`
	}{Contract, JSONProfile, m.OwnerRef, m.GroupRef, m.SessionBindingContract, m.ConversationRef, m.Expected, templateHash, wireHash, files}
	encoded, err := json.Marshal(binding)
	if err != nil {
		return BoundRequest{}, reject("invalid_managed_binding", "files")
	}
	bindingHash, err := domainHash("bps-files-binding/1", encoded)
	if err != nil {
		return BoundRequest{}, err
	}
	for _, result := range results {
		if result.WireSHA256 != "" && result.WireSHA256 != wireHash || result.BindingSHA256 != "" && result.BindingSHA256 != bindingHash {
			return BoundRequest{}, reject("invalid_managed_binding", "ensure_results")
		}
	}
	return BoundRequest{Wire: wire, TemplateSHA256: templateHash, WireSHA256: wireHash, BindingSHA256: bindingHash}, nil
}

func BindingHash(m AdmitManifest, results []EnsureResult) (string, error) {
	bound, err := Bind(m, results)
	return bound.BindingSHA256, err
}
