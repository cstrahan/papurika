package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cstrahan/papurika"
	"github.com/spf13/cobra"
)

// targeting holds the selector flags shared by target-taking commands.
type targeting struct {
	list, pcre, grain, grainPCRE, pillar, pillarPCRE, rng, compound, nodegroup bool
	batch                                                                      string
	batchMode                                                                  string
	clientBatch                                                                bool // deprecated alias for --batch-mode=client
	batchConcurrency                                                           int
}

const (
	batchModeClient = "client"
	batchModeServer = "server"
)

// serverBatch reports whether batching should go through salt's own
// local_batch rather than papurika's client-side fan-out.
func (t *targeting) serverBatch() bool { return t.batchMode == batchModeServer }

// batchOpts returns the client-side-batch RunOptions implied by the flags —
// empty unless a --batch size was given and the mode is client-side.
func (t *targeting) batchOpts() []papurika.RunOption {
	if t.batch == "" || t.serverBatch() {
		return nil
	}
	opts := []papurika.RunOption{papurika.WithClientBatch(t.batch)}
	if t.batchConcurrency > 0 {
		opts = append(opts, papurika.WithBatchConcurrency(t.batchConcurrency))
	}
	return opts
}

// registerSelectors adds only the tgt_type flags. Low-level (one client = one
// subcommand) commands take these but NOT the batch-mode machinery: they map
// straight onto a salt client with no smart defaults, which is the tiering
// contract in design/cli.md §A.
func (t *targeting) registerSelectors(c *cobra.Command) {
	f := c.Flags()
	f.BoolVarP(&t.list, "list", "L", false, "target type: comma-separated list")
	f.BoolVarP(&t.pcre, "pcre", "E", false, "target type: minion-id PCRE")
	f.BoolVarP(&t.grain, "grain", "G", false, "target type: grain glob (key:val)")
	f.BoolVar(&t.grainPCRE, "grain-pcre", false, "target type: grain PCRE")
	f.BoolVarP(&t.pillar, "pillar", "I", false, "target type: pillar glob")
	f.BoolVar(&t.pillarPCRE, "pillar-pcre", false, "target type: pillar PCRE")
	f.BoolVarP(&t.rng, "range", "R", false, "target type: range expression")
	f.BoolVarP(&t.compound, "compound", "C", false, "target type: compound")
	f.BoolVarP(&t.nodegroup, "nodegroup", "N", false, "target type: nodegroup")
}

// register adds the selectors plus the batching flags, for the high-level verbs.
func (t *targeting) register(c *cobra.Command) {
	t.registerSelectors(c)
	f := c.Flags()
	f.StringVar(&t.batch, "batch", "", "batch size (e.g. 10 or 25%)")
	f.StringVar(&t.batchMode, "batch-mode", batchModeClient,
		"how to batch: client (chunk + fan out; keeps jids and live progress) or server (salt local_batch; no jid, no progress)")
	f.IntVar(&t.batchConcurrency, "batch-concurrency", 0, "max client-side batch chunks gathered at once (default 8)")
	f.BoolVar(&t.clientBatch, "client-batch", false, "deprecated: client-side batching is now the default; use --batch-mode server to opt out")
	_ = f.MarkHidden("client-batch")
}

// resolveBatch validates the batching flags and warns about the combinations
// that quietly do less than they look like they do.
//
// Server-side local_batch gets the loud warning because it is the one mode that
// cannot satisfy the never-lose-a-jid rule: salt-api buffers the whole response
// and returns no jid at all (verified — async.md §F). It used to be the default,
// which meant the single most pepper-like invocation anyone could type,
// `--batch 10`, silently threw the jid away.
func (t *targeting) resolveBatch() error {
	if t.clientBatch {
		fmt.Fprintln(os.Stderr, "papurika: --client-batch is deprecated and now redundant; client-side batching is the default")
		t.batchMode = batchModeClient
	}
	switch t.batchMode {
	case batchModeClient, batchModeServer:
	default:
		return usageErr(fmt.Sprintf("--batch-mode must be %q or %q, got %q", batchModeClient, batchModeServer, t.batchMode))
	}
	if t.batch == "" {
		return nil
	}
	if t.serverBatch() {
		fmt.Fprintln(os.Stderr, "papurika: --batch-mode server returns no jid and no live progress (salt-api buffers the whole local_batch response); the run is not recoverable if this process dies")
		if t.batchConcurrency > 0 {
			fmt.Fprintln(os.Stderr, "papurika: --batch-concurrency is ignored under --batch-mode server; the batch size is the concurrency there")
		}
	}
	return nil
}

