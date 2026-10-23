package papurika

import (
	"encoding/json"
	"sort"
)

// Status is the normalized outcome for a single minion within a command. It folds
// salt's three "no response" surfacings (sync string, async omission, missing ret
// event) into one explicit value, so callers never pattern-match English strings.
type Status int

const (
	// StatusPending: targeted but no terminal outcome yet (in-flight, or the run
	// was interrupted before this minion returned). Zero value.
	StatusPending Status = iota
	// StatusResponded: the minion returned a value. A non-zero Retcode or a failed
	// state still counts as Responded — inspect Retcode/Value for command success.
	StatusResponded
	// StatusNoResponse: targeted but the minion never returned (down/unreachable).
	StatusNoResponse
	// StatusError: a structured per-minion error, e.g. "'x' is not available."
	StatusError
	// StatusNotMatched: the target matched no minion.
	StatusNotMatched
)

func (s Status) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusResponded:
		return "responded"
	case StatusNoResponse:
		return "noresponse"
	case StatusError:
		return "error"
	case StatusNotMatched:
		return "notmatched"
	default:
		return "unknown"
	}
}

func (s Status) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// RawValue wraps a verbatim salt return value, providing typed accessors without
// discarding the original bytes (honesty preserved).
type RawValue struct {
	raw json.RawMessage
}

// NewRawValue wraps raw JSON bytes.
func NewRawValue(b json.RawMessage) RawValue { return RawValue{raw: b} }

