package papurika

import "fmt"

// ClientType is a salt-api netapi client. It selects which subsystem a lowstate
// command targets (execution modules, runners, wheel, ssh) and whether it runs
// synchronously or returns a jid immediately.
type ClientType string

const (
	ClientLocal       ClientType = "local"
	ClientLocalAsync  ClientType = "local_async"
	ClientLocalBatch  ClientType = "local_batch"
	ClientLocalSubset ClientType = "local_subset"
	ClientRunner      ClientType = "runner"
	ClientRunnerAsync ClientType = "runner_async"
	ClientWheel       ClientType = "wheel"
	ClientWheelAsync  ClientType = "wheel_async"
	ClientSSH         ClientType = "ssh"
)

// IsAsync reports whether the client returns a jid immediately rather than
// blocking for returns.
func (c ClientType) IsAsync() bool {
	switch c {
	case ClientLocalAsync, ClientRunnerAsync, ClientWheelAsync:
		return true
	default:
		return false
	}
}

// TgtType is a salt targeting expression type (mapped to the lowstate "tgt_type"
// field). The zero value is treated as TgtGlob by the wire encoder.
type TgtType string

const (
	TgtGlob       TgtType = "glob"
	TgtList       TgtType = "list"
	TgtPCRE       TgtType = "pcre"
	TgtGrain      TgtType = "grain"
	TgtGrainPCRE  TgtType = "grain_pcre"
	TgtPillar     TgtType = "pillar"
	TgtPillarPCRE TgtType = "pillar_pcre"
	TgtRange      TgtType = "range"
	TgtCompound   TgtType = "compound"
	TgtNodegroup  TgtType = "nodegroup"
)

// Command is a single salt-api lowstate dict. Build one with Local/Runner/Wheel
// and the With* helpers, or set fields directly for full control. The client
// injects "full_return": true at send time for every non-wheel command (wheel is
// already in full-return shape), so callers never set it.
type Command struct {
	Client  ClientType
	Fun     string
	Tgt     string         // local / local_* / ssh
	TgtType TgtType        // defaults to glob on the wire
	Arg     []any          // positional args (local / ssh)
	Kwarg   map[string]any // keyword args
	Batch   string         // "10" | "25%" — local_batch
	Subset  int            // local_subset

	// JID, when set, is submitted as the job's id instead of letting the master
	// mint one. salt-api honors it verbatim. The async engine sets this itself
	// so that a submit whose response is lost is still recoverable; set it by
	// hand only if you are tracking jids yourself. Must have salt's shape —
	// 20 digits, optionally followed by "_" and a suffix.
	//
	// Reusing a jid does NOT dedupe at the master: it republishes into the
	// existing job-cache entry. (The *minion* drops a jid it has already run,
	// but only best-effort — that queue is in-memory and capped.)
	JID string

	// Idempotent declares that running this command more than once is
	// harmless, which lets papurika re-send it when it cannot prove whether the
	// first attempt executed.
	//
	// It is NOT needed for the common case. On an ambiguous failure the async
	// engine reconciles against /jobs/<jid> first, and a job it can prove never
	// ran is re-sent regardless of this field — proving it didn't run is
	// stronger evidence than any declaration. This field only governs what
	// happens when reconciliation itself is impossible or inconclusive: a
	// client type that takes no jid, or a master too unreachable to ask.
	//
	// Default (false) is therefore the careful one: if papurika cannot tell
	// whether your command ran, it will not run it again.
	Idempotent bool

	// Extra is merged verbatim into the lowstate dict (escape hatch for fields
	// we don't model). It does not override the structured fields above.
	Extra map[string]any
}

// Local builds a "local" execution command against tgt.
func Local(tgt, fun string, args ...any) Command {
	return Command{Client: ClientLocal, Tgt: tgt, Fun: fun, Arg: args}
}

// Runner builds a master-side "runner" command. Runner functions take only
// keyword args; add them with WithKwarg.
func Runner(fun string) Command {
	return Command{Client: ClientRunner, Fun: fun}
}

// Wheel builds a master-side "wheel" command (key/config management).
func Wheel(fun string) Command {
	return Command{Client: ClientWheel, Fun: fun}
}

// WithKwarg returns a copy of c with kwarg k=v set.
func (c Command) WithKwarg(k string, v any) Command {
	kw := make(map[string]any, len(c.Kwarg)+1)
	for key, val := range c.Kwarg {
		kw[key] = val
	}
	kw[k] = v
	c.Kwarg = kw
	return c
}

// WithTgtType returns a copy of c with the targeting type set.
func (c Command) WithTgtType(t TgtType) Command { c.TgtType = t; return c }

