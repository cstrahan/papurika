# papurika

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

## Install / build

```sh
go build ./...                 # build library + CLI
go test ./...                  # run the test suite
go build -o papurika ./cmd/papurika
```

Requires Go 1.23+.

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
- Ctrl-C prints the in-flight jid(s) + a `papurika watch <jid>` recovery hint and
  renders whatever was gathered.

### Config (`$XDG_CONFIG_HOME/papurika/config.toml`)

```toml
default_profile = "prod"

[profiles.prod]
master_url       = "https://salt.example.com:8000"
user             = "cstrahan"
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

v1 implementation of the design. Verified end-to-end against a live salt-api
(3008). Not yet released.
