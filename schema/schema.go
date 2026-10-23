// Package schema is the papurika.dev/result/v1 JSON document — the stable
// contract papurika's --output json|yaml emits and that scripts consume.
//
// It exists so that contract is one thing rather than three. The document used
// to be defined by unexported structs inside the CLI binary, which meant a Go
// consumer could not unmarshal papurika's own output without hand-writing the
// types, nothing but review stood between an edit and a silent break, and the
// version string was a literal repeated at each site that produced a document.
// All three now live here: [Version], the types, and [FromResult], which is the
// only thing that builds one.
//
// It is deliberately a *separate* package from papurika. The core model
// ([papurika.Result]) is shaped for the engine — maps keyed by minion id,
// Go-typed enums, unexported raw bytes — and is free to change. This document
// is shaped for a consumer: sorted slices, string enums, explicit counts, and
// a version to migrate against. Keeping them apart is what lets the first
// evolve without breaking the second.
//
// # Reading a document
//
//	var doc schema.Result
//	if err := json.Unmarshal(out, &doc); err != nil { ... }
//	if doc.Error != nil { return doc.Error }          // *Error implements error
//	for _, c := range doc.Commands {
//		for _, m := range c.Minions {
//			if !m.OK() { ... }                         // the documented ok rule
//		}
//	}
//
// # Compatibility
//
// Within v1, fields may be ADDED and new enum values may appear; a consumer
// must ignore what it does not recognize. Fields are not renamed, removed, or
// given a new meaning — that would be v2. Note the practical consequence of
// "new enum values may appear": branch on the values you handle and have a
// default, rather than assuming the set is closed.
//
// The per-minion and per-command shapes are specified in design/cli.md §B; the
// exit codes a CLI consumer sees alongside them are in §D.
package schema

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/cstrahan/papurika"
)

// Version is the schema identifier carried in every document's "schema" field.
// A consumer should check it before trusting the rest.
const Version = "papurika.dev/result/v1"

// Minion status values. These mirror [papurika.Status] as strings.
//
// The distinction that matters most is [StatusPending] vs [StatusNoResponse]:
// no-response is a *claim* — the gather's settle window elapsed and a
// saltutil.find_job probe found the minion not running the job — while pending
// is the absence of one, meaning the minion simply has not answered yet. A
// job-cache read (papurika job <jid>) can only ever produce pending, because
// GET /jobs/<jid> omits whoever has not returned and says nothing about why.
// Treating pending as a failure reports a healthy in-flight job as broken.
const (
	StatusResponded  = "responded"  // returned a value (check Retcode for success)
	StatusNoResponse = "noresponse" // probed and found absent
	StatusPending    = "pending"    // no terminal outcome yet
	StatusError      = "error"      // structured per-minion error
	StatusNotMatched = "notmatched" // the target matched no minion
)

// Summary outcome values.
const (
	OutcomeOK      = "ok"      // every minion responded and succeeded
	OutcomePartial = "partial" // some succeeded, some did not
	OutcomeFailed  = "failed"  // nothing succeeded
	OutcomePending = "pending" // nothing has failed; the job is not finished
	OutcomeNoMatch = "nomatch" // the target matched no minion
	OutcomeError   = "error"   // the request itself failed; see Result.Error
)

// Error kinds. These classify a request-level failure — authentication,
// transport, the clock — as distinct from a per-minion outcome, which is never
// an error here but a Status plus a Retcode.
const (
	ErrKindTimeout     = "timeout"
	ErrKindInterrupted = "interrupted"
	ErrKindAuthFailure = "auth_failure"
	ErrKindAuthDenied  = "auth_denied"
	ErrKindMalformed   = "malformed"
	ErrKindServerError = "server_error"
	ErrKindJobNotFound = "job_not_found"
	ErrKindNotFound    = "not_found"
	ErrKindUnsupported = "unsupported"
	ErrKindTransport   = "transport"
	ErrKindOther       = "error"
)

// Result is one papurika.dev/result/v1 document.
//
// Commands is never null — a document with no commands carries an empty list,
// so a consumer may iterate it unconditionally.
type Result struct {
	Schema   string    `json:"schema"`
	Summary  Summary   `json:"summary"`
	Commands []Command `json:"commands"`
	// Error is set when the request itself failed. It is present both on a
	// document that gathered nothing (Summary.Outcome is OutcomeError) and on a
	// partial one that was cut short, where the outcome still describes what
	// was gathered and this says what stopped it.
	Error *Error `json:"error,omitempty"`
}