// WithArg returns a copy of c with positional args appended.
func (c Command) WithArg(args ...any) Command {
	c.Arg = append(append([]any{}, c.Arg...), args...)
	return c
}

// lowstate renders the command to the wire map. When fullReturn is true it sets
// "full_return": true for non-wheel clients (wheel is already wrapped).
func (c Command) lowstate(fullReturn bool) map[string]any {
	m := map[string]any{"client": string(c.Client), "fun": c.Fun}
	for k, v := range c.Extra {
		m[k] = v
	}
	switch c.Client {
	case ClientRunner, ClientRunnerAsync, ClientWheel, ClientWheelAsync:
		// Runners/wheel take kwargs as top-level fields; no tgt.
		for k, v := range c.Kwarg {
			m[k] = v
		}
	default:
		m["tgt"] = c.Tgt
		tt := c.TgtType
		if tt == "" {
			tt = TgtGlob
		}
		m["tgt_type"] = string(tt)
		if len(c.Arg) > 0 {
			m["arg"] = c.Arg
		}
		if len(c.Kwarg) > 0 {
			m["kwarg"] = c.Kwarg
		}
		if c.Batch != "" {
			m["batch"] = c.Batch
		}
		if c.Subset > 0 {
			m["subset"] = c.Subset
		}
	}
	if c.JID != "" {
		m["jid"] = c.JID
	}
	if fullReturn && c.Client != ClientWheel && c.Client != ClientWheelAsync {
		m["full_return"] = true
	}
	return m
}

// validateLowstate checks the fields salt requires, on the rendered lowstate
// (not the Command), so that Extra overriding a structured field is caught too.
//
// This runs before the request leaves the client for a specific reason: cherrypy
// answers a lowstate missing fun or tgt with HTTP 500 and an opaque "An
// unexpected error occurred" — the same response it gives for an internal
// failure (cherrypy_learnings.md). Those two are indistinguishable from the
// outside, so papurika rules the first one out from the inside. A caller then
// gets ErrMalformed naming the missing field, and a 500 that does come back can
// honestly be reported as the server's fault (ErrServerError).
func validateLowstate(m map[string]any) error {
	nonEmpty := func(k string) bool {
		s, ok := m[k].(string)
		return ok && s != ""
	}
	if !nonEmpty("client") {
		return fmt.Errorf("%w: client is required", ErrMalformed)
	}
	if !nonEmpty("fun") {
		return fmt.Errorf("%w: fun is required", ErrMalformed)
	}
	// Runner and wheel act on the master and take no target; everything else
	// (local, local_*, ssh) targets minions and must say which.
	if _, targeted := m["tgt"]; targeted && !nonEmpty("tgt") {
		return fmt.Errorf("%w: tgt is required for client %q", ErrMalformed, m["client"])
	}
	// Nothing on salt's publish path validates a jid, so a malformed one is
	// accepted and then silently degrades StartTime and jid-ordered listings.
	// Catch it here instead.
	if j, ok := m["jid"].(string); ok && j != "" && !looksLikeJID(j) {
		return fmt.Errorf("%w: jid %q is not a salt jid (want 20 digits, optionally _suffix)", ErrMalformed, j)
	}
	return nil
}

// lowstates renders and validates a batch of commands for the wire.
func lowstates(cmds []Command, fullReturn bool) ([]map[string]any, error) {
	out := make([]map[string]any, len(cmds))
	for i, cmd := range cmds {
		m := cmd.lowstate(fullReturn)
		if err := validateLowstate(m); err != nil {
			if len(cmds) > 1 {
				return nil, fmt.Errorf("command %d: %w", i, err)
			}
			return nil, err
		}
		out[i] = m
	}
	return out, nil
}

// JobSpec is the convenience input to the async engine (Run/Submit/Watch). It
// describes one "local" execution; the engine picks the async client and handles
// gathering. For multi-command or non-local lowstate, use Command with Exec.
type JobSpec struct {
	Target  string
	TgtType TgtType
	Fun     string
	Arg     []any
	Kwarg   map[string]any
	Batch   string

	// Idempotent declares that running this job more than once is harmless.
	// See Command.Idempotent — in short, it is only consulted when papurika
	// cannot determine whether a lost submit actually reached the minions.
	Idempotent bool
}

// command converts the spec to a Command of the given client type.
func (s JobSpec) command(client ClientType) Command {
	return Command{
		Client:  client,
		Tgt:     s.Target,
		TgtType: s.TgtType,
		Fun:     s.Fun,
		Arg:     s.Arg,
		Kwarg:   s.Kwarg,
		Batch:   s.Batch,

		Idempotent: s.Idempotent,
	}
}
