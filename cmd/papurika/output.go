package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cstrahan/papurika"
	"github.com/cstrahan/papurika/state"
	sigsyaml "sigs.k8s.io/yaml"
)

// outputMode selects how results are rendered.
type outputMode string

const (
	outputPretty outputMode = "pretty"
	outputJSON   outputMode = "json"
	outputYAML   outputMode = "yaml"
	outputNDJSON outputMode = "ndjson"
	outputRaw    outputMode = "raw"
)

// renderOpts controls the pretty renderer (see design/cli-output-format.md).
type renderOpts struct {
	color       bool  // ANSI color enabled
	changesOnly bool  // highstate: hide clean (ok, unchanged) states
	verbose     bool  // show every state's duration / fuller detail
	err         error // request-level failure to report alongside a partial result
}

// failPolicy decides how minion outcomes collapse to an exit code.
type failPolicy string

const (
	failAny  failPolicy = "any"  // non-zero if ANY minion is not-ok (default)
	failAll  failPolicy = "all"  // non-zero only if EVERY minion is not-ok
	failNone failPolicy = "none" // never fail on minion outcomes
)

// Exit codes (see design/cli.md §D).
const (
	exitOK          = 0
	exitPolicyFail  = 1
	exitNoMatch     = 2
	exitUsage       = 64
	exitData        = 65
	exitUnavailable = 69
	exitAuth        = 77
	exitTimeout     = 124
	exitInterrupted = 130
	exitTerminated  = 143
)

// resultView is the stable papurika.dev/result/v1 JSON schema.
type resultView struct {
	Schema   string        `json:"schema"`
	Summary  summaryView   `json:"summary"`
	Commands []commandView `json:"commands"`
	Error    *errorView    `json:"error,omitempty"`
}

type summaryView struct {
	Outcome string `json:"outcome"`
	// JIDs carries every jid the run touched, so stdout (json/yaml) always holds
	// the recoverable jid(s) — the "never lose a jid" guarantee on the machine
	// path, mirroring the pretty per-command (jid …) header.
	JIDs       []string `json:"jids,omitempty"`
	Targeted   int      `json:"targeted"`
	Returned   int      `json:"returned"`
	Succeeded  int      `json:"succeeded"`
	Failed     int      `json:"failed"`
	NoResponse int      `json:"noresponse"`
	// Pending is minions with no terminal outcome yet: an in-flight job read
	// from the cache, or a run cut short before they returned. Kept apart from
	// NoResponse because the evidence differs — a non-responder was probed with
	// saltutil.find_job and found absent, a pending minion was never judged.
	Pending int `json:"pending"`
}

// errorView reports a request-level failure (auth/transport/malformed) inside
// the result document, so `--output json` always yields something parseable.
// Writing nothing at all on failure — as this once did — meant json.load()
// raised on exactly the runs a script most needs to inspect.
type errorView struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

type commandView struct {
	Index     int              `json:"index"`
	Client    string           `json:"client"`
	Fun       string           `json:"fun"`
	JID       string           `json:"jid,omitempty"`
	ExtraJIDs []string         `json:"extra_jids,omitempty"`
	Targeted  []string         `json:"targeted,omitempty"`
	Minions   []minionView     `json:"minions,omitempty"`
	Value     *json.RawMessage `json:"value,omitempty"`
	Retcode   *int             `json:"retcode,omitempty"`
}

type minionView struct {
	ID      string          `json:"id"`
	Status  string          `json:"status"`
	Retcode *int            `json:"retcode"`
	Return  json.RawMessage `json:"return"`
}

