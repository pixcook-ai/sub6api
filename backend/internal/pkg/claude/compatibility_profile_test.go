package claude

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const testProfileJSON = `{"lang":"js","package_version":"0.112.1","os":"Linux","arch":"x64","runtime":"node","runtime_version":"v22.19.0"}`

func TestParseCompatibilityProfile(t *testing.T) {
	p, err := ParseCompatibilityProfile(testProfileJSON)
	if err != nil || p.OS != "Linux" || len(p.Headers()) != 6 || p.Entrypoint != "cli" {
		t.Fatalf("profile parse failed: %+v %v", p, err)
	}
	for _, raw := range []string{
		`null`, `{}`, `{"os":"Linux"}`, testProfileJSON + `{}`,
		strings.Replace(testProfileJSON, `"Linux"`, `"Linux\r\nX-Test: yes"`, 1),
		strings.Replace(testProfileJSON, `"Linux"`, `" Linux"`, 1),
		strings.Replace(testProfileJSON, `"Linux"`, `"`+strings.Repeat("x", 129)+`"`, 1),
		strings.Replace(testProfileJSON, `"os":`, `"unknown":`, 1),
	} {
		if _, err := ParseCompatibilityProfile(raw); err == nil {
			t.Fatalf("accepted invalid profile: %q", raw)
		}
	}
}

