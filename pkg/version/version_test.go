package version

import (
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	tests := []struct {
		name    string
		version string
		commit  string
		date    string
		dirty   string
		want    string
	}{
		{name: "semver without date", version: "1.2.3", commit: "abcdef123456", dirty: "", want: "1.2.3"},
		{name: "dev falls back to commit", version: "dev", commit: "abcdef123456", dirty: "", want: "abcdef123456"},
		{name: "dev without commit", version: "dev", commit: "unknown", dirty: "", want: "dev"},
		{name: "empty version falls back to commit", version: "", commit: "abcdef123456", dirty: "", want: "abcdef123456"},
		{name: "dirty suffix", version: "1.2.3", commit: "abcdef123456", dirty: "-dirty", want: "1.2.3-dirty"},
		{name: "dirty suffix not duplicated", version: "1.2.3-dirty", commit: "abcdef123456", dirty: "-dirty", want: "1.2.3-dirty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Version, Commit, Date, Dirty = tt.version, tt.commit, tt.date, tt.dirty
			if got := String(); got != tt.want {
				t.Fatalf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStringIncludesSanitizedDate(t *testing.T) {
	Version, Commit, Date, Dirty = "1.2.3", "abcdef123456", "2024-01-02T03:04:05+00:00", ""
	got := String()
	if !strings.HasPrefix(got, "1.2.3_") {
		t.Fatalf("String() = %q, want a date suffix after the version", got)
	}
	if strings.ContainsAny(got, ": ") {
		t.Fatalf("String() = %q, want dates without separators", got)
	}
}

func TestNormalizeDateRemovesSeparators(t *testing.T) {
	for _, in := range []string{
		"2024-01-02T03:04:05+00:00",
		"2024-01-02 03:04:05",
		"  2024-01-02T03:04:05Z  ",
	} {
		got := normalizeDate(in)
		if strings.ContainsAny(got, ": ") {
			t.Fatalf("normalizeDate(%q) = %q, want no spaces or colons", in, got)
		}
	}
}

func TestShortCommit(t *testing.T) {
	if got := shortCommit("abcdef1234567890"); got != "abcdef123456" {
		t.Fatalf("shortCommit(long) = %q, want 12 characters", got)
	}
	if got := shortCommit("abc"); got != "abc" {
		t.Fatalf("shortCommit(short) = %q, want unchanged", got)
	}
}
