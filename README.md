# papurika

[![ci](https://github.com/cstrahan/papurika/actions/workflows/ci.yml/badge.svg)](https://github.com/cstrahan/papurika/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/cstrahan/papurika.svg)](https://pkg.go.dev/github.com/cstrahan/papurika)
[![License: MIT OR Apache-2.0](https://img.shields.io/badge/license-MIT%20OR%20Apache--2.0-blue.svg)](#license)

A Go client — importable **library** and **CLI** (`papurika`) — for SaltStack's
salt-api (`rest_cherrypy`). A more predictable, async-first alternative to
`pepper`.

Design: [`design/`](design/) (start with [`design/00-overview.md`](design/00-overview.md)).
The behavioral research that grounds it lives in [`research/`](research/).

## Why

salt-api returns wildly different response envelopes per client type, returns
HTTP 200 for outright failures, surfaces "no response" three different ways, and
its sync calls can pin a server worker indefinitely. papurika hides all of that
behind one normalized result model, an idiomatic Go error taxonomy, and an
async-first execution engine that never loses a job id.

## Install

**Binary release.** Grab a tarball (or a `.deb` / `.rpm`) for linux/macOS on
amd64/arm64 from the [releases page](https://github.com/cstrahan/papurika/releases):

```sh
VERSION=0.4.0
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m | sed -e 's/x86_64/amd64/' -e 's/aarch64/arm64/')

curl -sSfL "https://github.com/cstrahan/papurika/releases/download/v${VERSION}/papurika_${VERSION}_${OS}_${ARCH}.tar.gz" \
  | tar -xz papurika
./papurika version
```

Each release also ships a `checksums.txt`; verify with `sha256sum -c`.

**Go toolchain.**

```sh
go install github.com/cstrahan/papurika/cmd/papurika@latest
```

**As a library.**

```sh
go get github.com/cstrahan/papurika
```

**From source.**

```sh
go build ./...                 # build library + CLI
go test ./...                  # run the test suite
go build -o papurika ./cmd/papurika
```

Requires Go 1.24+.

## CLI quick start

```sh
# one-off, flags only
papurika --url https://salt:8000 --user me --eauth pam run '*' test.ping

# or configure a profile (TOML, XDG path) and just:
papurika run -C 'G@os:Ubuntu and web*' cmd.run 'systemctl restart nginx'
papurika submit '*' state.highstate        # fire-and-forget -> prints jid
papurika watch 20260628031054512551        # block until it finishes
papurika runner manage.up
```

- `--output pretty|json|raw` (defaults: pretty on a TTY, json when piped). JSON is
  the stable `papurika.dev/result/v1` schema — pipe it to `jq`.
- Targeting selectors mirror the `salt` CLI: `-L -E -G --grain-pcre -I
  --pillar-pcre -R -C -N`.
- Exit code reflects **all** minions/commands: `--fail any|all|none`,
  `--allow-noresponse`. See [`design/cli.md`](design/cli.md) §D for the table.
- Ctrl-C **and SIGTERM** print the in-flight jid(s) + a `papurika watch <jid>`
  recovery hint and render whatever was gathered — so a run killed by
  `timeout(1)`, CI or a supervisor is still recoverable.
- `--batch N` batches **client-side**, which is what keeps a recoverable jid and
  live progress. `--batch-mode server` uses salt's `local_batch` instead: durable
  across a client death, but it returns no jid and cannot stream.
- `papurika jobs` lists the master's whole job cache; narrow it with `--limit 50`
  or `--since 2h` (whatever they drop is reported on stderr).

`papurika profiles` lists what's configured, which profile the next command
would use, and where each password comes from — without printing any password
value, so it's safe to log or pipe (`--output json` for automation).

### Config (`$XDG_CONFIG_HOME/papurika/config.toml`)

```toml
default_profile = "prod"

[profiles.prod]
master_url       = "https://salt.example.com:8000"
user             = "me"
eauth            = "file"
password_command = ["pass", "show", "salt/prod"]   # secret never stored in the file
```

Credentials precedence: flags > env (`PAPURIKA_*`) > config > defaults. The
password comes from `password_command` (an exec helper — keychain, `pass`, GPG,
…), `PAPURIKA_PASSWORD`, or a discouraged plaintext `password`. Session tokens
are cached per endpoint (master+user+eauth) under `$XDG_CACHE_HOME/papurika`.

## Library

```go
import "github.com/cstrahan/papurika"

c, _ := papurika.New("https://salt:8000",
    papurika.WithCredentials("me", config.StaticSecret(os.Getenv("PW"))),
    papurika.WithEauth("pam"))

// async-first: submit, then gather over /events + /jobs, robust to SSE gaps
res, err := c.Run(ctx, papurika.JobSpec{Target: "*", Fun: "test.ping"})
for _, id := range res.NonResponders() { /* targeted − returned */ }

// fire-and-forget + follow up later
sub, _ := c.Submit(ctx, papurika.JobSpec{Target: "*", Fun: "state.highstate"})
c.Watch(ctx, sub.JIDs(), papurika.WatchAllReturned)
```

State returns decode through a companion package rather than bloating the core
model:

```go
import "github.com/cstrahan/papurika/state"

sr, err := state.ParseMinion(m)      // entries in salt's execution order
var conflict *state.ConflictError    // another run holds the minion's lock
if errors.As(err, &conflict) { /* follow conflict.JID instead */ }
for _, e := range sr.Failed() { /* ... */ }
```

Everything takes a `context.Context` (the authoritative wall-clock bound). On
cancellation `Run` returns the **partial** result (with jids and gathered
returns) alongside `ctx.Err()`. Request-level failures are typed errors
(`errors.Is(err, papurika.ErrAuthDenied)`, …); per-minion outcomes are a `Status`
+ `Retcode` on the result, never parsed strings.

## Scope

cherrypy-first (what prod runs). The error classifier also recognizes
`rest_tornado`'s divergent shapes, but tornado-only workarounds are out of scope
for v1.

## Status

**v0.4.0 — pre-1.0.** The design is implemented and verified end-to-end against a
live salt-api (Salt 3008). It is in real use, but it is early: **the Go API and
the CLI surface may change in any 0.x minor release.** Breaking changes are called
out in [`CHANGELOG.md`](CHANGELOG.md) with a migration note.

Two things are meant to be stable sooner than the rest: the
`papurika.dev/result/v1` JSON output schema (the contract scripts consume) and the
exit-code policy.

If you hit a salt-api shape papurika mis-normalizes, an issue with the verbatim
request/response is the single most useful thing you can file — this client is
built on empirically captured behavior (see [`research/`](research/)), not on the
salt docs.

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md). In short: `design/` is the source of
truth, behavioral claims need evidence from a live salt-api, and bug fixes come
with a regression test.

## License

Dual-licensed, at your option, under either of:

- Apache License, Version 2.0 ([`LICENSE-APACHE`](LICENSE-APACHE) or
  <http://www.apache.org/licenses/LICENSE-2.0>)
- MIT license ([`LICENSE-MIT`](LICENSE-MIT) or
  <http://opensource.org/licenses/MIT>)

`SPDX-License-Identifier: MIT OR Apache-2.0`

Unless you explicitly state otherwise, any contribution intentionally submitted
for inclusion in papurika by you, as defined in the Apache-2.0 license, shall be
dual licensed as above, without any additional terms or conditions.
