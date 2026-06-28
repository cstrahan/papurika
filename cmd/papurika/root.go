package main

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/cstrahan/papurika"
	"github.com/cstrahan/papurika/config"
	"github.com/spf13/cobra"
)

// global flags
var (
	flagConfig   string
	flagProfile  string
	flagURL      string
	flagUser     string
	flagEauth    string
	flagOutput   string
	flagTimeout  time.Duration
	flagInsecure bool
	flagNoColor  bool
	flagVerbose  int
	flagFail     string
	flagAllowNR  bool
	flagCACert   string
	flagSaltTO   time.Duration
	flagGatherTO time.Duration
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "papurika",
		Short:         "A salt-api client (a better pepper)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringVarP(&flagConfig, "config", "c", "", "config file (default: $XDG_CONFIG_HOME/papurika/config.toml)")
	pf.StringVarP(&flagProfile, "profile", "p", "", "config profile to use")
	pf.StringVar(&flagURL, "url", "", "salt-api URL (overrides profile)")
	pf.StringVarP(&flagUser, "user", "u", "", "eauth username (overrides profile)")
	pf.StringVar(&flagEauth, "eauth", "", "eauth backend (overrides profile)")
	pf.StringVarP(&flagOutput, "output", "o", "", "output mode: pretty|json|yaml|raw (default: pretty on a TTY, json when piped)")
	pf.DurationVar(&flagTimeout, "timeout", 0, "hard client wall-clock deadline (exit 124 if hit; 0 = none)")
	pf.DurationVar(&flagSaltTO, "salt-timeout", 0, "salt initial gather timeout floor")
	pf.DurationVar(&flagGatherTO, "gather-timeout", 0, "quiet window after which non-responders are settled (normal completion)")
	pf.BoolVar(&flagInsecure, "insecure", false, "skip TLS certificate verification")
	pf.StringVar(&flagCACert, "cacert", "", "PEM file of trusted CA certs")
	pf.BoolVar(&flagNoColor, "no-color", false, "disable ANSI color (also honors NO_COLOR)")
	pf.CountVarP(&flagVerbose, "verbose", "v", "increase stderr verbosity (repeatable)")
	pf.StringVar(&flagFail, "fail", "any", "exit-code policy: any|all|none")
	pf.BoolVar(&flagAllowNR, "allow-noresponse", false, "non-responders do not count as failure")

	root.AddCommand(
		// high-level
		newRunCmd(),
		newStateCmd(),
		newPingCmd(),
		newSubmitCmd(),
		newWatchCmd(),
		newJobCmd(),
		newJobsCmd(),
		newKillCmd(),
		newEventsCmd(),
		newKeyCmd(),
		// low-level (one client = one subcommand)
		newClientCmd("local <tgt> <fun> [args...]", "client=local (synchronous)", papurika.ClientLocal, false),
		newClientCmd("local-async <tgt> <fun> [args...]", "client=local_async → jid", papurika.ClientLocalAsync, false),
		newClientCmd("local-batch <tgt> <fun> [args...]", "client=local_batch (server-side)", papurika.ClientLocalBatch, false),
		newClientCmd("local-subset <tgt> <fun> [args...]", "client=local_subset", papurika.ClientLocalSubset, true),
		newRunnerCmd(),
		newMasterCmd("runner-async <fun> [k=v...]", "client=runner_async → jid", papurika.ClientRunnerAsync),
		newWheelCmd(),
		newMasterCmd("wheel-async <fun> [k=v...]", "client=wheel_async → jid", papurika.ClientWheelAsync),
		newClientCmd("ssh <tgt> <fun> [args...]", "client=ssh (synchronous)", papurika.ClientSSH, false),
		newLowstateCmd(),
	)
	return root
}

// buildClient resolves config + flags into a Client and the output mode.
func buildClient() (*papurika.Client, error) {
	cfg, err := config.Load(configPaths()...)
	if err != nil {
		return nil, err
	}
	r, err := cfg.Resolve(flagProfile, config.Overrides{
		MasterURL: flagURL,
		User:      flagUser,
		Eauth:     flagEauth,
		Insecure:  flagInsecure,
		CACert:    flagCACert,
		Timeout:   timeoutStr(),
	})
	if err != nil {
		return nil, err
	}
	opts := []papurika.Option{
		papurika.WithCredentials(r.User, r.Secret),
		papurika.WithEauth(r.Eauth),
		papurika.WithInsecureSkipVerify(!r.VerifySSL),
		papurika.WithTimeout(r.Timeout),
	}
	if r.CACert != "" {
		opts = append(opts, papurika.WithCACert(r.CACert))
	}
	if lg := buildLogger(); lg != nil {
		opts = append(opts, papurika.WithLogger(lg))
	}
	return papurika.New(r.MasterURL, opts...)
}

