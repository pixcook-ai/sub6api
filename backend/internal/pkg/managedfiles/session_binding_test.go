package managedfiles_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

func bindingClaims() managedfiles.SessionBindingClaims {
	return managedfiles.SessionBindingClaims{Version: managedfiles.SessionBindingContract, OwnerRef: "owner-1", GroupRef: "group-1", ConversationRef: "conversation-key-1", Expected: exampleManifest().Expected, Method: "POST", Path: "/v1/responses", WireSHA256: managedfiles.ResponsesWireSHA256([]byte(`{"model":"fixture","input":"hello"}`)), ExpiresAt: 1700000030}
}

func sealBindingFixture(raw []byte) string {
	mac := hmac.New(sha256.New, []byte(strings.Repeat("k", 32)))
	mac.Write([]byte("bps-session-binding/1\x00"))
	mac.Write(raw)
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestSessionBindingIndependentGoldenAndExactRequest(t *testing.T) {
	c := bindingClaims()
	header, err := managedfiles.SignSessionBinding(strings.Repeat("k", 32), c)
	if err != nil {
		t.Fatal(err)
	}
	// This literal was independently generated with Python json/hmac, not SignSessionBinding.
	const golden = "eyJ2ZXJzaW9uIjoiYnBzLXNlc3Npb24tYmluZGluZy8xIiwib3duZXJfcmVmIjoib3duZXItMSIsImdyb3VwX3JlZiI6Imdyb3VwLTEiLCJjb252ZXJzYXRpb25fcmVmIjoiY29udmVyc2F0aW9uLWtleS0xIiwiZXhwZWN0ZWQiOnsiYmFja2VuZF9pZCI6ImJhY2tlbmQtMSIsInN1YmplY3RfZmluZ2VycHJpbnQiOiJzaGEyNTY6MTExMTExMTExMTExMTExMTExMTExMTExMTExMTExMTExMTExMTExMTExMTExMTExMTExMTExMTExMTExMTExMSIsImNyZWRlbnRpYWxfZ2VuZXJhdGlvbiI6ImVwb2NoLTEifSwibWV0aG9kIjoiUE9TVCIsInBhdGgiOiIvdjEvcmVzcG9uc2VzIiwid2lyZV9zaGEyNTYiOiJjZGFkYjUzMWQ4ODlkZmYzMWZkOTMxMDAxZDhhYmVjYWRkZDkwYjRiMzhmNjQ5MTI0NTM2YmJjYmVjMTlhZGRmIiwiZXhwaXJlc19hdCI6MTcwMDAwMDAzMH0.sUnmwMrzYHwq0A7CYjeaSgfXuS4awgAWdxrzUxBXBTk"
	if header != golden {
		t.Fatal("binding disagrees with independent wire vector")
	}
	got, err := managedfiles.VerifySessionBinding(strings.Repeat("k", 32), header, []byte(`{"model":"fixture","input":"hello"}`), "POST", "/v1/responses", c.Expected, time.Unix(1700000000, 0))
	if err != nil || got != c {
		t.Fatalf("valid binding rejected: %v", err)
	}
	c.Path = "/responses"
	header, err = managedfiles.SignSessionBinding(strings.Repeat("k", 32), c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = managedfiles.VerifySessionBinding(strings.Repeat("k", 32), header, []byte(`{"model":"fixture","input":"hello"}`), "POST", "/responses", c.Expected, time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}
}

func TestSessionBindingVerifiesIndependentSeal(t *testing.T) {
	c := bindingClaims()
	raw, _ := json.Marshal(c)
	header := sealBindingFixture(raw)
	if got, err := managedfiles.VerifySessionBinding(strings.Repeat("k", 32), header, []byte(`{"model":"fixture","input":"hello"}`), "POST", "/v1/responses", c.Expected, time.Unix(1700000000, 0)); err != nil || got != c {
		t.Fatalf("independently sealed valid claims rejected: %v", err)
	}
	c.ExpiresAt = 1700000120
	raw, _ = json.Marshal(c)
	if _, err := managedfiles.VerifySessionBinding(strings.Repeat("k", 32), sealBindingFixture(raw), []byte(`{"model":"fixture","input":"hello"}`), "POST", "/v1/responses", c.Expected, time.Unix(1700000000, 0)); err != nil {
		t.Fatalf("exact max lifetime rejected: %v", err)
	}
}

func TestSessionBindingRejectsAuthenticatedNoncanonicalClaims(t *testing.T) {
	c := bindingClaims()
	raw, _ := json.Marshal(c)
	for name, altered := range map[string]string{
		"unknown":              strings.TrimSuffix(string(raw), "}") + `,"other":"value"}`,
		"duplicate":            strings.Replace(string(raw), `"owner_ref":"owner-1"`, `"owner_ref":"owner-1","owner_ref":"owner-1"`, 1),
		"case alias":           strings.Replace(string(raw), `"owner_ref"`, `"Owner_Ref"`, 1),
		"null":                 strings.Replace(string(raw), `"owner_ref":"owner-1"`, `"owner_ref":null`, 1),
		"missing conversation": strings.Replace(string(raw), `"conversation_ref":"conversation-key-1",`, "", 1),
		"empty conversation":   strings.Replace(string(raw), `conversation-key-1`, "", 1),
		"nested duplicate":     strings.Replace(string(raw), `"backend_id":"backend-1"`, `"backend_id":"backend-1","backend_id":"backend-1"`, 1),
		"nested unknown":       strings.Replace(string(raw), `"backend_id":"backend-1"`, `"backend_id":"backend-1","other":"x"`, 1),
		"float expiry":         strings.Replace(string(raw), `1700000030`, `1700000030.0`, 1),
		"trailing space":       string(raw) + " ",
		"field spacing":        strings.Replace(string(raw), `"version":`, `"version": `, 1),
		"unicode escape":       strings.Replace(string(raw), `owner-1`, `\u006fwner-1`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := managedfiles.VerifySessionBinding(strings.Repeat("k", 32), sealBindingFixture([]byte(altered)), []byte(`{"model":"fixture","input":"hello"}`), "POST", "/v1/responses", c.Expected, time.Unix(1700000000, 0))
			if err == nil {
				t.Fatal("accepted ambiguous or noncanonical signed claims")
			}
		})
	}
}

func TestSessionBindingRejectsTamperingRotationAndBounds(t *testing.T) {
	c := bindingClaims()
	raw, _ := json.Marshal(c)
	valid := sealBindingFixture(raw)
	type attempt struct {
		key, header, body, method, path string
		identity                        managedfiles.Identity
		now                             time.Time
	}
	base := attempt{strings.Repeat("k", 32), valid, `{"model":"fixture","input":"hello"}`, "POST", "/v1/responses", c.Expected, time.Unix(1700000000, 0)}
	for name, mutate := range map[string]func(*attempt){
		"key short":         func(a *attempt) { a.key = "short" },
		"key rotated":       func(a *attempt) { a.key = strings.Repeat("q", 32) },
		"header whitespace": func(a *attempt) { a.header = " " + a.header },
		"header padded":     func(a *attempt) { a.header += "=" },
		"header oversized":  func(a *attempt) { a.header = strings.Repeat("a", 4097) },
		"header multiple":   func(a *attempt) { a.header += ".x" },
		"tampered owner": func(a *attempt) {
			a.header = base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), "owner-1", "owner-2", 1))) + "." + strings.Split(valid, ".")[1]
		},
		"body byte":     func(a *attempt) { a.body += " " },
		"method":        func(a *attempt) { a.method = "GET" },
		"path":          func(a *attempt) { a.path = "/responses" },
		"backend":       func(a *attempt) { a.identity.BackendID = "other" },
		"subject":       func(a *attempt) { a.identity.SubjectFingerprint = "sha256:" + strings.Repeat("2", 64) },
		"generation":    func(a *attempt) { a.identity.CredentialGeneration = "rotated" },
		"expired":       func(a *attempt) { a.now = time.Unix(1700000030, 0) },
		"future window": func(a *attempt) { a.now = time.Unix(1699999909, 0) },
	} {
		t.Run(name, func(t *testing.T) {
			a := base
			mutate(&a)
			if _, err := managedfiles.VerifySessionBinding(a.key, a.header, []byte(a.body), a.method, a.path, a.identity, a.now); err == nil {
				t.Fatal("accepted invalid binding")
			}
		})
	}
	for name, mutate := range map[string]func(*managedfiles.SessionBindingClaims){
		"version":      func(c *managedfiles.SessionBindingClaims) { c.Version = "other" },
		"owner":        func(c *managedfiles.SessionBindingClaims) { c.OwnerRef = "\nowner" },
		"group":        func(c *managedfiles.SessionBindingClaims) { c.GroupRef = "" },
		"conversation": func(c *managedfiles.SessionBindingClaims) { c.ConversationRef = strings.Repeat("c", 257) },
		"identity":     func(c *managedfiles.SessionBindingClaims) { c.Expected.SubjectFingerprint = "bad" },
		"method":       func(c *managedfiles.SessionBindingClaims) { c.Method = "GET" },
		"path":         func(c *managedfiles.SessionBindingClaims) { c.Path = "/v1/responses?x" },
		"hash":         func(c *managedfiles.SessionBindingClaims) { c.WireSHA256 = strings.Repeat("A", 64) },
		"expiry":       func(c *managedfiles.SessionBindingClaims) { c.ExpiresAt = 0 },
	} {
		t.Run("sign/"+name, func(t *testing.T) {
			changed := c
			mutate(&changed)
			if _, err := managedfiles.SignSessionBinding(strings.Repeat("k", 32), changed); err == nil {
				t.Fatal("signed invalid claims")
			}
		})
	}
	if _, err := managedfiles.SignSessionBinding("short", c); err == nil {
		t.Fatal("signed with short private key")
	}
}

func TestSessionBindingManifestPairAndDigest(t *testing.T) {
	m := exampleManifest()
	legacy, err := managedfiles.Bind(m, []managedfiles.EnsureResult{exampleUpload(m)})
	if err != nil {
		t.Fatal(err)
	}
	m.SessionBindingContract = managedfiles.SessionBindingContract
	m.ConversationRef = "conversation-key-1"
	firstRecovery, err := managedfiles.RecoveryManifestHash(m)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(m)
	decoded, err := managedfiles.DecodeManifest(encoded)
	if err != nil || decoded.ConversationRef != m.ConversationRef {
		t.Fatalf("valid extended manifest rejected: %v", err)
	}
	bound, err := managedfiles.Bind(m, []managedfiles.EnsureResult{exampleUpload(m)})
	if err != nil {
		t.Fatal(err)
	}
	if bound.BindingSHA256 == legacy.BindingSHA256 || bound.WireSHA256 != legacy.WireSHA256 || bound.TemplateSHA256 != legacy.TemplateSHA256 {
		t.Fatal("conversation binding is not independent of wire/template")
	}
	m.ConversationRef = "conversation-key-2"
	second, err := managedfiles.Bind(m, []managedfiles.EnsureResult{exampleUpload(m)})
	if err != nil || second.BindingSHA256 == bound.BindingSHA256 {
		t.Fatal("conversation change escaped binding")
	}
	secondRecovery, err := managedfiles.RecoveryManifestHash(m)
	if err != nil || firstRecovery == secondRecovery {
		t.Fatal("conversation change escaped recovery manifest binding")
	}
	for _, pair := range [][2]string{{managedfiles.SessionBindingContract, ""}, {"", "conversation"}, {"other", "conversation"}, {managedfiles.SessionBindingContract, " bad"}} {
		m := exampleManifest()
		m.SessionBindingContract = pair[0]
		m.ConversationRef = pair[1]
		if managedfiles.ValidateManifest(m) == nil {
			t.Fatal("accepted invalid typed session pair")
		}
	}
	m = exampleManifest()
	legacyWire, _ := json.Marshal(m)
	for _, extra := range []string{`"session_binding_contract":""`, `"conversation_ref":"x"`, `"session_binding_contract":"bps-session-binding/1","conversation_ref":null`, `"session_binding_contract":"bps-session-binding/1","conversation_ref":""`, `"session_binding_contract":"bps-session-binding/1","conversation_ref":1`, `"Session_Binding_Contract":"bps-session-binding/1","conversation_ref":"x"`} {
		wire := `{` + extra + `,` + string(legacyWire[1:])
		if _, err := managedfiles.DecodeManifest([]byte(wire)); err == nil {
			t.Fatal("accepted malformed wire session pair")
		}
	}
	if strings.Contains(string(legacyWire), "conversation_ref") || strings.Contains(string(legacyWire), "session_binding_contract") {
		t.Fatal("legacy wire no longer omits extension")
	}
	m.SessionBindingContract = managedfiles.SessionBindingContract
	m.ConversationRef = "conversation"
	m.Files = nil
	if managedfiles.ValidateManifest(m) == nil {
		t.Fatal("session extension allowed empty Files admission")
	}
}
