//go:build recovery15native

package service

import "testing"

// Literal follows gateway request.go:message("user", text), as exported by
// NativeRecovery15's original-input capture. Preserve its exact signed bytes.
func TestPreviousResponseNative15ActualGatewayStringInputShape(t *testing.T) {
	record := native15RecordFixture()
	public := []byte(`{"id":"resp_local_00000000000000000000000000000001","object":"response","created_at":1,"model":"public","store":true,"status":"completed","output":[]}`)
	gateway := []byte(`{"object":"response","status":"completed","output":[]}`)
	snapshot := []byte(`{"version":1,"binding":{"owner_id":1,"group_id":3,"account_id":4,"model":"resolved","subject_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"question"}]}],"tools":[],"output":[],"replays":[]}`)
	if _, err := BuildStoredResponseNativeRecovery15(record, public, gateway, snapshot, native15VerifiedFixture(record, gateway, snapshot), 16384); err != nil {
		t.Fatal(err)
	}
}