// Summary folds every minion of every command into one set of counts, and is
// what the CLI's exit code is derived from (design/cli.md §D).
//
// Targeted is the total; Returned, NoResponse and Pending partition it, and
// Succeeded + Failed partition Returned.
type Summary struct {
	Outcome string `json:"outcome"`
	// JIDs is every jid the run touched, including each chunk of a batched
	// run. It is how a document satisfies papurika's never-lose-a-jid rule on
	// the machine-readable path: whatever else happened, the work named here
	// is still recoverable with `papurika watch <jid>`.
	JIDs       []string `json:"jids,omitempty"`
	Targeted   int      `json:"targeted"`
	Returned   int      `json:"returned"`
	Succeeded  int      `json:"succeeded"`
	Failed     int      `json:"failed"`
	NoResponse int      `json:"noresponse"`
	Pending    int      `json:"pending"`
}

// Command is one command's outcome. A request may carry several.
//
// Two shapes share this type, and which one you get depends on where the work
// ran. A minion-targeting command (an execution function, a state run) fills
// Minions. A master-side command (runner, wheel) fills Value instead and leaves
// Minions empty — there are no minions involved. Targeted == 0 is the cheapest
// way to tell them apart.
type Command struct {
	Index  int    `json:"index"`
	Client string `json:"client"`
	Fun    string `json:"fun"`
	JID    string `json:"jid,omitempty"`
	// ExtraJIDs holds the remaining jids when one command covers several jobs —
	// a client-side-batched run, where each chunk is its own job but the
	// per-minion results are merged into this single view. Use Summary.JIDs to
	// get them all without special-casing.
	ExtraJIDs []string `json:"extra_jids,omitempty"`
	Targeted  []string `json:"targeted,omitempty"`
	Minions   []Minion `json:"minions,omitempty"`
	// Value is the return of a master-side (runner/wheel) command, verbatim.
	Value   *json.RawMessage `json:"value,omitempty"`
	Retcode *int             `json:"retcode,omitempty"`
}

// Minion is one minion's outcome within a command.
//
// A command that *ran* and failed is Status [StatusResponded] with a non-zero
// Retcode — failure is not a separate status. That is deliberate: it keeps
// "the minion answered" and "the thing it ran worked" as two separate
// questions, so nothing is hidden behind one word.
type Minion struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	// Retcode is null when the minion did not report one, which is not the
	// same as zero — hence the pointer.
	Retcode *int `json:"retcode"`
	// Return is the minion's return value, exactly as salt sent it. For a
	// state run, decode it with the state package.
	Return json.RawMessage `json:"return"`
}

// OK reports whether this minion both answered and succeeded: status
// responded, and either no retcode or a zero one. It is the predicate the CLI's
// default exit policy uses, provided here so a consumer does not have to
// reimplement it — the "no retcode reported counts as ok" half is easy to miss.
func (m Minion) OK() bool {
	return m.Status == StatusResponded && (m.Retcode == nil || *m.Retcode == 0)
}

// Error reports a request-level failure: authentication, transport, a malformed
// request, or the clock running out. It implements error, so a consumer can
// return it directly.
//
// Per-minion and per-command outcomes are NOT errors — they are a [Minion]
// Status plus a Retcode. This is only for failures of the request itself.
type Error struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// FromResult builds a document from the library's result model.
func FromResult(res *papurika.Result) Result {
	v := Result{Schema: Version}
	if res == nil {
		v.Commands = []Command{}
		v.Summary.Outcome = OutcomeOK
		return v
	}
	for i, c := range res.Commands {
		cv := Command{
			Index: i, Client: string(c.Client), Fun: c.Fun,
			JID: c.JID, ExtraJIDs: c.ExtraJIDs, Targeted: c.Targeted, Retcode: c.Retcode,
		}
		ids := make([]string, 0, len(c.Minions))
		for id := range c.Minions {
			ids = append(ids, id)
		}
		sort.Strings(ids) // map iteration order must not reach the wire
		for _, id := range ids {
			m := c.Minions[id]
			ret := m.Value.Raw()
			if len(ret) == 0 {
				ret = json.RawMessage("null")
			}
			cv.Minions = append(cv.Minions, Minion{
				ID: id, Status: m.Status.String(), Retcode: m.Retcode, Return: ret,
			})
			v.Summary.Targeted++
			switch {
			case m.Status == papurika.StatusPending:
				v.Summary.Pending++
			case m.Status == papurika.StatusNoResponse:
				v.Summary.NoResponse++
			case m.OK():
				v.Summary.Returned++
				v.Summary.Succeeded++
			default:
				v.Summary.Returned++
				v.Summary.Failed++
			}
		}
		if c.Minions == nil && !c.Value.IsZero() {
			raw := c.Value.Raw()
			cv.Value = &raw
		}
		v.Commands = append(v.Commands, cv)
	}
	v.Summary.Outcome = outcome(res, v.Summary)
	v.Summary.JIDs = res.JIDs()
	if v.Commands == nil {
		v.Commands = []Command{} // a consumer iterating commands must not get null
	}
	return v
}

