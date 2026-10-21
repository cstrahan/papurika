// Package state decodes salt's state.apply / highstate returns into a typed
// model.
//
// It exists because a state return is the one minion value papurika cannot
// usefully normalize into the core Result: its shape is a map of per-state
// results whose `changes` payload differs per state module and per test-vs-real
// run, so folding it into MinionResult would bloat the model for every job that
// isn't a state run. Instead MinionResult keeps the raw bytes and this package
// decodes them on request.
//
// The wire shapes — and every claim below — are captured in
// research/salt-test-cherrypy/state-apply-shapes.md and the accompanying
// out-state-shapes/*.json fixtures.
//
// The trap this package exists to close: salt returns a *map* of state tag to
// result on a normal run, but a JSON *array* of error strings when the run never
// started — an SLS that didn't compile, or a second state run rejected because
// one is already in progress. Anything assuming map[stateID]result breaks on the
// list form, and treating the list as "no states ran, so nothing failed" turns a
// refused run into a clean one.
package state

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/cstrahan/papurika"
)

// Outcome classifies a single state's result.
//
// The three values of salt's `result` field are three distinct outcomes, not a
// boolean with a null: true succeeded, false failed, and null means "would
// change" under test=True.
type Outcome int

const (
	Clean       Outcome = iota // succeeded, changed nothing
	Changed                    // succeeded, made changes
	Failed                     // ran and failed
	Skipped                    // never ran: a requisite failed
	WouldChange                // test=True: would have changed something
)

func (o Outcome) String() string {
	switch o {
	case Clean:
		return "clean"
	case Changed:
		return "changed"
	case Failed:
		return "failed"
	case Skipped:
		return "skipped"
	case WouldChange:
		return "would-change"
	}
	return "unknown"
}

// Entry is one state's result inside a state run.
type Entry struct {
	// Tag is the raw result key, "<module>_|-<id>_|-<name>_|-<function>".
	Tag string `json:"-"`

	Name    string         `json:"name"`
	Result  *bool          `json:"result"` // true=ok, false=failed, nil=would-change
	Comment string         `json:"comment"`
	Changes map[string]any `json:"changes"`

	// Duration is the state's runtime in milliseconds.
	Duration  float64 `json:"duration"`
	StartTime string  `json:"start_time"`

	// RunNum is salt's render order for the state.
	RunNum int    `json:"__run_num__"`
	ID     string `json:"__id__"`
	SLS    string `json:"__sls__"`

	// StateRan is present and false only when the state was skipped because a
	// requisite failed. Absent on a state that actually ran.
	StateRan *bool `json:"__state_ran__"`
}

// Outcome classifies this entry.
//
// Order matters: a would-change entry can carry a populated Changes map (that is
// precisely what it is reporting), so the nil check must come first.
func (e Entry) Outcome() Outcome {
	switch {
	case e.Result == nil:
		return WouldChange
	case !*e.Result:
		if e.StateRan != nil && !*e.StateRan {
			return Skipped
		}
		return Failed
	default:
		if len(e.Changes) > 0 {
			return Changed
		}
		return Clean
	}
}

// StateID is the human-readable identifier for the state, preferring salt's
// explicit __id__ over the id segment of the tag.
func (e Entry) StateID() string {
	if e.ID != "" {
		return e.ID
	}
	return e.tagPart(1)
}

// TargetName is what the state acted on (a path, package, service, …),
// preferring salt's explicit name field over the tag segment.
func (e Entry) TargetName() string {
	if e.Name != "" {
		return e.Name
	}
	return e.tagPart(2)
}

// Module is the state module, e.g. "file" in "file.managed".
func (e Entry) Module() string { return e.tagPart(0) }

// Function is the state function, e.g. "managed" in "file.managed".
func (e Entry) Function() string { return e.tagPart(3) }

func (e Entry) tagPart(i int) string {
	comps := strings.SplitN(e.Tag, "_|-", 4)
	if len(comps) != 4 {
		return ""
	}
	return comps[i]
}

// Result is one minion's state run, with entries in salt's execution order.
type Result struct {
	Entries []Entry

	// Retcode is the minion's retcode when salt reported one. It is the cheap
	// authoritative verdict on the run: 0 = every state ok, 2 = ran with at
	// least one failure. (A retcode of 1 means the run never happened, which
	// surfaces as a *ConflictError or *CompileError from Parse instead.)
	Retcode *int
}

// OK reports whether every state succeeded.
//
// It prefers the minion's retcode — salt's own verdict, and cheaper and more
// trustworthy than re-deriving one by scanning sub-states — and only falls back
// to scanning when no retcode was reported.
func (r Result) OK() bool {
	if r.Retcode != nil {
		return *r.Retcode == 0
	}
	for _, e := range r.Entries {
		switch e.Outcome() {
		case Failed, Skipped:
			return false
		}
	}
	return true
}

// Failed returns the states that ran and failed.
func (r Result) Failed() []Entry { return r.byOutcome(Failed) }

// Changed returns the states that made changes.
func (r Result) Changed() []Entry { return r.byOutcome(Changed) }

// Skipped returns the states that never ran because a requisite failed.
func (r Result) Skipped() []Entry { return r.byOutcome(Skipped) }

