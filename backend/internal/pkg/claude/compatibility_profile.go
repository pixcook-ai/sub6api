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

// CompatibilityProfile describes explicitly configured SDK headers and optional
// per-model OAuth capability selections.
// CLI version, device identity and TLS remain independently managed.
type CompatibilityProfile struct {
	Lang           string `json:"lang"`
	PackageVersion string `json:"package_version"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	Runtime        string `json:"runtime"`
	RuntimeVersion string `json:"runtime_version"`
	// Entrypoint selects the observed CLI invocation mode. Omitted profiles
	// retain the legacy interactive CLI header behavior.
	Entrypoint string `json:"entrypoint,omitempty"`
	// Keep endpoint-specific capability selections immutable to callers.
	modelBetas            map[string][]string
	countTokensModelBetas map[string][]string
}

func ParseCompatibilityProfile(raw string) (CompatibilityProfile, error) {
	var p CompatibilityProfile
	defaultEntrypoint := "cli"
	// A non-nil default distinguishes an omitted optional field from JSON null.
	wire := struct {
		CompatibilityProfile
		Entrypoint            *string        `json:"entrypoint"`
		ModelBetas            modelBetaInput `json:"model_betas"`
		CountTokensModelBetas modelBetaInput `json:"count_tokens_model_betas"`
	}{Entrypoint: &defaultEntrypoint}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return p, fmt.Errorf("invalid compatibility profile JSON")
	}
	p = wire.CompatibilityProfile
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return p, fmt.Errorf("compatibility profile must contain one JSON object")
	}
	if wire.Entrypoint == nil || (*wire.Entrypoint != "cli" && *wire.Entrypoint != "sdk-cli") {
		return p, fmt.Errorf("invalid compatibility profile entrypoint")
	}
	p.Entrypoint = *wire.Entrypoint
	for name, value := range p.Headers() {
		if value == "" || len(value) > 128 || strings.TrimSpace(value) != value || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return p, fmt.Errorf("invalid compatibility profile field %s", name)
		}
	}
	var err error
	if p.modelBetas, err = validateModelBetas(wire.ModelBetas.values); err != nil {
		return p, fmt.Errorf("invalid model_betas: %w", err)
	}
	if p.countTokensModelBetas, err = validateModelBetas(wire.CountTokensModelBetas.values); err != nil {
		return p, fmt.Errorf("invalid count_tokens_model_betas: %w", err)
	}
	return p, nil
}

// Reject duplicate outer fields and model keys before encoding/json can merge
// or overwrite maps. This also distinguishes an omitted field from JSON null.
type modelBetaInput struct {
	seen   bool
	values map[string][]string
}

func (input *modelBetaInput) UnmarshalJSON(raw []byte) error {
	if input.seen {
		return fmt.Errorf("duplicate model beta field")
	}
	input.seen = true
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	start, err := dec.Token()
	if err != nil || start != json.Delim('{') {
		return fmt.Errorf("expected a model beta object")
	}
	input.values = make(map[string][]string)
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return err
		}
		model, ok := key.(string)
		if !ok {
			return fmt.Errorf("expected a model identifier")
		}
		if _, exists := input.values[model]; exists {
			return fmt.Errorf("duplicate model identifier")
		}
		var tokens []string
		if err := dec.Decode(&tokens); err != nil {
			return err
		}
		input.values[model] = tokens
	}
	_, err = dec.Token()
	return err
}

func validateModelBetas(raw map[string][]string) (map[string][]string, error) {
	if len(raw) > 64 {
		return nil, fmt.Errorf("expected an object with at most 64 models")
	}
	if len(raw) == 0 {
		return nil, nil
	}
	normalized := make(map[string][]string, len(raw))
	for model, tokens := range raw {
		if !validCompatibilityToken(model, 256) {
			return nil, fmt.Errorf("invalid model identifier")
		}
		model = NormalizeModelID(model)
		if _, exists := normalized[model]; exists {
			return nil, fmt.Errorf("duplicate normalized model identifier")
		}
		if len(tokens) < 2 || len(tokens) > 32 {
			return nil, fmt.Errorf("expected 2 to 32 beta tokens per model")
		}
		seen := make(map[string]bool, len(tokens))
		for _, token := range tokens {
			if !validCompatibilityToken(token, 128) || seen[token] {
				return nil, fmt.Errorf("invalid or duplicate beta token")
			}
			seen[token] = true
		}
		if !seen[BetaClaudeCode] || !seen[BetaOAuth] {
			return nil, fmt.Errorf("Claude Code and OAuth beta tokens are required")
		}
		normalized[model] = append([]string(nil), tokens...)
	}
	return normalized, nil
}

func validCompatibilityToken(value string, maxLength int) bool {
	if len(value) == 0 || len(value) > maxLength {
		return false
	}
	for _, c := range value {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}

// MimicBetas returns the explicitly selected complete base set for one endpoint
// and normalized upstream model. Unconfigured models keep the existing policy.
func (p CompatibilityProfile) MimicBetas(modelID string, countTokens bool) ([]string, bool) {
	models := p.modelBetas
	if countTokens {
		models = p.countTokensModelBetas
	}
	tokens, ok := models[NormalizeModelID(modelID)]
	return append([]string(nil), tokens...), ok
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
			Entrypoint: "cli",
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
