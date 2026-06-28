package papurika

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
	if fullReturn && c.Client != ClientWheel && c.Client != ClientWheelAsync {
		m["full_return"] = true
	}
	return m
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
	}
}