func (v RawValue) String() (string, bool) {
	var s string
	if err := json.Unmarshal(v.raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func (v RawValue) Bool() (bool, bool) {
	var b bool
	if err := json.Unmarshal(v.raw, &b); err != nil {
		return false, false
	}
	return b, true
}

func (v RawValue) Int() (int64, bool) {
	var n int64
	if err := json.Unmarshal(v.raw, &n); err != nil {
		return 0, false
	}
	return n, true
}

func (v RawValue) Map() (map[string]any, bool) {
	var m map[string]any
	if err := json.Unmarshal(v.raw, &m); err != nil {
		return nil, false
	}
	return m, true
}

// Decode unmarshals the raw value into the caller's type.
func (v RawValue) Decode(into any) error { return json.Unmarshal(v.raw, into) }

// Raw returns the exact wire bytes.
func (v RawValue) Raw() json.RawMessage { return v.raw }

// IsZero reports whether the value carries no bytes.
func (v RawValue) IsZero() bool { return len(v.raw) == 0 }

// MarshalJSON emits the underlying value (or null), so a Result serializes to the
// documented papurika.dev/result/v1 schema.
func (v RawValue) MarshalJSON() ([]byte, error) {
	if len(v.raw) == 0 {
		return []byte("null"), nil
	}
	return v.raw, nil
}

func (v *RawValue) UnmarshalJSON(b []byte) error {
	v.raw = append(v.raw[:0], b...)
	return nil
}

// MinionResult is the normalized outcome for one minion.
type MinionResult struct {
	ID      string   `json:"id"`
	Status  Status   `json:"status"`
	Retcode *int     `json:"retcode"`
	Value   RawValue `json:"return"`

	raw json.RawMessage // verbatim per-minion value
}

// Raw returns the verbatim per-minion wire bytes (may be nil).
func (m MinionResult) Raw() json.RawMessage { return m.raw }

// OK reports the conventional success predicate: responded with a zero (or
// absent) retcode.
func (m MinionResult) OK() bool {
	return m.Status == StatusResponded && (m.Retcode == nil || *m.Retcode == 0)
}

// CompletionReason explains why a command's gather loop terminated.
type CompletionReason int

const (
	ReasonNone        CompletionReason = iota
	ReasonAllReturned                  // every targeted minion returned
	ReasonSettled                      // gather window elapsed; leftover minions are non-responders
	ReasonDeadline                     // hard ctx deadline / --timeout elapsed (not normal completion)
	ReasonCanceled                     // ctx canceled (e.g. SIGINT)
	ReasonStreamFatal                  // unrecoverable gather error (e.g. jid aged out mid-run)
)

func (r CompletionReason) String() string {
	switch r {
	case ReasonAllReturned:
		return "all_returned"
	case ReasonSettled:
		return "settled"
	case ReasonDeadline:
		return "deadline"
	case ReasonCanceled:
		return "canceled"
	case ReasonStreamFatal:
		return "stream_fatal"
	default:
		return "none"
	}
}

func (r CompletionReason) MarshalJSON() ([]byte, error) { return json.Marshal(r.String()) }

// CommandResult is the normalized outcome of a single lowstate command.
type CommandResult struct {
	Client ClientType `json:"client"`
	Fun    string     `json:"fun"`

	// Async submit (local_async / POST /minions): the jid and the authoritative
	// targeted set. Minions fills in as returns are gathered.
	JID      string   `json:"jid,omitempty"`
	Targeted []string `json:"targeted,omitempty"`

	// ExtraJIDs holds the remaining jids when ONE CommandResult covers several
	// jobs — a client-side-batched run, where each chunk is its own job but the
	// per-minion results are merged into a single unified view (async.md §F).
	// JID stays the first chunk's, so single-jid consumers are unaffected;
	// Result.JIDs() unions both, which is what makes the "never lose a jid"
	// guarantee hold for a batched run. Nil in the ordinary one-job case.
	ExtraJIDs []string `json:"extra_jids,omitempty"`

	// local / local_batch: per-minion outcomes (nil for runner/wheel).
	Minions map[string]MinionResult `json:"minions,omitempty"`

	// runner / wheel: single value (zero for local).
	Value   RawValue `json:"value,omitempty"`
	Retcode *int     `json:"retcode,omitempty"`
	Success *bool    `json:"success,omitempty"`

	Complete bool             `json:"complete"`
	Reason   CompletionReason `json:"reason,omitempty"`

	raw json.RawMessage // verbatim return[i] entry
}

// Raw returns the verbatim return[i] wire bytes.
func (c CommandResult) Raw() json.RawMessage { return c.raw }

// Result is the normalized outcome of one request, which may carry several
// commands (parallel to the request array, in order).
type Result struct {
	Commands []CommandResult `json:"commands"`
}

// JIDs returns every distinct jid this result touched, in sorted order.
func (r *Result) JIDs() []string {
	seen := map[string]struct{}{}
	for _, c := range r.Commands {
		if c.JID != "" {
			seen[c.JID] = struct{}{}
		}
		for _, j := range c.ExtraJIDs {
			if j != "" {
				seen[j] = struct{}{}
			}
		}
	}
	return sortedKeys(seen)
}

// Responders returns the ids of minions that returned a value, across all
// commands, sorted.
func (r *Result) Responders() []string {
	seen := map[string]struct{}{}
	for _, c := range r.Commands {
		for id, m := range c.Minions {
			if m.Status == StatusResponded {
				seen[id] = struct{}{}
			}
		}
	}
	return sortedKeys(seen)
}

// NonResponders returns the ids that were targeted but did not reach a Responded
// status (no-response, or still pending at interruption), across all commands.
// This is targeted − returned; it never consults manage.status or parses strings.
func (r *Result) NonResponders() []string {
	seen := map[string]struct{}{}
	for _, c := range r.Commands {
		for _, id := range c.Targeted {
			m, ok := c.Minions[id]
			if !ok || m.Status == StatusNoResponse || m.Status == StatusPending {
				seen[id] = struct{}{}
			}
		}
	}
	return sortedKeys(seen)
}

// Failed returns minion results that responded with a non-zero retcode or carry a
// per-minion error status, across all commands.
func (r *Result) Failed() []MinionResult {
	var out []MinionResult
	for _, c := range r.Commands {
		for _, m := range c.Minions {
			if m.Status == StatusError || (m.Status == StatusResponded && m.Retcode != nil && *m.Retcode != 0) {
				out = append(out, m)
			}
		}
	}
	return out
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// intPtr is a small helper for the decoders.
func intPtr(n int) *int { return &n }
