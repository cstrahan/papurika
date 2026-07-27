# papurika — async execution engine

The heart of the client: submitting jobs, gathering returns robustly over the
event bus with `/jobs` reconciliation, `context` cancellation, interrupt/partial
results, `watch`, fire-and-forget, batching, and liveness. Builds on the types in
[library.md](library.md) (`Client`, `Result`, `CommandResult`, `MinionResult`,
`Status`, the error taxonomy). See [00-overview.md](00-overview.md) for decisions.

Why async-first (`cherrypy_learnings.md`): a sync `POST /` blocks the HTTP
connection — and a cherrypy worker thread — for the whole job; salt's `timeout`
doesn't bound wall-clock (`timeout + gather_job_timeout`), and a *live* long
command makes a sync call wait essentially forever. So the ergonomic path submits
`local_async` and gathers out-of-band.

---

## A. Async lifecycle & the SSE + /jobs reconciliation engine

One async job (one jid) is an `Execution`: it gathers returns from **two
independent, racing sources** and folds them into one authoritative per-minion
view. SSE is the low-latency path; `/jobs` polling is the truth/backfill path.

```go
type Job struct {
    JID      string
    Targeted []string   // salt's "minions" list from the async submit — AUTHORITATIVE target set
    Missing  []string   // from salt/job/<jid>/new `missing` — known-absent at publish time
    Fun      string
    Client   ClientType
}

// Streamed progress, one per per-minion return (channel/callback; §F, used by CLI).
type Update struct {
    JID    string
    Result MinionResult
    Source Source
}
type Source int
const (SourceEvent Source = iota; SourcePoll; SourceMissing)
```

The gather loop:

1. **Subscribe first.** Register interest in `salt/job/<jid>/ret/*` on the shared
   `EventStream` **before** POSTing the async job — the documented ordering, which
   avoids the race where a fast minion returns before we're listening.
2. **Submit.** `POST /` with `client: local_async`; capture `{jid, minions}` into
   `Job`. From this instant the master retains the job (its `master_job_cache`), so
   the run is recoverable by jid — which the CLI surfaces on stdout (§C/§E); if the
   process dies before that, the job still exists on the master (find it via
   `jobs.list_jobs`).
3. **Seed.** Every targeted minion starts `StatusPending`. When the
   `salt/job/<jid>/new` event's `missing` list arrives, mark those `StatusNoResponse`
   immediately (`SourceMissing`) — don't wait the whole deadline for minions the
   master already knows are gone.
4. **One reducer, two producers.** A single goroutine owns the
   `map[string]MinionResult` (no shared-state locks). It selects over: filtered
   `ret` events → `{Responded, retcode, value}`, and `/jobs/<jid>` poll snapshots →
   backfill + the explicit `Error` field.
5. **Idempotent merge.** A minion id is a unique key; whichever source reports it
   first wins `Pending → Responded`, the second is a no-op. This is what makes SSE
   loss survivable — a `ret` dropped during a reconnect gap is backfilled by the
   next poll; duplicate late events are harmless.
6. **Completion (three terminal conditions).** The gather ends on the first of:
   (a) every targeted minion reaches a terminal status
   (`responded ∪ noResponse == Targeted`) → `ReasonAllReturned`; (b) the
   **settle window** elapses with returns quiescent → leftover `Pending` minions
   become `NoResponse` and the run completes **normally** as `ReasonSettled`
   (this mirrors salt's own `timeout + gather_job_timeout`; a genuinely-down
   minion lands here → reported as no-response, exit per `--fail` policy, **not** a
   timeout error); or (c) the hard `ctx` deadline / cancel fires →
   `ReasonDeadline` / `ReasonCanceled` (§B). We decide **per minion** (partial
   jobs populate `Result` for responders while down minions stay absent), not by
   "`return[0]` non-empty."

```go
// Internal driver shared by Run/Watch/Submit-then-gather.
type Execution struct { job Job; minions map[string]MinionResult; /* ... */ }
```

**Polling cadence — `/jobs` is a safety net, not the clock** (each
`GET /jobs/<jid>` spawns a runner — not free):

- **Lazy first poll:** none for `initialPollDelay` (~2s). Short jobs (`test.ping`)
  complete on SSE alone with **zero** `/jobs` calls.
- **Backoff:** then 2s → 4s → 8s, steady at `maxPollInterval` (~10s).
- **SSE-gap trigger:** when the `EventStream` signals a reconnect (it died and
  recovered — `ChunkedEncodingError`, e.g. master restart), force an **immediate**
  backfill poll, then resume. This is the concrete answer to "survive SSE gaps."
- **Short-circuit:** the instant the reducer sees all-targeted-terminal from events,
  cancel the poller — never poll a finished job.
