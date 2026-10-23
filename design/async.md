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

- **The settle window** (`WithSaltTimeout` + `WithGatherTimeout`) — how long
  returns may stay quiet before we run a **liveness re-check** on the stragglers.
  It is **not** a give-up timeout. Each new return extends the quiet period. When
  it elapses, the gather sends **`saltutil.find_job <jid>`** to the still-pending
  minions (bounded server-side so a dead minion can't stall it): those still
  running the job keep being waited on; those that don't answer (or aren't running
  it) are settled as `StatusNoResponse`. That's a **normal** completion
  (`ReasonSettled`, exit per `--fail`), not a timeout — so a genuinely-down minion
  is reported down while a *slow-but-alive* one is **not** mislabeled. We must
  self-bound this way because salt-api emits **no "missing" signal** for async
  gathers — a down-but-key-accepted minion simply never appears in `/jobs` and is
  absent from `new.missing` (verified) — and it mirrors what salt's own CLI does
  (`timeout + gather_job_timeout` + `find_job`).
- **The hard deadline** (`context.Context` / `WithDeadline` / CLI `--timeout`) — the
  authoritative wall-clock bound. Hitting it is a **timeout** (`ReasonDeadline` →
  exit 124); cancel/SIGINT is `ReasonCanceled` (exit 130). Whichever timer fires
  first wins, so a `--timeout` shorter than the settle window turns a slow run into
  a timeout, while the common case (no `--timeout`) settles normally.

