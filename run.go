package papurika

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Poll cadence for the /jobs reconciliation safety net. Package vars so tests
// can shrink them; /jobs polling is not free (spawns a runner per call), so the
// first poll is lazy and the interval backs off.
var (
	initialPollDelay = 2 * time.Second
	pollBackoffMax   = 10 * time.Second
)

// Source identifies where a gathered minion result came from.
type Source int

const (
	SourceEvent   Source = iota // arrived via /events SSE
	SourcePoll                  // backfilled via /jobs polling
	SourceMissing               // master flagged the minion absent (salt/job/<jid>/new)
	SourceFindJob               // settled no-response after a saltutil.find_job liveness check
)

// Update is a streamed per-minion progress event (delivered to OnUpdate).
type Update struct {
	JID    string
	Minion MinionResult
	Source Source
}

// Job is the identity of a submitted async job.
type Job struct {
	JID      string
	Targeted []string
	Fun      string
	Client   ClientType
}

// Submission is the result of a fire-and-forget Submit: the jid(s) only.
type Submission struct {
	Jobs []Job
}

// JIDs returns the submitted jids.
func (s *Submission) JIDs() []string {
	out := make([]string, 0, len(s.Jobs))
	for _, j := range s.Jobs {
		out = append(out, j.JID)
	}
	return out
}

type runConfig struct {
	deadline         time.Duration
	saltTimeout      time.Duration
	gatherTimeout    time.Duration
	onUpdate         func(Update)
	onSubmit         func(Job)
	killOnCancel     bool
	clientBatch      string // non-empty => client-side batching at this size
	batchConcurrency int    // max client-batch chunks gathered concurrently
}

// defaultRunConfig imposes no timeouts. There is no default hard deadline
// (WithDeadline) and no default settle window (WithSaltTimeout/WithGatherTimeout)
// — a caller opts into any wall-clock bound explicitly. When the settle knobs are
// unset, gather() uses an internal liveness-check cadence (see the settleTimer
// case): it re-probes stragglers with saltutil.find_job rather than giving up, so
// nothing here silently bounds a healthy run.
func defaultRunConfig() runConfig {
	return runConfig{batchConcurrency: 8}
}

// RunOption configures Run/Submit/Watch.
type RunOption func(*runConfig)

// WithDeadline bounds the whole operation in wall-clock time (a convenience over
// setting a deadline on ctx). The job keeps running on the master past it.
func WithDeadline(d time.Duration) RunOption { return func(c *runConfig) { c.deadline = d } }

// WithSaltTimeout / WithGatherTimeout size the client-side settle window — the
// quiet period after which gather runs its saltutil.find_job liveness re-check
// (not a give-up timeout; see gather). Neither is the authoritative bound — that
// is the context / WithDeadline.
func WithSaltTimeout(d time.Duration) RunOption   { return func(c *runConfig) { c.saltTimeout = d } }
func WithGatherTimeout(d time.Duration) RunOption { return func(c *runConfig) { c.gatherTimeout = d } }

// OnUpdate registers a per-minion progress callback.
func WithOnUpdate(fn func(Update)) RunOption { return func(c *runConfig) { c.onUpdate = fn } }

// WithOnSubmit registers a callback invoked once per async job the moment it is
// submitted (jid + targeted set known), before gathering begins — the hook that
// lets a streaming consumer record the jid before any return arrives, so an
// interrupt can never lose it. For client-side batching it fires once per chunk
// jid (possibly concurrently — the callback must be safe for concurrent use).
func WithOnSubmit(fn func(Job)) RunOption { return func(c *runConfig) { c.onSubmit = fn } }

// WithKillOnCancel sends saltutil.kill_job when ctx is canceled (default: leave
// the job running, jid recoverable).
func WithKillOnCancel(v bool) RunOption { return func(c *runConfig) { c.killOnCancel = v } }

// WithClientBatch switches to client-side batching at the given size ("10",
// "25%"): resolve the target, chunk it, fan out async per chunk, merge.
func WithClientBatch(size string) RunOption { return func(c *runConfig) { c.clientBatch = size } }

