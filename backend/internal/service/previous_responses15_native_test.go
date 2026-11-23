//go:build recovery15native

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// Install this test first, and observe real RED before the implementation file.
// This tests the durable material API; process restart and PostgreSQL atomicity
// remain separate repository/SDK integration gates.
func TestPreviousResponseNative15BindsPersistedPublicAndSnapshot(t *testing.T) {
	record := native15RecordFixture()
	public := []byte(`{"id":"resp_local_00000000000000000000000000000001","object":"response","created_at":1,"model":"public","store":true,"status":"completed","output":[]}`)
	snapshot := []byte(`{"version":1,"binding":{"owner_id":1,"group_id":3,"account_id":4,"model":"resolved","subject_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"question"}]}],"tools":[],"output":[],"replays":[]}`)
	gateway := []byte(`{"id":"upstream","object":"response","model":"resolved","status":"completed","output":[]}`)
	proof := native15VerifiedFixture(record, gateway, snapshot)
	material, err := BuildStoredResponseNativeRecovery15(record, public, gateway, snapshot, proof, 16384)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateStoredResponseNativeRecovery15(record, public, material, 16384); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*StoredResponse){
		func(r *StoredResponse) { r.UserID++ },
		func(r *StoredResponse) { r.GroupID++ },
		func(r *StoredResponse) { r.AccountID++ },
		func(r *StoredResponse) { r.Model = "different" },
		func(r *StoredResponse) { r.ResolvedModel = "different" },
		func(r *StoredResponse) { r.Binding = "different" },
		func(r *StoredResponse) { r.Input = json.RawMessage(`"different"`) },
	} {
		copy := *record
		change(&copy)
		if ValidateStoredResponseNativeRecovery15(&copy, public, material, 16384) == nil {
			t.Fatal("accepted changed record binding")
		}
	}
	changed := bytes.Replace(public, []byte(`"output":[]`), []byte(`"output":[{"type":"message","role":"assistant","content":[]}]`), 1)
	if ValidateStoredResponseNativeRecovery15(record, changed, material, 16384) == nil {
		t.Fatal("accepted a different terminal")
	}
	if _, err = BuildStoredResponseNativeRecovery15(record, public, gateway, append(snapshot, ' '), proof, 16384); err == nil {
		t.Fatal("accepted different exact signed snapshot bytes")
	}
	var stored map[string]any
	if json.Unmarshal(material, &stored) != nil {
		t.Fatal("invalid material")
	}
	// Simulate JSONB canonical reserialization, preserving signed bytes in the
	// base64 snapshot string instead of depending on JSON object key order.
	reencoded, _ := json.Marshal(stored)
	if err = ValidateStoredResponseNativeRecovery15(record, public, reencoded, 16384); err != nil {
		t.Fatal(err)
	}
}

