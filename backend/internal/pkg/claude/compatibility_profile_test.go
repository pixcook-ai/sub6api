package claude

import (
	"strings"
	"testing"
)

const testProfileJSON = `{"lang":"js","package_version":"0.112.1","os":"Linux","arch":"x64","runtime":"node","runtime_version":"v22.19.0"}`

func TestParseCompatibilityProfile(t *testing.T) {
	p, err := ParseCompatibilityProfile(testProfileJSON)
	if err != nil || p.OS != "Linux" || len(p.Headers()) != 6 {
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
