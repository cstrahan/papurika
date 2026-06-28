# papurika — repo guide

**papurika** is a Go client for SaltStack's `salt-api`: an importable **library**
(`github.com/cstrahan/papurika`) plus a **CLI** (`cmd/papurika`). It's a more
predictable, async-first replacement for `pepper`. Targets `rest_cherrypy` (what
prod runs). Go 1.26.

salt-api is full of traps — HTTP 200 for outright failures, wildly different
response envelopes per client type, "no response" surfacing three different ways,
a broken `manage.status`, token eviction, batch responses that don't stream. The
docs in this repo encode hard-won, empirically-verified knowledge about all of it.
**Read them before changing behavior — don't re-learn the hard way.**

## Read these first, in full (start of every session)

1. `design/00-overview.md` — then `design/library.md`, `design/async.md`,
   `design/cli.md`, `design/high-availability.md`. This is the **source of truth
   for intended shape and behavior.**
2. `docs/salt-api-rest_cherrypy.md` — the authoritative API contract.
3. `research/README.md` — what the research harnesses are.
4. `research/salt-test-cherrypy/cherrypy_learnings.md` — the **load-bearing**
   empirical findings the client is built around.

Also valuable: `research/pepper_design_analysis.md` (what we're improving on) and
`research/minion-status-and-liveness.md` (why we don't trust `manage.status`).

## Repo shape

- **Root `*.go` + `cmd/papurika/` + `config/` + `internal/{sse,tokencache}/`** —
  the client itself.
  - Public import contract = package **`papurika`** + **`config`**. Keep it small
    and stable. `internal/` and all unexported code are free to change.
  - **`envelope.go` and `transport.go` are unexported code in the *root* package,
    not `internal/`** — they produce the public `Result`/error types, so a separate
    package would create an import cycle. (See `design/library.md` §A.)
- **`design/`** — the client's design docs (intent). Treat as source of truth.
- **`docs/`** — generated salt-api reference(s) (via `fetch_netapi_docs.py`).
- **`research/`** — throwaway-but-kept experiments: dockerized salt harnesses,
  verbatim request/response captures (`out/`), the learnings write-ups, and the
  upstream salt fix patches/PRs. Not part of the shipped client.

## Core invariants — do not violate

- **Async-first.** Never block on a sync long-running command.
- **HTTP 200 ≠ success.** Classify on status *and* body. Per-minion / per-command
  outcomes are a `Status` + `Retcode` on the result, **not** Go errors; only
  request-level failures (auth/transport/malformed) are errors.
- **Never lose a jid.** Once a job is submitted, every return path — success,
  deadline, Ctrl-C, `context.Canceled` — must yield the jid(s) and partial
  results. (NDJSON leads with a `submitted` line; the CLI prints the jid on
  interrupt.)
- **Compute liveness ourselves** (`targeted − returned`); never trust
  `manage.status` (broken in 3008).
- **Normalize in the library, not the formatter.** Fold salt's divergent shapes
  into the one `Result`/`MinionResult` model; **preserve raw bytes** (`RawValue`)
  so nothing is silently lost.
- **Output discipline.** Results → stdout; progress/diagnostics/errors → stderr.
  `--output json|ndjson` on stdout must stay clean and parseable.
- **cherrypy-first.** Model tornado's shapes cheaply where free; do **not** add
  tornado-only workarounds without an explicit decision.

## Policies to uphold

1. **Design ↔ code lockstep.** If a change alters the *effective design or
   behavior*, update the relevant `design/*.md` in the **same change**. If
   something is designed-but-not-yet-built, mark it clearly (a status banner, like
   `design/high-availability.md`) rather than letting the doc imply it exists.
2. **Reference integrity.** If you move/rename/delete a file or directory, update
   **every** reference to it — design links, doc pointers, code comments, READMEs,
   `apply-salt-patch.sh`-style scripts. `grep -r` the old path before and after.
3. **Empirical grounding over memory.** Don't assert salt-api behavior from
   training knowledge. Verify against the live containers or the `research/`
   captures, and **record new findings in the learnings docs with the evidence**
   (the exact request/response, version). Treat recalled "facts" as suspect until
   confirmed.
4. **Tests as regression guards.** When you fix a bug, add a test that **fails
   before and passes after** — and prove it (temporarily revert the fix, watch it
   fail). Prefer real captures as fixtures. Run `go test ./...` and
   `go test -race ./...` for anything concurrent (the event stream, gather).
5. **Green before commit.** `gofmt -w`, `go vet ./...`, `go build ./...`,
   `go test ./...` all clean.
6. **Don't silently implement an "Open question."** The design docs list
   deliberately-deferred decisions; resolving one usually needs facts (e.g. prod
   topology) or the user. Resolve it, then update the docs.
7. **Keep the public API small.** Don't widen package `papurika`/`config`
   casually; prefer unexported or `internal/`.
8. **Secrets never get committed** (GPG material / `secrets.sls` are gitignored).
   Don't point commands or tests at **prod** without intent — use the throwaway
   containers below.
9. **Commit style.** Imperative subject; body explains *why*; end with the
   trailer `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
   (Note: the *upstream saltstack* repo forbids AI attribution — that rule is for
   PRs there, not for this repo. See `research/salt-test-cherrypy/` patch commits.)

## Dev environment — live salt containers

Throwaway dockerized salt masters for verification (creds `saltdev`/`saltdev`,
eauth `pam`). Recreate/patch with
`research/salt-test-cherrypy/{up.sh,apply-salt-patch.sh,add-second-minion.sh}`:

| container | module | endpoint | notes |
|---|---|---|---|
| `papurika-salt` | rest_cherrypy | `http://localhost:8000` | 3008.1 + token/batch patch (matches prod) |
| `papurika-salt-tornado` | rest_tornado | `http://localhost:8001` | for degradation checks |
| `papurika-salt-3006` | rest_cherrypy | `https://localhost:8002` | 3006.9, **HTTPS** self-signed (use `--insecure`) |

Build/run: `go build ./...`; `go build -o /tmp/papurika ./cmd/papurika`. Quick
manual run: a `config.toml` with a profile + `PAPURIKA_PASSWORD=saltdev`, or
`--url/--user/--eauth` flags.

## Gotchas (each cost real time)

- **zsh word-splitting:** an unquoted `$VAR` is **one** argument in zsh, not
  split. Don't build commands via unquoted vars — inline the args or use a shell
  function. (Misdiagnosed twice as "the API is broken.")
- **Bash-tool cwd persists** across calls — a `cd` sticks; later commands run from
  there. Use absolute paths or `cd` back to the repo root.
- **CLI flag placement:** global/persistent flags (`-p`, `-o`, `-v`, `--timeout`,
  `--insecure`) go anywhere; **subcommand-local flags** (`--batch`,
  `--client-batch`, targeting `-G/-C/-L/...`, `--subset`) go **after** the
  subcommand (cobra resolves the subcommand by scanning args).
- **Server-side `--batch` can't stream and surfaces no jid** — salt-api buffers
  the whole `local_batch` response (verified). Use `--client-batch` for streaming
  progress + recoverable jids.
- **`manage.status` reports dead minions as `up` on 3008** (a regression) and
  unpatched 3008 **evicts tokens** within ~60s. Both are patched in prod and in
  the `papurika-salt` container; details + the upstream fixes are in the learnings
  doc and `research/salt-test-cherrypy/`.
