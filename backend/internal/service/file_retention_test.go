package service

import (
	"errors"
	"strings"
	"testing"
)

func TestFileExpiryInputExplicitRetention(t *testing.T) {
	var input FileExpiryInput
	if err := input.Set("expires_after[seconds]", "7200"); err != nil {
		t.Fatal(err)
	}
	if err := input.Set("expires_after[anchor]", "created_at"); err != nil {
		t.Fatal(err)
	}
	seconds, err := input.RetentionSeconds("batch", 3600)
	if err != nil {
		t.Fatal(err)
	}
	if seconds == nil || *seconds != 7200 {
		t.Fatalf("explicit retention must override batch and configured defaults, got %v", seconds)
	}
}

func TestFileExpiryInputSetRejectsMalformedFields(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
		value string
		input FileExpiryInput
	}{
		{name: "unknown", field: "expires_after", value: "{}"},
		{name: "unknown subfield", field: "expires_after[other]", value: "created_at"},
		{name: "duplicate anchor", field: "expires_after[anchor]", value: "other", input: FileExpiryInput{Anchor: "created_at", AnchorSet: true}},
		{name: "duplicate seconds", field: "expires_after[seconds]", value: "7200", input: FileExpiryInput{Seconds: "3600", SecondsSet: true}},
		{name: "anchor too long", field: "expires_after[anchor]", value: strings.Repeat("a", 128)},
		{name: "seconds too long", field: "expires_after[seconds]", value: strings.Repeat("1", 128)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := tc.input
			if err := input.Set(tc.field, tc.value); !errors.Is(err, ErrInvalidFileRetention) {
				t.Fatalf("want invalid retention, got %v", err)
			}
			if input != tc.input {
				t.Fatalf("rejected field changed existing expiry input: %+v", input)
			}
		})
	}
}

func TestFileExpiryInputRequiresExactCreatedAtPolicy(t *testing.T) {
	valid := FileExpiryInput{Anchor: "created_at", Seconds: "3600", AnchorSet: true, SecondsSet: true}
	for _, tc := range []struct {
		name string
		edit func(*FileExpiryInput)
	}{
		{"missing anchor", func(i *FileExpiryInput) { i.Anchor, i.AnchorSet = "", false }},
		{"missing seconds", func(i *FileExpiryInput) { i.Seconds, i.SecondsSet = "", false }},
		{"wrong anchor", func(i *FileExpiryInput) { i.Anchor = "last_active_at" }},
		{"padded anchor", func(i *FileExpiryInput) { i.Anchor = "created_at " }},
		{"oversized anchor", func(i *FileExpiryInput) { i.Anchor = strings.Repeat("a", 128) }},
		{"oversized seconds", func(i *FileExpiryInput) { i.Seconds = strings.Repeat("0", 128) + "3600" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := valid
			tc.edit(&input)
			if seconds, err := input.RetentionSeconds("user_data", 0); !errors.Is(err, ErrInvalidFileRetention) || seconds != nil {
				t.Fatalf("want invalid retention without TTL, got %v, %v", seconds, err)
			}
		})
	}
	for _, text := range []string{"", "0", "3599", "2592001", "3600.0", "3.6e3", "+3600", "-3600", " 3600", "3600 ", "3600\n", "null", "0xE10", "٣٦٠٠", "３６００", "3600\x00", "9223372036854775808"} {
		t.Run("seconds="+text, func(t *testing.T) {
			input := valid
			input.Seconds = text
			if seconds, err := input.RetentionSeconds("user_data", 0); !errors.Is(err, ErrInvalidFileRetention) || seconds != nil {
				t.Fatalf("want invalid retention without TTL, got %v, %v", seconds, err)
			}
		})
	}
	for _, text := range []string{"3600", "2592000", "0003600"} {
		input := valid
		input.Seconds = text
		seconds, err := input.RetentionSeconds("user_data", 0)
		if err != nil || seconds == nil || (*seconds != 3600 && *seconds != 2592000) {
			t.Fatalf("valid decimal boundary %q rejected: %v, %v", text, seconds, err)
		}
	}
}

func TestFileExpiryInputDefaultPolicy(t *testing.T) {
	for _, tc := range []struct {
		purpose        string
		defaultSeconds int64
		want           int64
	}{
		{"batch", 0, 2592000},
		{"batch", 3600, 2592000},
		{"user_data", 0, 0},
		{"assistants", 0, 0},
		{"user_data", 3600, 3600},
		{"user_data", 2592000, 2592000},
	} {
		seconds, err := (FileExpiryInput{}).RetentionSeconds(tc.purpose, tc.defaultSeconds)
		if err != nil {
			t.Fatalf("valid default for %s rejected: %v", tc.purpose, err)
		}
		if tc.want == 0 {
			if seconds != nil {
				t.Fatalf("%s must not expire by default", tc.purpose)
			}
		} else if seconds == nil || *seconds != tc.want {
			t.Fatalf("%s default retention must be %d, got %v", tc.purpose, tc.want, seconds)
		}
	}

	explicit := FileExpiryInput{Anchor: "created_at", Seconds: "7200", AnchorSet: true, SecondsSet: true}
	for _, invalidDefault := range []int64{-1, 1, 3599, 2592001} {
		for _, purpose := range []string{"user_data", "batch"} {
			for _, input := range []FileExpiryInput{{}, explicit} {
				if seconds, err := input.RetentionSeconds(purpose, invalidDefault); !errors.Is(err, ErrInvalidFileRetention) || seconds != nil {
					t.Fatalf("invalid configured retention must fail without fallback: default=%d, ttl=%v, err=%v", invalidDefault, seconds, err)
				}
			}
		}
	}
	for _, input := range []FileExpiryInput{
		{Anchor: "created_at"},
		{Seconds: "3600"},
		{Anchor: "created_at", Seconds: "3600"},
		{Anchor: strings.Repeat("a", 128)},
		{Seconds: strings.Repeat("0", 128)},
	} {
		if seconds, err := input.RetentionSeconds("user_data", 0); !errors.Is(err, ErrInvalidFileRetention) || seconds != nil {
			t.Fatalf("unmarked or oversized fields cannot bypass explicit input validation: %+v, %v, %v", input, seconds, err)
		}
	}
}
