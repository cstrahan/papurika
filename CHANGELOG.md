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

[Unreleased]: https://github.com/cstrahan/papurika/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/cstrahan/papurika/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/cstrahan/papurika/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/cstrahan/papurika/releases/tag/v0.1.0