// WithBatchConcurrency caps how many client-side-batch chunks are gathered at
// once (default 8; n < 1 is treated as 1). It applies to WithClientBatch only:
// server-side local_batch has no separate concurrency knob — there the batch
// size IS the concurrency (the master serializes the waves), so tune that with
// the batch size instead. Lower it to be gentle on the master (e.g. GPG-pillar
// render load); raise it to drain a large fleet faster.
func WithBatchConcurrency(n int) RunOption { return func(c *runConfig) { c.batchConcurrency = n } }

func buildRunConfig(opts []RunOption) runConfig {
	c := defaultRunConfig()
	for _, fn := range opts {
		fn(&c)
	}
	return c
}

// Submit fires an async job (or client-side-batched jobs) and returns
// immediately with the jid(s). No gathering is performed.
func (c *Client) Submit(ctx context.Context, spec JobSpec, opts ...RunOption) (*Submission, error) {
	cfg := buildRunConfig(opts)
	if cfg.clientBatch != "" {
		return c.submitClientBatch(ctx, spec, cfg)
	}
	job, err := c.submitOne(ctx, spec)
	if errors.Is(err, errNoMatch) {
		return &Submission{}, nil // nothing matched; no jids
	}
	if err != nil {
		return nil, err
	}
	return &Submission{Jobs: []Job{job}}, nil
}

// errNoMatch is an internal sentinel: the async submit matched no minion
// (salt returns {"return":[{}]} with no jid). It is not an error to the caller —
// Run/Submit translate it to an empty result.
var errNoMatch = errors.New("papurika: target matched no minion")

// submitOne POSTs a single local_async job and returns its identity.
//
// It mints the jid itself rather than letting the master do it, which is what
// makes a lost response recoverable: when the POST fails ambiguously we already
// know the jid to ask about. See reconcileSubmit.
func (c *Client) submitOne(ctx context.Context, spec JobSpec) (Job, error) {
	cmd := spec.command(ClientLocalAsync)
	if cmd.JID == "" {
		cmd.JID = newJID()
	}
	job, err := c.sendJob(ctx, spec, cmd)
	if err != nil && !errors.Is(err, errNoMatch) {
		return c.reconcileSubmit(ctx, spec, cmd, err)
	}
	return job, err
}

// sendJob POSTs the job and reads its identity out of the response.
func (c *Client) sendJob(ctx context.Context, spec JobSpec, cmd Command) (Job, error) {
	res, err := c.Exec(ctx, cmd)
	if err != nil {
		return Job{}, err
	}
	cr := res.Commands[0]
	if cr.JID == "" {
		// No jid + no targeted minions => the target matched nothing.
		if len(cr.Targeted) == 0 {
			return Job{}, errNoMatch
		}
		return Job{}, apiError(ErrMalformed, 0, "", string(cr.Raw()), ModuleUnknown)
	}
	return Job{JID: cr.JID, Targeted: cr.Targeted, Fun: spec.Fun, Client: ClientLocalAsync}, nil
}

// reconcileSubmit decides what a failed submit means, now that we know the jid
// it would have used.
//
// The master writes the job-cache entry (_prep_jid, then save_load inside
// _prep_pub) strictly BEFORE it publishes to minions (_send_pub) — verified
// against salt's source and live, see cherrypy_learnings.md. So the question
// "did /jobs/<jid> resolve?" answers "did anything execute?" in the only
// direction that matters:
//
//   - not found  => save_load never ran => _send_pub never ran => nothing
//     executed, and re-sending is safe no matter what the command does.
//   - found      => the job exists and may be running right now. Adopt it and
//     gather; never re-send, or we double-execute.
//   - can't tell => the master is unreachable enough that we cannot ask. Only
//     here does Command.Idempotent get a say.
//
// The failure that is NOT ambiguous — one that proves the request never left —
// was already retried by the transport, so anything arriving here is either
// ambiguous or terminal.
func (c *Client) reconcileSubmit(ctx context.Context, spec JobSpec, cmd Command, subErr error) (Job, error) {
	if !isAmbiguousSubmit(subErr) || ctx.Err() != nil {
		return Job{}, subErr
	}
	c.log.Warn("submit failed ambiguously; reconciling against the job cache",
		"jid", cmd.JID, "fun", spec.Fun, "err", subErr)

	jr, err := c.Job(ctx, cmd.JID)
	switch {
	case err == nil && jr != nil:
		// The job exists. Adopt it rather than submitting again.
		targeted := jr.Result.Targeted
		c.log.Info("submit landed after all; adopting the existing job", "jid", cmd.JID)
		return Job{JID: cmd.JID, Targeted: targeted, Fun: spec.Fun, Client: ClientLocalAsync}, nil

	case errors.Is(err, ErrJobNotFound):
		// Proven never published. Re-send under the same jid: it stays
		// recoverable, and a stale duplicate would be dropped by the minion.
		c.log.Info("submit never reached salt; re-sending", "jid", cmd.JID)
		return c.sendJob(ctx, spec, cmd)

	default:
		// We could not ask. Without evidence, only an explicit declaration that
		// repeating is harmless justifies sending again.
		if !cmd.Idempotent && !c.retryNonIdempotent {
			c.log.Warn("cannot determine whether the job was submitted; not re-sending",
				"jid", cmd.JID, "reconcile_err", err)
			return Job{}, fmt.Errorf("%w (jid %s may or may not be running; check `papurika job %s`)",
				subErr, cmd.JID, cmd.JID)
		}
		c.log.Warn("cannot reconcile, but the command is declared idempotent; re-sending",
			"jid", cmd.JID, "reconcile_err", err)
		return c.sendJob(ctx, spec, cmd)
	}
}

