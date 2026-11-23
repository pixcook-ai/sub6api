//go:build recovery15native

package managedfiles

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func nativeTerminalBindingFixture15() NativeTerminalBinding15 {
	return NativeTerminalBinding15{PublicID: "resp_local_00000000000000000000000000000001", UserID: 1, APIKeyID: 2, GroupID: 0, AccountID: 3, PublicModel: "fixture-public", ResolvedModel: "fixture-native", BackendID: "backend", SubjectDigest: strings.Repeat("a", 64), CredentialGeneration: "generation-1", AdmissionID: "responses-" + strings.Repeat("b", 64), AdmissionBindingSHA256: strings.Repeat("c", 64), RequestWireSHA256: strings.Repeat("d", 64), Nonce: strings.Repeat("e", 64)}
}
func nativeTerminalFixture15(t *testing.T) ([]byte, []byte, []byte, NativeTerminalBinding15, time.Time) {
	t.Helper()
	now := time.Unix(1800000000, 0)
	terminal := []byte(`{ "object" : "response", "status":"completed", "output":[] }`)
	snapshot := []byte(`{ "version" : 1, "input" : [] }`)
	binding := nativeTerminalBindingFixture15()
	raw, err := EncodeNativeTerminal15(strings.Repeat("k", 32), binding, []byte(`{}`), terminal, snapshot, now, 60*time.Second, 4096)
	if err != nil {
		t.Fatal(err)
	}
	return raw, terminal, snapshot, binding, now
}
func TestNativeTerminal15PreservesExactBytesAndAuthenticatesBinding(t *testing.T) {
	raw, terminal, snapshot, binding, now := nativeTerminalFixture15(t)
	receipt, gotPublic, gotSnapshot, err := VerifyNativeTerminal15(raw, strings.Repeat("k", 32), binding, now, 4096)
	if err != nil || !bytes.Equal(gotPublic, terminal) || !bytes.Equal(gotSnapshot, snapshot) {
		t.Fatalf("exact payloads lost: %v", err)
	}
	if receipt.NativeWireSHA256 != "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a" {
		t.Fatal("native wire digest did not bind actual sent bytes")
	}
	if bytes.Contains(raw, []byte(strings.Repeat("k", 32))) {
		t.Fatal("service key leaked into envelope")
	}
	// A valid receipt belongs only to one current trusted invocation.
	mutations := []func(*NativeTerminalBinding15){
		func(b *NativeTerminalBinding15) { b.PublicID = "resp_local_00000000000000000000000000000002" },
		func(b *NativeTerminalBinding15) { b.UserID++ }, func(b *NativeTerminalBinding15) { b.APIKeyID++ },
		func(b *NativeTerminalBinding15) { b.GroupID++ }, func(b *NativeTerminalBinding15) { b.AccountID++ },
		func(b *NativeTerminalBinding15) { b.PublicModel = "other" }, func(b *NativeTerminalBinding15) { b.ResolvedModel = "other" },
		func(b *NativeTerminalBinding15) { b.BackendID = "other" }, func(b *NativeTerminalBinding15) { b.SubjectDigest = strings.Repeat("f", 64) },
		func(b *NativeTerminalBinding15) { b.CredentialGeneration = "other" }, func(b *NativeTerminalBinding15) { b.Nonce = strings.Repeat("f", 64) },
		func(b *NativeTerminalBinding15) { b.RequestWireSHA256 = strings.Repeat("f", 64) }, func(b *NativeTerminalBinding15) { b.AdmissionBindingSHA256 = strings.Repeat("f", 64) },
		func(b *NativeTerminalBinding15) { b.AdmissionID = "responses-" + strings.Repeat("f", 64) },
	}
	for i, mutate := range mutations {
		changed := binding
		mutate(&changed)
		_, pub, priv, err := VerifyNativeTerminal15(raw, strings.Repeat("k", 32), changed, now, 4096)
		if err == nil || pub != nil || priv != nil {
			t.Fatalf("binding mutation %d released payload", i)
		}
	}
}
func TestNativeTerminal15RejectsTamperingAmbiguityExpiryAndBounds(t *testing.T) {
	raw, _, _, binding, now := nativeTerminalFixture15(t)
	patch := func(field string, value any) []byte {
		var object map[string]any
		if err := json.Unmarshal(raw, &object); err != nil {
			t.Fatal(err)
		}
		object[field] = value
		result, err := json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	groupNull := bytes.Replace(raw, []byte(`"group_id":0`), []byte(`"group_id":null`), 1)
	groupMissing := bytes.Replace(raw, []byte(`"group_id":0,`), nil, 1)
	cases := []struct {
		name  string
		raw   []byte
		key   string
		at    time.Time
		limit int
	}{
		{"wrong key", raw, strings.Repeat("x", 32), now, 4096},
		{"tampered public", patch("terminal_base64", base64.StdEncoding.EncodeToString([]byte(`{"object":"response","status":"completed","output":[1]}`))), strings.Repeat("k", 32), now, 4096},
		{"tampered snapshot", patch("snapshot_base64", base64.StdEncoding.EncodeToString([]byte(`{"version":2}`))), strings.Repeat("k", 32), now, 4096},
		{"tampered native digest", patch("native_wire_sha256", strings.Repeat("f", 64)), strings.Repeat("k", 32), now, 4096},
		{"unknown field", patch("unknown", true), strings.Repeat("k", 32), now, 4096},
		{"duplicate key", append([]byte(`{"version":"bps-native-terminal/1",`), raw[1:]...), strings.Repeat("k", 32), now, 4096},
		{"group null", groupNull, strings.Repeat("k", 32), now, 4096},
		{"group absent", groupMissing, strings.Repeat("k", 32), now, 4096},
		{"expired", raw, strings.Repeat("k", 32), now.Add(60 * time.Second), 4096},
		{"too far in future", raw, strings.Repeat("k", 32), now.Add(-6 * time.Second), 4096},
		{"small payload budget", raw, strings.Repeat("k", 32), now, 16},
		{"invalid budget", raw, strings.Repeat("k", 32), now, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			receipt, pub, priv, err := VerifyNativeTerminal15(tc.raw, tc.key, binding, tc.at, tc.limit)
			if err == nil || pub != nil || priv != nil || receipt != (NativeTerminalReceipt15{}) {
				t.Fatal("unverified payload released")
			}
		})
	}
}
func TestNativeTerminal15RejectsUnconfirmedOrOversizedProducerPayload(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for _, terminal := range []string{
		`{"object":"response","status":"incomplete","output":[]}`,
		`{"object":"response","status":"completed","error":{"message":"failed"},"output":[]}`,
		`{"object":"response","status":"completed","incomplete_details":{"reason":"budget"},"output":[]}`,
		`{"object":"response","status":"completed","status":"failed"}`,
	} {
		raw, err := EncodeNativeTerminal15(strings.Repeat("k", 32), nativeTerminalBindingFixture15(), []byte(`{}`), []byte(terminal), []byte(`{"version":1}`), now, time.Minute, 4096)
		if err == nil || raw != nil {
			t.Fatal("unconfirmed terminal signed")
		}
	}
	raw, err := EncodeNativeTerminal15(strings.Repeat("k", 32), nativeTerminalBindingFixture15(), []byte(`{}`), []byte(`{"object":"response","status":"completed","output":[]}`), []byte(`{"large":"`+strings.Repeat("x", 4096)+`"}`), now, time.Minute, 4096)
	if err == nil || raw != nil {
		t.Fatal("oversized snapshot signed")
	}
}
