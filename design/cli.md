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
papurika jobs   [--active] [--limit N] [--since DUR|TIME]  # job cache listing; --active = running now (jobs.active)
papurika job    <jid>                     # one job's full normalized return (Client.Job)
papurika kill   <jid>                     # saltutil.kill_job (Client.Kill)
papurika events [--filter <tag>]          # tail the event bus (Client.Events)
papurika key    list|accept|reject|delete|finger <match>
papurika profiles                        # configured profiles + which one is selected (secrets redacted)
papurika schema                          # JSON Schema for the result document (no network, no credentials)
papurika server                          # netapi module, salt version, enabled clients for this endpoint
papurika version                         # version, commit and build date (also --version)
```

**`profiles` and `schema` are the two commands that touch no network and need
no credentials.** `schema` prints the JSON Schema (draft 2020-12) for
`papurika.dev/result/v1`, generated from the same descriptor that pins the
field set ([library.md](library.md) §B.2) — so an installed binary describes
its own output, and what a consumer validates against is what that binary
actually emits rather than whatever a doc says. `--output yaml` renders it as
YAML; every other mode prints the schema itself, since a JSON Schema is
already the artifact.

The schema is deliberately **permissive in the two ways v1 is permissive**: it
does not constrain `additionalProperties`, and it advertises the known values
of `status`/`outcome`/`error.kind` as `examples` rather than `enum`. Both
follow from the compatibility rule below — v1 may gain properties and
vocabulary values — and an `enum` would make a document from a newer papurika
fail validation against an older copy of the schema, which is precisely the
breakage the versioning exists to prevent.

The rest of this section describes `profiles`. It reads the config file and
nothing else, so it works on a box that has never authenticated. It answers
"what can this install connect to, and which endpoint am I about to hit",
which is otherwise only discoverable by reading TOML by hand.

It prints **no password value, ever** — not from `password` (reported as
present-and-plaintext, value withheld) and not by resolving `password_command`
(the argv is shown, since naming where a secret lives is not the secret, and
hiding it would make the command useless for debugging a failing helper). That
makes the output safe to log, paste into a ticket, or pipe to automation.

Two things it surfaces that are otherwise easy to get wrong:

- **Which profile this invocation would use, and why** — `--profile`, then
  `PAPURIKA_PROFILE`, then `default_profile`, then a profile named `default`
  (the convention the AWS CLI established: a lone profile called `default` just
  works, with no config key pointing at it). Note the ordering — an explicit
  `default_profile` outranks the convention, so a config with both
  `default_profile = "prod"` and a `[profiles.default]` uses `prod`. It also
  flags when the selected profile is not defined at all.
- **A plaintext password in the config file**, warned on stderr. The JSON form
  carries the same fact as `secret.plaintext`, so stdout stays pure data.

Machine-readable under `--output json|yaml`, schema `papurika.dev/profiles/v1`,
profiles sorted by name so the ordering is stable.

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

`run` = `submit` + `watch` glued, with the jid carried out on the rendered result
(§C/§E), not a stderr echo. `watch`/`run` are
**return-immediately-if-already-there** (async.md §E): a jid already in its target
state renders and exits without blocking.

**`jobs` lists everything unless you narrow it.** `GET /jobs` returns the
master's *entire* job cache — tens of thousands of rows on a real master, and a
read that can take minutes there before a byte reaches us. `--limit N` (newest
first) and `--since DUR` bound what you read, and they are **opt-in**: a default
cap would make the command silently answer a narrower question than it was
asked, and a listing that omits jobs reads as "there are no more" — which is how
an operator concludes a job never ran. For the same reason, whatever those flags
*do* drop is reported on stderr. Both filter **client-side**: `GET /jobs` takes
no query parameters, and the `jobs.list_jobs` runner applies `start_time` only
*after* materializing the whole cache (`salt/runners/jobs.py`) — and silently
skips the filter entirely when python-dateutil is absent on the master, which is
indistinguishable from "no jobs in that window". So these bound what you read
and render, **not** what the master does.

`--since` takes **either** a duration back from now (`2h`, `90m`) **or** an
absolute ISO 8601 instant (`2026-10-23`, `2026-10-23T14:30`,
`2026-10-23T14:30:00Z`, `2026-10-23T14:30:00+02:00`; a space may stand in for
the `T`). A duration is what you want interactively; an absolute timestamp is
what you want in a script or an after-the-fact investigation, where "2h ago"
means something different every time it runs. Both resolve to one cutoff
instant at flag-parse time, so nothing downstream knows which was typed.

**A timestamp with no zone is read as UTC, not local time.** salt mints jids
from `_utc_now()` (`salt/utils/jid.py`) and the `StartTime` this listing prints
is that same UTC instant, so a timestamp copied out of the listing and pasted
back into `--since` means what it looks like it means. An explicit offset is
honored as given, and the stderr report always echoes the **resolved UTC
cutoff** — `… jobs started before 2026-10-23T14:30:00Z not shown (--since 2h)`
— so the interpretation is never left to guesswork. A negative duration is
rejected rather than silently hiding everything, which would be
indistinguishable from an empty cache.

Matching is on the jid's fixed-width UTC timestamp prefix, the same property
`sortJobRows` already relies on; a jid that does not parse is kept rather than
hidden.

---

## B. Output model

`--output json|pretty|yaml|ndjson|raw`. Default: **pretty** when stdout is a TTY,
**json** when piped. **Zero dependency on Salt's Python outputters** — we render
natively.

**No HTML escaping.** `encoding/json` escapes `<`, `>` and `&` by default so a
document can be dropped into a `<script>` block; papurika turns that off. stdout
is for people and for `jq`, and the escaping only produced `\u003c` noise in
exactly the payloads most worth reading — `cmd.run` output with redirects, XML,
HTML, SQL. It also made `json` and `yaml` disagree about identical data, since
the YAML path decodes the escapes on its way through. The escaped and unescaped
forms parse to the same string, so this is a difference in bytes, not in meaning;
a consumer embedding the output in HTML must escape it itself, as it would for
any other tool's output.

The **JSON output is the unified result model** ([library.md](library.md) §B) —
a stable, versioned, documented schema. It's the contract scripts consume; pretty
is a lossy projection of it; `yaml` is the same model as YAML.

**`ndjson`** is the streaming sibling of `json`: one self-contained JSON object
per line, emitted *as the run progresses*. For a gathering run it emits a
`{"type":"submitted","jid":…,"targeted":[…]}` line **first** (the instant the jid
exists — so the jid is never lost to an interrupt, satisfying the no-lost-jid rule
on the stdout stream itself), then one `{"type":"minion",…}` line per return, then
a trailing `{"type":"summary",…}`. Ideal for piping a large/long run into `jq -c`
or a live processor. Caveat: **`--batch-mode server` can't stream** — salt-api
buffers the whole `local_batch` response and returns no jid (verified;
`async.md` §F), so under server-side batch ndjson degrades to minion + summary
lines emitted at the end with no `submitted`/jid line. Client-side batching (the
**default** for `--batch`) streams and keeps jids.

### JSON (`papurika.dev/result/v1`)

**Defined in the `schema` package** ([library.md](library.md) §B.2), not in the
CLI: `schema.Version`, the types, the status/outcome/error-kind constants, and
`schema.FromResult`, which is the only thing that builds a document. A Go
consumer imports it and unmarshals directly; `schema.Minion.OK()` is the
per-minion ok rule from §D, so nobody has to reimplement it. The field set is
pinned by a test, so adding a field is a reviewable diff and renaming or
removing one fails the build.

```json
{
  "schema": "papurika.dev/result/v1",
  "summary": { "outcome": "partial",
               "targeted": 2, "returned": 1, "succeeded": 1, "failed": 0,
               "noresponse": 1, "pending": 0 },
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

**`pending` vs `noresponse` is an evidence distinction, and the summary keeps
them in separate buckets.** `noresponse` is a *claim*: the gather's settle window
elapsed and a `saltutil.find_job` probe found the minion not running the job
(async.md §G). `pending` is the absence of a claim — the minion has not answered
*yet*. A bare job-cache read can only ever produce `pending`, because
`GET /jobs/<jid>` simply omits whoever has not returned and says nothing about
why; so `papurika job <jid>` on a job that is still running reports
`outcome: pending` and exits **0**, since nothing has failed. Pending counts
against the exit policy only when the gather *ended* with minions still pending
(`reason: stream_fatal` — the jid aged out mid-run), because those will never
return.

**`error` object.** Any run that ends in a request-level failure carries an
`error: {kind, message}` alongside the rest of the document; `kind` is one of
`timeout | interrupted | auth_failure | auth_denied | malformed | server_error |
job_not_found | not_found | unsupported | transport | error`. When the failure
left *nothing* gathered, the document is still emitted, with
`summary.outcome: "error"` and `commands: []` — see §C.

### Pretty (native renderers, selected off `fun`/`out`)

The pretty renderer is specified in full in its own companion doc,
**[cli-output-format.md](cli-output-format.md)** — the shared frame (glyphs, the
green/cyan/red/yellow color semantics, indentation), every per-shape renderer, and
the **highstate-aware** state renderer (compact schema-aware change/diff rendering,
the three failure flavors, the `--changes-only` knob). The examples below are a
digest.

`run '*' test.ping` (partial — minion2 down):
```
test.ping  (jid 20260627222840835836)
  ✓ builtin.minion        True
  ⋯ builtin.minion2        (no response)
2 targeted · 1 ok · 0 failed · 1 no-response
```
(a `· N pending` segment is appended when any minion is pending, so the footer
never contradicts the per-minion `(pending)` lines above it)

`run '*' cmd.run 'exit 7'`:
```
cmd.run 'exit 7'  (jid 20260627222843908511)
  ✗ builtin.minion        retcode 7   (no output)
  ✗ builtin.minion2       retcode 7   (no output)
2 targeted · 0 ok · 2 failed
```

`state.apply` (highstate renderer — see [cli-output-format.md](cli-output-format.md) §D):
```
state.apply  (jid 20260627222840835836)
  ● web1        retcode 0
    ✓ nginx_pkg          pkg.installed
    ✓ nginx_conf         file.managed  /etc/nginx/nginx.conf
        + new file
    ✓ nginx_svc          service.running                          [312ms]
  ● web2        retcode 2
    ✗ nginx_conf         Unable to manage file: Source file salt://… not found
    ⊘ nginx_svc          requisite failed: nginx.nginx_conf
  2 minions · 4 states · 3 ok (1 changed) · 1 failed · 1 skipped
```
(cyan `✓` = changed, green `✓` = clean, red `✗` = failed, `⊘` = skipped; the `●`
minion header is colored by that minion's worst state.)

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
  jid(s), `watch`'s final state, the `events` stream. **This is where the jid(s)
  live** — the pretty `(jid …)` header (with `· PARTIAL` and a `resume: papurika
  watch <jid>` line when a run is cut short), `summary.jids` in json/yaml, the
  ndjson `submitted` line. Nothing else on stdout.
- **stderr** — *everything else*: progress, per-minion streaming ticks, warnings
  (deprecated flag, version skew, `--batch-mode server`, a truncated `jobs`
  listing), auth/connection errors, and the human summary
  line **when `--output json`** (so stdout stays one pure JSON document; in pretty
  mode the summary footer is part of the stdout render). On interrupt/deadline it
  gets **one terse line** — jid-free in pretty (the jid is already on stdout,
  human-readable), or a single `resume: papurika watch <jid>` in the machine
  formats (where a human can't eyeball stdout). No multi-line jid banner, no
  per-submit jid echo — the result carries the jid (§E).

**A request-level failure still writes a document, in the machine formats.**
"Results only on stdout" used to mean *nothing* on stdout when auth failed or
the clock ran out with nothing gathered — so `--output json` produced zero
bytes on exactly the runs a script most needs to inspect, and `json.load()`
raised instead of reporting the failure. json/yaml now emit a well-formed
`papurika.dev/result/v1` document with `summary.outcome: "error"`, empty
`commands`, and the `error` object (§B); ndjson emits an `{"type":"error",…}`
line followed by a summary line. **pretty and raw still emit nothing** — a human
already has the stderr line, and a stub document is only noise. The failure
message itself stays on stderr in every mode, so this adds to stdout without
taking anything off stderr.
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
| 143 | Terminated — SIGTERM (128 + 15) |

Distinguishing 2 (no match) from 1 (matched-but-failed), and 124 (timeout) from 1,
is information pepper discarded. Codes map from the library errors
([library.md](library.md) §C): `ErrAuthFailure`/`ErrAuthDenied`→77,
`ErrMalformed`→65 (or 64 if pre-flight), `ErrTransport`→69, deadline→124.

**The context error outranks the transport sentinel.** A deadline or a Ctrl-C
that lands mid-HTTP-exchange arrives wrapped in `ErrTransport` — the clock
interrupts a request, so of course it does — and must still map to 124/130, not
69. That requires the cause to survive the wrap: `APIError` carries the
underlying error in `Cause` and unwraps to *both* it and `Kind`, so
`errors.Is(err, context.DeadlineExceeded)` holds on a timed-out POST
([library.md](library.md) §C). Collapsing the two, as an earlier version did,
made `--timeout` exit 69 when the master was slow to answer and 124 only when it
was slow to *gather* — the same clock, two codes.

**Pending is not a failure.** Minions with no terminal outcome (`status:
pending`) count against the policy only when the gather ENDED with them still
pending — a `stream_fatal` jid that aged out mid-run. A job that is merely still
running, which is what `papurika job <jid>` reads out of the cache, exits **0**:
nothing has failed yet. See §B.

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
it. On SIGINT / **SIGTERM** / deadline papurika does **not** abandon the job:

**SIGTERM counts.** Anything running papurika under `timeout(1)`, a CI job, or a
supervisor kills it with SIGTERM, not SIGINT. Handling only SIGINT meant those
callers got **zero bytes** — no partial result, no stderr line, no jid — which is
the one outcome the engine exists to prevent. Both signals take the drain path
below; they differ only in the exit code (130 vs 143) and the word on the stderr
line (`interrupted` vs `terminated`).

1. Drain what the engine gathered and **render the partial result to stdout**
   (respecting `--output`; un-returned minions shown `pending`/`noresponse`). The
   command is marked `· PARTIAL` and the jid + a `resume: papurika watch <jid>`
   line are part of that stdout render — **the jid rides out on the result**.
2. Emit **one terse stderr line**: jid-free in pretty (`papurika: interrupted —
   partial result above; jid(s) recoverable`), since the jid is already visible on
   stdout; or a single `papurika: interrupted — resume: papurika watch <jid>` in
   the machine formats, where a human can't eyeball stdout.
3. Exit **130** (SIGINT), **143** (SIGTERM), or **124** (deadline).

`papurika run '*' state.highstate`, Ctrl-C mid-run (pretty):
```
                                    ← stdout ↓
state.highstate  (jid 20260627222843908511)  · PARTIAL
  ● builtin.minion    retcode 0
    ✓ … 12 states ok
  ⋯ builtin.minion2   (pending)
2 minion(s) · 12 states · 12 ok

  resume: papurika watch 20260627222843908511
                                    ← stderr ↓
papurika: interrupted — partial result above; jid(s) recoverable
```
With `--output json`, only the JSON (carrying `summary.jids`) hits stdout, and the
single stderr line becomes `papurika: interrupted — resume: papurika watch
20260627222843908511`. A second Ctrl-C exits 130 immediately without draining.

That second-signal escape needs an explicit handler, not `signal.NotifyContext`:
the latter cancels on the first signal but leaves itself installed, so a second
Ctrl-C re-cancels an already-cancelled context and does nothing at all.

**The compromise.** We rely on flushing stdout at interrupt rather than pre-echoing
the jid to stderr "just in case". A `SIGKILL` before that flush loses the jid — but
so could a kill between a stderr pre-echo and its flush; there is no absolute
guarantee short of the master's `master_job_cache`, and pushing the jid onto the
result keeps normal-path output far less noisy. Worth it.

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
| `--batch N\|N%` | batch size (both modes) | `JobSpec.Batch` |
| `--batch-mode client\|server` | **default `client`**: chunk + fan out, keeping jids and live progress. `server` is salt's `local_batch`: no jid, no progress (warned on stderr) | adds `WithServerBatch()` |
| `--batch-concurrency N` | max client-side batch chunks gathered at once (default 8; ignored under `--batch-mode server`) | — |
| `--subset N` | subset targeting | `local_subset` |
| `--async` / `--detach` | fire-and-forget on any verb (= `submit`) | `*_async` |
| `--fail-any\|--fail-all\|--fail-none` | exit policy (§D) | — |
| `--allow-noresponse` | non-responders count as ok | — |
| `--kill-on-cancel` | kill the job on Ctrl-C/cancel (default: keep running) | — |
| `--no-color` (also honors `NO_COLOR`, non-TTY) | disable ANSI | — |
| `--changes-only` | highstate pretty: hide clean (ok, unchanged) states ([cli-output-format.md](cli-output-format.md) §D.6) | — |
| `-v, -vv` | verbosity → stderr (request bodies at `-vv`) | — |
| `--insecure` / `--cacert PATH` | per-client TLS | — |
| `arg` positionals, `k=v` | function args/kwargs | `arg`, `kwarg` |

Note the `--batch-mode` **default is `client`**, and this is load-bearing: `--batch
10` is the single most pepper-like thing a migrating user can type, and under
server-side batching it silently returns **no jid** (async.md §F) — a never-lose-a-jid
violation invisible at the call site. Server-side batching is still available,
but you have to ask for it, and saying so prints the tradeoff on stderr. The cost
of the flip is that batching now resolves the target client-side and orchestrates
from this process, so it dies with the process and costs N× API calls; `--batch-mode
server` remains the durable/large-fleet choice. (`--client-batch`, which
previously opted *in* to this, is now a hidden no-op alias that warns.) The CLI
is a thin pass-through to the same library default — `--batch N` sets
`JobSpec.Batch` and `--batch-mode server` adds `WithServerBatch()` — so the two
tiers cannot disagree about what a batch size means (async.md §F). The
batching flags are high-level-only: the low-level `local-batch` subcommand takes a
plain `--batch` and nothing else, per the tiering contract in §A.

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