// isAmbiguousSubmit reports whether err leaves it unknown whether the master
// acted on the request. Auth, authz and malformed-request failures are
// terminal: the request was understood and rejected, so nothing was published
// and there is nothing to reconcile.
func isAmbiguousSubmit(err error) bool {
	switch {
	case errors.Is(err, ErrAuthDenied), errors.Is(err, ErrAuthFailure),
		errors.Is(err, ErrMalformed), errors.Is(err, ErrUnsupported),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return false
	case errors.Is(err, ErrServerError), errors.Is(err, ErrTransport):
		return true
	}
	return false
}

// noMatchResult is the empty-but-valid result for a target that matched nothing.
func noMatchResult(spec JobSpec) *Result {
	return &Result{Commands: []CommandResult{{
		Client: ClientLocal, Fun: spec.Fun, Minions: map[string]MinionResult{}, Complete: true,
	}}}
}

// Run submits and gathers to completion or deadline, returning the normalized
// Result. On ctx cancellation/deadline it returns the PARTIAL result (non-nil)
// alongside ctx.Err(), so the jid(s) and gathered returns are never lost.
func (c *Client) Run(ctx context.Context, spec JobSpec, opts ...RunOption) (res *Result, err error) {
	cfg := buildRunConfig(opts)
	if cfg.deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.deadline)
		defer cancel()
	}

	// Server-side batch is a synchronous POST (one jid, master-orchestrated). It
	// blocks until done; we decode the merged per-chunk result.
	if spec.Batch != "" && cfg.clientBatch == "" {
		cmd := spec.command(ClientLocalBatch)
		r, e := c.Exec(ctx, cmd)
		if e != nil {
			return r, e
		}
		return r, nil
	}

	// Client-side batch: fan out async per chunk through the same gather.
	if cfg.clientBatch != "" {
		return c.runClientBatch(ctx, spec, cfg)
	}

	// Default: one async job, gathered over events + /jobs.
	job, err := c.submitOne(ctx, spec)
	if errors.Is(err, errNoMatch) {
		return noMatchResult(spec), nil
	}
	if err != nil {
		return &Result{}, err
	}
	if cfg.onSubmit != nil {
		cfg.onSubmit(job)
	}
	cr := c.gather(ctx, job, cfg)
	res = &Result{Commands: []CommandResult{cr}}
	if cr.Reason == ReasonCanceled {
		return res, context.Canceled
	}
	if cr.Reason == ReasonDeadline {
		return res, context.DeadlineExceeded
	}
	return res, nil
}