func configPaths() []string {
	if flagConfig != "" {
		return []string{flagConfig}
	}
	return nil // config.Load falls back to XDG defaults
}

func timeoutStr() string {
	if flagTimeout > 0 {
		return flagTimeout.String()
	}
	return ""
}

func runOpts() []papurika.RunOption {
	var o []papurika.RunOption
	if flagTimeout > 0 {
		o = append(o, papurika.WithDeadline(flagTimeout))
	}
	if flagSaltTO > 0 {
		o = append(o, papurika.WithSaltTimeout(flagSaltTO))
	}
	if flagGatherTO > 0 {
		o = append(o, papurika.WithGatherTimeout(flagGatherTO))
	}
	if fn := progressFunc(); fn != nil {
		o = append(o, papurika.WithOnUpdate(fn))
	}
	return o
}

// buildLogger maps -v repetition to an slog level on stderr. Returns nil at -v0
// (the Client then defaults to a discard logger).
func buildLogger() *slog.Logger {
	if flagVerbose <= 0 {
		return nil
	}
	lvl := slog.LevelWarn
	switch {
	case flagVerbose == 2:
		lvl = slog.LevelInfo
	case flagVerbose >= 3:
		lvl = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// progressFunc streams per-minion progress to stderr as returns arrive — only
// when stderr is a TTY (interactive) or verbosity is on, so piped/JSON output
// stays clean.
func progressFunc() func(papurika.Update) {
	if !isTTY(os.Stderr) && flagVerbose == 0 {
		return nil
	}
	return func(u papurika.Update) {
		m := u.Minion
		var s string
		switch {
		case m.Status == papurika.StatusNoResponse:
			s = "no response"
		case m.OK():
			s = "ok"
		case m.Retcode != nil:
			s = fmt.Sprintf("retcode %d", *m.Retcode)
		default:
			s = m.Status.String()
		}
		fmt.Fprintf(os.Stderr, "  → %s  %s\n", m.ID, s)
	}
}

func policy() failPolicy {
	switch flagFail {
	case "all":
		return failAll
	case "none":
		return failNone
	default:
		return failAny
	}
}

// resolveOutput picks the output mode, defaulting to json when stdout is not a TTY.
func resolveOutput() outputMode {
	switch flagOutput {
	case "json":
		return outputJSON
	case "yaml":
		return outputYAML
	case "raw":
		return outputRaw
	case "pretty":
		return outputPretty
	default:
		if isTTY(os.Stdout) {
			return outputPretty
		}
		return outputJSON
	}
}

func useColor() bool {
	if flagNoColor || os.Getenv("NO_COLOR") != "" {
		return false
	}
	return isTTY(os.Stdout)
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// emit renders a result to stdout and returns an exitErr carrying the exit code.
// It also prints a recovery banner to stderr on interruption/timeout so the jid
// is never lost.
func emit(res *papurika.Result, err error) error {
	// Pure failure (auth/transport/malformed with nothing gathered): print the
	// error to stderr and emit nothing to stdout — don't render a misleading
	// empty "ok" result.
	if err != nil && !resultHasContent(res) {
		fmt.Fprintf(os.Stderr, "papurika: %v\n", err)
		return &exitErr{code: exitCode(res, err, policy(), flagAllowNR)}
	}
	if res != nil {
		_ = render(os.Stdout, res, resolveOutput(), useColor())
	}
	if err == nil && isNoMatch(res) {
		fmt.Fprintln(os.Stderr, "papurika: no minions matched the target")
	}
	// Partial results (interrupt/deadline) that DID gather something: keep the jid
	// recoverable.
	if err != nil {
		if jids := res.JIDs(); len(jids) > 0 {
			fmt.Fprintf(os.Stderr, "papurika: interrupted/incomplete — jid(s): %v\n", jids)
			fmt.Fprintf(os.Stderr, "papurika: resume with: papurika watch %s\n", joinJIDs(jids))
		}
	}
	return &exitErr{code: exitCode(res, err, policy(), flagAllowNR)}
}

// resultHasContent reports whether a result carries anything worth rendering
// (a jid, a targeted set, per-minion results, or a runner/wheel value).
func resultHasContent(res *papurika.Result) bool {
	if res == nil {
		return false
	}
	for _, c := range res.Commands {
		if c.JID != "" || len(c.Targeted) > 0 || len(c.Minions) > 0 || !c.Value.IsZero() {
			return true
		}
	}
	return false
}

func joinJIDs(jids []string) string {
	out := ""
	for i, j := range jids {
		if i > 0 {
			out += " "
		}
		out += j
	}
	return out
}

// exitErr carries a process exit code out through cobra.
type exitErr struct{ code int }

func (e *exitErr) Error() string { return fmt.Sprintf("exit %d", e.code) }
