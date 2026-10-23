# papurika — design overview

**papurika** is a Go client for SaltStack's salt-api: an importable **library**
plus a **CLI** (`papurika`), built to replace `pepper`. This is the design that
settles shape and behavior — the source of truth for what papurika is meant to
do. The implementation exists and is kept in lockstep with these docs.

This is the index. The detail lives in:

- **[library.md](library.md)** — public Go API, the unified result model, the
  error taxonomy, the HTTP transport (retries + re-auth), config/secrets/token cache.
- **[async.md](async.md)** — the async-first execution engine, SSE + `/jobs`
  reconciliation, `watch`, fire-and-forget, interrupt/partial-results, batching, liveness.
- **[cli.md](cli.md)** — the command surface, output model, stdout/stderr policy,
  exit-code policy, interrupt UX, flags.
- **[high-availability.md](high-availability.md)** — *(design note, not yet
  implemented)* jid affinity behind a multi-master LB: the failure modes, the
  server-/client-side mitigations, and what the code does today. To be reconciled
  into the docs above once the approach is settled.

Grounding (every behavioral claim traces to these):
[`research/pepper_design_analysis.md`](../research/pepper_design_analysis.md),
[`research/salt-test-cherrypy/cherrypy_learnings.md`](../research/salt-test-cherrypy/cherrypy_learnings.md),
[`docs/salt-api-rest_cherrypy.md`](../docs/salt-api-rest_cherrypy.md),
[`research/minion-status-and-liveness.md`](../research/minion-status-and-liveness.md).

---

## Goals

1. **A predictable library.** Third parties import papurika and consume one
   normalized result model and idiomatic Go errors — never salt-api's divergent
   envelopes or English error strings. (This is the analogue of importing pepper's
   `libpepper`, done right.)
2. **A robust, async-first CLI.** Long commands never pin a server worker; jobs are
   tracked over the event bus with `/jobs` reconciliation; a jid is never lost.
3. **Honest classification.** HTTP 200 is not "success." Every per-minion and
   per-command outcome is classified, and the process exit code reflects all of them.
4. **Sane state.** XDG paths, profiles, secrets via an exec helper (no plaintext),
   a token cache that doesn't cross-contaminate environments.

## Scope (v1)

**cherrypy-first.** Prod runs `rest_cherrypy` (`:8000`, often HTTPS). We design and
test against it. The error classifier *also* recognizes `rest_tornado`'s divergent
auth/error shapes (cheap, since the classifier inspects bodies anyway), but we do
**not** build tornado-only workarounds (no dual `/jobs` envelope parser, no forced
client-side batch fallback). Tornado-unsupported features (`wheel`, `local_batch`,
`local_subset`, `ssh`) fail fast with a clear message rather than silently degrading.
Designing for tornado parity is a deliberate future decision, not a v1 constraint.

## Committed decisions

These are stated **once here** and referenced elsewhere — not re-litigated per doc.

| Decision | Choice | Rationale |
|---|---|---|
| Module path | `github.com/cstrahan/papurika` | — |
| Binary name | `papurika` | — |
| CLI framework | `cobra` + `pflag` | subcommand tree with grouped help; Go-ecosystem default |
| Config format | **TOML** | clear profile sections, fewer footguns than YAML |
| Config/cache location | **XDG** (`$XDG_CONFIG_HOME`, `$XDG_CACHE_HOME`; legacy fallback) | fixes pepper's `$HOME` dotfiles |
| Secrets | exec `password_command` preferred; plaintext discouraged fallback | fixes pepper's plaintext password |
| Token cache key | `master_url \| user \| eauth` | fixes pepper#205 (profile collision) |
| Default execution mode | **async-first** (`run` = submit → gather) | sync pins a cherrypy worker; jid stays recoverable |
| ctx cancel behavior | stop gathering, **do not** auto-kill the job (opt-in `WithKillOnCancel`) | cancel = "stop waiting", not "destroy work" |
| `full_return: true` | always injected by the client | uniform envelope + surfaces `retcode` |
| Liveness | computed (`targeted − returned`); never `manage.status` | `manage.status` broken in 3008 |
| Batching default | server-side `local_batch` (normalized); client-side opt-in | durability/one-JID vs interactive control |
| JSON output schema | versioned `papurika.dev/result/v1`, defined in the public `schema/` package | stable contract for scripts — importable, and its field set pinned by a test |

