package main

import (
	"strings"
	"testing"
)

// The release build stamps version/commit/date via ldflags; that stamp is the
// identity of a published binary, so it must survive verbatim (abbreviated, but
// never re-tagged "dirty" from the builder's local tree).
func TestVersionStringUsesStampedValues(t *testing.T) {
	defer func(v, c, d string) { version, commit, date = v, c, d }(version, commit, date)
	version, commit, date = "v0.1.0", "abcdef1234567890abcdef", "2026-10-21T00:00:00Z"

	got := versionString()
	for _, want := range []string{"papurika v0.1.0", "(abcdef123456)", "built 2026-10-21T00:00:00Z"} {
		if !strings.Contains(got, want) {
			t.Errorf("versionString() = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "dirty") {
		t.Errorf("versionString() = %q, a stamped release build must not be marked dirty", got)
	}
}

func TestVersionStringFallsBackToDevel(t *testing.T) {
	defer func(v, c, d string) { version, commit, date = v, c, d }(version, commit, date)
	version, commit, date = "", "", ""

	// No ldflags and no module version (tests build as "(devel)") => "devel".
	if got := versionString(); !strings.Contains(got, "papurika devel") {
		t.Errorf("versionString() = %q, want it to contain %q", got, "papurika devel")
	}
}
