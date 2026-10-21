# Contributing to papurika

Thanks for looking. papurika is pre-1.0 and moving, so the most valuable
contributions right now are **bug reports with captured evidence** and fixes that
come with a regression test.

## The one thing to know

papurika is built on *empirically verified* salt-api behavior, not on the salt
documentation — because the two disagree often enough to have cost real time.
salt-api returns HTTP 200 for outright failures, uses a different response
envelope per client type, surfaces "no response" three different ways, and ships
a `manage.status` that reports dead minions as `up` on 3008.

So: **don't assert salt-api behavior from memory.** Verify it against a live
salt-api and record what you saw.

- [`research/salt-test-cherrypy/cherrypy_learnings.md`](research/salt-test-cherrypy/cherrypy_learnings.md)
  — the load-bearing findings this client is built around.
- [`research/`](research/) — dockerized salt harnesses and verbatim
  request/response captures.
- [`design/`](design/) — the source of truth for intended shape and behavior.
  Start with [`design/00-overview.md`](design/00-overview.md).

## Filing a bug

The single most useful report is **the verbatim request and response** plus the
salt version, especially for anything papurika mis-normalizes. `--output raw`
preserves what came off the wire, and `-vv` logs the exchange to stderr.

## Development

```sh
go build ./...
go test ./...
go test -race ./...     # required for anything touching the event stream or gather
```

Live salt masters to verify against (throwaway, dockerized; creds
`saltdev`/`saltdev`, eauth `pam`) are in
[`research/salt-test-cherrypy/`](research/salt-test-cherrypy/) — see `up.sh`.

### Before you open a PR

1. **Green.** `gofmt -w .`, `go vet ./...`, `go build ./...`, `go test ./...`,
   and `go test -race ./...` all clean. CI enforces all of these.
2. **A fix comes with a regression test** that fails before and passes after.
   Prove it: revert the fix temporarily and watch the test fail. Prefer real
   captures from `research/` as fixtures.
3. **Design and code move together.** If a change alters effective design or
   behavior, update the relevant `design/*.md` in the *same* change. If something
   is designed-but-not-built, mark it as such rather than letting the doc imply
   it exists.
4. **Add a `CHANGELOG.md` entry** under `## [Unreleased]`.
5. **Keep the public API small.** Packages `papurika` and `config` are the
   third-party import contract; prefer unexported or `internal/`. Pre-1.0 we can
   still break things, but not casually.
6. **Reference integrity.** If you move or rename a file, update every reference
   to it — design links, doc pointers, code comments, scripts. `grep -r` the old
   path before and after.

### Core invariants

These are not up for casual revision; see `design/` for the reasoning.

- **Async-first.** Never block on a sync long-running command.
- **HTTP 200 ≠ success.** Classify on status *and* body. Per-minion and
  per-command outcomes are a `Status` + `Retcode` on the result, **not** Go
  errors; only request-level failures (auth, transport, malformed) are errors.
- **Never lose a jid.** Every return path — success, deadline, Ctrl-C,
  `context.Canceled` — yields the jid(s) and partial results.
- **Compute liveness ourselves** (`targeted − returned`, re-checked with
  `saltutil.find_job`); never trust `manage.status`.
- **Normalize in the library, not the formatter**, and preserve raw bytes
  (`RawValue`) so nothing is silently lost.
- **Output discipline.** Results → stdout; progress, diagnostics and errors →
  stderr. `--output json|ndjson` on stdout stays clean and parseable.

### Commits

Imperative subject; the body explains *why*.

### Secrets

Never commit secrets. GPG material and `secrets.sls` are gitignored. Don't point
commands or tests at a production salt master — use the throwaway containers.

## Licensing of contributions

papurika is dual-licensed under MIT and Apache-2.0. Unless you explicitly state
otherwise, any contribution you intentionally submit for inclusion in papurika,
as defined in the Apache-2.0 license, shall be dual licensed as above, without
any additional terms or conditions.
