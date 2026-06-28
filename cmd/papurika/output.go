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
	Index   int              `json:"index"`
	Client  string           `json:"client"`
	Fun     string           `json:"fun"`
	JID     string           `json:"jid,omitempty"`
	Minions []minionView     `json:"minions,omitempty"`
	Value   *json.RawMessage `json:"value,omitempty"`
	Retcode *int             `json:"retcode,omitempty"`
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
		cv := commandView{Index: i, Client: string(c.Client), Fun: c.Fun, JID: c.JID, Retcode: c.Retcode}
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
	v.Summary.Outcome = outcome(v.Summary)
	return v
}

func outcome(s summaryView) string {
	switch {
	case s.Targeted == 0:
		return "ok" // runner/wheel or no-minion command
	case s.Failed == 0 && s.NoResponse == 0:
		return "ok"
	case s.Succeeded == 0:
		return "failed"
	default:
		return "partial"
	}
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
		head := c.Fun
		if c.JID != "" {
			head = fmt.Sprintf("%s  (jid %s)", c.Fun, c.JID)
		}
		fmt.Fprintln(w, head)

		if len(c.Minions) > 0 {
			ids := make([]string, 0, len(c.Minions))
			for id := range c.Minions {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			var ok, fail, nr int
			for _, id := range ids {
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
			fmt.Fprintf(w, "%d targeted · %d ok · %d failed · %d no-response\n\n", len(ids), ok, fail, nr)
		} else if !c.Value.IsZero() {
			var pretty any
			if c.Value.Decode(&pretty) == nil {
				b, _ := json.MarshalIndent(pretty, "  ", "  ")
				fmt.Fprintf(w, "  %s\n\n", string(b))
			} else {
				fmt.Fprintf(w, "  %s\n\n", string(c.Value.Raw()))
			}
		}
	}
	return nil
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
	// A local command that matched no minion (nothing targeted, no runner value).
	if s.Targeted == 0 {
		hasValue := false
		for _, c := range res.Commands {
			if !c.Value.IsZero() {
				hasValue = true
			}
		}
		if !hasValue && localish(res) {
			return exitNoMatch
		}
		return exitOK
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

func localish(res *papurika.Result) bool {
	for _, c := range res.Commands {
		switch c.Client {
		case papurika.ClientLocal, papurika.ClientLocalAsync, papurika.ClientLocalBatch, papurika.ClientLocalSubset, papurika.ClientSSH:
			return true
		}
	}
	return false
}