```go
func (c *Client) Run(ctx context.Context, spec JobSpec, opts ...RunOption) (*Result, error)

type RunOption func(*runConfig)
func WithDeadline(d time.Duration) RunOption       // hard wall-clock deadline (== a ctx deadline) -> exit 124
func WithSaltTimeout(d time.Duration) RunOption    // settle window: initial quiet before the first find_job re-check
func WithGatherTimeout(d time.Duration) RunOption  // settle window: quiet period (extended per return) between re-checks
func WithOnUpdate(fn func(Update)) RunOption        // streaming progress (alt to a channel)
func WithServerBatch() RunOption                   // §F: JobSpec.Batch via salt's local_batch (no jid)
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

## B.1 Master-side jobs (runner / wheel) — `ExecAsync`

The async rule is not only about `local`. A **runner that touches the fleet is
a long-running command too**: `jobs.active` fans out a live `saltutil.running`
to every minion, and any minion that fails to answer costs the master's full
`timeout + gather_job_timeout` (~15s). Run synchronously, that holds a cherrypy
worker for the whole window — the exact thing §A exists to avoid.

`ExecAsync(ctx, cmd, opts…)` submits `runner_async` / `wheel_async` and gathers
the return, returning the **same `*Result` sync `Exec` produces** so nothing
downstream branches on how it was obtained.

The gather mirrors the local one, for the same reason — the event stream is
lossy, so events alone would hang on a dropped `/ret`:

| source | detail |
|---|---|
| events | `salt/run/<jid>/ret` (wheel: `salt/wheel/<jid>/`). One return, not a per-minion fan-out. Subscribe to the **prefix** before submitting — the jid is only known afterwards — then match on the full tag. |
| `/jobs/<jid>` | Backstop poll with the usual backoff. A runner jid **is** resolvable, with the envelope one level deeper than a local job: `info[0].Result.master.return`, keyed by the pseudo-minion `master`. |

Both decode the envelope `{fun, jid, user, return, success}` and surface
`return` as the command's value.

**`success` is not a success signal.** A runner called with missing arguments
returns `success: true` with salt's usage text as its return *value* (verified
— `cherrypy_learnings.md`). The flag means "the runner executed". `RunnerFailed`
exposes the one case a client can act on; everything else has to be judged from
the value.

A deadline bounds **our waiting, not the runner** — it keeps running on the
master, and the partial `Result` carries the jid so it stays reachable.

---

## C. Interruption / partial results — never lose the jid

Hard requirement: on `Ctrl-C`, ctx cancel, or hard failure, the caller still gets
which minions completed (with results), which didn't, and the **jid(s)** to follow
up. Go lets us return *both* a value and an error — so we return the partial
`Result` **alongside** the error, never instead of it.

### C.1 The jid is ours before the request leaves

The engine **mints the jid itself** and submits under it, rather than reading one
out of the response. salt-api honors a `jid` in the lowstate (verified — see
`cherrypy_learnings.md`). This closes the one hole in "never lose a jid": a
submit whose *response* is lost previously left a job running under an id nobody
could name.

Format: salt's 20-digit UTC timestamp, then `_` and random hex (the shape salt's
own `unique_jid` uses, and what `is_jid`/`jid_to_time` accept). The random
suffix is load-bearing — `gen_jid` is a bare timestamp, so two papurika
processes submitting in the same microsecond would mint the same jid, and a jid
collision is **silent**: the master reuses the existing job-cache entry instead
of erroring. Cost to be aware of: the timestamp is the *client's* clock where
salt would have used the master's, so a skewed client writes jobs whose
`StartTime` is off by the skew.

### C.2 Reconciling an ambiguous submit

Knowing the jid up front turns "did this run?" from a guess into a lookup,
because of the ordering inside `salt/master.py`'s `publish()`:

```
_prep_jid   → jid dir created
_prep_pub   → save_load   ⇒ GET /jobs/<jid> resolves from here on
_send_pub   → minions actually receive the job
```

The cache entry is written **strictly before** any minion can receive anything,
so the inference runs in the direction we need:

| `GET /jobs/<jid>` | what it proves | engine does |
|---|---|---|
| `ErrJobNotFound` | `save_load` never ran ⇒ `_send_pub` never ran ⇒ **nothing executed** | re-send, under the same jid |
| resolves | published, or about to be | **adopt** it and gather — never re-send |
| the lookup itself fails | nothing | stop; return the error **carrying the jid** |

Two consequences worth stating plainly:

- A job proven not to have run is re-sent **regardless of `ResendOnUnknown`** —
  that outcome is not "unknown", so the field does not apply. Proving it didn't
  run is stronger evidence than any declaration, so even `system.reboot` is safe
  to re-send there.
- `Command.ResendOnUnknown` / `JobSpec.ResendOnUnknown` governs **only the
  third row** — the case where the master is too unreachable to ask. The name is
  scoped on purpose: the field decides what happens when the outcome is
  *unknown*, so the re-send in row one is visibly not something it overrides.
  Default (false) is the careful one: if papurika cannot tell whether your
  command ran, it will not run it again, and the error names the jid so you can
  look yourself.

Failures that are *terminal* rather than ambiguous — authz, authn, malformed,
ctx cancellation — skip reconciliation entirely: salt understood the request and
rejected it, so nothing was published and there is nothing to ask about.

**Known gap:** if `save_load` succeeds but `_send_pub` fails, the job exists in
the cache though no minion ever got it. papurika adopts and then reports
non-responders — a misdiagnosis ("minions are down" vs "it never went out"),
though the remedy, re-running, is the same. Also: `runner`/`wheel` reach the
master through `cmd_sync`, which takes no client jid, so those paths cannot be
reconciled at all.

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
`Result.JIDs()` returns them all and `Minions` is unified across them. The
merged `CommandResult` carries the first in `JID` and the rest in `ExtraJIDs`,
so a single-jid consumer is unaffected while nothing is lost; they are sorted,
not ordered by which chunk happened to finish first. The CLI names every one of
them in the pretty header, the `resume:` line, `summary.jids`, and one ndjson
`submitted` line per chunk.

**CLI mapping** ([cli.md](cli.md) §E): the CLI installs its own SIGINT/SIGTERM
handler (not `signal.NotifyContext`, which cannot implement the
second-signal escape); the first signal cancels ctx → `Run` returns partial `Result` + error → the
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

Default **client-side**; **server-side `local_batch`** on explicit opt-in
(`WithServerBatch()` / `--batch-mode server`). Either way the engine normalizes
the shape so callers never see per-chunk fan-out.

**`JobSpec.Batch` is the size; the mode is an option.** This holds in the
library and the CLI alike, so the two cannot drift: `spec.Batch = "10"` (or
`--batch 10`) says *how big*, and `WithServerBatch()` (or `--batch-mode
server`) says *how*. `JobSpec.Batch` is therefore no longer a verbatim copy of
salt's lowstate `batch` key — that role belongs to **`Command.Batch`**, which
`Exec` sends untouched, and the two-tier split in library.md §A is exactly
what makes this safe: `Command` mirrors salt, `JobSpec` is the engine's
convenience input and may mean something the engine implements itself.

**Why client-side is the default.** Server-side batching cannot produce a jid —
not "does not bother to", *cannot*: the sync response carries none (verified,
below). That makes `--batch 10` the one invocation where papurika silently
breaks its own never-lose-a-jid rule, and it is exactly what someone migrating
from pepper types first. Correctness of the core guarantee beats the durability
win, so the default flipped; the durability win is still one flag away, and
choosing it prints what you are giving up on stderr.

The flip is not free, and the tradeoff is real: client-side batching resolves the
target up front (so it can go stale), orchestrates from the client process (so it
dies with that process), and costs N× API calls. For a very large fleet or an
unattended CI run that must survive the client, `--batch-mode server` is still
the right answer — it is simply no longer the answer you get by accident.

**Server-side (`--batch-mode server`).** Send `client: local_batch`, `batch: "<n|pct>"`. This is
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
client-side batching** — it's the only mechanism that can, which is why it is
the default.

**Client-side (default).** Set `spec.Batch`: resolve target → static
minion list (a discovery `test.ping` or key list), chunk it, and fan out one
`local_async` per chunk through the **same `Execution` machinery** (each chunk is a
tracked jid). The engine merges all chunk `Result`s into one unified `Result`
(`JIDs()` = all chunk jids). Buys adaptive sizing, fail-fast thresholds, operator
pauses — at the cost of target staleness, jid fan-out, client-as-orchestrator
fragility (dies with the process), and N× API load (`pepper_design_analysis.md`
§2.12 / §3.1). It's also the only batch option if we ever support tornado.

```go
spec.Batch = "10"                                 // or "25%" — the size, in either mode
func WithServerBatch() RunOption                  // use salt's local_batch instead (no jid, no progress)
func WithBatchConcurrency(n int) RunOption        // cap chunks gathered at once (default 8; GPG-pillar politeness)
// not yet implemented: fail-fast threshold, inter-chunk pause.
```

**`Submit` rejects `WithServerBatch`** with an error wrapping `ErrUnsupported`.
`local_batch` is a synchronous call that returns no jid, so "fire and forget,
here are the jids" is not a thing it can do. Before this was explicit, `Submit`
silently *dropped* the batch size and published one job to the entire
target — the caller asked to limit the blast radius and got the opposite, with
nothing on any stream to say so.

`WithBatchConcurrency` is **client-side only** — server-side `local_batch` has no
separate concurrency knob (the batch size *is* the concurrency; the master
serializes the waves), so there it's tuned via the batch size, not this option.
The CLI says so on stderr rather than ignoring it quietly.

**Progress to the CLI:** the **non-batch** and **client-side-batch** paths feed the
`WithOnUpdate` / `WithOnSubmit` stream (each per-minion return is one `Update`;
each chunk submit is one `Job`), so live progress and NDJSON streaming work. The
**server-side-batch** path is a single blocking call that does *not* fire those
callbacks — its results materialize all at once when the call returns, so there's
no live progress there (consistent with the buffered-response finding above).
**Default, library and CLI alike:** `spec.Batch != ""` ⇒ client-side fan-out;
`spec.Batch != ""` + `WithServerBatch()` ⇒ salt's `local_batch`; no batch ⇒ a
single `local_async`. The CLI is a thin pass-through — `--batch N` sets
`spec.Batch`, `--batch-mode server` adds `WithServerBatch()` — so there is no
layer at which the two disagree about what a batch size means.

An earlier revision flipped only the CLI, on the reasoning that this was a UX
default rather than a library semantic. That was wrong in a way worth
recording: the never-lose-a-jid rule is a property of the *engine*, not of the
command line, and leaving the library defaulted to a mode that structurally
cannot produce a jid meant every non-CLI caller kept the footgun — with no
stderr to warn them, because a library has none.

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
pre-seeds `StatusNoResponse` *when populated*, but salt-api frequently leaves it
empty even for a down-but-key-accepted minion (verified — such a minion never
surfaces in `/jobs` either), so the **`saltutil.find_job` re-check (§B)** is what
actually separates down from slow before a straggler is settled as a non-responder.
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
2. ~~**One shared `EventStream` per `Client`**~~ — **settled and implemented.**
   One `/events` connection per `Client`, refcounted by in-flight gathers
   (`acquireEvents`): opened on first use, closed when the last job finishes, so
   an idle `Client` holds nothing. Previously each gather opened its own, so
   connections scaled with in-flight jobs — a 50-host deploy pinned 50 cherrypy
   worker threads, all carrying the identical unfiltered firehose.

   The thundering herd this entry predicted is real and is handled: a reconnect
   pulses every subscriber, so the `/jobs` backfill is now scheduled with
   jitter (`reconnectBackfillMax`, 750ms) instead of fired inline.

   Two consequences of sharing that a per-job stream hid, both now load-bearing:
   a subscription or reconnect listener that a job fails to unregister leaks for
   the life of the `Client`, not just the job — so `Reconnects()` returns an
   unregister func (as `Subscribe` already did) and gather releases both.

   Still open: an SSE *pool* for isolation under heavy embedding. One connection
   is right for a CLI and for a deploy tool; a long-lived server multiplexing
   many tenants might want more than one. No evidence we need it yet.
3. **Polling defaults** (`initialPollDelay` 2s, `maxPollInterval` 10s, exp ramp) —
   per-call only, or also a `Client`-level default? Tighter for interactive CLI?
4. **HA without shared job cache** — a jid from master A may be unresolvable on
   master B (and the SSE bus is per-master). Full treatment, failure modes, and
   mitigations in **[high-availability.md](high-availability.md)** (not yet
   implemented). Open: pin `Watch`/follow-up to the submitting endpoint vs. assume
   a sticky LB / shared `master_job_cache`.
5. **Client-side batch target staleness** — resolve target→list once up front, or
   re-resolve per chunk on a churning fleet?