## Architecture / layering

```
┌─────────────────────────────────────────────────────────────┐
│ cmd/papurika  (CLI: cobra commands, output rendering, exit)   │  → cli.md
├─────────────────────────────────────────────────────────────┤
│ papurika      (PUBLIC library: Client, Result model, errors,  │  → library.md
│                Command/JobSpec, Run/Submit/Watch/Exec/…)       │     async.md
│   · envelope.go   wire-shape decoders → unified model (unexported)
│   · transport.go  pooled *http.Client, retry, re-auth, classify (unexported)
│ config        (PUBLIC: TOML, profiles, XDG, SecretSource)      │
│ state         (PUBLIC: typed decoder for state.apply returns)  │
├─────────────────────────────────────────────────────────────┤
│ internal/sse         shared /events stream (fan-out, reconnect)│
│ internal/tokencache  0600 file store keyed by endpoint         │
└─────────────────────────────────────────────────────────────┘
```

The **public** surface (`papurika`, `config`, `state`) is small and stable — that's the
third-party import contract. The wire decoders and HTTP transport are **unexported
code in the root package** (not `internal/`) because they produce the public
`Result`/error types — a separate package would create an import cycle; lowercase
identifiers keep them just as hidden. The genuinely independent utilities (SSE
reader, token-cache store) live in `internal/`. The CLI is *just another consumer*
of the public library — no privileged access, which keeps the library honest.

## Cross-cutting principles

- **Async-first.** A sync `POST /` blocks the HTTP connection — and on cherrypy a
  worker thread — for the whole job; a long command can hang indefinitely. The
  ergonomic path submits `local_async`, subscribes to `/events` first, and gathers.
  See [async.md](async.md).
- **HTTP 200 ≠ success.** A 200 routinely carries authz denials (tornado),
  unknown-function strings, non-zero retcodes, and failed states. The transport
  classifies status **and** body into typed errors; per-minion outcomes become an
  explicit `Status`, never a parsed English string. See [library.md](library.md).
- **Normalize in the library, not the formatter.** salt-api's envelope differs per
  client type and per batch/non-batch; the `internal/envelope` decoders fold all of
  it into one `Result` before anything renders it. pepper's bug was sniffing shapes
  in the output layer. See [library.md](library.md).
- **Never lose a jid.** Once a job is submitted, every return path — success,
  deadline, `Ctrl-C`, `context.Canceled` — yields the jid(s) and whatever partial
  results were gathered. The CLI carries the jid(s) **on stdout** (the rendered
  result: the pretty `(jid …)` header, `summary.jids` in json, the ndjson
  `submitted` line) rather than leaning on a stderr banner — so a job stays
  recoverable from the result itself. See [async.md](async.md) and [cli.md](cli.md).
- **Compute liveness ourselves.** Non-responders = targeted − returned (folding
  salt's three "no response" surfacings into one status). Never `manage.status`.

## Coverage checklist

Every question that motivated this design maps to a section:

| Question | Where |
|---|---|
| CLI interface: subcommands, flags, output, stdout/stderr, exit code, high vs low level | [cli.md](cli.md) §A–F |
| Internals: layers / separation of concerns / module boundaries | this doc; [library.md](library.md) §A |
| Importable library: public vs internal interfaces | [library.md](library.md) §A |
| HTTP retries & re-auth | [library.md](library.md) §D |
| Async-first; where SSE fits; robust job tracking under intermittent failures | [async.md](async.md) §A |
| Interrupt → partial results (done vs not) + recoverable jids | [async.md](async.md) §C; [cli.md](cli.md) §E |
| `watch` command (block-until-state, return-if-already-there) | [async.md](async.md) §E; [cli.md](cli.md) §A |
| Fire-and-forget mode | [async.md](async.md) §D; [cli.md](cli.md) §A |
| (Gemini 1) Response normalization & Go data models | [library.md](library.md) §B |
| (Gemini 2) Error classification & idiomatic Go errors | [library.md](library.md) §C |
| (Gemini 3) Config, secrets, token caching (XDG & profiles) | [library.md](library.md) §E |
| (Gemini 4) Timeouts & context integration | [async.md](async.md) §B |
| (Gemini 5) Batching strategy | [async.md](async.md) §F |
| (Gemini 6) Minion liveness & non-responders | [async.md](async.md) §G |
