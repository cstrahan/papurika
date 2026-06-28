# papurika — CLI design

The command surface and UX for the `papurika` binary — a better `pepper`. Built on
**cobra**/**pflag**. Consumes only the public library ([library.md](library.md)) and
its async engine ([async.md](async.md)); see [00-overview.md](00-overview.md) for
decisions. We keep pepper's good parts (`full_return:true` always, `salt`-CLI
targeting parity, eauth token on `/run`) and fix its UX failures (sync-by-default,
Ctrl-C drops the jid, `result[0]`-only exit codes, plaintext config, non-XDG paths,
Salt-outputter dependency).

---

## A. Command tree

Default verb is `papurika run <tgt> <fun> [args…]` — the muscle-memory path from
`salt`/`pepper`, but **async under the hood** (`Submit` → gather → render), so a
long command never pins a worker and Ctrl-C never loses the jid. Two tiers, shown
as cobra command groups in `--help`.

### High-level (ergonomic — operator workflows)

```
papurika run    <tgt> <fun> [args…]     # exec function; async engine, renders on completion
papurika state  <tgt> <sls|highstate>   # state.apply / state.highstate wrapper
papurika ping   <tgt>                    # test.ping + up/down classification (Client.Ping)
papurika submit <tgt> <fun> [args…]     # fire-and-forget → prints jid(s), exits 0 (Client.Submit)
papurika watch  <jid>…                   # block until jid(s) reach target state, then render (Client.Watch)
papurika jobs   [--active|--list]        # job listing (runner jobs.list_jobs / jobs.active)
papurika job    <jid>                     # one job's full normalized return (Client.Job)
papurika kill   <jid>                     # saltutil.kill_job (Client.Kill)
papurika events [--filter <tag>]          # tail the event bus (Client.Events)
papurika key    list|accept|reject|delete|finger <match>
```

### Low-level (to-the-metal — one client = one subcommand)

```
papurika local        <tgt> <fun> [args…]    # client=local (synchronous Exec)
papurika local-async  <tgt> <fun> [args…]    # client=local_async → jid
papurika local-batch  <tgt> <fun> --batch N  # client=local_batch (server-side)
papurika local-subset <tgt> <fun> --subset N # client=local_subset
papurika runner       <fun> [k=v…]           # client=runner
papurika runner-async <fun> [k=v…]           # client=runner_async → jid
papurika wheel        <fun> [k=v…]           # client=wheel
papurika wheel-async  <fun> [k=v…]           # client=wheel_async → jid
papurika ssh          <tgt> <fun> [args…]    # client=ssh (via /run)
papurika lowstate     -f cmds.json           # raw lowstate list → POST /; verbatim
```

**Tiering contract.** Low-level commands map one client → one subcommand, no smart
defaults, no retry of side-effectful POSTs, predictable lowstate — for scripts and
debugging, mirroring exactly what salt-api accepts (`Client.Exec` with explicit
`[]Command`). High-level commands compose the async engine and pick clients for
you: `run` → `Client.Run` (`local_async`), `state` is `run` specialized to
`state.*`, `ping`/`jobs`/`job` are `runner` specialized, `key` is `wheel`
specialized. **Anything a high-level command does is reproducible by hand with a
low-level one** — that's the promise.

**Targeting flags** attach to any `<tgt>` command (`run`/`state`/`ping`/`local*`/`ssh`),
with `salt`-CLI parity → `tgt_type`:

```
(positional)      glob (default)      -I, --pillar       pillar
-L, --list        list                    --pillar-pcre  pillar_pcre
-E, --pcre        pcre                -R, --range        range
-G, --grain       grain               -C, --compound     compound
    --grain-pcre  grain_pcre          -N, --nodegroup    nodegroup
```

With a `-X` selector the leading positional is dropped: `papurika run -G os:Ubuntu test.ping`.
Two selectors at once is a usage error (exit 64).

**Examples**

```bash
papurika run '*' test.ping
papurika run -C 'web* and G@os:Ubuntu' cmd.run 'systemctl restart nginx'
papurika run web1 state.apply nginx pillar='{"ver":"1.25"}' --output json
papurika submit '*' state.highstate              # → 20260627222843908511 (stdout), exit 0
papurika watch 20260627222843908511 --timeout 600
papurika runner manage.up
papurika key accept 'web*' --output json
papurika lowstate -f burst.json --output json    # hand-built multi-command lowstate, verbatim
papurika job 20260627222843908511                # recover a job after a dropped Ctrl-C
```

`run` = `submit` + `watch` glued, with the jid printed to stderr the instant it
exists. `watch`/`run` are **return-immediately-if-already-there** (async.md §E): a
jid already in its target state renders and exits without blocking.

---

## B. Output model

`--output json|pretty|yaml|ndjson|raw`. Default: **pretty** when stdout is a TTY,
**json** when piped. **Zero dependency on Salt's Python outputters** — we render
natively.

The **JSON output is the unified result model** ([library.md](library.md) §B) —
a stable, versioned, documented schema. It's the contract scripts consume; pretty
is a lossy projection of it; `yaml` is the same model as YAML.

**`ndjson`** is the streaming sibling of `json`: one self-contained JSON object
per line, emitted *as the run progresses*. For a gathering run it emits a
`{"type":"submitted","jid":…,"targeted":[…]}` line **first** (the instant the jid
exists — so the jid is never lost to an interrupt, satisfying the no-lost-jid rule
on the stdout stream itself), then one `{"type":"minion",…}` line per return, then
a trailing `{"type":"summary",…}`. Ideal for piping a large/long run into `jq -c`
or a live processor. Caveat: **server-side `--batch` can't stream** — salt-api
buffers the whole `local_batch` response and returns no jid (verified;
`async.md` §F), so under server-side batch ndjson degrades to minion + summary
lines emitted at the end with no `submitted`/jid line. Use **`--client-batch`** for
streaming + jids while batching.

### JSON (`papurika.dev/result/v1`)

```json
{
  "schema": "papurika.dev/result/v1",
  "summary": { "outcome": "partial",
               "targeted": 2, "returned": 1, "succeeded": 1, "failed": 0, "noresponse": 1 },
  "commands": [
    { "index": 0, "client": "local", "fun": "test.ping",
      "tgt": "*", "tgt_type": "glob", "jid": "20260627222840835836",
      "minions": [
        { "id": "builtin.minion",  "status": "responded", "retcode": 0, "return": true },
        { "id": "builtin.minion2", "status": "noresponse", "retcode": null, "return": null }
      ] }
  ]
}
```

`status` ∈ `responded | noresponse | error | notmatched | pending` (the
`MinionResult.Status` values). Command failure is *not* a separate status — it's
`status: responded` with a non-zero `retcode` (or a failed-state `return`), so
nothing is hidden and scripts branch on `retcode`. `jid`, raw `return`, and `out`
(e.g. `"highstate"`) are preserved verbatim. `local_batch`'s per-chunk fan-out is
already merged into one `minions[]` by the library before rendering.

### Pretty (native renderers, selected off `fun`/`out`)

`run '*' test.ping` (partial — minion2 down):
```
test.ping  (jid 20260627222840835836)
  ✓ builtin.minion        True
  ⋯ builtin.minion2        (no response)
2 targeted · 1 ok · 1 no-response
```

`run '*' cmd.run 'exit 7'`:
```
cmd.run 'exit 7'  (jid 20260627222843908511)
  ✗ builtin.minion        retcode 7   (no output)
  ✗ builtin.minion2       retcode 7   (no output)
2 targeted · 0 ok · 2 failed
```

`state.apply` (highstate renderer):
```
state.apply nginx  (jid 20260627222840835836)
  builtin.minion        retcode 0
    ✓ pkg_|-nginx                installed                         (612ms)
    ✓ service_|-nginx            running                           (44ms)
  builtin.minion2       retcode 2
    ✗ file_|-/etc/nginx/nginx.conf   permission denied             (8ms)
Summary: 2 minions · 3/4 states ok · 1 failed
```

`runner manage.up`:
```
manage.up
  ● builtin.minion
  ● builtin.minion2
2 up
```

`key list`:
```
accepted   (2)   builtin.minion  builtin.minion2
unaccepted (1)   web3.example
rejected   (0)
denied     (0)
```

`grains.items` renders a key-aligned tree per minion. `yaml` = the v1 model as
YAML; `raw` = the salt-api `return` payload untouched (escape hatch for "what did
the server actually send"). Every pretty shape has the JSON equivalent above it —
same data, same `status` values.

---

## C. stdout vs stderr

Strict separation so `--output json | jq` is always clean:

- **stdout** — *results only*: the rendered result (pretty or JSON), `submit`'s
  jid(s), `watch`'s final state, the `events` stream. Nothing else.
- **stderr** — *everything else*: progress, per-minion streaming ticks, the
  in-flight jid banner, warnings (deprecated flag, version skew), auth/connection
  errors, and the human summary line **when `--output json`** (so stdout stays one
  pure JSON document; in pretty mode the summary footer is part of the stdout render).
- **Progress / streaming** → stderr, and **only when stderr is a TTY**. Piped or
  `--output json`, progress is suppressed unless `-v`. So
  `papurika run '*' fun --output json > out.json` yields a clean parseable file
  while a human still sees a live progress table on the terminal.

The library's `WithOnUpdate` callbacks (async.md §F) render to a stderr-bound progress
sink; a non-TTY stderr or `--output json` swaps it for a no-op (or `-v` line ticks).

---

## D. Exit-code policy

Fixes pepper's `result[0]`-only bug: papurika folds **every minion across every
command** into the summary, then applies a policy. The summary object that drives
the exit code is the same one emitted as `summary` in JSON — CI can branch on
either.

Per-minion **ok** predicate (default): `status == responded && retcode == 0` (or no
retcode reported). `noresponse`, `error`, `notmatched`, and `responded` with
`retcode != 0` / failed-state are **not-ok**.

Policy flags (mutually exclusive; generalize pepper's first-result-only versions to
all minions × all commands):

| Flag | Non-zero exit when… |
|---|---|
| `--fail-any` (**default**) | *any* minion in *any* command is not-ok |
| `--fail-all` | *every* minion is not-ok |
| `--fail-none` | never on minion outcomes — only transport/auth/usage errors |

`--allow-noresponse` makes `noresponse` count as ok (a down box is sometimes
expected); default is it counts as not-ok.

Exit-code table:

| Code | Meaning |
|---|---|
| 0 | Full success — policy satisfied |
| 1 | Policy failure — partial/total minion failure per `--fail-*` |
| 2 | No minions matched the target (distinct from "all failed") |
| 64 | Usage error — bad flags, conflicting selectors (`EX_USAGE`) |
| 65 | Data error — malformed lowstate / bad JSON to `lowstate` (`EX_DATAERR`) |
| 69 | Service unavailable — salt-api unreachable / TLS failure (`EX_UNAVAILABLE`) |
| 77 | Auth failure — login rejected, token expired, eauth denied (`EX_NOPERM`) |
| 124 | Timeout — `--timeout` elapsed before target state (matches `timeout(1)`) |
| 130 | Interrupted — SIGINT (128 + 2) |

Distinguishing 2 (no match) from 1 (matched-but-failed), and 124 (timeout) from 1,
is information pepper discarded. Codes map from the library errors
([library.md](library.md) §C): `ErrAuthFailure`/`ErrAuthDenied`→77,
`ErrMalformed`→65 (or 64 if pre-flight), `ErrTransport`→69, deadline→124.

**Down minion → 1, not 124.** A genuinely-down minion is a *normal* partial
completion: the gather **settle window** (async.md §B) elapses, the minion is
reported `noresponse`, and the exit code follows `--fail` (1 by default). Exit
**124** is reserved for the **hard** `--timeout` actually elapsing. So
`papurika ping '*'` with one box down exits 1; `papurika --timeout 2s run '*'
'sleep 60'` exits 124. Tune the settle window with `--salt-timeout` /
`--gather-timeout`.

---

## E. Interrupt UX

The library yields partial results + jid on cancel (async.md §C); the CLI consumes
it. On SIGINT papurika does **not** abandon the job:

1. Print the in-flight jid(s) to **stderr** immediately (also printed at submit time).
2. Drain what the engine gathered and **render the partial result** (respecting
   `--output`; un-returned minions shown `pending`/`noresponse`).
3. Exit **130**.

`papurika run '*' state.highstate`, Ctrl-C mid-run:
```
^C
papurika: interrupted — job still running on the master
papurika: jid 20260627222843908511  (resume: papurika watch 20260627222843908511)
papurika: gathered 1 of 2 minions

state.highstate  (jid 20260627222843908511, PARTIAL)
  ✓ builtin.minion        12 states ok
  ⋯ builtin.minion2        (pending — not yet returned)
1 of 2 returned · interrupted
```
(Banner lines on stderr, result block on stdout per §C; with `--output json` only
the JSON hits stdout.) A second Ctrl-C exits 130 immediately without draining. This
is the central fix for "Ctrl-C drops the jid": it's on stderr before any work and
again on interrupt, and `watch <jid>` resumes.

---

## F. Global flags

| Flag | Purpose | Lowstate field |
|---|---|---|
| `-c, --config PATH` | config file (XDG default `$XDG_CONFIG_HOME/papurika/config.toml`) | — |
| `-p, --profile NAME` | named profile (url/eauth/user) | — |
| `--url`, `--eauth`, `-u/--user` | override endpoint/eauth/user | — |
| `--output json\|pretty\|yaml\|raw` | render mode (pretty TTY / json piped) | — |
| targeting selectors (§A) | `-L -E -G --grain-pcre -I --pillar-pcre -R -C -N` | `tgt_type` |
| `--timeout DUR` | **hard** wall-clock deadline (ctx); hitting it → exit 124 | — |
| `--salt-timeout DUR` | settle window: initial wait for returns | — |
| `--gather-timeout DUR` | settle window: quiet period (extended per return) before settling non-responders → exit per `--fail` | — |
| `--batch N\|N%` | batching | `batch` (+ `local_batch`) |
| `--batch-mode server\|client` | server-side `local_batch` vs client-side fan-out | selects path |
| `--subset N` | subset targeting | `local_subset` |
| `--async` / `--detach` | fire-and-forget on any verb (= `submit`) | `*_async` |
| `--fail-any\|--fail-all\|--fail-none` | exit policy (§D) | — |
| `--allow-noresponse` | non-responders count as ok | — |
| `--kill-on-cancel` | kill the job on Ctrl-C/cancel (default: keep running) | — |
| `--no-color` (also honors `NO_COLOR`, non-TTY) | disable ANSI | — |
| `-v, -vv` | verbosity → stderr (request bodies at `-vv`) | — |
| `--insecure` / `--cacert PATH` | per-client TLS | — |
| `arg` positionals, `k=v` | function args/kwargs | `arg`, `kwarg` |

Orthogonality fixes for pepper's mess: `--async` is one flag meaning fire-and-forget
on *any* verb (the per-client zoo lives in the low-level subcommands, not in human
flags); batching is `--batch` + an explicit `--batch-mode` (not overloaded client
strings); the password never lives in config (config holds url/eauth/user; the
secret comes from `password_command`, `PAPURIKA_PASSWORD`, a prompt, or the token
cache — [library.md](library.md) §E). `NO_COLOR` is always honored.

---

## Open questions

1. **Default `--output` when piped** — I default to `json`; some operators expect
   pretty-but-uncolored in pipes (like `git`). Confirm json.
2. **NDJSON streaming** — for long batch runs, a `--output ndjson` mode that emits
   one result object per line as minions return (cleanest CI streaming). Worth a
   distinct schema variant, or defer?
3. **`events` output shape** — emit the v1 result model where applicable, or always
   raw SSE frames as NDJSON? (Event frames are structurally unlike job returns.)
4. **`run` default = async** — confirm `run` should be `local_async`+gather (sync
   `local` only via the low-level `local` subcommand), rather than sync with
   `--async` opt-in. (We chose async-first; this is the CLI consequence.)