func toView(res *papurika.Result, okPred func(papurika.MinionResult) bool) resultView {
	v := resultView{Schema: "papurika.dev/result/v1"}
	for i, c := range res.Commands {
		cv := commandView{Index: i, Client: string(c.Client), Fun: c.Fun, JID: c.JID, ExtraJIDs: c.ExtraJIDs, Targeted: c.Targeted, Retcode: c.Retcode}
		ids := make([]string, 0, len(c.Minions))
		for id := range c.Minions {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			m := c.Minions[id]
			ret := m.Value.Raw()
			if len(ret) == 0 {
				ret = json.RawMessage("null")
			}
			cv.Minions = append(cv.Minions, minionView{ID: id, Status: m.Status.String(), Retcode: m.Retcode, Return: ret})
			v.Summary.Targeted++
			switch {
			case m.Status == papurika.StatusPending:
				v.Summary.Pending++
			case m.Status == papurika.StatusNoResponse:
				v.Summary.NoResponse++
			case okPred(m):
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
		v.Commands = []commandView{} // a consumer iterating commands must not get null
	}
	return v
}

// errorKind names a request-level failure in the stable vocabulary the v1
// schema exposes. It is deliberately coarser than the sentinel set — scripts
// branch on the exit code; this is for the human reading the JSON.
func errorKind(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "interrupted"
	case errors.Is(err, papurika.ErrAuthFailure):
		return "auth_failure"
	case errors.Is(err, papurika.ErrAuthDenied):
		return "auth_denied"
	case errors.Is(err, papurika.ErrMalformed):
		return "malformed"
	case errors.Is(err, papurika.ErrServerError):
		return "server_error"
	case errors.Is(err, papurika.ErrJobNotFound):
		return "job_not_found"
	case errors.Is(err, papurika.ErrNotFound):
		return "not_found"
	case errors.Is(err, papurika.ErrUnsupported):
		return "unsupported"
	case errors.Is(err, papurika.ErrTransport):
		return "transport"
	default:
		return "error"
	}
}

func newErrorView(err error) *errorView {
	if err == nil {
		return nil
	}
	return &errorView{Kind: errorKind(err), Message: err.Error()}
}

// errorOnlyView is the document emitted when a run failed with nothing
// gathered. It is a valid papurika.dev/result/v1 document whose outcome says
// "error" outright, so it can be parsed like any other and cannot be mistaken
// for a successful empty run.
func errorOnlyView(err error) resultView {
	return resultView{
		Schema:   "papurika.dev/result/v1",
		Summary:  summaryView{Outcome: "error"},
		Commands: []commandView{},
		Error:    newErrorView(err),
	}
}

// renderErrorOnly writes the content-less-failure document in the machine
// formats. pretty and raw emit nothing — the message is already on stderr, and
// a human does not need a stub document.
func renderErrorOnly(w io.Writer, mode outputMode, err error) {
	switch mode {
	case outputJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(errorOnlyView(err))
	case outputYAML:
		if b, merr := sigsyaml.Marshal(errorOnlyView(err)); merr == nil {
			_, _ = w.Write(b)
		}
	case outputNDJSON:
		enc := json.NewEncoder(w)
		_ = enc.Encode(ndjsonError{Type: "error", Error: err.Error()})
		_ = enc.Encode(ndjsonSummary{Type: "summary", summaryView: errorOnlyView(err).Summary})
	}
}

func outcome(res *papurika.Result, s summaryView) string {
	if s.Targeted == 0 {
		// A local command that matched no minion is "nomatch" (consistent with
		// exit 2); a runner/wheel command (value-bearing, no minions) is "ok".
		if isNoMatch(res) {
			return "nomatch"
		}
		return "ok"
	}
	switch {
	case s.Failed == 0 && s.NoResponse == 0 && s.Pending == 0:
		return "ok"
	// Nothing has gone wrong yet — the job simply is not finished. Reporting
	// this as "failed" is what made `papurika job <jid>` alarming to run on a
	// healthy long job.
	case s.Failed == 0 && s.NoResponse == 0:
		return "pending"
	case s.Succeeded == 0 && s.Pending == 0:
		return "failed"
	default:
		return "partial"
	}
}

// isNoMatch reports whether every command is a local-family command that matched
// no minion (no per-minion results and no value) — i.e. the target hit nothing.
func isNoMatch(res *papurika.Result) bool {
	if res == nil || len(res.Commands) == 0 {
		return false
	}
	for _, c := range res.Commands {
		switch c.Client {
		case papurika.ClientLocal, papurika.ClientLocalAsync, papurika.ClientLocalBatch, papurika.ClientLocalSubset, papurika.ClientSSH:
			// A jid or a targeted set means something WAS dispatched (e.g. an async
			// submit) — not a no-match.
			if len(c.Minions) > 0 || !c.Value.IsZero() || c.JID != "" || len(c.Targeted) > 0 {
				return false
			}
		default:
			return false // runner/wheel are never "no match"
		}
	}
	return true
}

func defaultOK(m papurika.MinionResult) bool { return m.OK() }

// commandJIDs is every jid behind one CommandResult: the primary plus any
// extras a client-side-batched run merged into it.
func commandJIDs(c papurika.CommandResult) []string {
	if c.JID == "" {
		return nil
	}
	return append([]string{c.JID}, c.ExtraJIDs...)
}

// render writes the result in the chosen mode to w (stdout). Only the pretty
// path consults opts (color/changes-only/verbose); the machine formats ignore it.
func render(w io.Writer, res *papurika.Result, mode outputMode, opts renderOpts) error {
	switch mode {
	case outputJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		v := toView(res, defaultOK)
		v.Error = newErrorView(opts.err)
		return enc.Encode(v)
	case outputYAML:
		// sigs.k8s.io/yaml round-trips via JSON, so the json tags (and the v1
		// schema) carry over verbatim.
		v := toView(res, defaultOK)
		v.Error = newErrorView(opts.err)
		b, err := sigsyaml.Marshal(v)
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	case outputNDJSON:
		return writeNDJSON(w, res)
	case outputRaw:
		for _, c := range res.Commands {
			if len(c.Raw()) > 0 {
				fmt.Fprintln(w, string(c.Raw()))
			}
		}
		return nil
	default:
		return renderPretty(w, res, opts)
	}
}

// --- NDJSON: one self-contained JSON object per line (newline-delimited) ---

type ndjsonSubmit struct {
	Type     string   `json:"type"` // "submitted"
	JID      string   `json:"jid"`
	Targeted []string `json:"targeted"`
}

type ndjsonMinion struct {
	Type    string          `json:"type"` // "minion"
	JID     string          `json:"jid,omitempty"`
	ID      string          `json:"id"`
	Status  string          `json:"status"`
	Retcode *int            `json:"retcode"`
	Return  json.RawMessage `json:"return"`
}

type ndjsonValue struct {
	Type    string          `json:"type"` // "value" (runner/wheel)
	JID     string          `json:"jid,omitempty"`
	Fun     string          `json:"fun"`
	Retcode *int            `json:"retcode,omitempty"`
	Success *bool           `json:"success,omitempty"`
	Return  json.RawMessage `json:"return"`
}

type ndjsonSummary struct {
	Type        string `json:"type"` // "summary"
	summaryView        // carries jids (see summaryView.JIDs)
}

type ndjsonError struct {
	Type  string `json:"type"` // "error"
	Error string `json:"error"`
}

func minionLine(jid string, m papurika.MinionResult) ndjsonMinion {
	ret := m.Value.Raw()
	if len(ret) == 0 {
		ret = json.RawMessage("null")
	}
	return ndjsonMinion{Type: "minion", JID: jid, ID: m.ID, Status: m.Status.String(), Retcode: m.Retcode, Return: ret}
}

// writeNDJSON emits a complete (non-streamed) result as NDJSON: a submitted line
// for any async-shaped command, a minion line per minion (or a value line for
// runner/wheel), then a summary line. Used by emit-based commands.
func writeNDJSON(w io.Writer, res *papurika.Result) error {
	enc := json.NewEncoder(w)
	for _, c := range res.Commands {
		if c.JID != "" || len(c.Targeted) > 0 {
			_ = enc.Encode(ndjsonSubmit{Type: "submitted", JID: c.JID, Targeted: c.Targeted})
		}
		// A batched run has more jobs behind this one merged result; each needs
		// a line, or the stream carries fewer jids than the run created.
		for _, j := range c.ExtraJIDs {
			_ = enc.Encode(ndjsonSubmit{Type: "submitted", JID: j})
		}
		for _, id := range sortedMinionIDs(c.Minions) {
			_ = enc.Encode(minionLine(c.JID, c.Minions[id]))
		}
		if len(c.Minions) == 0 && !c.Value.IsZero() {
			ret := c.Value.Raw()
			if len(ret) == 0 {
				ret = json.RawMessage("null")
			}
			_ = enc.Encode(ndjsonValue{Type: "value", JID: c.JID, Fun: c.Fun, Retcode: c.Retcode, Success: c.Success, Return: ret})
		}
	}
	v := toView(res, defaultOK)
	return enc.Encode(ndjsonSummary{Type: "summary", summaryView: v.Summary})
}

// ndjsonStreamer drives the streaming NDJSON path for run/state/ping. It emits a
// "submitted" line the instant a jid exists (so the jid is the FIRST line and is
// never lost to an interrupt), then a "minion" line per return, then a trailing
// "summary". Safe for concurrent callbacks (client-side batch).
type ndjsonStreamer struct {
	mu   sync.Mutex
	enc  *json.Encoder
	seen map[string]bool
}

func newNDJSONStreamer(w io.Writer) *ndjsonStreamer {
	return &ndjsonStreamer{enc: json.NewEncoder(w), seen: map[string]bool{}}
}

func (s *ndjsonStreamer) onSubmit(j papurika.Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.enc.Encode(ndjsonSubmit{Type: "submitted", JID: j.JID, Targeted: j.Targeted})
}

func (s *ndjsonStreamer) onUpdate(u papurika.Update) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen[u.Minion.ID] {
		return
	}
	s.seen[u.Minion.ID] = true
	_ = s.enc.Encode(minionLine(u.JID, u.Minion))
}