// resolve validates every targeting/batching flag and returns the tgt_type.
// Commands call this once, instead of tgtType, so no path can skip the
// batch-mode checks.
func (t *targeting) resolve() (papurika.TgtType, error) {
	tt, err := t.tgtType()
	if err != nil {
		return "", err
	}
	if err := t.resolveBatch(); err != nil {
		return "", err
	}
	return tt, nil
}

func (t *targeting) tgtType() (papurika.TgtType, error) {
	set := map[papurika.TgtType]bool{}
	if t.list {
		set[papurika.TgtList] = true
	}
	if t.pcre {
		set[papurika.TgtPCRE] = true
	}
	if t.grain {
		set[papurika.TgtGrain] = true
	}
	if t.grainPCRE {
		set[papurika.TgtGrainPCRE] = true
	}
	if t.pillar {
		set[papurika.TgtPillar] = true
	}
	if t.pillarPCRE {
		set[papurika.TgtPillarPCRE] = true
	}
	if t.rng {
		set[papurika.TgtRange] = true
	}
	if t.compound {
		set[papurika.TgtCompound] = true
	}
	if t.nodegroup {
		set[papurika.TgtNodegroup] = true
	}
	if len(set) > 1 {
		return "", usageErr("at most one targeting selector may be given")
	}
	for tt := range set {
		return tt, nil
	}
	return papurika.TgtGlob, nil
}

// parseArgs splits trailing "k=v" tokens into kwargs and the rest into positional
// args. salt's CLI convention.
func parseArgs(args []string) (pos []any, kw map[string]any) {
	for _, a := range args {
		if k, v, ok := strings.Cut(a, "="); ok && isIdent(k) {
			if kw == nil {
				kw = map[string]any{}
			}
			var jv any
			if json.Unmarshal([]byte(v), &jv) == nil {
				kw[k] = jv
			} else {
				kw[k] = v
			}
			continue
		}
		pos = append(pos, a)
	}
	return pos, kw
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func newRunCmd() *cobra.Command {
	tg := &targeting{}
	c := &cobra.Command{
		Use:   "run <tgt> <fun> [args...]",
		Short: "Run an execution function (async; renders on completion)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := specFromArgs(tg, args)
			if err != nil {
				return err
			}
			cl, err := buildClient()
			if err != nil {
				return err
			}
			extra := tg.batchOpts()
			return runSpec(cmd, cl, spec, extra...)
		},
	}
	tg.register(c)
	return c
}

func newSubmitCmd() *cobra.Command {
	tg := &targeting{}
	c := &cobra.Command{
		Use:   "submit <tgt> <fun> [args...]",
		Short: "Fire-and-forget: submit async and print the jid(s)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := specFromArgs(tg, args)
			if err != nil {
				return err
			}
			cl, err := buildClient()
			if err != nil {
				return err
			}
			opts := tg.batchOpts()
			sub, err := cl.Submit(cmd.Context(), spec, opts...)
			if err != nil {
				fmt.Fprintf(os.Stderr, "papurika: %v\n", err)
				return &exitErr{code: exitUnavailable}
			}
			for _, j := range sub.JIDs() {
				fmt.Fprintln(os.Stdout, j)
			}
			return &exitErr{code: exitOK}
		},
	}
	tg.register(c)
	return c
}

func newWatchCmd() *cobra.Command {
	var forState string
	c := &cobra.Command{
		Use:   "watch <jid>...",
		Short: "Block until jid(s) reach a target state, then render",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			want := papurika.WatchAllReturned
			if forState == "job-done" {
				want = papurika.WatchJobDone
			}
			cl, err := buildClient()
			if err != nil {
				return err
			}
			out, werr := cl.Watch(cmd.Context(), args, want, runOpts()...)
			// Merge per-jid results into one for rendering.
			merged := &papurika.Result{}
			for _, jid := range args {
				if r := out[jid]; r != nil {
					merged.Commands = append(merged.Commands, r.Commands...)
				}
			}
			return emit(merged, werr)
		},
	}
	c.Flags().StringVar(&forState, "for", "all-returned", "target state: all-returned|job-done")
	return c
}

