package managedfiles

import (
	"encoding/json"
	"strings"
	"testing"
)

func recoveryTestManifest(t *testing.T) AdmitManifest {
	t.Helper()
	m, err := DecodeManifest([]byte(`{"contract":"bps-files/1","expected":{"backend_id":"recovery-backend","credential_generation":"recovery-generation","subject_fingerprint":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"files":[{"bytes":17,"file_ref":"file-public-a","filename":"fixture.pdf","mime":"application/pdf","mode":"existing","operation_id":"operation-old","positions":[{"input":0,"part":1}],"remote_id":"file-native-old","sha256":"a04dc6c38580462e46df7968a7d91a006455989bb839a6c9c1cf5fb4c4551a47"}],"group_ref":"group-fixture-0123456789","json_profile":"bps-files-json/1","owner_ref":"owner-fixture-0123456789","request_id":"request-fixture-0123456789","request_template":{"input":[{"content":[{"text":"read these synthetic files","type":"input_text"},{"file_id":"file-public-a","type":"input_file"}],"role":"user","type":"message"}],"model":"fixture-model"}}`))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func recoveryTestPayload(t *testing.T, m AdmitManifest) []byte {
	t.Helper()
	hash, err := RecoveryManifestHash(m)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"error": map[string]any{
		"type": "invalid_request_error", "code": "managed_metadata_missing", "param": nil,
		"message":  "owner-scoped native metadata is unavailable",
		"recovery": RecoveryMissing{Contract: "bps-metadata-recovery/1", Identity: m.Expected, RequestID: m.RequestID, ManifestSHA256: hash, MissingRefs: []string{m.Files[0].FileRef}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRecoveryManifestHashMatchesGatewayGolden(t *testing.T) {
	m := recoveryTestManifest(t)
	hash, err := RecoveryManifestHash(m)
	if err != nil {
		t.Fatal(err)
	}
	const expected = "08f8dcb318f31bea51511da3177a6c688bec6c9e0ce7bcccfb088c359ae1c651"
	if hash != expected {
		t.Fatalf("hash=%s want=%s", hash, expected)
	}
	got, err := ParseRecoveryMissing(409, recoveryTestPayload(t, m), m)
	if err != nil || got == nil {
		t.Fatalf("missing=%v err=%v", got, err)
	}
	if got.ManifestSHA256 != expected || strings.Join(got.MissingRefs, ",") != "file-public-a" {
		t.Fatalf("unexpected report: %#v", got)
	}
}

func TestRecoveryMissingRejectsUntrustedEvidence(t *testing.T) {
	m := recoveryTestManifest(t)
	good := string(recoveryTestPayload(t, m))
	for _, tc := range []struct {
		name, raw string
		status    int
	}{
		{"wrong status", good, 200},
		{"legacy code only", `{"error":{"code":"managed_metadata_missing"}}`, 409},
		{"duplicate root", strings.TrimSuffix(good, "}") + `,"error":{}}`, 409},
		{"duplicate nested", strings.Replace(good, `"contract":`, `"contract":"bps-metadata-recovery/1","contract":`, 1), 409},
		{"case alias", strings.Replace(good, `"missing_refs"`, `"Missing_Refs"`, 1), 409},
		{"wrong code", strings.Replace(good, "managed_metadata_missing", "backend_conflict", 1), 409},
		{"wrong type", strings.Replace(good, "invalid_request_error", "server_error", 1), 409},
		{"non-null param", strings.Replace(good, `"param":null`, `"param":"files"`, 1), 409},
		{"wrong message", strings.Replace(good, "owner-scoped native metadata is unavailable", "secret-token", 1), 409},
		{"wrong contract", strings.Replace(good, "bps-metadata-recovery/1", "bps-metadata-recovery/2", 1), 409},
		{"wrong backend", strings.Replace(good, "recovery-backend", "different-backend", 1), 409},
		{"wrong generation", strings.Replace(good, "recovery-generation", "different-generation", 1), 409},
		{"wrong subject", strings.Replace(good, "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64), 1), 409},
		{"wrong request", strings.Replace(good, "request-fixture-0123456789", "other-request", 1), 409},
		{"wrong digest", strings.Replace(good, "08f8dcb318f31bea51511da3177a6c688bec6c9e0ce7bcccfb088c359ae1c651", strings.Repeat("a", 64), 1), 409},
		{"empty refs", strings.Replace(good, `["file-public-a"]`, `[]`, 1), 409},
		{"null refs", strings.Replace(good, `["file-public-a"]`, `null`, 1), 409},
		{"null ref", strings.Replace(good, `["file-public-a"]`, `[null]`, 1), 409},
		{"duplicate ref", strings.Replace(good, `["file-public-a"]`, `["file-public-a","file-public-a"]`, 1), 409},
		{"unknown ref", strings.Replace(good, `["file-public-a"]`, `["file-other"]`, 1), 409},
		{"remote ref", strings.Replace(good, `["file-public-a"]`, `["file-native-old"]`, 1), 409},
		{"unknown root", strings.TrimSuffix(good, "}") + `,"debug":"secret-token"}`, 409},
		{"null recovery", `{"error":{"type":"invalid_request_error","code":"managed_metadata_missing","param":null,"message":"owner-scoped native metadata is unavailable","recovery":null}}`, 409},
		{"trailing body", good + `{}`, 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRecoveryMissing(tc.status, []byte(tc.raw), m)
			if err == nil || got != nil {
				t.Fatalf("accepted untrusted evidence: %#v %v", got, err)
			}
			if strings.Contains(err.Error(), "secret-token") {
				t.Fatal("error leaks private payload")
			}
		})
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(good), &object); err != nil {
		t.Fatal(err)
	}
	failure := object["error"].(map[string]any)
	recovery := failure["recovery"].(map[string]any)
	identity := recovery["identity"].(map[string]any)
	for _, group := range []map[string]any{object, failure, recovery, identity} {
		keys := make([]string, 0, len(group))
		for key := range group {
			keys = append(keys, key)
		}
		for _, key := range keys {
			value := group[key]
			for _, change := range []string{"missing", "null", "case"} {
				if key == "param" && change == "null" {
					continue
				}
				t.Run(key+"_"+change, func(t *testing.T) {
					delete(group, key)
					if change == "null" {
						group[key] = nil
					}
					if change == "case" {
						group[strings.ToUpper(key)] = value
					}
					raw, err := json.Marshal(object)
					if err != nil {
						t.Fatal(err)
					}
					got, err := ParseRecoveryMissing(409, raw, m)
					if err == nil || got != nil {
						t.Fatalf("accepted %s %s", key, change)
					}
					delete(group, strings.ToUpper(key))
					group[key] = value
				})
			}
		}
	}
}

func TestRecoveryManifestHashNormalizesTypedDTOButPreservesNumberSpelling(t *testing.T) {
	m := recoveryTestManifest(t)
	original, err := RecoveryManifestHash(m)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), `"mode":"existing"`, `"mode":"existing","part":""`, 1))
	normalized, err := DecodeManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := RecoveryManifestHash(normalized)
	if err != nil || hash != original {
		t.Fatalf("optional normalization %s %v", hash, err)
	}
	m.RequestTemplate = []byte(strings.Replace(string(m.RequestTemplate), "read these", `\u0072ead these`, 1))
	hash, err = RecoveryManifestHash(m)
	if err != nil || hash != original {
		t.Fatalf("unicode normalization %s %v", hash, err)
	}
	m.RequestTemplate = []byte(strings.Replace(string(m.RequestTemplate), `"model":`, `"temperature":1.0,"model":`, 1))
	decimal, err := RecoveryManifestHash(m)
	if err != nil {
		t.Fatal(err)
	}
	m.RequestTemplate = []byte(strings.Replace(string(m.RequestTemplate), `"temperature":1.0`, `"temperature":1e0`, 1))
	exponent, err := RecoveryManifestHash(m)
	if err != nil || exponent == decimal {
		t.Fatalf("numeric lexeme lost %s %v", exponent, err)
	}
	m.Contract = "unsupported"
	if _, err := RecoveryManifestHash(m); err == nil {
		t.Fatal("invalid manifest hashed")
	}
}

func TestRecoveryMissingRequiresSortedExistingSubsetAndExactOriginalManifest(t *testing.T) {
	m := recoveryTestManifest(t)
	second := m.Files[0]
	second.FileRef = "file-public-b"
	second.RemoteID = "file-native-b"
	second.OperationID = "operation-b"
	second.Positions = []Position{{Input: 0, Part: 2}}
	m.Files = append(m.Files, second)
	m.RequestTemplate = []byte(strings.Replace(string(m.RequestTemplate), `"file_id":"file-public-a","type":"input_file"}`, `"file_id":"file-public-a","type":"input_file"},{"file_id":"file-public-b","type":"input_file"}`, 1))
	good := recoveryTestPayload(t, m)
	for _, tc := range []struct {
		name, refs string
		valid      bool
	}{
		{"sorted batch", `["file-public-a","file-public-b"]`, true},
		{"second subset", `["file-public-b"]`, true},
		{"unsorted batch", `["file-public-b","file-public-a"]`, false},
		{"wrong scalar", `[1]`, false},
		{"oversized subset", "[" + strings.TrimSuffix(strings.Repeat(`"file-public-a",`, MaxUniqueFiles+1), ",") + "]", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(strings.Replace(string(good), `["file-public-a"]`, tc.refs, 1))
			missing, err := ParseRecoveryMissing(409, raw, m)
			if tc.valid {
				if err != nil || missing == nil {
					t.Fatalf("rejected valid subset %v", err)
				}
			} else if err == nil || missing != nil {
				t.Fatal("accepted invalid subset")
			}
		})
	}
	for _, change := range []struct {
		name  string
		alter func(*AdmitManifest)
	}{
		{"file order", func(v *AdmitManifest) { v.Files[0], v.Files[1] = v.Files[1], v.Files[0] }},
		{"owner", func(v *AdmitManifest) { v.OwnerRef += "-other" }},
		{"group", func(v *AdmitManifest) { v.GroupRef += "-other" }},
		{"metadata", func(v *AdmitManifest) { v.Files[0].Filename = "other.txt" }},
		{"operation", func(v *AdmitManifest) { v.Files[0].OperationID = "another-operation" }},
		{"remote", func(v *AdmitManifest) { v.Files[0].RemoteID = "file-other-native" }},
		{"template", func(v *AdmitManifest) {
			v.RequestTemplate = []byte(strings.Replace(string(v.RequestTemplate), "fixture-model", "another-model", 1))
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			copy := m
			copy.Files = append([]File(nil), m.Files...)
			change.alter(&copy)
			missing, err := ParseRecoveryMissing(409, good, copy)
			if err == nil || missing != nil {
				t.Fatal("accepted evidence for another manifest")
			}
		})
	}
	upload := m
	upload.Files = append([]File(nil), m.Files...)
	upload.Files[0].Mode = "upload"
	upload.Files[0].Part = "upload_a"
	upload.Files[0].RemoteID = ""
	upload.Files[0].OperationID = ""
	missing, err := ParseRecoveryMissing(409, recoveryTestPayload(t, upload), upload)
	if err == nil || missing != nil {
		t.Fatal("upload ref accepted as missing existing copy")
	}
}
