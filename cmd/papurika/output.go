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

	"github.com/cstrahan/papurika"
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
	color       bool // ANSI color enabled
	changesOnly bool // highstate: hide clean (ok, unchanged) states
	verbose     bool // show every state's duration / fuller detail
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
)

// resultView is the stable papurika.dev/result/v1 JSON schema.
type resultView struct {
	Schema   string        `json:"schema"`
	Summary  summaryView   `json:"summary"`
	Commands []commandView `json:"commands"`
}

type summaryView struct {
	Outcome    string `json:"outcome"`
	Targeted   int    `json:"targeted"`
	Returned   int    `json:"returned"`
	Succeeded  int    `json:"succeeded"`
	Failed     int    `json:"failed"`
	NoResponse int    `json:"noresponse"`
}

type commandView struct {
	Index    int              `json:"index"`
	Client   string           `json:"client"`
	Fun      string           `json:"fun"`
	JID      string           `json:"jid,omitempty"`
	Targeted []string         `json:"targeted,omitempty"`
	Minions  []minionView     `json:"minions,omitempty"`
	Value    *json.RawMessage `json:"value,omitempty"`
	Retcode  *int             `json:"retcode,omitempty"`
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
		cv := commandView{Index: i, Client: string(c.Client), Fun: c.Fun, JID: c.JID, Targeted: c.Targeted, Retcode: c.Retcode}
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
			case m.Status == papurika.StatusNoResponse || m.Status == papurika.StatusPending:
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
	return v
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
	case s.Failed == 0 && s.NoResponse == 0:
		return "ok"
	case s.Succeeded == 0:
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

// render writes the result in the chosen mode to w (stdout). Only the pretty
// path consults opts (color/changes-only/verbose); the machine formats ignore it.
func render(w io.Writer, res *papurika.Result, mode outputMode, opts renderOpts) error {
	switch mode {
	case outputJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(toView(res, defaultOK))
	case outputYAML:
		// sigs.k8s.io/yaml round-trips via JSON, so the json tags (and the v1
		// schema) carry over verbatim.
		b, err := sigsyaml.Marshal(toView(res, defaultOK))
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
	Type string   `json:"type"` // "summary"
	JIDs []string `json:"jids,omitempty"`
	summaryView
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
	return enc.Encode(ndjsonSummary{Type: "summary", JIDs: res.JIDs(), summaryView: v.Summary})
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
	_ = s.enc.Encode(ndjsonSummary{Type: "summary", JIDs: res.JIDs(), summaryView: v.Summary})
}

func renderPretty(w io.Writer, res *papurika.Result, opts renderOpts) error {
	g := glyphs{color: opts.color}
	for _, c := range res.Commands {
		renderCommand(w, c, opts, g)
	}
	return nil
}

func renderCommand(w io.Writer, c papurika.CommandResult, opts renderOpts, g glyphs) {
	head := c.Fun
	if c.JID != "" {
		head = fmt.Sprintf("%s  (jid %s)", c.Fun, c.JID)
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
}

func renderExecMinions(w io.Writer, c papurika.CommandResult, g glyphs) {
	var ok, fail, nr int
	for _, id := range sortedMinionIDs(c.Minions) {
		m := c.Minions[id]
		switch {
		case m.Status == papurika.StatusNoResponse:
			fmt.Fprintf(w, "  %s %s   (no response)\n", g.dim("⋯"), id)
			nr++
		case m.Status == papurika.StatusPending:
			fmt.Fprintf(w, "  %s %s   (pending)\n", g.dim("⋯"), id)
			nr++
		case m.OK():
			if v, ok2 := structured(m.Value); ok2 {
				fmt.Fprintf(w, "  %s %s\n", g.green("✓"), id)
				writeTree(w, v, "      ")
			} else {
				fmt.Fprintf(w, "  %s %s   %s\n", g.green("✓"), id, summarize(m))
			}
			ok++
		default:
			fmt.Fprintf(w, "  %s %s   %s\n", g.red("✗"), id, summarize(m))
			fail++
		}
	}
	fmt.Fprintf(w, "%d targeted · %d ok · %d failed · %d no-response\n\n", len(c.Minions), ok, fail, nr)
}

// stateEntry is one state's result inside a highstate return (see
// research/salt-test-cherrypy/state-apply-shapes.md for the wire shapes).
type stateEntry struct {
	Name     string         `json:"name"`
	Result   *bool          `json:"result"` // true=ok, false=fail, null=test/would-change
	Comment  string         `json:"comment"`
	Changes  map[string]any `json:"changes"`
	Duration float64        `json:"duration"` // ms
	RunNum   int            `json:"__run_num__"`
	ID       string         `json:"__id__"`
	StateRan *bool          `json:"__state_ran__"` // present & false only when skipped
}

// stateCat classifies a per-state outcome for glyph/color/count purposes.
type stateCat int

const (
	catClean   stateCat = iota // result true, no changes
	catChanged                 // result true, with changes
	catFailed                  // result false, ran
	catSkipped                 // result false, __state_ran__ false (requisite)
	catWould                   // result null (test would-change)
)

func classify(e stateEntry) stateCat {
	switch {
	case e.Result == nil:
		return catWould
	case !*e.Result:
		if e.StateRan != nil && !*e.StateRan {
			return catSkipped
		}
		return catFailed
	default:
		if len(e.Changes) > 0 {
			return catChanged
		}
		return catClean
	}
}

func anyHighstate(minions map[string]papurika.MinionResult) bool {
	for _, m := range minions {
		if m.Status == papurika.StatusResponded && looksHighstate(m.Value) {
			return true
		}
	}
	return false
}

func looksHighstate(v papurika.RawValue) bool {
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
	return false
}

// useHighstate reports whether a command's per-minion results should render with
// the highstate renderer: any minion looks like a highstate dict, or it's a
// state.* function and some minion value is a list (the compile-error / conflict
// shape — which no longer looks like a dict, so we key off the function too).
func useHighstate(c papurika.CommandResult) bool {
	if anyHighstate(c.Minions) {
		return true
	}
	if strings.HasPrefix(c.Fun, "state.") {
		for _, m := range c.Minions {
			if m.Status == papurika.StatusResponded && isJSONArray(m.Value.Raw()) {
				return true
			}
		}
	}
	return false
}

func renderStateMinions(w io.Writer, c papurika.CommandResult, opts renderOpts, g glyphs) {
	var totStates, totOK, totChanged, totFailed, totSkipped, totWould, compileErr, nr int
	for _, id := range sortedMinionIDs(c.Minions) {
		m := c.Minions[id]
		if m.Status != papurika.StatusResponded {
			fmt.Fprintf(w, "  %s %s   (%s)\n", g.dim("⋯"), id, m.Status)
			nr++
			continue
		}
		rc := 0
		if m.Retcode != nil {
			rc = *m.Retcode
		}

		// Compile error / concurrency conflict: the minion value is a list of
		// error strings, not a states dict.
		if isJSONArray(m.Value.Raw()) {
			var errs []string
			_ = m.Value.Decode(&errs)
			fmt.Fprintf(w, "  %s %s   retcode %d\n", g.red("●"), id, rc)
			renderCompileError(w, errs, g)
			compileErr++
			continue
		}

		var states map[string]stateEntry
		if m.Value.Decode(&states) != nil {
			fmt.Fprintf(w, "  %s %s   retcode %d\n", g.dim("●"), id, rc)
			fmt.Fprintf(w, "    %s\n", g.dim("(unparseable state return)"))
			continue
		}

		type row struct {
			tag string
			e   stateEntry
		}
		rows := make([]row, 0, len(states))
		for tag, e := range states {
			rows = append(rows, row{tag, e})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].e.RunNum < rows[j].e.RunNum })

		// Pre-pass: per-minion counts + worst category (for the header color).
		var nOK, nChanged, nFailed, nSkipped, nWould int
		worst := catClean
		for _, r := range rows {
			switch classify(r.e) {
			case catClean:
				nOK++
			case catChanged:
				nOK++
				nChanged++
				if worst == catClean {
					worst = catChanged
				}
			case catFailed:
				nFailed++
				worst = catFailed
			case catSkipped:
				nSkipped++
				worst = catFailed // a skip implies an upstream failure
			case catWould:
				nWould++
				if worst == catClean {
					worst = catChanged
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
		case catChanged:
			hdr = g.cyan("●")
		case catFailed:
			hdr = g.red("●")
		}

		// --changes-only: a minion with nothing but clean states collapses to one line.
		if opts.changesOnly && nChanged == 0 && nFailed == 0 && nSkipped == 0 && nWould == 0 {
			fmt.Fprintf(w, "  %s %s   retcode %d   %s\n", hdr, id, rc,
				g.dim(fmt.Sprintf("(%s, all clean)", plural(len(rows), "state"))))
			continue
		}
		fmt.Fprintf(w, "  %s %s   retcode %d\n", hdr, id, rc)

		for _, r := range rows {
			cat := classify(r.e)
			if opts.changesOnly && cat == catClean {
				continue
			}
			mod, sid, name, fun := parseTag(r.tag, r.e)
			var glyph string
			switch cat {
			case catClean:
				glyph = g.green("✓")
			case catChanged:
				glyph = g.cyan("✓")
			case catFailed:
				glyph = g.red("✗")
			case catSkipped:
				glyph = g.dim("⊘")
			case catWould:
				glyph = g.yellow("~")
			}
			line := fmt.Sprintf("    %s %s", glyph, sid)
			switch cat {
			case catClean, catChanged:
				sec := g.dim(mod + "." + fun)
				if name != "" && name != sid {
					sec += "  " + g.dim(name)
				}
				line += "   " + sec
			case catSkipped:
				line += "   " + g.dim(skippedText(r.e.Comment))
			default: // failed / would-change: the comment is the reason
				if r.e.Comment != "" {
					line += "   " + firstLine(r.e.Comment)
				}
			}
			if r.e.Duration > 0 && (r.e.Duration >= 100 || opts.verbose) {
				line += "   " + g.dim(fmt.Sprintf("[%.0fms]", r.e.Duration))
			}
			fmt.Fprintln(w, line)
			if cat == catChanged || cat == catFailed || cat == catWould {
				renderChanges(w, r.e.Changes, "        ", g)
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
	fmt.Fprintf(w, "%s\n\n", strings.Join(parts, " · "))
}

// renderCompileError renders a list-valued (didn't-compile) minion return. The
// concurrency-conflict variant gets a friendlier, actionable line + jid.
func renderCompileError(w io.Writer, errs []string, g glyphs) {
	for _, e := range errs {
		if jid, ok := conflictJID(e); ok {
			fmt.Fprintf(w, "    %s a state run is already in progress (jid %s)\n", g.red("✗"), jid)
			fmt.Fprintf(w, "        %s\n", g.dim("run `papurika watch "+jid+"` to follow it"))
			return
		}
	}
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
		fmt.Fprintf(w, "%sstdout: %s\n", indent, firstLine(s))
		printed = true
	}
	if s := anyString(changes["stderr"]); s != "" {
		fmt.Fprintf(w, "%s%s\n", indent, g.red("stderr: "+firstLine(s)))
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

// parseTag splits a salt state tag "<module>_|-<id>_|-<name>_|-<func>", preferring
// the explicit __id__/name fields when present.
func parseTag(tag string, e stateEntry) (mod, id, name, fun string) {
	comps := strings.SplitN(tag, "_|-", 4)
	if len(comps) == 4 {
		mod, id, name, fun = comps[0], comps[1], comps[2], comps[3]
	}
	if e.ID != "" {
		id = e.ID
	}
	if e.Name != "" {
		name = e.Name
	}
	return mod, id, name, fun
}

func skippedText(comment string) string {
	c := firstLine(comment)
	const p = "One or more requisite failed:"
	if strings.HasPrefix(c, p) {
		return "requisite failed:" + strings.TrimPrefix(c, p)
	}
	return c
}

// conflictJID extracts the jid from salt's "is running as PID … with jid <jid>"
// concurrency-conflict message.
func conflictJID(s string) (string, bool) {
	const marker = "with jid "
	i := strings.Index(s, marker)
	if i < 0 || !strings.Contains(s, "is running as PID") {
		return "", false
	}
	rest := strings.TrimSpace(s[i+len(marker):])
	if j := strings.IndexAny(rest, " \n\t\""); j >= 0 {
		rest = rest[:j]
	}
	return rest, rest != ""
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

// structured decodes a RawValue and reports whether it's a map/slice (worth a
// tree) rather than a scalar (worth a one-line summary).
func structured(v papurika.RawValue) (any, bool) {
	var d any
	if v.Decode(&d) != nil {
		return nil, false
	}
	return d, !isScalar(d)
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

// summarize renders a one-line value summary for a minion.
func summarize(m papurika.MinionResult) string {
	if m.Retcode != nil && *m.Retcode != 0 {
		if s, ok := m.Value.String(); ok && s != "" {
			return fmt.Sprintf("retcode %d: %s", *m.Retcode, firstLine(s))
		}
		return fmt.Sprintf("retcode %d", *m.Retcode)
	}
	if b, ok := m.Value.Bool(); ok {
		if b {
			return "True"
		}
		return "False"
	}
	if s, ok := m.Value.String(); ok {
		return firstLine(s)
	}
	raw := string(m.Value.Raw())
	return firstLine(raw)
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
		case errors.Is(err, papurika.ErrTransport):
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