func newPingCmd() *cobra.Command {
	tg := &targeting{}
	c := &cobra.Command{
		Use:   "ping <tgt>",
		Short: "test.ping with up/down classification",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tt, err := tg.resolve()
			if err != nil {
				return err
			}
			cl, err := buildClient()
			if err != nil {
				return err
			}
			// Ping is Run with test.ping; route through runSpec for ndjson/progress.
			return runSpec(cmd, cl, papurika.JobSpec{Target: args[0], TgtType: tt, Fun: "test.ping"})
		},
	}
	tg.register(c)
	return c
}

func newRunnerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "runner <fun> [k=v...]",
		Short: "Run a master-side runner",
		Long: "Run a runner module on the master (client=runner).\n\n" +
			"A runner runs on the MASTER, not on minions, so its output is a single " +
			"value — not a per-minion map. Under --output json|yaml that value lands " +
			"at commands[0].value, where an execution function's per-minion results " +
			"would be at commands[0].minions[]. A consumer handling both shapes needs " +
			"both paths; summary.targeted is 0 for a runner, which is the cheapest way " +
			"to tell them apart.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, kw := parseArgs(args[1:])
			rc := papurika.Runner(args[0])
			for k, v := range kw {
				rc = rc.WithKwarg(k, v)
			}
			cl, err := buildClient()
			if err != nil {
				return err
			}
			res, rerr := cl.Exec(cmd.Context(), rc)
			return emit(res, rerr)
		},
	}
}

func newWheelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "wheel <fun> [k=v...]",
		Short: "Run a master-side wheel function (key/config management)",
		Long: "Run a wheel module on the master (client=wheel).\n\n" +
			"Like a runner, this executes on the MASTER, so under --output json|yaml " +
			"the payload is a single value at commands[0].value — not a per-minion " +
			"map at commands[0].minions[]. summary.targeted is 0.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, kw := parseArgs(args[1:])
			wc := papurika.Wheel(args[0])
			for k, v := range kw {
				wc = wc.WithKwarg(k, v)
			}
			cl, err := buildClient()
			if err != nil {
				return err
			}
			res, werr := cl.Exec(cmd.Context(), wc)
			return emit(res, werr)
		},
	}
}

func newJobCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "job <jid>",
		Short: "Show a single job's normalized return",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := buildClient()
			if err != nil {
				return err
			}
			jr, jerr := cl.Job(cmd.Context(), args[0])
			if jerr != nil {
				return emit(nil, jerr)
			}
			return emit(&papurika.Result{Commands: []papurika.CommandResult{jr.Result}}, nil)
		},
	}
}