// finish flushes any minions that completed before streaming caught them, an
// error line on a content-less failure, and the summary.
func (s *ndjsonStreamer) finish(res *papurika.Result, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range res.Commands {
		for _, id := range sortedMinionIDs(c.Minions) {
			if !s.seen[id] {
				s.seen[id] = true
				_ = s.enc.Encode(minionLine(c.JID, c.Minions[id]))
			}
		}
	}
	if err != nil && !resultHasContent(res) {
		_ = s.enc.Encode(ndjsonError{Type: "error", Error: err.Error()})
	}
	v := toView(res, defaultOK)
	_ = s.enc.Encode(ndjsonSummary{Type: "summary", summaryView: v.Summary})
}

func renderPretty(w io.Writer, res *papurika.Result, opts renderOpts) error {
	g := glyphs{color: opts.color}
	for _, c := range res.Commands {
		renderCommand(w, c, opts, g)
	}
	return nil
}

func renderCommand(w io.Writer, c papurika.CommandResult, opts renderOpts, g glyphs) {
	// A command cut short (Ctrl-C / deadline / fatal stream error) is PARTIAL: its
	// jid stays on stdout (in this header) so the run is recoverable without
	// relying on stderr. Reason is None for a normal sync command, so this never
	// mislabels a completed one.
	incomplete := c.Reason == papurika.ReasonCanceled ||
		c.Reason == papurika.ReasonDeadline ||
		c.Reason == papurika.ReasonStreamFatal
	head := c.Fun
	if jids := commandJIDs(c); len(jids) == 1 {
		head = fmt.Sprintf("%s  (jid %s)", c.Fun, jids[0])
	} else if len(jids) > 1 {
		// A client-side-batched run is several jobs behind one merged view.
		// Naming only the first would make the others unrecoverable.
		head = fmt.Sprintf("%s  (jids %s)", c.Fun, strings.Join(jids, " "))
	}
	if incomplete {
		head += "  · " + g.yellow("PARTIAL")
	}
	fmt.Fprintln(w, head)

	switch {
	case len(c.Minions) > 0:
		if useHighstate(c) {
			renderStateMinions(w, c, opts, g)
		} else {
			renderExecMinions(w, c, g)
		}
	case c.JID != "" || len(c.Targeted) > 0: // async submit (fire-and-forget)
		fmt.Fprintf(w, "  submitted · %d minion(s) targeted\n", len(c.Targeted))
		for _, id := range c.Targeted {
			fmt.Fprintf(w, "    %s\n", id)
		}
		fmt.Fprintln(w)
	case !c.Value.IsZero():
		switch {
		case strings.HasPrefix(c.Fun, "manage."):
			renderManage(w, c.Value, g)
		case strings.Contains(c.Fun, "key.list"):
			renderKeyList(w, c.Value)
		default:
			renderValue(w, c.Value)
		}
	default:
		fmt.Fprintf(w, "  %s\n\n", g.dim("(no minions matched)"))
	}

	// The recovery affordance lives on stdout (the result), not stderr.
	if incomplete && len(commandJIDs(c)) > 0 {
		fmt.Fprintf(w, "  %s\n\n", g.dim("resume: papurika watch "+strings.Join(commandJIDs(c), " ")))
	}
}