func TestConfiguredCompatibilityProfileHeaders(t *testing.T) {
	previous := configuredCompatibilityProfile
	t.Cleanup(func() { configuredCompatibilityProfile = previous })
	p, err := ParseCompatibilityProfile(testProfileJSON)
	if err != nil {
		t.Fatal(err)
	}
	configuredCompatibilityProfile = p
	for name, want := range p.Headers() {
		if got := DefaultHeaders()[name]; got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if DefaultHeaders()["User-Agent"] != DefaultUserAgent() {
		t.Fatal("profile changed CLI version source")
	}
}

func TestLoadCompatibilityProfile(t *testing.T) {
	t.Setenv(CompatibilityProfileEnv, testProfileJSON)
	if got := loadCompatibilityProfile(); got.OS != "Linux" {
		t.Fatal("environment profile was not loaded")
	}
	t.Setenv(CompatibilityProfileEnv, "")
	if got := loadCompatibilityProfile(); got.OS != StainlessOS {
		t.Fatal("legacy default changed without explicit configuration")
	}
	t.Setenv(CompatibilityProfileEnv, `{"os":"Linux"}`)
	defer func() {
		if recover() == nil {
			t.Fatal("invalid explicit configuration did not stop startup")
		}
	}()
	loadCompatibilityProfile()
}

func TestConfiguredCompatibilityProfileHeadlessEntrypoint(t *testing.T) {
	previous := configuredCompatibilityProfile
	t.Cleanup(func() { configuredCompatibilityProfile = previous })
	raw := strings.TrimSuffix(testProfileJSON, "}") + `,"entrypoint":"sdk-cli"}`
	p, err := ParseCompatibilityProfile(raw)
	if err != nil {
		t.Fatalf("headless profile rejected: %v", err)
	}
	configuredCompatibilityProfile = p
	if ua := DefaultHeaders()["User-Agent"]; !strings.HasSuffix(ua, " (external, sdk-cli)") {
		t.Fatalf("headless profile User-Agent = %q", ua)
	}
	for _, value := range []string{"", "unknown", " sdk-cli", "sdk-cli\r\n"} {
		invalid := strings.Replace(raw, `"sdk-cli"`, strconv.Quote(value), 1)
		if _, err := ParseCompatibilityProfile(invalid); err == nil {
			t.Fatalf("accepted invalid entrypoint %q", value)
		}
	}
	if _, err := ParseCompatibilityProfile(strings.Replace(raw, `"sdk-cli"`, "null", 1)); err == nil {
		t.Fatal("accepted null entrypoint")
	}
}

func TestParseCompatibilityProfileModelBetas(t *testing.T) {
	raw := strings.TrimSuffix(testProfileJSON, "}") + `,"model_betas":{"claude-sonnet-4-5-20250929":["claude-code-20250219","oauth-2025-04-20","thinking-token-count-2026-05-13"]},"count_tokens_model_betas":{"claude-sonnet-4-5-20250929":["claude-code-20250219","oauth-2025-04-20","token-counting-2024-11-01"]}}`
	p, err := ParseCompatibilityProfile(raw)
	if err != nil {
		t.Fatalf("explicit per-model endpoint betas rejected: %v", err)
	}
	for _, test := range []struct {
		count bool
		last  string
	}{{false, "thinking-token-count-2026-05-13"}, {true, "token-counting-2024-11-01"}} {
		want := []string{"claude-code-20250219", "oauth-2025-04-20", test.last}
		got, ok := p.MimicBetas("claude-sonnet-4-5", test.count)
		if !ok || !slices.Equal(got, want) {
			t.Fatalf("endpoint %t beta selection = %v, %t", test.count, got, ok)
		}
		got[0] = "changed-by-caller"
		again, _ := p.MimicBetas("claude-sonnet-4-5-20250929", test.count)
		if !slices.Equal(again, want) {
			t.Fatal("caller mutated the configured beta selection")
		}
		if tokens, ok := p.MimicBetas("claude-haiku-4-5", test.count); ok || tokens != nil {
			t.Fatal("unconfigured model acquired an override")
		}
	}
	messageOnly := strings.TrimSuffix(testProfileJSON, "}") + `,"model_betas":{"claude-sonnet-4-5":["claude-code-20250219","oauth-2025-04-20"]}}`
	p, err = ParseCompatibilityProfile(messageOnly)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.MimicBetas("claude-sonnet-4-5", true); ok {
		t.Fatal("messages override leaked to count_tokens")
	}
	countOnly := strings.Replace(messageOnly, `"model_betas"`, `"count_tokens_model_betas"`, 1)
	p, err = ParseCompatibilityProfile(countOnly)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.MimicBetas("claude-sonnet-4-5", false); ok {
		t.Fatal("count_tokens override leaked to messages")
	}
	p, err = ParseCompatibilityProfile(strings.TrimSuffix(testProfileJSON, "}") + `,"model_betas":{},"count_tokens_model_betas":{}}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, count := range []bool{false, true} {
		if _, ok := p.MimicBetas("claude-sonnet-4-5", count); ok {
			t.Fatal("empty override object changed the default policy")
		}
	}
}

func TestParseCompatibilityProfileRejectsInvalidModelBetas(t *testing.T) {
	base := []string{"claude-code-20250219", "oauth-2025-04-20"}
	tooMany := make(map[string][]string)
	for i := range 65 {
		tooMany["model-"+strconv.Itoa(i)] = base
	}
	tooManyTokens := append([]string(nil), base...)
	for i := range 31 {
		tooManyTokens = append(tooManyTokens, "beta-"+strconv.Itoa(i))
	}
	for _, field := range []string{"model_betas", "count_tokens_model_betas"} {
		for _, invalid := range []any{
			nil, []string{},
			map[string][]string{"model": nil},
			map[string][]string{"model": {"claude-code-20250219"}},
			map[string][]string{"model": {"oauth-2025-04-20", "other-beta"}},
			map[string][]string{"model": {"claude-code-20250219", "oauth-2025-04-20", "oauth-2025-04-20"}},
			map[string][]string{"model": {"claude-code-20250219", "oauth-2025-04-20", "a,b"}},
			map[string][]string{"model": {"claude-code-20250219", "oauth-2025-04-20", "beta\r\nX-Test: yes"}},
			map[string][]string{"model": {"claude-code-20250219", "oauth-2025-04-20", strings.Repeat("x", 129)}},
			map[string][]string{"model": tooManyTokens},
			map[string][]string{" bad-model": base},
			map[string][]string{strings.Repeat("x", 257): base},
			map[string][]string{"claude-sonnet-4-5": base, "claude-sonnet-4-5-20250929": base},
			tooMany,
		} {
			encoded, err := json.Marshal(invalid)
			if err != nil {
				t.Fatal(err)
			}
			raw := strings.TrimSuffix(testProfileJSON, "}") + `,"` + field + `":` + string(encoded) + `}`
			if _, err := ParseCompatibilityProfile(raw); err == nil {
				t.Fatalf("accepted invalid %s: %s", field, encoded)
			}
		}
	}
}

func TestParseCompatibilityProfileRejectsDuplicateBetaKeys(t *testing.T) {
	const models = `{"model":["claude-code-20250219","oauth-2025-04-20"]}`
	for _, field := range []string{"model_betas", "count_tokens_model_betas"} {
		for _, suffix := range []string{
			`,"` + field + `":` + models + `,"` + field + `":{}`,
			`,"` + field + `":{},"` + strings.ToUpper(field) + `":` + models,
			`,"` + field + `":{"model":["claude-code-20250219","oauth-2025-04-20"],"model":["claude-code-20250219","oauth-2025-04-20","other-beta"]}`,
		} {
			if _, err := ParseCompatibilityProfile(strings.TrimSuffix(testProfileJSON, "}") + suffix + `}`); err == nil {
				t.Fatalf("accepted duplicate keys for %s", field)
			}
		}
	}
}