func newJobsCmd() *cobra.Command {
	var active bool
	var limit int
	since := &sinceFlag{}
	c := &cobra.Command{
		Use:   "jobs",
		Short: "List jobs from the master job cache (--active: only those running now)",
		Long: "List jobs the master has recorded (runner jobs.list_jobs).\n\n" +
			"With --active, list only jobs executing right now (runner jobs.active), " +
			"including which minions are still working and under what pid. The job " +
			"cache holds finished jobs too, so \"what is running\" is only answerable " +
			"with --active.\n\n" +
			"--active is a live query, not a cache read: it asks every minion what " +
			"it is running. That is fast when every minion answers (~0.3s), but any " +
			"minion that does NOT answer costs the master's full timeout + " +
			"gather_job_timeout — 15s by default, however few are missing. A fleet " +
			"with dead hosts in it pays that on every call.\n\n" +
			"It runs the runner asynchronously and collects the result off the event " +
			"bus, so that wait does not hold a salt-api worker thread. --timeout " +
			"bounds how long papurika waits, not the runner, which keeps going on the " +
			"master and stays reachable by its jid.\n\n" +
			"By default the whole cache is listed. --limit N (newest first) and " +
			"--since narrow it, and whatever they drop is reported on stderr, " +
			"never silently. Both filter client-side, because GET /jobs takes no " +
			"query parameters and the jobs.list_jobs runner filters only after " +
			"reading the whole cache anyway — so they bound what you read, not what " +
			"the master does. On a master with a large job cache the read itself can " +
			"take minutes however you filter.\n\n" +
			"--since accepts either a duration back from now (2h, 90m) or an " +
			"absolute ISO 8601 timestamp (2026-10-23, 2026-10-23T14:30, " +
			"2026-10-23T14:30:00Z, 2026-10-23T14:30:00+02:00). A timestamp with no " +
			"zone is read as UTC, because that is what salt mints jids in and what " +
			"the StartTime column shows — so a timestamp copied out of this listing " +
			"means what it looks like. The stderr report echoes the resolved UTC " +
			"cutoff either way.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := buildClient()
			if err != nil {
				return err
			}
			if active {
				ctx := cmd.Context()
				if flagTimeout > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, flagTimeout)
					defer cancel()
				}
				// Say so before blocking: a silent 15s read looks like a hang.
				fmt.Fprintln(os.Stderr, "papurika: asking every minion what it is running (jobs.active); any minion that does not answer costs ~15s")
				jobs, jerr := cl.ActiveJobs(ctx)
				if jerr != nil {
					// Interrupting us does not stop the runner — it keeps going
					// on the master. Hand back the jid so the work is still
					// reachable rather than implying it was lost.
					var je *papurika.JobError
					if errors.As(jerr, &je) && je.JID != "" {
						// A runner lands in the job cache only when it
						// finishes, so `job <jid>` answers "not found" until
						// then. Don't send anyone to a command that cannot
						// work yet.
						fmt.Fprintf(os.Stderr,
							"papurika: stopped waiting; the runner is still going on the master. "+
								"Once it finishes: `papurika job %s` (until then that reports not-found)\n",
							je.JID)
					}
					return emit(nil, jerr)
				}
				return writeFilteredJobs(cmd, activeJobRows(jobs), since, limit)
			}
			jobs, jerr := cl.Jobs(cmd.Context())
			if jerr != nil {
				return emit(nil, jerr)
			}
			return writeFilteredJobs(cmd, cachedJobRows(jobs), since, limit)
		},
	}
	c.Flags().BoolVar(&active, "active", false, "only jobs running right now (runner jobs.active)")
	c.Flags().IntVar(&limit, "limit", noJobLimit, "show at most N jobs, newest first (default: all)")
	c.Flags().Var(since, "since", "only jobs started at or after this point: a duration back from now (2h, 90m) or an ISO 8601 timestamp (2026-10-23, 2026-10-23T14:30:00Z); a timestamp with no zone is read as UTC")
	return c
}

// noJobLimit is the default for `papurika jobs --limit`: show everything.
//
// A real master's job cache is tens of thousands of rows, so a cap is often
// what you want — but it has to be asked for. Defaulting to one would mean the
// command silently answers a narrower question than it was asked, and a
// listing that omits jobs is how an operator concludes a job never ran. Reach
// for --limit or --since when you want less.
const noJobLimit = 0

// writeFilteredJobs applies --since/--limit, reports on stderr whatever they
// dropped, and renders the rest. Nothing is ever dropped silently: a listing
// that quietly omits jobs reads as "there are no more", which is how an
// operator concludes a job never ran.
func writeFilteredJobs(cmd *cobra.Command, rows []jobRow, since *sinceFlag, limit int) error {
	total := len(rows)
	kept, agedOut, truncated := filterJobRows(rows, since.cutoff, limit)
	if agedOut > 0 {
		// Name the resolved instant, not just the flag: "--since 2026-10-23"
		// and "--since 36h" both land somewhere specific, and the operator
		// should not have to work out where.
		fmt.Fprintf(os.Stderr, "papurika: %d of %d jobs started before %s not shown (--since %s)\n",
			agedOut, total, since.cutoff.UTC().Format(time.RFC3339), since.raw)
	}
	if truncated > 0 {
		fmt.Fprintf(os.Stderr, "papurika: showing the newest %d jobs; %d more not shown (raise or drop --limit)\n", limit, truncated)
	}
	if err := writeJobListing(cmd.OutOrStdout(), kept); err != nil {
		return err
	}
	return &exitErr{code: exitOK}
}

func newKillCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "kill <jid>",
		Short: "Kill a running job on its minions (saltutil.kill_job)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := buildClient()
			if err != nil {
				return err
			}
			res, kerr := cl.Kill(cmd.Context(), args[0])
			return emit(res, kerr)
		},
	}
}

