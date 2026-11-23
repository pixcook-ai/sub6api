package managedfiles_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

func TestSharedWireFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/bps-files-v1-fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Success []struct {
			Name              string                      `json:"name"`
			Manifest          managedfiles.AdmitManifest  `json:"manifest"`
			Results           []managedfiles.EnsureResult `json:"ensure_results"`
			CanonicalTemplate string                      `json:"canonical_template"`
			CanonicalWire     string                      `json:"canonical_wire"`
			TemplateHash      string                      `json:"template_sha256"`
			WireHash          string                      `json:"wire_sha256"`
			BindingHash       string                      `json:"binding_sha256"`
		} `json:"success"`
		Canonical []struct {
			Name, Raw, Canonical string
			TemplateHash         string `json:"template_sha256"`
			WireHash             string `json:"wire_sha256"`
		} `json:"canonical"`
		Reject []struct {
			Name, Raw, Code string
			Base64          string `json:"raw_base64"`
		} `json:"reject_json"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Success {
		t.Run(tc.Name, func(t *testing.T) {
			result, err := managedfiles.Bind(tc.Manifest, tc.Results)
			if err != nil {
				t.Fatal(err)
			}
			if string(result.Wire) != tc.CanonicalWire || result.TemplateSHA256 != tc.TemplateHash || result.WireSHA256 != tc.WireHash || result.BindingSHA256 != tc.BindingHash {
				t.Fatalf("bound result mismatch: %+v", result)
			}
		})
	}
	for _, tc := range fixture.Canonical {
		t.Run(tc.Name, func(t *testing.T) {
			got, err := managedfiles.Canonical([]byte(tc.Raw))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.Canonical {
				t.Fatalf("canonical got%s want%s", got, tc.Canonical)
			}
			template, err := managedfiles.TemplateHash([]byte(tc.Raw))
			if err != nil {
				t.Fatal(err)
			}
			wire, err := managedfiles.WireHash([]byte(tc.Raw))
			if err != nil {
				t.Fatal(err)
			}
			if template != tc.TemplateHash || wire != tc.WireHash {
				t.Fatalf("domain hashes mismatch: %s %s", template, wire)
			}
		})
	}
	for _, tc := range fixture.Reject {
		t.Run(tc.Name, func(t *testing.T) {
			data := []byte(tc.Raw)
			if tc.Base64 != "" {
				data, err = base64.StdEncoding.DecodeString(tc.Base64)
				if err != nil {
					t.Fatal(err)
				}
			}
			_, err := managedfiles.Canonical(data)
			var typed *managedfiles.Error
			if !errors.As(err, &typed) || typed.Code != tc.Code {
				t.Fatalf("got %v want%s", err, tc.Code)
			}
		})
	}
}