// gather collects returns for one job from the event bus and /jobs polling until
// every targeted minion has returned or ctx fires. It always returns a populated
// CommandResult (never loses the jid).
func (c *Client) gather(ctx context.Context, job Job, cfg runConfig) CommandResult {
	cr := CommandResult{
		Client:   ClientLocal,
		Fun:      job.Fun,
		JID:      job.JID,
		Targeted: append([]string(nil), job.Targeted...),
		Minions:  map[string]MinionResult{},
	}
	for _, id := range job.Targeted {
		cr.Minions[id] = MinionResult{ID: id, Status: StatusPending}
	}

	// Subscribe to this job's events before doing anything else. Best-effort: if
	// the stream can't open, we fall back to polling only.
	var events <-chan Event
	var reconnects <-chan struct{}
	if stream, err := c.Events(ctx); err == nil {
		defer stream.Close()
		events, _ = stream.Subscribe("salt/job/" + job.JID + "/")
		reconnects = stream.Reconnects()
	}

	// The gather window: how long returns stay quiet before we run a liveness
	// check on the stragglers (a saltutil.find_job re-probe — see the settleTimer
	// case). It is NOT a give-up timeout: a minion still running the job keeps
	// being waited on. Settling a minion as a non-responder is a NORMAL completion
	// (exit per policy), distinct from the hard ctx deadline / --timeout (exit
	// 124). Each new return extends the quiet period by gatherTimeout.
	settleWindow := cfg.saltTimeout + cfg.gatherTimeout
	if settleWindow <= 0 {
		settleWindow = 15 * time.Second
	}
	settleTimer := time.NewTimer(settleWindow)
	defer settleTimer.Stop()
	resetSettle := func() {
		if !settleTimer.Stop() {
			select {
			case <-settleTimer.C:
			default:
			}
		}
		q := cfg.gatherTimeout
		if q <= 0 {
			q = 10 * time.Second
		}
		settleTimer.Reset(q)
	}

	terminal := func() int {
		n := 0
		for _, m := range cr.Minions {
			if m.Status == StatusResponded || m.Status == StatusNoResponse {
				n++
			}
		}
		return n
	}
	setMinion := func(m MinionResult, src Source) {
		cur, ok := cr.Minions[m.ID]
		if ok && cur.Status != StatusPending {
			return // already terminal
		}
		if !ok {
			cr.Targeted = append(cr.Targeted, m.ID) // a minion the submit didn't list
		}
		cr.Minions[m.ID] = m
		if m.Status == StatusResponded {
			resetSettle() // a fresh return — extend the quiet window
		}
		if cfg.onUpdate != nil {
			cfg.onUpdate(Update{JID: job.JID, Minion: m, Source: src})
		}
	}
	settle := func() CommandResult {
		for id, m := range cr.Minions {
			if m.Status == StatusPending {
				m.Status = StatusNoResponse
				cr.Minions[id] = m
				if cfg.onUpdate != nil {
					cfg.onUpdate(Update{JID: job.JID, Minion: m, Source: SourcePoll})
				}
			}
		}
		cr.Complete, cr.Reason = true, ReasonSettled
		return cr
	}

	if terminal() == len(cr.Targeted) && len(cr.Targeted) > 0 {
		cr.Complete, cr.Reason = true, ReasonAllReturned
		return cr
	}

	pollTimer := time.NewTimer(initialPollDelay) // lazy first poll
	defer pollTimer.Stop()
	pollInterval := initialPollDelay
	maxPoll := pollBackoffMax

	poll := func() bool {
		jr, err := c.Job(ctx, job.JID)
		if err != nil {
			if errors.Is(err, ErrJobNotFound) {
				cr.Reason = ReasonStreamFatal
				return true // give up: jid aged out / wrong master
			}
			return false // transient; keep trying
		}
		for id, m := range jr.Result.Minions {
			if m.Status == StatusResponded {
				m.ID = id
				setMinion(m, SourcePoll)
			}
		}
		return terminal() == len(cr.Targeted) && len(cr.Targeted) > 0
	}

	// stopped ends the gather because ctx died — a hard deadline (exit 124) or a
	// cancel, never the "settled as non-responder" normal completion.
	stopped := func() CommandResult {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			cr.Reason = ReasonDeadline
		} else {
			cr.Reason = ReasonCanceled
		}
		if cfg.killOnCancel {
			_, _ = c.Kill(context.WithoutCancel(ctx), job.JID)
		}
		return cr
	}

	for {
		select {
		case <-ctx.Done():
			return stopped()

		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if done := c.applyEvent(&cr, ev, setMinion); done {
				cr.Complete, cr.Reason = true, ReasonAllReturned
				return cr
			}

		case <-settleTimer.C:
			// Gather window quiescent. Poll once for stragglers, then run a
			// saltutil.find_job liveness check on whoever is still pending — salt-api
			// gives no "missing" signal for async gathers (verified), so this is how
			// we tell a slow-but-alive minion (keep waiting) from a down one (settle
			// as no-response) instead of blindly giving up on both.
			if poll() {
				return c.finish(&cr)
			}
			var pending []string
			for id, m := range cr.Minions {
				if m.Status == StatusPending {
					pending = append(pending, id)
				}
			}
			running := c.findRunning(ctx, job.JID, pending)
			if ctx.Err() != nil {
				// The probe was cut short by the deadline/cancel, not answered. An
				// unanswered probe is not evidence that these minions are down —
				// report the deadline rather than settling them as non-responders.
				return stopped()
			}
			poll() // a minion may have finished DURING the probe; record it before judging
			stillAlive := false
			for _, id := range pending {
				if cr.Minions[id].Status != StatusPending {
					continue // it returned during the probe (poll picked it up)
				}
				if running[id] {
					stillAlive = true // still running the job — keep waiting for its return
					continue
				}
				setMinion(MinionResult{ID: id, Status: StatusNoResponse}, SourceFindJob)
			}
			if stillAlive {
				resetSettle()
				continue
			}
			return settle()

		case <-reconnects:
			if poll() {
				return c.finish(&cr)
			}

		case <-pollTimer.C:
			if poll() {
				return c.finish(&cr)
			}
			if pollInterval < maxPoll {
				pollInterval *= 2
				if pollInterval > maxPoll {
					pollInterval = maxPoll
				}
			}
			pollTimer.Reset(pollInterval)
		}
	}
}

