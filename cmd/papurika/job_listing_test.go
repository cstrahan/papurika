package main

import (
	"fmt"
	"testing"
	"time"
)

// `jobs` must answer the question it was asked: list everything unless the
// caller narrows it. A default cap would silently answer a narrower one, and an
// omitted job reads as a job that never ran.
func TestJobsListsEverythingByDefault(t *testing.T) {
	root := newRootCmd()
	c, _, err := root.Find([]string{"jobs"})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Flags().Lookup("limit").DefValue; got != "0" {
		t.Errorf("--limit default = %q, want %q (no limit)", got, "0")
	}
	if got := c.Flags().Lookup("since").DefValue; got != "" {
		t.Errorf("--since default = %q, want no window", got)
	}

	rows := make([]jobRow, 500)
	for i := range rows {
		rows[i].JID = fmt.Sprintf("2026102314%010d", i)
	}
	kept, agedOut, truncated := filterJobRows(rows, time.Time{}, noJobLimit)
	if len(kept) != len(rows) || agedOut != 0 || truncated != 0 {
		t.Errorf("kept %d of %d (agedOut %d, truncated %d); want all", len(kept), len(rows), agedOut, truncated)
	}
}

func TestFilterJobRows(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	rows := []jobRow{
		{JID: jidCutoff(now.Add(-30 * time.Minute))},
		{JID: jidCutoff(now.Add(-90 * time.Minute))},
		{JID: jidCutoff(now.Add(-10 * time.Hour))},
		{JID: "not-a-jid"},
	}

	kept, agedOut, truncated := filterJobRows(rows, now.Add(-time.Hour), 0)
	if agedOut != 2 {
		t.Errorf("agedOut = %d, want 2", agedOut)
	}
	if truncated != 0 {
		t.Errorf("truncated = %d, want 0", truncated)
	}
	// An undatable jid is kept, never silently hidden.
	if len(kept) != 2 || kept[1].JID != "not-a-jid" {
		t.Errorf("kept = %+v, want the recent job plus the unparseable one", kept)
	}

	kept, _, truncated = filterJobRows(rows, time.Time{}, 2)
	if len(kept) != 2 || truncated != 2 {
		t.Errorf("limit: kept %d truncated %d, want 2 and 2", len(kept), truncated)
	}
}
