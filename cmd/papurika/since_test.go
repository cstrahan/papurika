package main

import (
	"testing"
	"time"
)

func TestSinceFlagAbsoluteTimestamps(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
	}{
		// Explicit zones are honored as given.
		{"2026-10-23T14:30:00Z", time.Date(2026, 10, 23, 14, 30, 0, 0, time.UTC)},
		{"2026-10-23T16:30:00+02:00", time.Date(2026, 10, 23, 14, 30, 0, 0, time.UTC)},
		{"2026-10-23T14:30:00.5Z", time.Date(2026, 10, 23, 14, 30, 0, 5e8, time.UTC)},
		// No zone => UTC, so a timestamp copied out of the listing (which prints
		// salt's UTC StartTime) means what it looks like.
		{"2026-10-23T14:30:00", time.Date(2026, 10, 23, 14, 30, 0, 0, time.UTC)},
		{"2026-10-23 14:30:00", time.Date(2026, 10, 23, 14, 30, 0, 0, time.UTC)},
		{"2026-10-23T14:30", time.Date(2026, 10, 23, 14, 30, 0, 0, time.UTC)},
		{"2026-10-23 14:30", time.Date(2026, 10, 23, 14, 30, 0, 0, time.UTC)},
		{"2026-10-23", time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		var s sinceFlag
		if err := s.Set(c.in); err != nil {
			t.Errorf("Set(%q): %v", c.in, err)
			continue
		}
		if !s.cutoff.Equal(c.want) {
			t.Errorf("Set(%q) = %s, want %s", c.in, s.cutoff.UTC(), c.want)
		}
		if s.String() != c.in {
			t.Errorf("String() = %q, want the text as typed %q", s.String(), c.in)
		}
	}
}

func TestSinceFlagDurations(t *testing.T) {
	for _, in := range []string{"2h", "90m", "36h", "1h30m", "45s"} {
		var s sinceFlag
		before := time.Now()
		if err := s.Set(in); err != nil {
			t.Fatalf("Set(%q): %v", in, err)
		}
		d, _ := time.ParseDuration(in)
		// The cutoff is now-d, resolved at Set time.
		if s.cutoff.After(before.Add(-d).Add(time.Second)) || s.cutoff.Before(before.Add(-d).Add(-time.Second)) {
			t.Errorf("Set(%q) cutoff = %s, want ~%s", in, s.cutoff, before.Add(-d))
		}
	}
}

func TestSinceFlagRejects(t *testing.T) {
	// A negative duration would put the cutoff in the future and silently hide
	// everything, which looks identical to an empty job cache.
	for _, in := range []string{"-2h", "yesterday", "2026-13-01", "20261023", "2h ago", "10/23/2026"} {
		var s sinceFlag
		if err := s.Set(in); err == nil {
			t.Errorf("Set(%q) accepted, giving cutoff %s; want an error", in, s.cutoff)
		}
	}
}

// Durations and timestamps must not be able to shadow each other.
func TestSinceFlagFormsDoNotCollide(t *testing.T) {
	var s sinceFlag
	if err := s.Set("2026-10-23"); err != nil {
		t.Fatal(err)
	}
	if s.cutoff.Year() != 2026 {
		t.Errorf("a date parsed as something else: %s", s.cutoff)
	}
	if err := s.Set("2h"); err != nil {
		t.Fatal(err)
	}
	if s.cutoff.Year() < 2000 {
		t.Errorf("a duration parsed as something else: %s", s.cutoff)
	}
}