// finish marks a gather complete (used when polling reaches terminality).
func (c *Client) finish(cr *CommandResult) CommandResult {
	if cr.Reason == ReasonNone {
		cr.Complete, cr.Reason = true, ReasonAllReturned
	}
	return *cr
}

// findRunning asks the given minions, via saltutil.find_job <jid>, which are
// still executing the job. A minion running it returns a non-empty job-info map;
// one that isn't returns {}; an unreachable one doesn't answer (surfaces as a
// no-response string, i.e. a non-map value). So "still running" == a non-empty
// map. The probe is bounded server-side (short timeout/gather_job_timeout) so a
// dead minion in the set can't stall it for salt's ~15s default. A failed probe
// returns the empty set (treat as none-running → settle).
func (c *Client) findRunning(ctx context.Context, jid string, ids []string) map[string]bool {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out
	}
	cmd := Local(strings.Join(ids, ","), "saltutil.find_job", jid).WithTgtType(TgtList)
	cmd.Extra = map[string]any{"timeout": 2, "gather_job_timeout": 2}
	res, err := c.Exec(ctx, cmd)
	if err != nil || res == nil || len(res.Commands) == 0 {
		return out
	}
	for id, m := range res.Commands[0].Minions {
		if mp, ok := m.Value.Map(); ok && len(mp) > 0 {
			out[id] = true
		}
	}
	return out
}

// applyEvent folds a salt event into the gather state. Returns true when the job
// is complete. Handles ret/<minion> (responded) and /new (missing pre-seed).
func (c *Client) applyEvent(cr *CommandResult, ev Event, set func(MinionResult, Source)) bool {
	switch {
	case strings.Contains(ev.Tag, "/ret/"):
		id := ev.Tag[strings.LastIndex(ev.Tag, "/")+1:]
		var d struct {
			Return  json.RawMessage `json:"return"`
			Retcode *int            `json:"retcode"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		val := d.Return
		if len(val) == 0 {
			val = json.RawMessage("null")
		}
		set(MinionResult{ID: id, Status: StatusResponded, Retcode: d.Retcode, Value: NewRawValue(val), raw: val}, SourceEvent)
	case strings.HasSuffix(ev.Tag, "/new"):
		var d struct {
			Missing []string `json:"missing"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			for _, id := range d.Missing {
				set(MinionResult{ID: id, Status: StatusNoResponse}, SourceMissing)
			}
		}
	}
	n := 0
	for _, m := range cr.Minions {
		if m.Status == StatusResponded || m.Status == StatusNoResponse {
			n++
		}
	}
	return n == len(cr.Targeted) && len(cr.Targeted) > 0
}

