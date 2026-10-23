# Changelog

All notable changes to papurika are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

**papurika is pre-1.0.** While the major version is `0`, the Go API (packages
`papurika`, `config` and `state`) and the CLI surface may change in any minor
release.
Breaking changes are always called out under **Changed** / **Removed** with a
migration note. The `papurika.dev/result/v1` JSON output schema is versioned
separately and is intended to stay stable for scripts.

## [Unreleased]

<!--
Add entries here as you go, under these headings (drop the ones you don't use):

### Added       — new features
### Changed     — changes in existing behavior (call out anything breaking)
### Deprecated  — soon-to-be-removed features
### Removed     — removed features
### Fixed       — bug fixes
### Security    — vulnerability fixes
-->

### Added

- **`papurika jobs --active`** — list only jobs running right now (runner
  `jobs.active`), with the minions still working and their pids. The job cache
  that plain `jobs` lists holds finished jobs too, so "what is running" was not
  answerable from it. The design documented this flag; it was never built.
- **`papurika.Client.ActiveJobs`** — the library equivalent, folding salt's
  `Running: [{"m2": 19816}, {"m1": 19815}]` list-of-single-key-maps into a
  `map[string]int` of minion to pid.
- **`papurika.Client.ExecAsync`** — run a master-side command (`runner_async` /
  `wheel_async`) and gather its return off the event bus, with a `/jobs/<jid>`
  poll as backstop, instead of holding a synchronous connection open. Returns
  the same `*Result` sync `Exec` does. `jobs --active` now uses it, so a
  fleet-wide runner no longer pins a cherrypy worker for the duration.
- **`papurika server`** / **`papurika.Client.ServerInfo`** — report which
  salt-api the current profile points at: netapi module (rest_cherrypy vs
  rest_tornado), the master's salt version, and the netapi client types it has
  enabled. Behavior differs enough between salt versions and between the two
  modules that this is worth checking directly, especially across masters that
  are not all on the same release. Everything but the version comes from an
  unauthenticated request, so it works before credentials exist.
- **`papurika.JobError`** — an error carrying the jid of the job it concerns,
  for methods whose success type (a map, a slice) has nowhere to put one. The
  jid is included in `Error()`, so even a caller that only prints the error
  keeps it.
- **`papurika.RunnerFailed`** — detects salt's invalid-arguments runner return,
  which cannot be detected from `success`: a runner called with missing
  arguments reports `success: true` and puts the usage text in its return
  value.
- **`papurika jobs --limit N` and `--since DUR`** — bound a listing that is
  otherwise the master's *entire* job cache. Both are **opt-in**: the default
  is still to list everything, because a default cap would silently answer a
  narrower question than the one asked. Anything either flag drops is reported
  on stderr rather than silently omitted, since a truncated listing otherwise
  reads as "there are no more jobs". Both filter client-side: `GET /jobs` takes
  no query parameters, and the `jobs.list_jobs` runner applies `start_time`
  only after materializing the whole cache — and skips the filter entirely,
  logging only on the master, when python-dateutil is missing there. `--since`
  compares the jid's fixed-width UTC timestamp prefix, which is how salt mints
  them.
- **`error` object in the `papurika.dev/result/v1` schema** — `{kind, message}`,
  present whenever a run ended in a request-level failure. `kind` is a stable
  vocabulary (`timeout`, `interrupted`, `auth_failure`, `auth_denied`,
  `malformed`, `server_error`, `job_not_found`, `not_found`, `unsupported`,
  `transport`, `error`). Additive: existing consumers are unaffected.
- **`summary.pending` in the result schema** — minions with no terminal outcome
  yet, kept separate from `summary.noresponse`. See **Fixed**, below.
- **`papurika.APIError.Cause`** — the underlying failure (the `net/url` error,
  or the context error) behind a sentinel. `Unwrap` now returns both it and
  `Kind`, so `errors.Is(err, context.DeadlineExceeded)` works on a timed-out
  request.

### Changed

- **`--batch N` now batches client-side by default** (was: salt's server-side
  `local_batch`). Server-side batching returns **no jid at all** — salt-api
  buffers the whole response and the sync reply carries none — so `--batch 10`,
  the most pepper-like invocation a migrating user can type, silently discarded
  the one thing papurika exists to keep. Client-side batching keeps a
  recoverable jid per chunk and streams live progress.

  **Migration:** pass `--batch-mode server` to restore the old behavior. It is
  still the right choice for a very large fleet or an unattended run that must
  survive the client process, because client-side batching resolves the target
  up front, orchestrates from this process, and costs N× API calls — it now
  prints what you are giving up on stderr. `--batch-mode` is also now a real
  flag (the design had always specified it); `--client-batch` is a hidden,
  deprecated no-op alias that warns.
- **The low-level `local`/`local-async`/`local-subset`/`ssh` subcommands no
  longer accept `--batch`, `--batch-mode` or `--batch-concurrency`**, and
  `local-batch` accepts only a plain `--batch`. Those flags never did anything
  on most of them; the low-level tier maps one client to one salt client with
  no smart defaults, and client-side batching is an engine behavior the
  high-level verbs compose.
- **`papurika job <jid>` on a job that is still running now exits 0**, reporting
  `outcome: pending`, instead of exiting 1 with `outcome: failed`. See **Fixed**.

### Deprecated

- **`--client-batch`** — client-side batching is now the default, so the flag is
  a no-op. It is hidden from `--help`, warns when used, and will be removed in a
  future 0.x. Use `--batch-mode server` if you wanted the *other* behavior.

### Fixed

- **SIGTERM produced no output at all — no partial result, no jid, nothing.**
  Only SIGINT was handled, so anything running papurika under `timeout(1)`, a
  CI job, or a supervisor got zero bytes and lost the jid of a job that was
  still running on the master: the exact failure the engine exists to prevent.
  SIGTERM now takes the same drain path as SIGINT (partial result + jid on
  stdout, one terse stderr line) and exits **143**. Relatedly, a *second*
  interrupt now really does exit immediately: `signal.NotifyContext` leaves
  itself installed after the first signal, so the documented "second Ctrl-C
  exits 130 without draining" had never worked.
- **`--output json` wrote zero bytes on a request-level failure**, so
  `json.load()` raised instead of reporting the failure, on exactly the runs a
  script most needs to inspect. json/yaml now emit a well-formed
  `papurika.dev/result/v1` document with `summary.outcome: "error"`, empty
  `commands`, and the new `error` object; ndjson emits an `error` line plus a
  summary line. `pretty` and `raw` still emit nothing to stdout, and the
  message stays on stderr in every mode.
- **`papurika job <jid>` reported a still-running job as failed.** Minions that
  had not answered yet were synthesized as `noresponse` — indistinguishable
  from a dead minion — giving `outcome: failed` and exit 1 for a perfectly
  healthy long job, at the moment the command exists to reassure an operator.
  A job-cache read carries no evidence about *why* a minion is missing, so it
  now reports `pending`; only the gather, which probes with
  `saltutil.find_job`, promotes that to `noresponse`. `pending` counts against
  the exit policy only when a gather *ended* with minions still pending (a jid
  that aged out mid-run), because those will never return. The pretty footer
  reports pending in its own bucket instead of contradicting the
  `(pending)` lines above it.
- **A deadline or a Ctrl-C that landed mid-request exited 69, not 124/130.**
  The transport built its cancellation error as a bare `ErrTransport`,
  discarding `ctx.Err()`, so `--timeout` gave 124 when the *gather* ran out of
  clock but 69 when the initial POST did — the same clock, two exit codes — and
  the message was an unhelpful bare `transport error`. The context error is now
  preserved through `APIError.Cause` and reaches both `errors.Is` and the
  message (`transport error: context deadline exceeded`).

- **`papurika jobs` and `papurika job <jid>` failed with "malformed request" on
  any master that had ever run a list-targeted job.** Salt types the `Target`
  field as a string for most tgt_types but as a **list** for `tgt_type: list`,
  and papurika decoded it as a string, failing the entire response. papurika
  creates such jobs itself — every `saltutil.find_job` liveness probe is
  list-targeted — so this degraded on its own after the first `run`. It also
  broke `/jobs/<jid>`, which `watch` and the gather's reconciliation depend on,
  silently falling back to events only. An unrecognized `Target` shape now
  degrades to its raw text rather than failing the listing.
- **`papurika jobs` ignored `--output`**, always printing JSON. It now renders
  in the selected mode like every other command.
- **Runner and wheel commands silently dropped positional arguments.**
  `Runner("salt.cmd").WithArg("test.version")` built a lowstate with no `arg`,
  so salt received a call with its required argument missing. salt-api does
  accept `arg` for these clients; only papurika's builder withheld it. Nastier
  than a normal bug because salt reports an invalid runner call as
  `success: true` with its usage text as the return value, so the failure
  arrives looking like a result.
- **`Module` marshalled to JSON as a bare integer**, which tells a consumer
  nothing. It now marshals by name (`"rest_cherrypy"`).
- **Interrupting `jobs --active` lost the jid**, reporting a bare "context
  canceled" for a runner that was still going on the master — a violation of
  the never-lose-a-jid rule introduced with the command. The jid now rides out
  on a `JobError`, and the CLI says to look it up **once the runner finishes**:
  unlike a local job, a runner is written to the job cache only on completion,
  so `papurika job <jid>` reports not-found for as long as it is running.
- **`jobs --active` looked like it hung.** It is a live fan-out to every
  minion, and any minion that does not answer costs the master's full
  `timeout` + `gather_job_timeout` (~15s), so on a fleet with a dead host it
  always pays that — while papurika sat silent for up to its 120s HTTP
  timeout. It now says what it is doing before blocking, and `--timeout` is
  passed to salt-api as the runner's own timeout so the master stops waiting
  when you do, with a hint explaining the resulting 504.

## [0.4.0] - 2026-10-23

Adds `papurika profiles`, so "what can this install connect to, and which
endpoint am I about to hit" is answerable without reading TOML by hand — and
without printing any password. **One breaking change:** the last-resort profile
name is now `default` rather than `main`.

### Added

- **`papurika profiles`** — lists the configured profiles, which one the current
  invocation would use (and whether that came from `--profile`,
  `PAPURIKA_PROFILE`, `default_profile`, or the fallback to `main`), and where
  each profile's password comes from. **No password value is ever printed**, so
  the output is safe to log or pipe; a plaintext password in the config file is
  reported as present and warned about on stderr, never shown. Reads only the
  config file — no network, no credentials — so it works before any login.
  Machine-readable under `--output json|yaml` as `papurika.dev/profiles/v1`.
- **`config.Config.SourcePath`** — the file a `Config` was loaded from, so
  callers can report which config is in effect.
- **`config.Config.SelectProfile`** — reports which profile a given explicit
  choice resolves to and why. `Resolve` now uses it, so anything reporting the
  selection agrees with what actually runs.
- **`config.DefaultProfileName`** — the profile name used when nothing else
  selects one.

### Changed

- **The last-resort profile name is now `default`, was `main`** (breaking).
  Follows the convention the AWS CLI established: a lone profile called
  `default` just works, with no config key pointing at it. Note the ordering is
  unchanged — an explicit `default_profile` still outranks it, so a config with
  both `default_profile = "prod"` and a `[profiles.default]` section uses
  `prod`.

  *Migration:* rename `[profiles.main]` to `[profiles.default]`, or set
  `default_profile = "main"` explicitly. Only affects configs that relied on
  the fallback; anything naming its profile explicitly is unaffected.

## [0.3.0] - 2026-10-21

Makes a lost submit recoverable rather than ambiguous, and stops `/events`
connections scaling with in-flight jobs. **Two breaking API changes** — a rename
and a signature change, each with a migration note under *Changed* — plus one
behavior change to how event streams are opened.

### Added

- **Submit reconciliation.** `Run`/`Submit` now mint the job id themselves and
  submit under it, so a submit whose response is lost is still answerable. On an
  ambiguous failure the engine asks `/jobs/<jid>` what actually happened and
  either **adopts** the running job or **re-sends** it. This is exact, not
  heuristic: the master writes the job-cache entry strictly before publishing to
  minions, so `ErrJobNotFound` *proves* nothing executed — such a job is re-sent
  even when it is wildly non-idempotent.
- **`Command.ResendOnUnknown` / `JobSpec.ResendOnUnknown`** — allows papurika to
  send a command again when it cannot determine whether the first attempt
  executed. Consulted **only** where reconciliation cannot answer: a client type
  that carries no jid (`runner`, `wheel`), or a master too unreachable to ask.
  The zero value is the careful one: if papurika cannot tell whether a command
  ran, it does not run it again, and the error carries the jid.
- **`Command.JID`** — submit under a jid you choose. The async engine sets this
  itself; set it by hand only when tracking jids yourself. Validated against
  salt's shape (20 digits, optional `_suffix`) before sending, because nothing
  on salt's publish path validates it and a malformed jid silently degrades
  `StartTime` and jid-ordered listings.

### Changed

- **One `/events` connection per `Client`, not per job.** Gathers now share a
  single refcounted SSE stream, opened on first use and closed when the last
  in-flight job finishes. Previously every concurrent job opened its own, so a
  50-host deploy held 50 connections — each pinning a cherrypy worker thread and
  each carrying the same unfiltered event firehose. The `/jobs` backfill that a
  reconnect triggers is now jittered, since one blip wakes every job at once.

- **`EventStream.Reconnects()` now returns `(<-chan struct{}, func())`**
  (breaking) — the second value unregisters the listener, matching `Subscribe`.

  *Migration:* `ch := es.Reconnects()` becomes `ch, stop := es.Reconnects()`;
  call `stop()` when done. Required now that a stream outlives the jobs using
  it: a listener left registered leaks for the life of the `Client`.

- **`WithRetryNonIdempotent()` is renamed `WithResendOnUnknown()`** (breaking).
  Its meaning narrowed when reconciliation landed: it no longer governs
  "ambiguous failures" broadly — most of those now get a definite answer from
  the job cache — but specifically the residue where no answer is available.
  The old name also read as a licence to retry anything, which it never was.

  *Migration:* rename the call. Behavior is otherwise unchanged.

### Fixed

- `tokencache.MemStore` was not safe for concurrent use — its map was unguarded,
  while `Client` is documented as safe for concurrent use and reaches the store
  from every in-flight request, so concurrent `Run` calls on one `Client` could
  race on it. Found by the shared-event-stream tests, which were the first to
  drive concurrent jobs through a single `Client`.

## [0.2.0] - 2026-10-21

Adds a typed decoder for state returns and makes command-POST retry safe by
default. **Two behavior changes to check before upgrading** — see *Changed*.

### Added

- **`state` package** — a typed decoder for `state.apply` / highstate returns:
  `state.ParseMinion(m)` (or `state.Parse(v)`) yields per-state entries in salt's
  execution order, each classified as clean / changed / failed / skipped /
  would-change. A run that never started is reported as a typed
  `*state.ConflictError` (another state run holds the minion's lock, with its
  jid) or `*state.CompileError` — never as an empty result, which is what makes
  a refused run look like a successful one. `Result.OK()` prefers the minion's
  retcode, salt's own verdict, over re-deriving one by scanning sub-states.

- **`WithRetryNonIdempotent()`** — opts a `Client` into retrying command POSTs on
  *ambiguous* failures too (500/502/503/504, read timeouts). Use it only when
  every command sent through that `Client` is safe to run more than once; for a
  mixed workload, use two `Client`s.
- **`ErrServerError`** — a new sentinel for HTTP 500, distinct from
  `ErrMalformed`. papurika now validates the rendered lowstate (`client`, `fun`,
  `tgt`) before sending, which rules out salt's own missing-field cause, so a 500
  can honestly be attributed to the server. An invalid lowstate fails fast with
  `ErrMalformed` naming the missing field and never reaches the wire.

### Changed

- **Command POSTs are now retried on failures that prove salt never acted on
  them** — connection-establishment errors (DNS, `ECONNREFUSED`, host/net
  unreachable) and HTTP 429. Previously they were never retried at all. They are
  still sent exactly once on every *ambiguous* failure, which now includes 503:
  salt raises it for `SaltReqTimeoutError` as well as `SaltDaemonNotRunning`, so
  a congested master is indistinguishable from an absent one and the request may
  have been published.
- **Breaking:** a 500 now surfaces as `ErrServerError` rather than
  `ErrMalformed`, and the CLI exits 69 (unavailable) rather than 65 (data error)
  for it.

  *Migration:* code matching `errors.Is(err, papurika.ErrMalformed)` to catch a
  server failure must add (or switch to) `papurika.ErrServerError`. The two now
  mean different things — `ErrMalformed` is "your request was wrong", and after
  this release it is raised client-side, before sending, naming the missing
  field. Scripts keying off the CLI's exit 65 for a 500 should expect 69.
- The CLI's highstate renderer now consumes the public `state` package instead of
  keeping a private copy of the model. Rendering is unchanged.

## [0.1.0] - 2026-10-21

First public release. Pre-1.0: expect breaking changes while the design settles.

### Added

- **Go library** (`github.com/cstrahan/papurika`) — a `Client` over salt-api's
  `rest_cherrypy` with one normalized `Result` / `MinionResult` model, so callers
  never parse salt's per-client-type response envelopes themselves. Raw bytes are
  preserved on every result (`RawValue`), so normalization never loses data.
- **Async-first execution.** `Run` submits `local_async` and gathers over
  `/events` with `/jobs` reconciliation, so a long command never pins a cherrypy
  worker. `Submit` is fire-and-forget; `Watch` re-attaches to existing jids.
- **Job ids are never lost.** Every return path — success, deadline, `Ctrl-C`,
  `context.Canceled` — yields the jid(s) plus whatever partial results were
  gathered.
- **Honest classification.** HTTP 200 is not treated as success: request-level
  failures are typed Go errors (`ErrAuthDenied`, `ErrJobNotFound`, …), while
  per-minion and per-command outcomes are an explicit `Status` + `Retcode` on the
  result rather than a parsed English string.
- **Computed minion liveness.** Non-responders are derived as
  `targeted − returned` and re-checked with `saltutil.find_job` before being
  settled, rather than trusting `manage.status` (broken on Salt 3008).
- **CLI** (`papurika`) — high-level `run`, `state`, `ping`, `submit`, `watch`,
  `job`, `jobs`, `kill`, `events`, `key`, plus low-level one-subcommand-per-client
  escape hatches (`local`, `local-async`, `local-batch`, `local-subset`, `runner`,
  `runner-async`, `wheel`, `wheel-async`, `ssh`, `lowstate`).
- **Output modes** `--output pretty|json|yaml|ndjson|raw`, defaulting to pretty on
  a TTY and json when piped. Results go to stdout; progress and diagnostics go to
  stderr. The json schema is the versioned `papurika.dev/result/v1`.
- **Exit-code policy** reflecting *all* minions and commands, via
  `--fail any|all|none` and `--allow-noresponse`.
- **Targeting selectors** mirroring the `salt` CLI: `-L -E -G --grain-pcre -I
  --pillar-pcre -R -C -N`, plus `--subset`.
- **Client-side batching** (`--client-batch`) with configurable concurrency,
  giving streaming progress and recoverable jids — unlike server-side `--batch`,
  which salt-api buffers in full and which surfaces no jid.
- **Config and credentials** — TOML profiles at XDG paths, precedence
  flags > env (`PAPURIKA_*`) > config > defaults, secrets via an exec
  `password_command` (keychain, `pass`, GPG, …) instead of a plaintext password,
  and a session-token cache keyed by `master_url | user | eauth` so profiles
  can't cross-contaminate.
- **`papurika version`** (and `--version`), stamped at release time with the
  version, commit and build date.

### Fixed

- A hard deadline (`--timeout` / `WithDeadline`) that expired *during* the
  `saltutil.find_job` liveness probe was misreported: the cut-short probe was
  read as "nobody is running the job", so still-pending minions were settled as
  **no-response** and the run reported a normal completion instead of the
  deadline (exit 124). An unanswered probe is no longer taken as evidence that a
  minion is down.

[Unreleased]: https://github.com/cstrahan/papurika/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/cstrahan/papurika/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/cstrahan/papurika/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/cstrahan/papurika/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/cstrahan/papurika/releases/tag/v0.1.0
