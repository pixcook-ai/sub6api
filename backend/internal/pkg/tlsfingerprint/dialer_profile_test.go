package tlsfingerprint

import (
	"bytes"
	"slices"
	"testing"

	utls "github.com/refraction-networking/utls"
)

func TestApplyPresetKeepsProfileSupportedVersions(t *testing.T) {
	want := []uint16{0xfafa, utls.VersionTLS13, utls.VersionTLS12}
	profile := &Profile{SupportedVersions: slices.Clone(want)}
	// ApplyPreset replaces the GREASE value in each connection's version list.
	// Use distinct seeds so one preset cannot accidentally mask a shared slice.
	apply := func(seed byte) []uint16 {
		t.Helper()
		config := &utls.Config{ServerName: "example.test", Rand: bytes.NewReader(bytes.Repeat([]byte{seed}, 4096))}
		conn := utls.UClient(nil, config, utls.HelloCustom)
		if err := conn.ApplyPreset(buildClientHelloSpecFromProfile(profile)); err != nil {
			t.Fatalf("apply TLS preset: %v", err)
		}
		for _, extension := range conn.Extensions {
			if versions, ok := extension.(*utls.SupportedVersionsExtension); ok {
				return versions.Versions
			}
		}
		t.Fatal("missing supported versions extension")
		return nil
	}

	first := apply(0x11)
	second := apply(0x22)
	if !slices.Equal(profile.SupportedVersions, want) {
		t.Errorf("profile versions changed to %x, want %x", profile.SupportedVersions, want)
	}
	if !slices.Equal(first, []uint16{0x1a1a, utls.VersionTLS13, utls.VersionTLS12}) ||
		!slices.Equal(second, []uint16{0x2a2a, utls.VersionTLS13, utls.VersionTLS12}) {
		t.Errorf("connection versions = %x and %x, want distinct GREASE values followed by TLS 1.3 and TLS 1.2", first, second)
	}
}