// Ping runs test.ping and classifies liveness via the unified result.
func (c *Client) Ping(ctx context.Context, target string, tgtType TgtType, opts ...RunOption) (*Result, error) {
	return c.Run(ctx, JobSpec{Target: target, TgtType: tgtType, Fun: "test.ping"}, opts...)
}

// Kill sends saltutil.kill_job for jid (SIGKILL on the minions; idempotent).
func (c *Client) Kill(ctx context.Context, jid string) (*Result, error) {
	return c.Exec(ctx, Local("*", "saltutil.kill_job", jid))
}

// --- client-side batching ---

func (c *Client) resolveTargets(ctx context.Context, spec JobSpec) ([]string, error) {
	// Discover the live target set with a quick test.ping (also our liveness model).
	res, err := c.Run(ctx, JobSpec{Target: spec.Target, TgtType: spec.TgtType, Fun: "test.ping"})
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	return res.Responders(), nil
}

func (c *Client) submitClientBatch(ctx context.Context, spec JobSpec, cfg runConfig) (*Submission, error) {
	targets, err := c.resolveTargets(ctx, spec)
	if err != nil {
		return nil, err
	}
	sub := &Submission{}
	for _, chunk := range chunkTargets(targets, cfg.clientBatch) {
		js := spec
		js.Target = strings.Join(chunk, ",")
		js.TgtType = TgtList
		js.Batch = ""
		job, err := c.submitOne(ctx, js)
		if errors.Is(err, errNoMatch) {
			continue // a chunk's minions vanished between resolve and submit
		}
		if err != nil {
			return sub, err
		}
		sub.Jobs = append(sub.Jobs, job)
	}
	return sub, nil
}

func (c *Client) runClientBatch(ctx context.Context, spec JobSpec, cfg runConfig) (*Result, error) {
	targets, err := c.resolveTargets(ctx, spec)
	if err != nil {
		return &Result{}, err
	}
	chunks := chunkTargets(targets, cfg.clientBatch)
	merged := CommandResult{Client: ClientLocal, Fun: spec.Fun, Minions: map[string]MinionResult{}}
	var mu sync.Mutex
	sem := make(chan struct{}, max(1, cfg.batchConcurrency))
	var wg sync.WaitGroup
	var canceled bool

	for _, chunk := range chunks {
		wg.Add(1)
		go func(chunk []string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			js := spec
			js.Target = strings.Join(chunk, ",")
			js.TgtType = TgtList
			js.Batch = ""
			job, err := c.submitOne(ctx, js)
			if err != nil {
				return
			}
			if cfg.onSubmit != nil {
				cfg.onSubmit(job)
			}
			cr := c.gather(ctx, job, cfg)
			mu.Lock()
			merged.Targeted = append(merged.Targeted, cr.Targeted...)
			for id, m := range cr.Minions {
				merged.Minions[id] = m
			}
			if merged.JID == "" {
				merged.JID = cr.JID
			}
			if cr.Reason == ReasonCanceled || cr.Reason == ReasonDeadline {
				canceled = true
			}
			mu.Unlock()
		}(chunk)
	}
	wg.Wait()
	res := &Result{Commands: []CommandResult{merged}}
	if canceled {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return res, context.DeadlineExceeded
		}
		return res, context.Canceled
	}
	merged.Complete = true
	res.Commands[0] = merged
	return res, nil
}

func chunkTargets(targets []string, size string) [][]string {
	n := parseBatchSize(size, len(targets))
	if n <= 0 {
		n = 1
	}
	var out [][]string
	for i := 0; i < len(targets); i += n {
		end := min(i+n, len(targets))
		out = append(out, targets[i:end])
	}
	return out
}

// parseBatchSize interprets "N" or "N%" against the total target count.
func parseBatchSize(size string, total int) int {
	size = strings.TrimSpace(size)
	if strings.HasSuffix(size, "%") {
		var pct float64
		if _, err := fmt.Sscanf(size, "%f%%", &pct); err == nil && pct > 0 {
			n := int(float64(total) * pct / 100.0)
			if n < 1 {
				n = 1
			}
			return n
		}
	}
	var n int
	if _, err := fmt.Sscanf(size, "%d", &n); err == nil {
		return n
	}
	return 1
}