func newLowstateCmd() *cobra.Command {
	var file string
	c := &cobra.Command{
		Use:   "lowstate -f <cmds.json>",
		Short: "POST a raw lowstate array (verbatim) to /",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if file == "" {
				return usageErr("--file is required")
			}
			b, err := os.ReadFile(file)
			if err != nil {
				fmt.Fprintf(os.Stderr, "papurika: %v\n", err)
				return &exitErr{code: exitData}
			}
			var raw []map[string]any
			if err := json.Unmarshal(b, &raw); err != nil {
				fmt.Fprintf(os.Stderr, "papurika: invalid lowstate JSON: %v\n", err)
				return &exitErr{code: exitData}
			}
			cmds := make([]papurika.Command, len(raw))
			for i, m := range raw {
				cmds[i] = commandFromMap(m)
			}
			cl, err := buildClient()
			if err != nil {
				return err
			}
			res, rerr := cl.Exec(cmd.Context(), cmds...)
			return emit(res, rerr)
		},
	}
	c.Flags().StringVarP(&file, "file", "f", "", "path to a JSON lowstate array")
	return c
}

// specFromArgs builds a JobSpec from positional <tgt> <fun> [args...] + flags.
func specFromArgs(tg *targeting, args []string) (papurika.JobSpec, error) {
	tt, err := tg.resolve()
	if err != nil {
		return papurika.JobSpec{}, err
	}
	pos, kw := parseArgs(args[2:])
	return papurika.JobSpec{
		Target: args[0], TgtType: tt, Fun: args[1],
		Arg: pos, Kwarg: kw, Batch: batchIfServer(tg),
	}, nil
}

// batchIfServer returns the lowstate `batch` field, which is only set when the
// user explicitly asked for salt-side batching. Client-side batching (the
// default) is driven by WithClientBatch instead and must leave this empty, or
// salt would batch the chunks again.
func batchIfServer(tg *targeting) string {
	if tg.serverBatch() {
		return tg.batch
	}
	return ""
}

// commandFromMap converts a raw lowstate dict into a Command (best-effort; unknown
// fields go to Extra).
func commandFromMap(m map[string]any) papurika.Command {
	c := papurika.Command{Extra: map[string]any{}}
	for k, v := range m {
		switch k {
		case "client":
			if s, ok := v.(string); ok {
				c.Client = papurika.ClientType(s)
			}
		case "fun":
			c.Fun, _ = v.(string)
		case "tgt":
			c.Tgt, _ = v.(string)
		case "tgt_type", "expr_form":
			if s, ok := v.(string); ok {
				c.TgtType = papurika.TgtType(s)
			}
		case "arg":
			if a, ok := v.([]any); ok {
				c.Arg = a
			}
		case "kwarg":
			if mm, ok := v.(map[string]any); ok {
				c.Kwarg = mm
			}
		case "batch":
			c.Batch, _ = v.(string)
		default:
			c.Extra[k] = v
		}
	}
	return c
}

// newStateCmd is `run` specialized to state.*: with sls names it runs
// state.apply <sls...>, with none it runs state.highstate.
func newStateCmd() *cobra.Command {
	tg := &targeting{}
	c := &cobra.Command{
		Use:   "state <tgt> [sls...]",
		Short: "Apply state (state.apply <sls...> / state.highstate)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tt, err := tg.resolve()
			if err != nil {
				return err
			}
			pos, kw := parseArgs(args[1:])
			fun := "state.highstate"
			var arg []any
			if len(pos) > 0 {
				fun, arg = "state.apply", pos
			}
			spec := papurika.JobSpec{Target: args[0], TgtType: tt, Fun: fun, Arg: arg, Kwarg: kw, Batch: batchIfServer(tg)}
			cl, err := buildClient()
			if err != nil {
				return err
			}
			extra := tg.batchOpts()
			return runSpec(cmd, cl, spec, extra...)
		},
	}
	tg.register(c)
	return c
}

// newKeyCmd groups the wheel key.* operations.
func newKeyCmd() *cobra.Command {
	key := &cobra.Command{Use: "key", Short: "Minion key management (wheel key.*)"}
	key.AddCommand(
		keySub("list", "List all keys (accepted/pending/rejected/denied)", "key.list_all", false),
		keySub("finger", "Show key fingerprint(s)", "key.finger", true),
		keySub("accept", "Accept pending minion key(s)", "key.accept", true),
		keySub("reject", "Reject minion key(s)", "key.reject", true),
		keySub("delete", "Delete minion key(s)", "key.delete", true),
	)
	return key
}