func renderExecMinions(w io.Writer, c papurika.CommandResult, g glyphs) {
	var ok, fail, nr, pend int
	for _, id := range sortedMinionIDs(c.Minions) {
		m := c.Minions[id]
		switch {
		case m.Status == papurika.StatusNoResponse:
			fmt.Fprintf(w, "  %s %s   (no response)\n", g.dim("⋯"), id)
			nr++
		case m.Status == papurika.StatusPending:
			fmt.Fprintf(w, "  %s %s   (pending)\n", g.dim("⋯"), id)
			pend++
		default:
			renderMinionValue(w, m, g)
			if m.OK() {
				ok++
			} else {
				fail++
			}
		}
	}
	// Pending is its own bucket: counting it as "no-response" contradicted the
	// per-minion line right above, which already said "(pending)".
	line := fmt.Sprintf("%d targeted · %d ok · %d failed · %d no-response", len(c.Minions), ok, fail, nr)
	if pend > 0 {
		line += fmt.Sprintf(" · %d pending", pend)
	}
	fmt.Fprintf(w, "%s\n\n", line)
}

// renderMinionValue renders one minion's exec return in full — never truncated.
// A structured value becomes a tree; a multi-line (or long) string becomes an
// indented block below the header (so cmd.run stdout/stderr, incl. newlines, is
// shown whole); short scalars render inline. Failure prefixes the retcode.
func renderMinionValue(w io.Writer, m papurika.MinionResult, g glyphs) {
	glyph := g.green("✓")
	if !m.OK() {
		glyph = g.red("✗")
	}
	rc := ""
	if m.Retcode != nil && *m.Retcode != 0 {
		rc = g.dim(fmt.Sprintf("(retcode %d)", *m.Retcode))
	}
	header := func(inline string) {
		parts := []string{"  " + glyph + " " + m.ID}
		if rc != "" {
			parts = append(parts, rc)
		}
		if inline != "" {
			parts = append(parts, inline)
		}
		fmt.Fprintln(w, strings.Join(parts, "   "))
	}

	var d any
	if m.Value.Decode(&d) != nil { // undecodable: show raw bytes
		raw := strings.TrimRight(string(m.Value.Raw()), "\n")
		switch {
		case raw == "":
			header(g.dim("(no output)"))
		case strings.Contains(raw, "\n"):
			header("")
			writeBlock(w, raw, "      ")
		default:
			header(raw)
		}
		return
	}
	if !isScalar(d) { // map / list → tree
		header("")
		writeTree(w, d, "      ")
		return
	}
	if s, ok := d.(string); ok {
		switch {
		case s == "":
			header(g.dim("(no output)"))
		case strings.Contains(s, "\n") || len(s) > inlineMax:
			header("")
			writeBlock(w, s, "      ")
		default:
			header(s)
		}
		return
	}
	if b, ok := d.(bool); ok { // test.ping etc. — salt's True/False
		if b {
			header("True")
		} else {
			header("False")
		}
		return
	}
	header(scalarString(d)) // number / null
}

// inlineMax is the longest single-line string still shown inline after the id;
// longer (or multi-line) strings become an indented block.
const inlineMax = 100

// writeBlock writes s verbatim (newlines preserved), each line prefixed with
// indent. A trailing newline is trimmed so there's no dangling blank line.
func writeBlock(w io.Writer, s, indent string) {
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		fmt.Fprintf(w, "%s%s\n", indent, line)
	}
}

// useHighstate reports whether a command's per-minion results should render with
// the highstate renderer: any minion value looks like a state return (a states
// map or the compile-error/conflict list), or the function itself is state.*.
func useHighstate(c papurika.CommandResult) bool {
	for _, m := range c.Minions {
		if m.Status != papurika.StatusResponded {
			continue
		}
		if state.Looks(m.Value) {
			return true
		}
		// A state.* job whose value is a list is the didn't-run shape; key off
		// the function too, since that list carries no state-ish marker.
		if strings.HasPrefix(c.Fun, "state.") && isJSONArray(m.Value.Raw()) {
			return true
		}
	}
	return false
}

func renderStateMinions(w io.Writer, c papurika.CommandResult, opts renderOpts, g glyphs) {
	var totStates, totOK, totChanged, totFailed, totSkipped, totWould, compileErr, nr, pend int
	for _, id := range sortedMinionIDs(c.Minions) {
		m := c.Minions[id]
		if m.Status != papurika.StatusResponded {
			fmt.Fprintf(w, "  %s %s   (%s)\n", g.dim("⋯"), id, m.Status)
			if m.Status == papurika.StatusPending {
				pend++
			} else {
				nr++
			}
			continue
		}
		rc := 0
		if m.Retcode != nil {
			rc = *m.Retcode
		}

		sr, err := state.ParseMinion(m)
		if err != nil {
			// The run never started. A conflict is actionable (the competing run
			// has a jid); a compile failure is not.
			var conflict *state.ConflictError
			var compile *state.CompileError
			switch {
			case errors.As(err, &conflict):
				fmt.Fprintf(w, "  %s %s   retcode %d\n", g.red("●"), id, rc)
				renderConflict(w, conflict, g)
				compileErr++
			case errors.As(err, &compile):
				fmt.Fprintf(w, "  %s %s   retcode %d\n", g.red("●"), id, rc)
				renderCompileError(w, compile.Messages, g)
				compileErr++
			default:
				fmt.Fprintf(w, "  %s %s   retcode %d\n", g.dim("●"), id, rc)
				fmt.Fprintf(w, "    %s\n", g.dim("(unparseable state return)"))
			}
			continue
		}
		rows := sr.Entries // already in salt's execution order

		// Pre-pass: per-minion counts + worst category (for the header color).
		var nOK, nChanged, nFailed, nSkipped, nWould int
		worst := state.Clean
		for _, e := range rows {
			switch e.Outcome() {
			case state.Clean:
				nOK++
			case state.Changed:
				nOK++
				nChanged++
				if worst == state.Clean {
					worst = state.Changed
				}
			case state.Failed:
				nFailed++
				worst = state.Failed
			case state.Skipped:
				nSkipped++
				worst = state.Failed // a skip implies an upstream failure
			case state.WouldChange:
				nWould++
				if worst == state.Clean {
					worst = state.Changed
				}
			}
		}
		totStates += len(rows)
		totOK += nOK
		totChanged += nChanged
		totFailed += nFailed
		totSkipped += nSkipped
		totWould += nWould

		hdr := g.green("●")
		switch worst {
		case state.Changed:
			hdr = g.cyan("●")
		case state.Failed:
			hdr = g.red("●")
		}

		// --changes-only: a minion with nothing but clean states collapses to one line.
		if opts.changesOnly && nChanged == 0 && nFailed == 0 && nSkipped == 0 && nWould == 0 {
			fmt.Fprintf(w, "  %s %s   retcode %d   %s\n", hdr, id, rc,
				g.dim(fmt.Sprintf("(%s, all clean)", plural(len(rows), "state"))))
			continue
		}
		fmt.Fprintf(w, "  %s %s   retcode %d\n", hdr, id, rc)

		for _, e := range rows {
			cat := e.Outcome()
			if opts.changesOnly && cat == state.Clean {
				continue
			}
			sid, name := e.StateID(), e.TargetName()
			var glyph string
			switch cat {
			case state.Clean:
				glyph = g.green("✓")
			case state.Changed:
				glyph = g.cyan("✓")
			case state.Failed:
				glyph = g.red("✗")
			case state.Skipped:
				glyph = g.dim("⊘")
			case state.WouldChange:
				glyph = g.yellow("~")
			}
			line := fmt.Sprintf("    %s %s", glyph, sid)
			switch cat {
			case state.Clean, state.Changed:
				sec := g.dim(e.Module() + "." + e.Function())
				if name != "" && name != sid {
					sec += "  " + g.dim(name)
				}
				line += "   " + sec
			case state.Skipped:
				line += "   " + g.dim(skippedText(e.Comment))
			default: // failed / would-change: the comment is the reason
				if e.Comment != "" {
					line += "   " + firstLine(e.Comment)
				}
			}
			if e.Duration > 0 && (e.Duration >= 100 || opts.verbose) {
				line += "   " + g.dim(fmt.Sprintf("[%.0fms]", e.Duration))
			}
			fmt.Fprintln(w, line)
			if cat == state.Changed || cat == state.Failed || cat == state.WouldChange {
				renderChanges(w, e.Changes, "        ", g)
			}
		}
	}

	// Footer: honest, disjoint buckets (changed counts only ok-with-changes).
	parts := []string{plural(len(c.Minions), "minion")}
	if totStates > 0 {
		parts = append(parts, plural(totStates, "state"))
		okPart := fmt.Sprintf("%d ok", totOK)
		if totChanged > 0 {
			okPart += fmt.Sprintf(" (%d changed)", totChanged)
		}
		parts = append(parts, okPart)
		if totFailed > 0 {
			parts = append(parts, fmt.Sprintf("%d failed", totFailed))
		}
		if totSkipped > 0 {
			parts = append(parts, fmt.Sprintf("%d skipped", totSkipped))
		}
		if totWould > 0 {
			parts = append(parts, fmt.Sprintf("%d would-change", totWould))
		}
	}
	if compileErr > 0 {
		parts = append(parts, fmt.Sprintf("%d did not compile", compileErr))
	}
	if nr > 0 {
		parts = append(parts, fmt.Sprintf("%d no-response", nr))
	}
	if pend > 0 {
		parts = append(parts, fmt.Sprintf("%d pending", pend))
	}
	fmt.Fprintf(w, "%s\n\n", strings.Join(parts, " · "))
}

// renderConflict renders a run refused because one is already in progress. The
// competing run has a jid, so the work is followable — say so.
func renderConflict(w io.Writer, e *state.ConflictError, g glyphs) {
	if e.JID == "" {
		fmt.Fprintf(w, "    %s %s\n", g.red("✗"), e.Error())
		return
	}
	fmt.Fprintf(w, "    %s a state run is already in progress (jid %s)\n", g.red("✗"), e.JID)
	fmt.Fprintf(w, "        %s\n", g.dim("run `papurika watch "+e.JID+"` to follow it"))
}

// renderCompileError renders a run that never started because the SLS could not
// be rendered or resolved.
func renderCompileError(w io.Writer, errs []string, g glyphs) {
	fmt.Fprintf(w, "    %s %s\n", g.red("✗"), "did not compile")
	for _, e := range errs {
		for _, line := range strings.Split(strings.TrimRight(e, "\n"), "\n") {
			fmt.Fprintf(w, "        %s\n", g.dim(line))
		}
	}
}

// renderChanges renders a state's changes compactly, special-casing the common
// schemas (diff, command output, old→new) and falling back to a tree.
func renderChanges(w io.Writer, changes map[string]any, indent string, g glyphs) {
	if len(changes) == 0 {
		return
	}
	changes = consolidateChanges(changes)
	if d, ok := changes["diff"]; ok {
		renderDiff(w, d, indent, g)
		if len(changes) > 1 {
			rest := make(map[string]any, len(changes)-1)
			for k, v := range changes {
				if k != "diff" {
					rest[k] = v
				}
			}
			renderChangeTree(w, rest, indent)
		}
		return
	}
	if _, hasPid := changes["pid"]; hasPid {
		if _, hasRC := changes["retcode"]; hasRC {
			renderCmdChanges(w, changes, indent, g)
			return
		}
	}
	renderChangeTree(w, changes, indent)
}

// consolidateChanges folds salt's redundant "clean removal" shape into one list.
// file.directory with clean:True returns each removed path BOTH as a per-path
// entry {path:{removed:"Removed due to clean"}} AND in an aggregate removed:[paths]
// list (verified against salt source + 3008.1; test mode emits only the per-path
// entries). We render a single removed:[paths] list. Pretty-only — json/raw keep
// salt's verbatim bytes.
func consolidateChanges(changes map[string]any) map[string]any {
	removed := map[string]bool{}
	if lst, ok := changes["removed"].([]any); ok {
		for _, p := range lst {
			if s, ok := p.(string); ok {
				removed[s] = true
			}
		}
	}
	isRemovedEntry := func(v any) bool {
		mp, ok := v.(map[string]any)
		if !ok || len(mp) != 1 {
			return false
		}
		_, ok = mp["removed"]
		return ok
	}
	for k, v := range changes {
		if k != "removed" && isRemovedEntry(v) {
			removed[k] = true
		}
	}
	if len(removed) == 0 {
		return changes
	}
	out := make(map[string]any, len(changes))
	for k, v := range changes {
		if k == "removed" {
			continue
		}
		if removed[k] && isRemovedEntry(v) {
			continue // drop the per-path duplicate
		}
		out[k] = v
	}
	paths := make([]string, 0, len(removed))
	for p := range removed {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	lst := make([]any, len(paths))
	for i, p := range paths {
		lst[i] = p
	}
	out["removed"] = lst
	return out
}

func renderChangeTree(w io.Writer, changes map[string]any, indent string) {
	keys := make([]string, 0, len(changes))
	for k := range changes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := changes[k]
		if mp, ok := v.(map[string]any); ok {
			if old, oOK := mp["old"]; oOK {
				if nw, nOK := mp["new"]; nOK && len(mp) == 2 {
					fmt.Fprintf(w, "%s%s: %s → %s\n", indent, k, oldNew(old), scalarString(nw))
					continue
				}
			}
			fmt.Fprintf(w, "%s%s:\n", indent, k)
			writeTree(w, mp, indent+"  ")
			continue
		}
		if isScalar(v) {
			fmt.Fprintf(w, "%s%s: %s\n", indent, k, scalarString(v))
		} else {
			fmt.Fprintf(w, "%s%s:\n", indent, k)
			writeTree(w, v, indent+"  ")
		}
	}
}

func renderDiff(w io.Writer, d any, indent string, g glyphs) {
	s, ok := d.(string)
	if !ok {
		fmt.Fprintf(w, "%sdiff: %s\n", indent, scalarString(d))
		return
	}
	if s == "New file" {
		fmt.Fprintf(w, "%s%s\n", indent, g.green("+ new file"))
		return
	}
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		var c string
		switch {
		case strings.HasPrefix(line, "@@"):
			c = g.cyan(line)
		case strings.HasPrefix(line, "+"):
			c = g.green(line)
		case strings.HasPrefix(line, "-"):
			c = g.red(line)
		default:
			c = g.dim(line)
		}
		fmt.Fprintf(w, "%s%s\n", indent, c)
	}
}

func renderCmdChanges(w io.Writer, changes map[string]any, indent string, g glyphs) {
	printed := false
	if s := anyString(changes["stdout"]); s != "" {
		renderLabeled(w, "stdout", s, indent, nil)
		printed = true
	}
	if s := anyString(changes["stderr"]); s != "" {
		renderLabeled(w, "stderr", s, indent, g.red)
		printed = true
	}
	if rc, ok := anyInt(changes["retcode"]); ok && rc != 0 {
		fmt.Fprintf(w, "%s%s\n", indent, g.red(fmt.Sprintf("exit %d", rc)))
		printed = true
	}
	if !printed {
		fmt.Fprintf(w, "%s%s\n", indent, g.dim("(no output)"))
	}
}

func skippedText(comment string) string {
	c := firstLine(comment)
	const p = "One or more requisite failed:"
	if strings.HasPrefix(c, p) {
		return "requisite failed:" + strings.TrimPrefix(c, p)
	}
	return c
}

func oldNew(v any) string {
	if s := scalarString(v); s != "" {
		return s
	}
	return "(none)"
}

func anyString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func anyInt(v any) (int, bool) {
	if f, ok := v.(float64); ok {
		return int(f), true
	}
	return 0, false
}

func isJSONArray(b []byte) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case '[':
			return true
		default:
			return false
		}
	}
	return false
}

func plural(n int, s string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, s)
	}
	return fmt.Sprintf("%d %ss", n, s)
}

// renderManage handles manage.up/down (a list) and manage.status ({up,down}).
func renderManage(w io.Writer, v papurika.RawValue, g glyphs) {
	var ud struct {
		Up   []string `json:"up"`
		Down []string `json:"down"`
	}
	if json.Unmarshal(v.Raw(), &ud) == nil && (ud.Up != nil || ud.Down != nil) {
		for _, h := range ud.Up {
			fmt.Fprintf(w, "  %s %s\n", g.green("●"), h)
		}
		for _, h := range ud.Down {
			fmt.Fprintf(w, "  %s %s\n", g.red("○"), h)
		}
		fmt.Fprintf(w, "%d up · %d down\n\n", len(ud.Up), len(ud.Down))
		return
	}
	var hosts []string
	if json.Unmarshal(v.Raw(), &hosts) == nil {
		for _, h := range hosts {
			fmt.Fprintf(w, "  %s %s\n", g.green("●"), h)
		}
		fmt.Fprintf(w, "%d\n\n", len(hosts))
		return
	}
	renderValue(w, v)
}