func TestPreviousResponseNative15GetRequiresFreshBindingAndAuthorization(t *testing.T) {
	record := native15RecordFixture()
	public := []byte(`{"id":"resp_local_00000000000000000000000000000001","object":"response","created_at":1,"model":"public","store":true,"status":"completed","output":[]}`)
	snapshot := []byte(`{"version":1,"binding":{"owner_id":1,"group_id":3,"account_id":4,"model":"resolved","subject_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"question"}]}],"tools":[],"output":[],"replays":[]}`)
	gateway := []byte(`{"object":"response","status":"completed","output":[]}`)
	recovery, err := BuildStoredResponseNativeRecovery15(record, public, gateway, snapshot, native15VerifiedFixture(record, gateway, snapshot), 16384)
	if err != nil {
		t.Fatal(err)
	}
	record.Recovery, record.PublicResponse = recovery, public
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	repo := &previousResponse15Repo{records: map[string]*StoredResponse{record.PublicID: record}}
	svc, _ := NewStoredResponseService(repo, cfg)
	checks := 0
	get := func(owner StoredResponseOwner, binding StoredResponseNativeReadBinding15, authorize PreviousResponseTextAuthorize15) ([]byte, int64, error) {
		return svc.ResolvePreviousResponseNative15(context.Background(), owner, record.PublicID, binding, authorize)
	}
	bind := StoredResponseNativeReadBinding15{PublicModel: "public", ResolvedModel: "resolved", AccountID: 4, BackendID: "backend", CredentialGeneration: "generation", SubjectDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	authorize := func(context.Context, StoredResponseOwner, *StoredResponse) error { checks++; return nil }
	got, account, err := get(StoredResponseOwner{1, 2, 3}, bind, authorize)
	if err != nil || account != 4 || !bytes.Equal(got, snapshot) || checks != 1 {
		t.Fatalf("get: %d %d %v", account, checks, err)
	}
	if _, _, err = get(StoredResponseOwner{2, 9, 3}, bind, authorize); err == nil {
		t.Fatal("cross owner")
	}
	changed := bind
	changed.AccountID = 5
	if _, _, err = get(StoredResponseOwner{1, 2, 3}, changed, authorize); err == nil {
		t.Fatal("changed current account")
	}
	if _, _, err = get(StoredResponseOwner{1, 2, 3}, bind, func(context.Context, StoredResponseOwner, *StoredResponse) error {
		return ErrBPSRequestAuthorizationRevoked
	}); !errors.Is(err, ErrBPSRequestAuthorizationRevoked) {
		t.Fatal("ignored current revocation")
	}
	if _, _, err = get(StoredResponseOwner{1, 2, 3}, bind, nil); err == nil {
		t.Fatal("missing authorizer")
	}
	record.ExpiresAt = time.Now().Add(100 * time.Millisecond)
	entered := false
	if _, _, err = get(StoredResponseOwner{1, 2, 3}, bind, func(_ context.Context, _ StoredResponseOwner, current *StoredResponse) error {
		entered = true
		time.Sleep(time.Until(current.ExpiresAt) + time.Millisecond)
		return nil
	}); !entered || !errors.Is(err, ErrStoredResponseNotFound) {
		t.Fatal("returned recovery that expired while fresh authorization waited, or did not exercise the wait")
	}
}

func TestPreviousResponseNative15FinalizeWithholdsCompletionOnPrivateWriteFailure(t *testing.T) {
	record := native15RecordFixture()
	record.State, record.ExecutionState, record.LeaseToken, record.Version = "in_progress", "not_sent", "lease", 1
	public := []byte(`{"id":"resp_local_00000000000000000000000000000001","object":"response","created_at":1,"model":"public","store":true,"status":"completed","output":[]}`)
	snapshot := []byte(`{"version":1,"binding":{"owner_id":1,"group_id":3,"account_id":4,"model":"resolved","subject_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"question"}]}],"tools":[],"output":[],"replays":[]}`)
	gateway := []byte(`{"object":"response","status":"completed","output":[]}`)
	proof := native15VerifiedFixture(record, gateway, snapshot)
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	db := &previousResponse15CaptureRepository{}
	svc, _ := NewStoredResponseService(db, cfg)
	result, err := svc.FinalizeStoredResponseNative15(context.Background(), record, public, gateway, snapshot, proof)
	if err != nil || len(result) == 0 || len(db.captured.Recovery) == 0 {
		t.Fatalf("finalize: %v", err)
	}
	if err = ValidateStoredResponseNativeRecovery15(record, result, db.captured.Recovery, cfg.Limits.MaxResponseBytes); err != nil {
		t.Fatal(err)
	}
	db.finishErr = errors.New("private write failed")
	result, err = svc.FinalizeStoredResponseNative15(context.Background(), record, public, gateway, snapshot, proof)
	if err == nil || result != nil {
		t.Fatal("released completed after failed atomic persistence")
	}
}

func TestPreviousResponseNative15AcceptsGatewayFunctionCallAndJSONBNumbers(t *testing.T) {
	record := native15RecordFixture()
	record.Parameters = json.RawMessage(`{"temperature":1e0}`)
	call := `{"type":"function_call","id":"fc_call","call_id":"call_read","name":"read","arguments":"{}","status":"completed","encrypted_function_args":[]}`
	public := []byte(`{"id":"resp_local_00000000000000000000000000000001","object":"response","created_at":1,"model":"public","store":true,"status":"completed","temperature":1e0,"output":[` + call + `]}`)
	snapshot := []byte(`{"version":1,"binding":{"owner_id":1,"group_id":3,"account_id":4,"model":"resolved","subject_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"question"}]}],"tools":[],"output":[` + call + `],"replays":[]}`)
	gateway := []byte(`{"object":"response","status":"completed","output":[` + call + `]}`)
	material, err := BuildStoredResponseNativeRecovery15(record, public, gateway, snapshot, native15VerifiedFixture(record, gateway, snapshot), 16384)
	if err != nil {
		t.Fatal(err)
	}
	// Literal expected representation models PostgreSQL's number expansion.
	persistedPublic := bytes.Replace(public, []byte(`"temperature":1e0`), []byte(`"temperature":1`), 1)
	record.Parameters = json.RawMessage(`{"temperature":1}`)
	if err = ValidateStoredResponseNativeRecovery15(record, persistedPublic, material, 16384); err != nil {
		t.Fatal(err)
	}
	badCall := bytes.Replace(public, []byte(`"encrypted_function_args":[]`), []byte(`"encrypted_function_args":["opaque"]`), 1)
	badGateway := bytes.Replace(gateway, []byte(`"encrypted_function_args":[]`), []byte(`"encrypted_function_args":["opaque"]`), 1)
	badSnapshot := bytes.Replace(snapshot, []byte(`"encrypted_function_args":[]`), []byte(`"encrypted_function_args":["opaque"]`), 1)
	if _, err = BuildStoredResponseNativeRecovery15(record, badCall, badGateway, badSnapshot, native15VerifiedFixture(record, badGateway, badSnapshot), 16384); err == nil {
		t.Fatal("nonempty encrypted arguments bypassed supported scope")
	}
}

func TestPreviousResponseNative15DifferentTurnAdmissionsKeepStableCredentialBinding(t *testing.T) {
	parent := native15RecordFixture()
	gateway := []byte(`{"object":"response","status":"completed","output":[]}`)
	parentPublic := []byte(`{"id":"resp_local_00000000000000000000000000000001","object":"response","created_at":1,"model":"public","store":true,"status":"completed","output":[]}`)
	parentSnapshot := []byte(`{"version":1,"binding":{"owner_id":1,"group_id":3,"account_id":4,"model":"resolved","subject_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"question"}]}],"tools":[],"output":[],"replays":[]}`)
	var err error
	parent.PublicResponse = parentPublic
	parent.Recovery, err = BuildStoredResponseNativeRecovery15(parent, parentPublic, gateway, parentSnapshot, native15VerifiedFixture(parent, gateway, parentSnapshot), 16384)
	if err != nil {
		t.Fatal(err)
	}
	child := *parent
	child.PublicID = "resp_local_00000000000000000000000000000002"
	child.PreviousResponseID = parent.PublicID
	child.Input = json.RawMessage(`"followup"`)
	child.Binding = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	childPublic := bytes.Replace(parentPublic, []byte(parent.PublicID), []byte(child.PublicID), 1)
	childTurn := bytes.Replace(parentSnapshot, []byte(`"content":[{"type":"input_text","text":"question"}]`), []byte(`"content":[{"type":"input_text","text":"followup"}]`), 1)
	leaf := []byte(`{"version":2,"binding":{"owner_id":1,"group_id":3,"account_id":4,"model":"resolved","subject_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"turns":[` + string(parentSnapshot) + `,` + string(childTurn) + `]}`)
	child.PublicResponse = childPublic
	child.Recovery, err = BuildStoredResponseNativeRecovery15(&child, childPublic, gateway, leaf, native15VerifiedFixture(&child, gateway, leaf), 16384)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateStoredResponseNativeRecoveryParent15(&child, childPublic, child.Recovery, parent, time.Now(), 16384); err != nil {
		t.Fatal(err)
	}
	otherKey := child
	otherKey.APIKeyID++
	if err = ValidateStoredResponseNativeRecoveryParent15(&otherKey, childPublic, child.Recovery, parent, time.Now(), 16384); !errors.Is(err, ErrStoredResponseNotFound) {
		t.Fatal("cross-key parent authorized", err)
	}
	// A different same-owner/account root is not the row named by previousID.
	siblingSnapshot := bytes.Replace(parentSnapshot, []byte(`"text":"question"`), []byte(`"text":"sibling question"`), 1)
	wrongLeaf := bytes.Replace(leaf, parentSnapshot, siblingSnapshot, 1)
	wrongMaterial, err := BuildStoredResponseNativeRecovery15(&child, childPublic, gateway, wrongLeaf, native15VerifiedFixture(&child, gateway, wrongLeaf), 16384)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateStoredResponseNativeRecoveryParent15(&child, childPublic, wrongMaterial, parent, time.Now(), 16384); err == nil {
		t.Fatal("completed with a sibling snapshot instead of actual durable parent")
	}
	request, err := NormalizeStoredResponseNativeRequest15([]byte(`{"model":"public","input":"followup"}`))
	if err != nil {
		t.Fatal(err)
	}
	admission := native15AdmissionFixture(request)
	admission.previousResponseID = parent.PublicID
	admission.parentSnapshot = siblingSnapshot
	child.nativeAdmission15 = admission
	if err = ValidateStoredResponseNativeReservationParent15(&child, parent, time.Now(), 16384); err == nil {
		t.Fatal("reserved with a sibling snapshot instead of actual durable parent")
	}
	admission.parentSnapshot = parentSnapshot
	if err = ValidateStoredResponseNativeReservationParent15(&child, parent, time.Now(), 16384); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	svc, _ := NewStoredResponseService(&previousResponse15Repo{records: map[string]*StoredResponse{parent.PublicID: parent, child.PublicID: &child}}, cfg)
	checks := 0
	bind := StoredResponseNativeReadBinding15{PublicModel: "public", ResolvedModel: "resolved", AccountID: 4, BackendID: "backend", CredentialGeneration: "generation", SubjectDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	got, account, err := svc.ResolvePreviousResponseNative15(context.Background(), StoredResponseOwner{1, 2, 3}, child.PublicID, bind, func(context.Context, StoredResponseOwner, *StoredResponse) error { checks++; return nil })
	if err != nil || account != 4 || checks != 2 || !bytes.Equal(got, leaf) {
		t.Fatalf("two-turn recovery: %d %d %v", account, checks, err)
	}
	for _, change := range []func(*StoredResponseNativeReadBinding15){func(b *StoredResponseNativeReadBinding15) { b.BackendID = "changed" }, func(b *StoredResponseNativeReadBinding15) { b.CredentialGeneration = "changed" }, func(b *StoredResponseNativeReadBinding15) {
		b.SubjectDigest = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	}} {
		changed := bind
		change(&changed)
		if _, _, err := svc.ResolvePreviousResponseNative15(context.Background(), StoredResponseOwner{1, 2, 3}, child.PublicID, changed, func(context.Context, StoredResponseOwner, *StoredResponse) error { return nil }); !errors.Is(err, ErrBPSRequestAuthorizationRevoked) {
			t.Fatal("changed stable credential identity authorized")
		}
	}
	child.ExpiresAt = time.Now().Add(100 * time.Millisecond)
	parentEntered := false
	_, _, err = svc.ResolvePreviousResponseNative15(context.Background(), StoredResponseOwner{1, 2, 3}, child.PublicID, bind, func(_ context.Context, _ StoredResponseOwner, current *StoredResponse) error {
		if current.PublicID == parent.PublicID {
			parentEntered = true
			time.Sleep(time.Until(child.ExpiresAt) + time.Millisecond)
		}
		return nil
	})
	if !parentEntered || !errors.Is(err, ErrStoredResponseNotFound) {
		t.Fatal("returned leaf that expired while an ancestor authorization waited, or did not exercise the wait")
	}
}

func native15RecordFixture() *StoredResponse {
	return &StoredResponse{PublicID: "resp_local_00000000000000000000000000000001", UserID: 1, APIKeyID: 2, GroupID: 3, AccountID: 4, Model: "public", ResolvedModel: "resolved", Binding: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Input: json.RawMessage(`"question"`), Parameters: json.RawMessage(`{}`), CreatedAt: time.Unix(1, 0), ExpiresAt: time.Now().Add(time.Hour), RecoveryVersion: 2, State: "completed", ExecutionState: "terminal"}
}

func native15VerifiedFixture(r *StoredResponse, gateway, snapshot []byte) *verifiedNativeTerminal15 {
	return &verifiedNativeTerminal15{publicID: r.PublicID, userID: r.UserID, apiKeyID: r.APIKeyID, groupID: r.GroupID, accountID: r.AccountID, publicModel: r.Model, resolvedModel: r.ResolvedModel, backendID: "backend", credentialGeneration: "generation", subjectDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", requestWireSHA256: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", nativeWireSHA256: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", terminalSHA256: storedNativeDigest15(gateway), snapshotSHA256: storedNativeDigest15(snapshot), nonce: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", admissionID: "responses-ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", admissionBindingSHA256: r.Binding}
}