// WouldChange returns the states a test=True run reports as pending. A non-empty
// result from a post-deploy test run is configuration drift.
func (r Result) WouldChange() []Entry { return r.byOutcome(WouldChange) }

func (r Result) byOutcome(o Outcome) []Entry {
	var out []Entry
	for _, e := range r.Entries {
		if e.Outcome() == o {
			out = append(out, e)
		}
	}
	return out
}

// Counts tallies entries by outcome.
func (r Result) Counts() map[Outcome]int {
	out := make(map[Outcome]int, 5)
	for _, e := range r.Entries {
		out[e.Outcome()]++
	}
	return out
}

// ConflictError reports that the minion refused the run because another state
// run already holds the lock on its state subsystem.
//
// A minion runs one state job at a time: a second state function — even a
// different SLS — is rejected outright rather than queued. The competing run has
// a jid, so the work is still followable (papurika watch <jid>).
type ConflictError struct {
	// JID of the state run already in progress, when salt named one.
	JID      string
	Messages []string
}

func (e *ConflictError) Error() string {
	if e.JID != "" {
		return fmt.Sprintf("a state run is already in progress on this minion (jid %s)", e.JID)
	}
	return "a state run is already in progress on this minion"
}

// CompileError reports that the state run never started because the SLS could
// not be rendered or resolved (a jinja error, a missing SLS, …).
type CompileError struct {
	Messages []string
}

func (e *CompileError) Error() string {
	if len(e.Messages) == 0 {
		return "the state run did not compile"
	}
	return "the state run did not compile: " + firstLine(e.Messages[0])
}

// Parse decodes a minion's state return.
//
// Prefer ParseMinion, which also supplies the retcode. A run that never started
// is reported as a *ConflictError or *CompileError, never as an empty Result —
// silently degrading those to "no states ran" is what makes a refused run look
// like a successful one.
func Parse(v papurika.RawValue) (*Result, error) { return parse(v, nil) }

// ParseMinion decodes a minion's state return, using its retcode as the
// authoritative verdict on the run (see Result.Retcode).
func ParseMinion(m papurika.MinionResult) (*Result, error) { return parse(m.Value, m.Retcode) }

func parse(v papurika.RawValue, retcode *int) (*Result, error) {
	raw := v.Raw()
	switch firstToken(raw) {
	case 0:
		return nil, fmt.Errorf("state: empty return")

	case '[':
		// The run never started. Salt reports why as a list of error strings.
		var msgs []string
		if err := json.Unmarshal(raw, &msgs); err != nil {
			return nil, fmt.Errorf("state: unreadable error list: %w", err)
		}
		if jid, ok := conflictJID(msgs); ok {
			return nil, &ConflictError{JID: jid, Messages: msgs}
		}
		return nil, &CompileError{Messages: msgs}

	case '{':
		var byTag map[string]Entry
		if err := json.Unmarshal(raw, &byTag); err != nil {
			return nil, fmt.Errorf("state: decoding state return: %w", err)
		}
		entries := make([]Entry, 0, len(byTag))
		for tag, e := range byTag {
			e.Tag = tag
			entries = append(entries, e)
		}
		// Salt assigns __run_num__ in execution order; fall back to the tag so
		// the ordering stays stable when it is absent.
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].RunNum != entries[j].RunNum {
				return entries[i].RunNum < entries[j].RunNum
			}
			return entries[i].Tag < entries[j].Tag
		})
		return &Result{Entries: entries, Retcode: retcode}, nil

	default:
		return nil, fmt.Errorf("state: unexpected state return shape")
	}
}

// Looks reports whether a minion value looks like a state return — either the
// states map (an entry carrying __run_num__) or the error-list form.
//
// Useful for deciding whether to render a result as a state run when the caller
// did not originate the job and so cannot key off the function name.
func Looks(v papurika.RawValue) bool {
	switch firstToken(v.Raw()) {
	case '[':
		var msgs []string
		return json.Unmarshal(v.Raw(), &msgs) == nil
	case '{':
		var m map[string]json.RawMessage
		if json.Unmarshal(v.Raw(), &m) != nil || len(m) == 0 {
			return false
		}
		for _, e := range m {
			var probe struct {
				RunNum *int `json:"__run_num__"`
			}
			if json.Unmarshal(e, &probe) == nil && probe.RunNum != nil {
				return true
			}
		}
	}
	return false
}

// firstToken returns the first non-whitespace byte of b, or 0 if there is none.
func firstToken(b []byte) byte {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return c
		}
	}
	return 0
}

// conflictJID extracts the jid from salt's concurrency-conflict message,
// 'The function "state.apply" is running as PID N and was started at … with jid <jid>'.
func conflictJID(msgs []string) (string, bool) {
	for _, s := range msgs {
		const marker = "with jid "
		i := strings.Index(s, marker)
		if i < 0 || !strings.Contains(s, "is running as PID") {
			continue
		}
		rest := strings.TrimSpace(s[i+len(marker):])
		if j := strings.IndexAny(rest, " \n\t\""); j >= 0 {
			rest = rest[:j]
		}
		if rest != "" {
			return rest, true
		}
	}
	return "", false
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
