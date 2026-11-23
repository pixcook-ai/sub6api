package managedfiles_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

func TestCanonicalPreservesNumbersAndNormalizesObjectStrings(t *testing.T) {
	raw := []byte(` { "z":1e400,"big":9007199254740993,"u":"\u0061<>&\u2028😀","a": [1,1.0,1e0,-0] } `)
	got, err := managedfiles.Canonical(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":[1,1.0,1e0,-0],"big":9007199254740993,"u":"a\u003c\u003e\u0026\u2028😀","z":1e400}`
	if string(got) != want {
		t.Fatalf("canonical mismatch\ngot %s\nwant%s", got, want)
	}
}

func TestParseRejectsAmbiguousAndInvalidJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
		code string
	}{
		{"duplicate", []byte(`{"a":1,"a":2}`), "duplicate_json_key"},
		{"escaped duplicate", []byte(`{"a":1,"\u0061":2}`), "duplicate_json_key"},
		{"nested duplicate", []byte(`[{"x":{"a":1,"a":2}}]`), "duplicate_json_key"},
		{"high surrogate", []byte(`{"s":"\ud800"}`), "invalid_managed_json"},
		{"low surrogate", []byte(`{"s":"\udc00"}`), "invalid_managed_json"},
		{"high then nonlow", []byte(`{"s":"\ud800\u0061"}`), "invalid_managed_json"},
		{"trailing value", []byte(`{} []`), "invalid_managed_json"},
		{"invalid utf8", []byte{'"', 0xff, '"'}, "invalid_managed_json"},
		{"depth", []byte(strings.Repeat("[", 129) + "0" + strings.Repeat("]", 129)), "managed_json_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := managedfiles.Parse(tc.raw)
			var typed *managedfiles.Error
			if !errors.As(err, &typed) || typed.Code != tc.code {
				t.Fatalf("got %v; want %s", err, tc.code)
			}
		})
	}
}

func TestCanonicalAcceptsPairedSurrogateAndLiteralEscape(t *testing.T) {
	got, err := managedfiles.Canonical([]byte(`{"pair":"\ud83d\ude00","literal":"\\ud800"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"literal":"\\ud800","pair":"😀"}` {
		t.Fatalf("got %s", got)
	}
}

func TestParseAppliesDepthLimitToEmptyAndPopulatedContainers(t *testing.T) {
	for _, leaf := range []string{"", "0"} {
		atLimit := []byte(strings.Repeat("[", 128) + leaf + strings.Repeat("]", 128))
		if _, err := managedfiles.Parse(atLimit); err != nil {
			t.Fatalf("128 array containers rejected: %v", err)
		}
		overLimit := []byte(strings.Repeat("[", 129) + leaf + strings.Repeat("]", 129))
		if _, err := managedfiles.Parse(overLimit); err == nil {
			t.Fatal("129 array containers accepted")
		}
	}
}