// renderKeyList handles wheel key.list_all.
func renderKeyList(w io.Writer, v papurika.RawValue) {
	var k struct {
		Minions  []string `json:"minions"`
		Pre      []string `json:"minions_pre"`
		Rejected []string `json:"minions_rejected"`
		Denied   []string `json:"minions_denied"`
	}
	if json.Unmarshal(v.Raw(), &k) != nil {
		renderValue(w, v)
		return
	}
	row := func(label string, hosts []string) {
		fmt.Fprintf(w, "  %-11s (%d)   %s\n", label, len(hosts), strings.Join(hosts, "  "))
	}
	row("accepted", k.Minions)
	row("unaccepted", k.Pre)
	row("rejected", k.Rejected)
	row("denied", k.Denied)
	fmt.Fprintln(w)
}

func renderValue(w io.Writer, v papurika.RawValue) {
	var any0 any
	if v.Decode(&any0) != nil {
		fmt.Fprintf(w, "  %s\n\n", string(v.Raw()))
		return
	}
	writeTree(w, any0, "  ")
	fmt.Fprintln(w)
}

// writeTree renders a decoded JSON value as an indented, sorted, YAML-ish tree —
// readable for grains, runner returns, etc. Scalars render inline; maps and
// slices nest.
func writeTree(w io.Writer, v any, indent string) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if isScalar(t[k]) {
				fmt.Fprintf(w, "%s%s: %s\n", indent, k, scalarString(t[k]))
			} else {
				fmt.Fprintf(w, "%s%s:\n", indent, k)
				writeTree(w, t[k], indent+"  ")
			}
		}
	case []any:
		for _, e := range t {
			if isScalar(e) {
				fmt.Fprintf(w, "%s- %s\n", indent, scalarString(e))
			} else {
				fmt.Fprintf(w, "%s-\n", indent)
				writeTree(w, e, indent+"  ")
			}
		}
	default:
		fmt.Fprintf(w, "%s%s\n", indent, scalarString(v))
	}
}

// renderLabeled prints a "label: value" change entry, inline when the value is a
// single short line, else "label:" followed by the full value as an indented
// block (newlines preserved — never truncated). colorLabel, if set, colors the
// label (e.g. red for stderr).
func renderLabeled(w io.Writer, label, s, indent string, colorLabel func(string) string) {
	lbl := label + ":"
	if colorLabel != nil {
		lbl = colorLabel(lbl)
	}
	if strings.Contains(s, "\n") || len(s) > inlineMax {
		fmt.Fprintf(w, "%s%s\n", indent, lbl)
		writeBlock(w, s, indent+"  ")
		return
	}
	fmt.Fprintf(w, "%s%s %s\n", indent, lbl, s)
}

func isScalar(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return false
	default:
		return true
	}
}

func scalarString(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case float64:
		// Render integers without a trailing .0.
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("%v", v)
	}
}

func sortedMinionIDs(minions map[string]papurika.MinionResult) []string {
	ids := make([]string, 0, len(minions))
	for id := range minions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	if len(s) > 120 {
		return s[:117] + "…"
	}
	return s
}

type glyphs struct{ color bool }

func (g glyphs) green(s string) string  { return g.wrap("32", s) }
func (g glyphs) red(s string) string    { return g.wrap("31", s) }
func (g glyphs) cyan(s string) string   { return g.wrap("36", s) }
func (g glyphs) yellow(s string) string { return g.wrap("33", s) }
func (g glyphs) dim(s string) string    { return g.wrap("2", s) }
func (g glyphs) wrap(code, s string) string {
	if !g.color {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

// exitCode maps a result + error to a process exit code per design/cli.md §D.
func exitCode(res *papurika.Result, err error, policy failPolicy, allowNoResponse bool) int {
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			return exitInterrupted
		case errors.Is(err, context.DeadlineExceeded):
			return exitTimeout
		case errors.Is(err, papurika.ErrAuthFailure), errors.Is(err, papurika.ErrAuthDenied):
			return exitAuth
		case errors.Is(err, papurika.ErrMalformed):
			return exitData
		// A 500 is the server failing, not our request being wrong — same
		// bucket as a transport failure, not as bad data.
		case errors.Is(err, papurika.ErrServerError), errors.Is(err, papurika.ErrTransport):
			return exitUnavailable
		case errors.Is(err, papurika.ErrJobNotFound), errors.Is(err, papurika.ErrNotFound):
			return exitPolicyFail
		default:
			return exitPolicyFail
		}
	}
	if res == nil {
		return exitOK
	}

	view := toView(res, defaultOK)
	s := view.Summary
	if s.Targeted == 0 {
		if isNoMatch(res) {
			return exitNoMatch // local target hit nothing
		}
		return exitOK // runner/wheel value
	}

	notOK := s.Failed
	if !allowNoResponse {
		notOK += s.NoResponse
	}
	// Pending is only benign while the job is genuinely still open — a cache
	// snapshot of a job that is running (`papurika job <jid>`), where nothing has
	// failed and exiting non-zero would be a lie. If the gather ENDED with
	// minions still pending (a jid aged out mid-run, Reason=stream_fatal), those
	// minions never will return, and that is a failure.
	if !abandonedPending(res) {
		// still open: pending costs nothing
	} else if !allowNoResponse {
		notOK += s.Pending
	}
	switch policy {
	case failNone:
		return exitOK
	case failAll:
		if notOK == s.Targeted {
			return exitPolicyFail
		}
		return exitOK
	default: // failAny
		if notOK > 0 {
			return exitPolicyFail
		}
		return exitOK
	}
}

