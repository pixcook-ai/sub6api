//go:build recovery15native

package managedfiles

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func nativeAdmissionFixture15(t *testing.T) (NativeResponsesPreflight15, NativeResponsesPreflightResult15, time.Time) {
	t.Helper()
	now := time.Unix(1800000000, 0)
	r := NativeResponsesPreflight15{Version: NativeResponsesContract15, MaxBytes: NativeResponsesMaxBytes15, Expected: Identity{BackendID: "backend", SubjectFingerprint: "sha256:" + strings.Repeat("a", 64), CredentialGeneration: "generation"}, Context: NativeResponsesContext15{UserID: 1, APIKeyID: 2, GroupID: 0, AccountID: 3, PublicModel: "public", ResolvedModel: "resolved", Nonce: strings.Repeat("b", 64)}, ClientRequest: json.RawMessage(`{"model":"public", "store":false,"input":"question","instructions":"current"}`), WireRequest: json.RawMessage(`{"model":"resolved", "store":false,"input":"question","instructions":"current"}`)}
	native := ResponsesWireSHA256([]byte(`{"model":"resolved","input":[]}`))
	binding, err := NativeResponsesBindingSHA25615(r, native)
	if err != nil {
		t.Fatal(err)
	}
	result := NativeResponsesPreflightResult15{Version: NativeResponsesContract15, PolicyVersion: NativeResponsesPolicy15, Identity: r.Expected, AdmissionID: "responses-" + strings.Repeat("c", 64), ExpiresAt: now.Add(30 * time.Second).Unix(), RequestWireSHA256: ResponsesWireSHA256(r.WireRequest), NativeWireSHA256: native, BindingSHA256: binding, MaxBytes: r.MaxBytes}
	return r, result, now
}
func TestNativeResponses15CodecPreservesExactDeltasAndGroupZero(t *testing.T) {
	r, result, now := nativeAdmissionFixture15(t)
	raw, err := EncodeNativeResponsesPreflight15(r)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeNativeResponsesPreflight15(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded.ClientRequest, r.ClientRequest) || !bytes.Equal(decoded.WireRequest, r.WireRequest) || decoded.Context.GroupID != 0 {
		t.Fatal("exact delta bytes changed")
	}
	if err = ValidateNativeResponsesPreflightResult15(decoded, result, now); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(r.Expected.SubjectFingerprint))
	if got := NativeResponsesSubjectDigest15(r.Expected); got != hex.EncodeToString(sum[:]) || strings.Contains(got, "sha256:") {
		t.Fatal("subject digest rule changed")
	}
	for _, bad := range [][]byte{append([]byte(`{"version":"bps-responses-native/1",`), raw[1:]...), bytes.Replace(raw, []byte(`"group_id":0,`), nil, 1), bytes.Replace(raw, []byte(`"group_id":0`), []byte(`"group_id":null`), 1), bytes.Replace(raw, []byte(`"previous_snapshot_base64":""`), []byte(`"previous_snapshot_base64":null`), 1), bytes.Replace(raw, []byte(`"previous_snapshot_base64":""`), []byte(`"previous_snapshot_base64":{}`), 1), append(raw, []byte(`{}`)...)} {
		if _, err = DecodeNativeResponsesPreflight15(bad); err == nil {
			t.Fatal("ambiguous or missing binding accepted")
		}
	}
}
func TestNativeResponses15RejectsDeltaMutationAndPolicyDowngrade(t *testing.T) {
	r, result, now := nativeAdmissionFixture15(t)
	for _, replacement := range []struct{ old, new string }{{`"instructions":"current"`, `"instructions":"changed"`}, {`"input":"question"`, `"input":"changed"`}, {`"instructions":"current"`, `"tools":[],"instructions":"current"`}} {
		altered := r
		altered.WireRequest = bytes.Replace(r.WireRequest, []byte(replacement.old), []byte(replacement.new), 1)
		if _, err := EncodeNativeResponsesPreflight15(altered); err == nil {
			t.Fatal("unadmitted delta mutation accepted")
		}
	}
	result.PolicyVersion = "bps-responses-puretext-2026-11-20-v1"
	if ValidateNativeResponsesPreflightResult15(r, result, now) == nil {
		t.Fatal("puretext downgrade accepted")
	}
	r.Version = ResponsesContract
	if _, err := EncodeNativeResponsesPreflight15(r); err == nil {
		t.Fatal("old contract accepted as native")
	}
}
func TestNativeResponses15ActivationBindsPostBeginCaptureAndNonce(t *testing.T) {
	r, result, now := nativeAdmissionFixture15(t)
	key := strings.Repeat("k", 32)
	publicID := "resp_local_" + strings.Repeat("d", 32)
	header, err := EncodeNativeResponsesActivation15(key, r, result, publicID, now)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := VerifyNativeResponsesActivation15(header, key, r, result, now)
	if err != nil || proof.Binding.PublicID != publicID || proof.NativeWireSHA256 != result.NativeWireSHA256 {
		t.Fatalf("activation proof mismatch: %v", err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(header)
	changed := bytes.Replace(raw, []byte(publicID), []byte("resp_local_"+strings.Repeat("e", 32)), 1)
	if _, err = VerifyNativeResponsesActivation15(base64.RawURLEncoding.EncodeToString(changed), key, r, result, now); err == nil {
		t.Fatal("unsigned replacement public ID accepted")
	}
	changedRequest := r
	changedRequest.Context.Nonce = strings.Repeat("f", 64)
	if _, err = VerifyNativeResponsesActivation15(header, key, changedRequest, result, now); err == nil {
		t.Fatal("foreign attempt nonce accepted")
	}
	if _, err = VerifyNativeResponsesActivation15(header, key, r, result, now.Add(30*time.Second)); err == nil {
		t.Fatal("expired activation accepted")
	}
}

func TestNativeResponses15NegotiatedLimitAndServiceKeyFailBeforeAdmission(t *testing.T) {
	r, result, now := nativeAdmissionFixture15(t)
	for _, limit := range []int{0, -1, NativeResponsesMaxBytes15 + 1} {
		bad := r
		bad.MaxBytes = limit
		if _, err := EncodeNativeResponsesPreflight15(bad); err == nil {
			t.Fatal("invalid negotiated limit accepted")
		}
	}
	small := r
	small.MaxBytes = 1
	if _, err := EncodeNativeResponsesPreflight15(small); err == nil {
		t.Fatal("request above negotiated limit accepted")
	}
	prior := r
	prior.MaxBytes = len(r.WireRequest)
	prior.PreviousSnapshotBase64 = base64.StdEncoding.EncodeToString([]byte(`{"padding":"` + strings.Repeat("x", len(r.WireRequest)) + `"}`))
	if _, err := EncodeNativeResponsesPreflight15(prior); err == nil {
		t.Fatal("snapshot above negotiated limit accepted")
	}
	result.MaxBytes--
	if ValidateNativeResponsesPreflightResult15(r, result, now) == nil {
		t.Fatal("changed negotiated limit accepted")
	}
	for _, key := range []string{"", strings.Repeat("k", 31), strings.Repeat("k", 1025)} {
		if ValidateNativeResponsesServiceKey15(key) == nil {
			t.Fatal("invalid proof key accepted")
		}
	}
	for _, key := range []string{strings.Repeat("k", 32), strings.Repeat("k", 1024)} {
		if err := ValidateNativeResponsesServiceKey15(key); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := EncodeNativeResponsesPreflight15(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{bytes.Replace(raw, []byte(`"max_bytes":16777216,`), nil, 1), bytes.Replace(raw, []byte(`"max_bytes":16777216`), []byte(`"max_bytes":null`), 1)} {
		if _, err := DecodeNativeResponsesPreflight15(bad); err == nil {
			t.Fatal("missing/null negotiated limit accepted")
		}
	}
}

func TestNativeResponses15AckBindsNativeResultAndPostBeginPublicID(t *testing.T) {
	_, result, _ := nativeAdmissionFixture15(t)
	key := strings.Repeat("k", 32)
	publicID := "resp_local_" + strings.Repeat("d", 32)
	ack, err := NativeResponsesAck15(key, result, publicID)
	if err != nil {
		t.Fatal(err)
	}
	if err = VerifyNativeResponsesAck15(key, result, publicID, ack); err != nil {
		t.Fatal(err)
	}
	if VerifyNativeResponsesAck15(key, result, "resp_local_"+strings.Repeat("e", 32), ack) == nil {
		t.Fatal("foreign public ID acknowledged")
	}
	changed := result
	changed.MaxBytes--
	if VerifyNativeResponsesAck15(key, changed, publicID, ack) == nil {
		t.Fatal("changed limit acknowledged")
	}
	changed = result
	changed.NativeWireSHA256 = strings.Repeat("f", 64)
	if VerifyNativeResponsesAck15(key, changed, publicID, ack) == nil {
		t.Fatal("changed native wire acknowledged")
	}
	if VerifyNativeResponsesAck15(key, result, publicID, "") == nil {
		t.Fatal("missing native ack accepted")
	}
}