// FromError builds the document for a request that failed with nothing
// gathered. It is a valid document whose outcome says "error" outright, so it
// parses like any other and cannot be mistaken for a successful empty run.
//
// This exists because the alternative — emitting nothing — means json.Unmarshal
// fails on exactly the runs a script most needs to inspect.
func FromError(err error) Result {
	return Result{
		Schema:   Version,
		Summary:  Summary{Outcome: OutcomeError},
		Commands: []Command{},
		Error:    NewError(err),
	}
}

// NewError classifies a library error into the document's error vocabulary.
// It returns nil for a nil error.
func NewError(err error) *Error {
	if err == nil {
		return nil
	}
	return &Error{Kind: ErrorKind(err), Message: err.Error()}
}

// ErrorKind names a request-level failure in the stable vocabulary above. It is
// deliberately coarser than papurika's sentinel set: a CLI consumer branches on
// the exit code, and this is for the one reading the JSON.
//
// The context checks come first on purpose. A deadline or a cancel that lands
// mid-request arrives wrapped in papurika.ErrTransport — the clock interrupts
// an HTTP exchange, so of course it does — and reporting that as "transport"
// would lose the only part the caller can act on.
func ErrorKind(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return ErrKindTimeout
	case errors.Is(err, context.Canceled):
		return ErrKindInterrupted
	case errors.Is(err, papurika.ErrAuthFailure):
		return ErrKindAuthFailure
	case errors.Is(err, papurika.ErrAuthDenied):
		return ErrKindAuthDenied
	case errors.Is(err, papurika.ErrMalformed):
		return ErrKindMalformed
	case errors.Is(err, papurika.ErrServerError):
		return ErrKindServerError
	case errors.Is(err, papurika.ErrJobNotFound):
		return ErrKindJobNotFound
	case errors.Is(err, papurika.ErrNotFound):
		return ErrKindNotFound
	case errors.Is(err, papurika.ErrUnsupported):
		return ErrKindUnsupported
	case errors.Is(err, papurika.ErrTransport):
		return ErrKindTransport
	default:
		return ErrKindOther
	}
}

// IsNoMatch reports whether every command is a minion-targeting one that
// matched nothing — i.e. the target hit no minion at all, which is a distinct
// condition from "matched, and they all failed" (exit 2 vs exit 1).
func IsNoMatch(res *papurika.Result) bool {
	if res == nil || len(res.Commands) == 0 {
		return false
	}
	for _, c := range res.Commands {
		switch c.Client {
		case papurika.ClientLocal, papurika.ClientLocalAsync, papurika.ClientLocalBatch,
			papurika.ClientLocalSubset, papurika.ClientSSH:
			// A jid or a targeted set means something WAS dispatched (e.g. an
			// async submit) — not a no-match.
			if len(c.Minions) > 0 || !c.Value.IsZero() || c.JID != "" || len(c.Targeted) > 0 {
				return false
			}
		default:
			return false // runner/wheel are never "no match"
		}
	}
	return true
}

func outcome(res *papurika.Result, s Summary) string {
	if s.Targeted == 0 {
		// A minion-targeting command that matched nothing is "nomatch"
		// (consistent with exit 2); a master-side one (value-bearing, no
		// minions) is "ok".
		if IsNoMatch(res) {
			return OutcomeNoMatch
		}
		return OutcomeOK
	}
	switch {
	case s.Failed == 0 && s.NoResponse == 0 && s.Pending == 0:
		return OutcomeOK
	// Nothing has gone wrong yet — the job simply is not finished. Reporting
	// this as "failed" is what made `papurika job <jid>` alarming to run on a
	// healthy long job.
	case s.Failed == 0 && s.NoResponse == 0:
		return OutcomePending
	case s.Succeeded == 0 && s.Pending == 0:
		return OutcomeFailed
	default:
		return OutcomePartial
	}
}