func keySub(use, short, fun string, needsMatch bool) *cobra.Command {
	args := cobra.NoArgs
	useStr := use
	if needsMatch {
		args = cobra.ExactArgs(1)
		useStr = use + " <match>"
	}
	return &cobra.Command{
		Use:   useStr,
		Short: short,
		Args:  args,
		RunE: func(cmd *cobra.Command, a []string) error {
			wc := papurika.Wheel(fun)
			if needsMatch {
				wc = wc.WithKwarg("match", a[0])
			}
			cl, err := buildClient()
			if err != nil {
				return err
			}
			res, werr := cl.Exec(cmd.Context(), wc)
			return emit(res, werr)
		},
	}
}

// newEventsCmd tails the salt event bus, printing one JSON object per event.
func newEventsCmd() *cobra.Command {
	var filter string
	c := &cobra.Command{
		Use:   "events",
		Short: "Tail the salt event bus (NDJSON to stdout)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := buildClient()
			if err != nil {
				return err
			}
			stream, err := cl.Events(cmd.Context())
			if err != nil {
				fmt.Fprintf(os.Stderr, "papurika: %v\n", err)
				return &exitErr{code: exitUnavailable}
			}
			defer stream.Close()
			ch, unsub := stream.Subscribe(filter)
			defer unsub()
			enc := json.NewEncoder(os.Stdout)
			for {
				select {
				case <-cmd.Context().Done():
					return &exitErr{code: exitInterrupted}
				case ev, ok := <-ch:
					if !ok {
						return &exitErr{code: exitOK}
					}
					_ = enc.Encode(struct {
						Tag  string          `json:"tag"`
						Data json.RawMessage `json:"data"`
					}{ev.Tag, ev.Data})
				}
			}
		},
	}
	c.Flags().StringVar(&filter, "filter", "", "only events whose tag starts with this prefix")
	return c
}

// --- low-level: one client = one subcommand ---

// clientCmdOpts are the extra lowstate knobs a particular low-level client
// accepts. They are per-client on purpose: the low-level tier mirrors exactly
// what salt-api takes for that client and nothing else.
type clientCmdOpts struct {
	subset bool // --subset N   (local_subset)
	batch  bool // --batch N|N% (local_batch)
}

// newClientCmd builds a to-the-metal command for a target-taking client
// (local/local_async/local_batch/local_subset/ssh), via a synchronous Exec.
//
// These take the targeting SELECTORS only — no --batch-mode, no
// --batch-concurrency. Client-side batching is an engine behavior the
// high-level verbs compose; down here `local-batch --batch N` means salt's
// local_batch and nothing else, which is what makes the tier predictable.
func newClientCmd(use, short string, client papurika.ClientType, extra clientCmdOpts) *cobra.Command {
	tg := &targeting{}
	var subset int
	var batch string
	c := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			tt, err := tg.tgtType()
			if err != nil {
				return err
			}
			pos, kw := parseArgs(args[2:])
			lc := papurika.Command{Client: client, Tgt: args[0], TgtType: tt, Fun: args[1], Arg: pos, Kwarg: kw, Batch: batch, Subset: subset}
			cl, err := buildClient()
			if err != nil {
				return err
			}
			res, rerr := cl.Exec(cmd.Context(), lc)
			return emit(res, rerr)
		},
	}
	tg.registerSelectors(c)
	if extra.subset {
		c.Flags().IntVar(&subset, "subset", 0, "number of minions in the subset")
	}
	if extra.batch {
		c.Flags().StringVar(&batch, "batch", "", "batch size (e.g. 10 or 25%); server-side local_batch, so no jid and no progress")
	}
	return c
}

// newMasterCmd builds a to-the-metal command for a no-target client
// (runner_async/wheel_async), via a synchronous Exec.
func newMasterCmd(use, short string, client papurika.ClientType) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, kw := parseArgs(args[1:])
			mc := papurika.Command{Client: client, Fun: args[0], Kwarg: kw}
			cl, err := buildClient()
			if err != nil {
				return err
			}
			res, rerr := cl.Exec(cmd.Context(), mc)
			return emit(res, rerr)
		},
	}
}

func usageErr(msg string) error { return &usageError{msg} }

type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }
