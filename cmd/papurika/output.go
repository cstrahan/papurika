package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/cstrahan/papurika"
)

// outputMode selects how results are rendered.
type outputMode string

const (
	outputPretty outputMode = "pretty"
	outputJSON   outputMode = "json"
	outputRaw    outputMode = "raw"
)

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

// render writes the result in the chosen mode to w (stdout).
func render(w io.Writer, res *papurika.Result, mode outputMode, color bool) error {
	switch mode {
	case outputJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(toView(res, defaultOK))
	case outputRaw:
		for _, c := range res.Commands {
			if len(c.Raw()) > 0 {
				fmt.Fprintln(w, string(c.Raw()))
			}
		}
		return nil
	default:
		return renderPretty(w, res, color)
	}
}

func renderPretty(w io.Writer, res *papurika.Result, color bool) error {
	g := glyphs{color: color}
	for _, c := range res.Commands {
		renderCommand(w, c, g)
	}
	return nil
}

func renderCommand(w io.Writer, c papurika.CommandResult, g glyphs) {
	head := c.Fun
	if c.JID != "" {
		head = fmt.Sprintf("%s  (jid %s)", c.Fun, c.JID)
	}
	fmt.Fprintln(w, head)

	switch {
	case len(c.Minions) > 0:
		if anyHighstate(c.Minions) {
			renderStateMinions(w, c, g)
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
			fmt.Fprintf(w, "  %s %s   %s\n", g.green("✓"), id, summarize(m))
			ok++
		default:
			fmt.Fprintf(w, "  %s %s   %s\n", g.red("✗"), id, summarize(m))
			fail++
		}
	}
	fmt.Fprintf(w, "%d targeted · %d ok · %d failed · %d no-response\n\n", len(c.Minions), ok, fail, nr)
}

// stateEntry is one state's result inside a highstate return.
type stateEntry struct {
	Name     string         `json:"name"`
	Result   *bool          `json:"result"` // true=ok, false=fail, null=test/no-op
	Comment  string         `json:"comment"`
	Changes  map[string]any `json:"changes"`
	Duration float64        `json:"duration"` // ms
	RunNum   int            `json:"__run_num__"`
	ID       string         `json:"__id__"`
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

func renderStateMinions(w io.Writer, c papurika.CommandResult, g glyphs) {
	var totalStates, totalOK, totalFail int
	for _, id := range sortedMinionIDs(c.Minions) {
		m := c.Minions[id]
		if m.Status != papurika.StatusResponded {
			fmt.Fprintf(w, "  %s %s   (%s)\n", g.dim("⋯"), id, m.Status)
			continue
		}
		rc := 0
		if m.Retcode != nil {
			rc = *m.Retcode
		}
		fmt.Fprintf(w, "  %s   (retcode %d)\n", id, rc)
		var states map[string]stateEntry
		if m.Value.Decode(&states) != nil {
			fmt.Fprintf(w, "    %s\n", g.dim("(unparseable state return)"))
			continue
		}
		ordered := make([]stateEntry, 0, len(states))
		for _, e := range states {
			ordered = append(ordered, e)
		}
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].RunNum < ordered[j].RunNum })
		for _, e := range ordered {
			totalStates++
			var glyph, name string
			switch {
			case e.Result == nil:
				glyph = g.dim("~") // test mode / no-op
			case *e.Result:
				glyph = g.green("✓")
				totalOK++
			default:
				glyph = g.red("✗")
				totalFail++
			}
			name = e.ID
			if name == "" {
				name = e.Name
			}
			line := fmt.Sprintf("    %s %s", glyph, name)
			if e.Comment != "" {
				line += "   " + firstLine(e.Comment)
			}
			if e.Duration > 0 {
				line += fmt.Sprintf("   (%.1fms)", e.Duration)
			}
			if len(e.Changes) > 0 {
				line += "  " + g.dim("[changed]")
			}
			fmt.Fprintln(w, line)
		}
	}
	fmt.Fprintf(w, "%d minion(s) · %d states · %d ok · %d failed\n\n", len(c.Minions), totalStates, totalOK, totalFail)
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
	var pretty any
	if v.Decode(&pretty) == nil {
		b, _ := json.MarshalIndent(pretty, "  ", "  ")
		fmt.Fprintf(w, "  %s\n\n", string(b))
	} else {
		fmt.Fprintf(w, "  %s\n\n", string(v.Raw()))
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

func (g glyphs) green(s string) string { return g.wrap("32", s) }
func (g glyphs) red(s string) string   { return g.wrap("31", s) }
func (g glyphs) dim(s string) string   { return g.wrap("2", s) }
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
