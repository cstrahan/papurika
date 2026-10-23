package main

import (
	"fmt"
	"strings"
	"time"
)

// sinceFlag is the value of `jobs --since`: either a duration back from now
// ("2h", "90m", "36h") or an absolute instant in ISO 8601 / RFC 3339 form
// ("2026-10-23", "2026-10-23T14:30", "2026-10-23T14:30:00Z",
// "2026-10-23T14:30:00+02:00").
//
// Both spellings resolve to one cutoff instant when the flag is parsed, so
// nothing downstream has to know which was used. A duration is the right thing
// to type interactively ("what ran in the last two hours"); an absolute
// timestamp is the right thing for a scripted or after-the-fact query, where
// "2h ago" means something different every time it runs.
type sinceFlag struct {
	raw    string    // exactly what the user typed, for the stderr report
	cutoff time.Time // zero means "no time filter"
}

// sinceLayouts are the accepted absolute spellings, most specific first.
//
// A space instead of a `T` is not strictly ISO 8601, but it is what people
// actually type and what most tools print, and it cannot be confused with
// anything else here.
var sinceLayouts = []string{
	time.RFC3339,          // 2026-10-23T14:30:00Z, …+02:00 (fractional seconds ok)
	"2006-01-02T15:04:05", // no zone
	"2006-01-02 15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04",
	"2006-01-02",
}

func (s *sinceFlag) Type() string { return "duration|time" }

func (s *sinceFlag) String() string { return s.raw }

// Set parses a duration first, then the absolute layouts. The two cannot
// collide: no duration parses as a date, and no date parses as a duration
// (time.ParseDuration requires a unit suffix).
func (s *sinceFlag) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		*s = sinceFlag{}
		return nil
	}
	if d, err := time.ParseDuration(v); err == nil {
		if d < 0 {
			return fmt.Errorf("--since %q is a negative duration, which would be in the future; use a positive one like 2h", v)
		}
		s.raw, s.cutoff = v, time.Now().Add(-d)
		return nil
	}
	for _, layout := range sinceLayouts {
		// A timestamp with no zone is read as UTC, not local time. salt mints
		// jids from _utc_now() (salt/utils/jid.py), and the StartTime
		// `papurika jobs` prints is that same UTC instant — so a timestamp
		// copied out of the listing and pasted back in here means what it
		// looked like it meant. An explicit offset (Z, +02:00) is honored as
		// given, and the stderr report always echoes the resolved UTC cutoff
		// so the interpretation is never left to guesswork.
		if t, err := time.ParseInLocation(layout, v, time.UTC); err == nil {
			s.raw, s.cutoff = v, t
			return nil
		}
	}
	return fmt.Errorf("--since %q is neither a duration (2h, 90m) nor a timestamp "+
		"(2026-10-23, 2026-10-23T14:30, 2026-10-23T14:30:00Z); timestamps without a "+
		"zone are read as UTC", v)
}
