package service

import (
	"net/http"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/pkg/claude"
)

func TestDefaultFingerprintAndMimicUseSameCompatibilityHeaders(t *testing.T) {
	s := &IdentityService{}
	fp := defaultFingerprint()
	ordinary, _ := http.NewRequest("POST", "https://example.invalid", nil)
	mimic, _ := http.NewRequest("POST", "https://example.invalid", nil)
	s.ApplyFingerprint(ordinary, &fp)
	applyClaudeCodeMimicHeaders(mimic, true, fp.UserAgent)
	for name, want := range claude.DefaultCompatibilityProfile().Headers() {
		if getHeaderRaw(ordinary.Header, name) != want || getHeaderRaw(mimic.Header, name) != want {
			t.Fatalf("inconsistent compatibility header %s", name)
		}
	}
}
