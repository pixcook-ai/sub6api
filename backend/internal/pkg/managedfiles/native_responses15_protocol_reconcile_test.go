//go:build recovery15native

package managedfiles

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

const attachmentNegotiatedPolicy15 = "bps-responses-native-attachments-2026-11-22-v1"

func attachmentProtocolRequest15(t *testing.T) (NativeResponsesPreflight15, NativeResponsesPreflightResult15) {
	t.Helper()
	r, result, _ := nativeAdmissionFixture15(t)
	r.Files = &NativeAttachmentAdmission15{AdmissionID: "admission-fixture", OwnerRef: "1", GroupRef: "0", BindingSHA256: strings.Repeat("c", 64), Snapshot: NativeAttachmentSnapshot15{Version: NativeAttachmentContract15, Copies: []NativeAttachmentWireCopy15{{PublicFileID: "file-public", FileVersion: 1, SHA256: strings.Repeat("d", 64), Filename: "fixture.pdf", MIME: "application/pdf", Bytes: 28, AccountID: 3, BackendID: r.Expected.BackendID, SubjectDigest: NativeResponsesSubjectDigest15(r.Expected), CredentialGeneration: r.Expected.CredentialGeneration, OperationID: "operation", RemoteID: "file-remote", InputTokens: 11}}, Positions: []NativeAttachmentPosition15{{Turn: 0, Input: 0, Field: "content", Part: 0, SourceKind: "message", CallInput: -1, CallItem: json.RawMessage("null"), CopyIndex: 0}}}}
	binding, err := NativeResponsesBindingSHA25615(r, result.NativeWireSHA256)
	if err != nil {
		t.Fatal(err)
	}
	result.BindingSHA256 = binding
	result.PolicyVersion = attachmentNegotiatedPolicy15
	return r, result
}
func TestNativeAttachmentProtocol15CodecRequiresNegotiatedPolicy(t *testing.T) {
	r, result := attachmentProtocolRequest15(t)
	_, _, now := nativeAdmissionFixture15(t)
	if err := ValidateNativeResponsesPreflightResult15(r, result, now); err != nil {
		t.Fatal("matching attachment policy rejected", err)
	}
	for _, bad := range []string{NativeResponsesPolicy15, "unknown-policy"} {
		changed := result
		changed.PolicyVersion = bad
		if ValidateNativeResponsesPreflightResult15(r, changed, now) == nil {
			t.Fatal("attachment result policy downgrade accepted")
		}
	}
	legacy, legacyResult, _ := nativeAdmissionFixture15(t)
	if ValidateNativeResponsesPreflightResult15(legacy, legacyResult, now) != nil {
		t.Fatal("legacy policy compatibility lost")
	}
	legacyResult.PolicyVersion = attachmentNegotiatedPolicy15
	if ValidateNativeResponsesPreflightResult15(legacy, legacyResult, now) == nil {
		t.Fatal("Files-free request accepted attachment policy")
	}
	key := strings.Repeat("k", 32)
	publicID := "resp_local_" + strings.Repeat("d", 32)
	header, err := EncodeNativeResponsesActivation15(key, r, result, publicID, now)
	if err != nil {
		t.Fatal(err)
	}
	activation, err := VerifyNativeResponsesActivation15(header, key, r, result, now)
	if err != nil || activation.PolicyVersion != attachmentNegotiatedPolicy15 {
		t.Fatal("activation did not retain negotiated policy", err)
	}
	encoded, err := base64.RawURLEncoding.DecodeString(header)
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(encoded, []byte(attachmentNegotiatedPolicy15), []byte(NativeResponsesPolicy15), 1)
	if _, err = VerifyNativeResponsesActivation15(base64.RawURLEncoding.EncodeToString(changed), key, r, result, now); err == nil {
		t.Fatal("activation policy downgrade accepted")
	}
	ack, err := NativeResponsesAck15(key, result, publicID)
	if err != nil || VerifyNativeResponsesAck15(key, result, publicID, ack) != nil {
		t.Fatal("attachment ACK roundtrip failed", err)
	}
}

func TestNativeAttachmentProtocol15CodecMatchesCrossRepositoryCanonicalBinding(t *testing.T) {
	r, result := attachmentProtocolRequest15(t)
	got, err := NativeResponsesBindingSHA25615(r, result.NativeWireSHA256)
	if err != nil {
		t.Fatal(err)
	}
	// Worked independently from the frozen protocol using Python json/hashlib.
	const want = "ff9472df08578e34632e9beea06b74f8b4d40a75e27620845b36f4e0cfe766b6"
	if got != want {
		t.Fatalf("cross-repository Files binding mismatch: got %s want %s", got, want)
	}
	raw, err := EncodeNativeResponsesPreflight15(r)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := canonicalNativeAttachmentAdmission15(r.Files)
	if err != nil || !bytes.Contains(raw, append([]byte(`"files":`), canonical...)) {
		t.Fatal("preflight Files is not canonical", err)
	}
	decoded, err := DecodeNativeResponsesPreflight15(raw)
	if err != nil {
		t.Fatal(err)
	}
	roundtrip, err := NativeResponsesBindingSHA25615(decoded, result.NativeWireSHA256)
	if err != nil || roundtrip != want {
		t.Fatal("Files binding changed during codec roundtrip", err)
	}
}

func TestNativeAttachmentProtocol15PreservesLegacyTextBinding(t *testing.T) {
	r, result, _ := nativeAdmissionFixture15(t)
	got, err := NativeResponsesBindingSHA25615(r, result.NativeWireSHA256)
	const want = "548814103ae544273425a318b1bd78ceffbc288730ecdab815a8b389cbffe611"
	if err != nil || got != want {
		t.Fatalf("legacy text binding changed: %s %v", got, err)
	}
}
