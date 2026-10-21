package papurika

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"
)

// newJID mints a job id for papurika to submit under.
//
// Why mint one at all: salt-api honors a jid supplied in the lowstate
// (verified — see cherrypy_learnings.md, "Client-supplied jids & submit
// recoverability"), and knowing the jid *before* the request goes out is what
// makes an ambiguous submit failure recoverable. Without it, a submit whose
// response is lost leaves a job running under a jid nobody can name.
//
// Format: salt's own `%Y%m%d%H%M%S%f` UTC timestamp — 20 digits — followed by
// `_` and random hex. Both halves matter:
//
//   - The 20-digit prefix is what salt.utils.jid.is_jid accepts and what
//     jid_to_time parses, so StartTime and jid-ordered listings keep working.
//     A jid that doesn't conform is still *accepted* by the publish path
//     (nothing validates it there) but degrades those, so we conform.
//   - The random suffix is not decoration. salt's gen_jid is a bare timestamp,
//     so two papurika processes submitting in the same microsecond would mint
//     the same jid — and a jid collision is *silent*: prep_jid reuses the
//     existing job-cache entry rather than erroring, so the second job lands on
//     top of the first. Salt's own `unique_jid` option takes the same
//     `<timestamp>_<suffix>` shape, so the form is well-trodden.
//
// Caveat worth knowing: the timestamp is this machine's clock, where salt would
// have used the master's. A client with a skewed clock writes jobs whose
// StartTime — and position in a date-filtered `jobs.list_jobs` — is off by the
// skew.
func newJID() string {
	var b [5]byte
	// A failure here is not worth surfacing: fall through with whatever the
	// buffer holds rather than fail a submit over suffix entropy.
	_, _ = rand.Read(b[:])
	now := time.Now().UTC() // once: two calls could straddle a second boundary
	return now.Format("20060102150405") + microsecondsOf(now) + "_" + hex.EncodeToString(b[:])
}

// microsecondsOf renders the sub-second part as salt's %f does: exactly six
// digits, zero-padded.
func microsecondsOf(t time.Time) string {
	us := t.Nanosecond() / 1000
	var d [6]byte
	for i := 5; i >= 0; i-- {
		d[i] = byte('0' + us%10)
		us /= 10
	}
	return string(d[:])
}

// looksLikeJID reports whether s has the shape salt's is_jid() accepts: exactly
// 20 digits, optionally followed by "_" and a suffix.
func looksLikeJID(s string) bool {
	digits := s
	if i := strings.IndexByte(s, '_'); i >= 0 {
		if i != 20 || i == len(s)-1 {
			return false
		}
		digits = s[:i]
	}
	if len(digits) != 20 {
		return false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return false
		}
	}
	return true
}