// abandonedPending reports whether any command finished with minions still
// pending — i.e. the gather gave up rather than the job merely being in flight.
// Only then do pending minions count against the exit policy.
func abandonedPending(res *papurika.Result) bool {
	for _, c := range res.Commands {
		if c.Reason == papurika.ReasonNone {
			continue // never terminated: the job is still running
		}
		for _, m := range c.Minions {
			if m.Status == papurika.StatusPending {
				return true
			}
		}
	}
	return false
}

// jobRow is one line of a job listing, flattened so the cached and active
// listings render through the same path.
type jobRow struct {
	JID       string         `json:"jid"`
	Function  string         `json:"fun,omitempty"`
	Target    string         `json:"target,omitempty"`
	User      string         `json:"user,omitempty"`
	StartTime string         `json:"start_time,omitempty"`
	Running   map[string]int `json:"running,omitempty"`  // active listing only
	Returned  []string       `json:"returned,omitempty"` // active listing only
}

func cachedJobRows(jobs map[string]papurika.JobMeta) []jobRow {
	rows := make([]jobRow, 0, len(jobs))
	for jid, m := range jobs {
		rows = append(rows, jobRow{
			JID: jid, Function: m.Function, Target: m.Target,
			User: m.User, StartTime: m.StartTime,
		})
	}
	return sortJobRows(rows)
}

func activeJobRows(jobs map[string]papurika.ActiveJob) []jobRow {
	rows := make([]jobRow, 0, len(jobs))
	for jid, a := range jobs {
		rows = append(rows, jobRow{
			JID: jid, Function: a.Function, Target: a.Target,
			User: a.User, StartTime: a.StartTime,
			Running: a.Running, Returned: a.Returned,
		})
	}
	return sortJobRows(rows)
}

// sortJobRows orders newest first. A jid is a UTC timestamp, so sorting the
// string descending is chronological.
func sortJobRows(rows []jobRow) []jobRow {
	sort.Slice(rows, func(i, j int) bool { return rows[i].JID > rows[j].JID })
	return rows
}

// jidCutoff renders a time as the lowest jid that could have been minted at or
// after it. Salt mints jids from _utc_now() as %Y%m%d%H%M%S%f (salt/utils/jid.py
// gen_jid), and so does papurika (jid.go), so the fixed-width prefix orders
// lexicographically exactly as it does chronologically — the same property
// sortJobRows already depends on.
func jidCutoff(t time.Time) string {
	return t.UTC().Format("20060102150405") + "000000"
}

// filterJobRows applies --since and --limit, returning the kept rows and how
// many were dropped by each. Rows must already be sorted newest-first. A zero
// cutoff means no time filter.
//
// The cutoff is an absolute instant, already resolved from whichever spelling
// of --since the user gave (sinceFlag), so this does not need to know the time
// now or which form was typed.
//
// Filtering is client-side on purpose. The obvious alternative, the
// jobs.list_jobs runner with start_time, applies its filter AFTER materializing
// the whole cache (salt/runners/jobs.py), so it saves no master-side work — and
// it silently skips the filter entirely when python-dateutil is missing on the
// master, which looks identical to "no jobs in that window".
func filterJobRows(rows []jobRow, since time.Time, limit int) (kept []jobRow, agedOut, truncated int) {
	kept = rows
	if !since.IsZero() {
		cutoff := jidCutoff(since)
		keep := make([]jobRow, 0, len(kept))
		for _, r := range kept {
			// An unparseable jid is kept rather than hidden: dropping rows we
			// cannot date would silently under-report.
			if len(r.JID) >= 20 && r.JID[:20] < cutoff {
				continue
			}
			keep = append(keep, r)
		}
		agedOut = len(kept) - len(keep)
		kept = keep
	}
	if limit > 0 && len(kept) > limit {
		truncated = len(kept) - limit
		kept = kept[:limit]
	}
	return kept, agedOut, truncated
}

// writeJobListing renders a job listing in the selected output mode. Job
// listings are not per-minion Results, so they do not go through render().
func writeJobListing(w io.Writer, rows []jobRow) error {
	switch resolveOutput() {
	case outputYAML:
		b, err := json.Marshal(rows)
		if err != nil {
			return err
		}
		y, err := sigsyaml.JSONToYAML(b)
		if err != nil {
			return err
		}
		_, err = w.Write(y)
		return err
	case outputPretty:
		renderJobListing(w, rows, glyphs{color: useColor()})
		return nil
	default: // json, ndjson, raw
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
}

func renderJobListing(w io.Writer, rows []jobRow, g glyphs) {
	if len(rows) == 0 {
		fmt.Fprintf(w, "%s\n", g.dim("no jobs"))
		return
	}
	for _, r := range rows {
		fmt.Fprintf(w, "%s  %s\n", r.JID, r.Function)
		detail := fmt.Sprintf("target=%s user=%s", orDash(r.Target), orDash(r.User))
		if r.StartTime != "" {
			detail += "  " + r.StartTime
		}
		fmt.Fprintf(w, "  %s\n", g.dim(detail))
		if len(r.Running) > 0 {
			ids := make([]string, 0, len(r.Running))
			for id := range r.Running {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			parts := make([]string, len(ids))
			for i, id := range ids {
				parts[i] = fmt.Sprintf("%s(pid %d)", id, r.Running[id])
			}
			fmt.Fprintf(w, "  %s %s\n", g.cyan("running:"), strings.Join(parts, " "))
		}
		if len(r.Returned) > 0 {
			fmt.Fprintf(w, "  %s %s\n", g.dim("returned:"), strings.Join(r.Returned, " "))
		}
	}
	fmt.Fprintf(w, "\n%s\n", g.dim(plural(len(rows), "job")))
}