- **Missing-jid guard:** if a poll returns the explicit `info[0].Error` *after* a
  successful submit (job aged out mid-run, or an HA pool routed us to a master
  without the shared cache — see [high-availability.md](high-availability.md)),
  surface `StreamFatal` with the jid — don't spin. A
  `/jobs` body with **no `info` array at all** (e.g. rest_tornado, whose shape we
  deliberately don't parse — cherrypy-first) is *not* treated as missing: it's
  "no data this poll", so the gather keeps reconciling via the event bus + settle
  window rather than bailing. So a job on tornado still gathers its returns over
  `/events` and settles non-responders normally.

**Shared event stream** (`internal/sse`) — one SSE connection multiplexes all
in-flight jobs:

```go
type EventStream struct { /* one SSE conn; per-jid subscribers; reconnect pulse */ }
func (s *EventStream) Subscribe(jidPrefix string) (<-chan Event, func()) // chan + unsubscribe
func (s *EventStream) Reconnects() <-chan struct{}                       // pulses on each reconnect
```

It auto-reconnects (there's no Last-Event-ID to resume) and pulses `Reconnects()`
so every live `Execution` forces a backfill poll. Because there's no idle
heartbeat, we also run a read deadline and treat past-deadline silence as a
reconnect trigger. (Thundering-herd note in Open Questions.)

---

## B. context.Context & the timeout model

There are **two distinct client-side timers**, and the distinction is the whole
point:

- **The settle window** (`WithSaltTimeout` + `WithGatherTimeout`) — how long the
  gather waits for returns before declaring the leftovers no-response. Each new
  return **extends** the quiet period by the gather timeout, so an active stream
  keeps it open. When it elapses, the run completes **normally** (`ReasonSettled`,
  exit per `--fail` policy). This is how a genuinely-down minion produces a partial
  result with exit 1 — not a timeout. It mirrors salt's own
  `timeout + gather_job_timeout`.
- **The hard deadline** (`context.Context` / `WithDeadline` / CLI `--timeout`) — the
  authoritative wall-clock bound. Hitting it is a **timeout** (`ReasonDeadline` →
  exit 124); cancel/SIGINT is `ReasonCanceled` (exit 130). Whichever timer fires
  first wins, so a `--timeout` shorter than the settle window turns a slow run into
  a timeout, while the common case (no `--timeout`) settles normally.

```go
func (c *Client) Run(ctx context.Context, spec JobSpec, opts ...RunOption) (*Result, error)

type RunOption func(*runConfig)
func WithDeadline(d time.Duration) RunOption       // hard wall-clock deadline (== a ctx deadline) -> exit 124
func WithSaltTimeout(d time.Duration) RunOption    // settle window: initial wait (default 5s)
func WithGatherTimeout(d time.Duration) RunOption  // settle window: quiet period extended per return (default 10s)
func WithOnUpdate(fn func(Update)) RunOption        // streaming progress (alt to a channel)
func WithClientBatch(size string, o ...BatchOption) RunOption // §F
func WithKillOnCancel(bool) RunOption              // §C; default false
```

These settle params are **client-side**; we do not currently inject
`timeout`/`gather_job_timeout` into the async lowstate (an async submit gathers in
the master's background regardless, so the client window is what governs when *we*
stop waiting). A live long job keeps `/jobs` empty for as long as it runs, which is
exactly why the client owns its own timing rather than trusting salt's.

**On ctx cancel/hard deadline:**
1. Stop the poller, unsubscribe from the stream (release connections/worker).
2. **Do not auto-kill the job** (cancel = "stop waiting", not "destroy work"). The
   master keeps running it; the jid stays resolvable for `keep_jobs_seconds` (24h).
   Killing is the opt-in `WithKillOnCancel(true)` → `saltutil.kill_job <jid>`.
3. Return the **partial `Result`** (§C) wrapped with `ctx.Err()`. `Reason` =
   `Canceled` or `Deadline`.

---

## C. Interruption / partial results — never lose the jid

Hard requirement: on `Ctrl-C`, ctx cancel, or hard failure, the caller still gets
which minions completed (with results), which didn't, and the **jid(s)** to follow
up. Go lets us return *both* a value and an error — so we return the partial
`Result` **alongside** the error, never instead of it.

```go
// Contract: once submission succeeds, *Result is non-nil on EVERY return path,
// including error returns. Result.JIDs() is always populated.
func (c *Client) Run(ctx context.Context, spec JobSpec, opts ...RunOption) (*Result, error)
```

```go
res, err := client.Run(ctx, spec) // res != nil even here
if err != nil {
    if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
        log.Printf("interrupted; jid=%v, %d/%d returned — resume: papurika watch %s",
            res.JIDs(), len(res.Responders()), len(res.Commands[0].Targeted),
            strings.Join(res.JIDs(), " "))
    }
}
```

The per-minion map reflects best-known state at the stop instant: responders are
`StatusResponded` with values; the rest are **`StatusPending`** (still running on
the master — look it up later via the jid) or **`StatusNoResponse`** (master
flagged missing). That `Pending` vs `NoResponse` distinction is the actionable
signal on interrupt.

**Multiple jids:** a client-side-batched run (§F) fans out to several jids;
`Result.JIDs()` returns them all and `Minions` is unified across them.

**CLI mapping** ([cli.md](cli.md) §E): the CLI uses `signal.NotifyContext` for
SIGINT; first `Ctrl-C` cancels ctx → `Run` returns partial `Result` + error → the
CLI renders the partial result **to stdout** with the jid in the `(jid …)` header
(pretty) / `summary.jids` (json), a `· PARTIAL` marker, and a `resume: papurika
watch <jid>` line — the jid rides out on the result. stderr gets just one terse
line (§C/§E). We accept that a `SIGKILL` before the stdout flush loses the jid: a
pre-echo to stderr is no more crash-proof, and keeping the happy path quiet is
worth it.

---

## D. Fire-and-forget mode

Opt-in: submit and return jids immediately, skipping all gather — handing the
caller fine-grained control. Two distinct entrypoints (not a boolean), so the
return type differs honestly:

```go
func (c *Client) Submit(ctx context.Context, spec JobSpec, opts ...RunOption) (*Submission, error)

type Submission struct { Jobs []Job }
func (s *Submission) JIDs() []string

// Run == Submit + gather to completion/deadline (§A/§B).
func (c *Client) Run(ctx context.Context, spec JobSpec, opts ...RunOption) (*Result, error)
```

`Submit` is the cheapest path (no subscribe, no gather): `POST local_async`, parse
`{jid, minions}`, return. The `ctx` here only bounds the HTTP submit. This is the
building block for "submit now, watch/kill/lookup on your own schedule" — pair it
with `Watch` (§E). CLI: `papurika submit …` / `papurika run --detach …`.

---

## E. The `watch` primitive

Given jids + a target state, **block until reached, but return immediately if
already there** (no deadlock), then return current state.

```go
type WatchState int
const (
    WatchAllReturned WatchState = iota // every targeted minion of every jid returned
    WatchJobDone                       // job present & has any Result (completed on master)
)

func (c *Client) Watch(ctx context.Context, jids []string, want WatchState, opts ...RunOption) (map[string]*Result, error)
```

The race to avoid is check-vs-subscribe ordering. We resolve it the same way as
`Execution`: **subscribe first, then take an authoritative `GET /jobs/<jid>`
snapshot**, and let the idempotent merge absorb any overlap. Per jid:

1. Subscribe to `salt/job/<jid>/ret/*`.
2. **Immediately** `GET /jobs/<jid>` once. This backfills history we never saw (a
   jid from hours ago is still in the 24h cache) *and* resolves the already-done
   case — so we never block on a finished job.
3. Decide "done" against the lookup ambiguity using the endpoint with the explicit
   `Error` field (not `lookup_jid`, which can't tell missing from pending):
   - `info[0].Error` → **missing/aged-out** → terminal entry (report, don't block forever).
   - `Result` keys == `Minions` → **all returned** (satisfies `WatchAllReturned`).
   - `Result` non-empty but `< Minions` → in progress (satisfies `WatchJobDone`; keep watching for `WatchAllReturned`).
4. If not yet satisfied, fall into the same event+poll reduce loop until satisfied
   or ctx deadline; return the current `*Result` per jid regardless (so a deadline
   still yields a snapshot).

Because step 2 runs before any blocking, an already-satisfied jid returns
essentially instantly — no deadlock. CLI: `papurika watch [--for all-returned|job-done] <jid>…`.

---

## F. Batching strategy

Default **server-side `local_batch`**; **client-side** opt-in for interactive UX.
Either way the engine normalizes the shape so callers never see per-chunk fan-out.

**Server-side (default).** Send `client: local_batch`, `batch: "<n|pct>"`. This is
a **synchronous** `POST /` — the master orchestrates the batching and returns
`return: [{chunk1},{chunk2},…]`; the engine **merges** the per-chunk maps into one
`map[string]MinionResult` (ids are unique keys; lossless) and unwraps the spliced
envelope (`ret` if present, else dict − metadata). Master-orchestrated, survives
client disconnect — the durability/CI/large-fleet choice.

Its limits are inherent to salt-api, **verified empirically** (see
`cherrypy_learnings.md` `local_batch`): rest_cherrypy **buffers the whole batch
response** and sends it with a fixed `Content-Length` only *after the entire job
completes* — it does **not** chunk/flush per batch. So server-side batch gives the
client **no live progress, no incremental output, and no jid** (the sync response
carries none). Reading the body incrementally is impossible because the server
doesn't stream it. (An earlier draft claimed progress "still flows" via per-minion
`ret` events — it doesn't, because the sync path neither subscribes to `/events`
nor has a jid to correlate.)

**If you need streaming progress or a recoverable jid while batching, use
client-side batching** — it's the only mechanism that can.

**Client-side (opt-in).** `WithClientBatch(size, …)`: resolve target → static
minion list (a discovery `test.ping` or key list), chunk it, and fan out one
`local_async` per chunk through the **same `Execution` machinery** (each chunk is a
tracked jid). The engine merges all chunk `Result`s into one unified `Result`
(`JIDs()` = all chunk jids). Buys adaptive sizing, fail-fast thresholds, operator
pauses — at the cost of target staleness, jid fan-out, client-as-orchestrator
fragility (dies with the process), and N× API load (`pepper_design_analysis.md`
§2.12 / §3.1). It's also the only batch option if we ever support tornado.

```go
func WithClientBatch(size string, opts ...BatchOption) RunOption
type BatchOption func(*batchConfig)
func WithMaxInflight(n int) BatchOption       // concurrency cap (also GPG-pillar politeness)
func WithFailFast(threshold float64) BatchOption
func WithPauseBetween(d time.Duration) BatchOption
```

**Progress to the CLI:** the **non-batch** and **client-side-batch** paths feed the
`WithOnUpdate` / `WithOnSubmit` stream (each per-minion return is one `Update`;
each chunk submit is one `Job`), so live progress and NDJSON streaming work. The
**server-side-batch** path is a single blocking call that does *not* fire those
callbacks — its results materialize all at once when the call returns, so there's
no live progress there (consistent with the buffered-response finding above).
**Default:** `spec.Batch != ""` and no `WithClientBatch` ⇒ server-side;
`WithClientBatch` ⇒ client-side; no batch ⇒ a single `local_async`.

---

## G. Liveness / non-responders

Computed structurally — never via `manage.status` (broken in 3008) and never by
scraping `"Minion did not return…"`:

```go
func (r *Result) NonResponders() []string {
    var out []string
    for _, cmd := range r.Commands {
        for _, id := range cmd.Targeted {
            if m, ok := cmd.Minions[id]; !ok || m.Status == StatusNoResponse || m.Status == StatusPending {
                out = append(out, id)
            }
        }
    }
    return out
}
```

Targeted = `Job.Targeted` (the async submit's `minions` list — authoritative).
Returned = minions we got a terminal `Responded` for (from `ret` events or `/jobs`
`Result` keys). **Non-responders = targeted − returned.** The `/new.missing` list
pre-seeds `StatusNoResponse` so known-absent minions are caught before the deadline.
Salt's three "no response" surfacings (sync string / async omission / missing `ret`
event) all collapse into the one `Status` field.

```go
// Ping == Run test.ping; alive == StatusResponded && Value.Bool()==true.
func (c *Client) Ping(ctx context.Context, target, tgtType string, opts ...RunOption) (*Result, error)
```

(If a fast TCP pre-filter is ever wanted we may consult `manage.present` as a
*hint*, confirmed by an actual return — never as the liveness source of truth.)

---

## Open questions

1. **Auto-kill on cancel** — defaulted to **off** (`WithKillOnCancel` to opt in).
   Confirm: cancel = stop waiting, work continues, jid recoverable.
2. **One shared `EventStream` per `Client`** — simplest, matches "subscribe once",
   but a single reconnect blip forces a backfill poll across *every* live job
   (thundering herd on `/jobs`). Mitigation: jitter the forced polls. Acceptable, or
   do we want a small SSE pool for isolation under heavy embedding?
3. **Polling defaults** (`initialPollDelay` 2s, `maxPollInterval` 10s, exp ramp) —
   per-call only, or also a `Client`-level default? Tighter for interactive CLI?
4. **HA without shared job cache** — a jid from master A may be unresolvable on
   master B (and the SSE bus is per-master). Full treatment, failure modes, and
   mitigations in **[high-availability.md](high-availability.md)** (not yet
   implemented). Open: pin `Watch`/follow-up to the submitting endpoint vs. assume
   a sticky LB / shared `master_job_cache`.
5. **Client-side batch target staleness** — resolve target→list once up front, or
   re-resolve per chunk on a churning fleet?
