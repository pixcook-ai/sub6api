package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
)

const CompatibilityProfileEnv = "SUB2API_CLAUDE_COMPATIBILITY_PROFILE"

// CompatibilityProfile describes an explicitly configured SDK header profile.
// CLI version, device identity and TLS remain independently managed.
type CompatibilityProfile struct {
	Lang           string `json:"lang"`
	PackageVersion string `json:"package_version"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	Runtime        string `json:"runtime"`
	RuntimeVersion string `json:"runtime_version"`
}

func ParseCompatibilityProfile(raw string) (CompatibilityProfile, error) {
	var p CompatibilityProfile
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, fmt.Errorf("invalid compatibility profile JSON")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return p, fmt.Errorf("compatibility profile must contain one JSON object")
	}
	for name, value := range p.Headers() {
		if value == "" || len(value) > 128 || strings.TrimSpace(value) != value || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return p, fmt.Errorf("invalid compatibility profile field %s", name)
		}
	}
	return p, nil
}

func (p CompatibilityProfile) Headers() map[string]string {
	return map[string]string{
		"X-Stainless-Lang":            p.Lang,
		"X-Stainless-Package-Version": p.PackageVersion,
		"X-Stainless-OS":              p.OS,
		"X-Stainless-Arch":            p.Arch,
		"X-Stainless-Runtime":         p.Runtime,
		"X-Stainless-Runtime-Version": p.RuntimeVersion,
	}
}

var configuredCompatibilityProfile = loadCompatibilityProfile()

func loadCompatibilityProfile() CompatibilityProfile {
	raw := os.Getenv(CompatibilityProfileEnv)
	if raw == "" {
		return CompatibilityProfile{
			Lang: "js", PackageVersion: StainlessPackageVersion, OS: StainlessOS,
			Arch: StainlessArch, Runtime: StainlessRuntime, RuntimeVersion: StainlessRuntimeVersion,
		}
	}
	p, err := ParseCompatibilityProfile(raw)
	if err != nil {
		panic(CompatibilityProfileEnv + ": " + err.Error())
	}
	return p
}

func DefaultCompatibilityProfile() CompatibilityProfile {
	return configuredCompatibilityProfile
}
