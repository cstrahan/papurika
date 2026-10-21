# `pepper` design analysis

A design review of **`pepper`**, the official SaltStack salt-api command-line
client (analyzed from the local checkout at `~/src/pepper`), written to inform
the alternative client we're building ("papurika"). pepper being dated and
buggy is the whole reason we're collecting this research; this doc names what it
gets right and wrong, and — for the wrong things — whether there's an obvious
best-in-class fix or genuine uncertainty about the right approach.

Findings are sorted into three buckets:

1. **[What pepper gets right](#1-what-pepper-gets-right)**
2. **[Wrong, with an obvious best-in-class fix](#2-wrong-with-an-obvious-fix)**
3. **[Wrong, but the best approach is still uncertain](#3-wrong-but-uncertain)**

Pepper-behaviour claims carry `file:line` references into `~/src/pepper`. The
salt-api behaviours they interact with are documented in our harness learnings:
[`salt-test-cherrypy/cherrypy_learnings.md`](salt-test-cherrypy/cherrypy_learnings.md),
[`salt-test-tornado/tornado_learnings.md`](salt-test-tornado/tornado_learnings.md),
[`salt-test-gpg-pillar/gpg_pillar_learnings.md`](salt-test-gpg-pillar/gpg_pillar_learnings.md),
and [`minion-status-and-liveness.md`](minion-status-and-liveness.md).

---

## Architecture at a glance

Pepper is a thin CLI over salt-api's `POST /` lowstate interface. Four modules:

| Module | Role | Notes |
|--------|------|-------|
| `pepper/libpepper.py` | HTTP transport (`Pepper` class) | **Two** HTTP stacks: urllib + requests |
| `pepper/cli.py` | optparse parsing, lowstate build, polling (834 lines) | `PepperCli` |
| `pepper/script.py` | output formatting + exit code (133 lines) | `Pepper.__call__` |
| `pepper/retcode.py` | maps minion `retcode`s → process exit code | `PepperRetcode` |
| `pepper/exceptions.py` | 3 flat exception types | — |

Entry point `scripts/pepper` constructs `script.Pepper()` and calls it. The flow:
`cli.run()` (`cli.py:804`) builds the lowstate, logs in, POSTs via
`libpepper.Pepper.low()` → `req()`, then `script.Pepper.__call__` formats each
result and computes the exit code. Output formatting **reuses Salt's own
outputters** when `import salt` succeeds (`script.py:15-22`, `HAS_SALT`), else it
dumps raw JSON. CLI parsing uses **`optparse`** (`cli.py:62`), and there are still
Python-2 compatibility shims (`cli.py:22-42`).

---

## 1. What pepper gets right

- **`full_return: true` is injected for every non-wheel client**
  (`cli.py:823-825`). This is exactly right: it normalizes `local`/`runner` into
  the same `{…, "ret"/"return": <value>, "retcode"/"success": …}` envelope that
  `wheel` already uses, and — crucially — surfaces `retcode` for exit-code
  mapping. (See "full_return" in `cherrypy_learnings.md`.) Caveat: it does **not**
  reconcile batch vs non-batch shapes — see §2.12.
- **Targeting-flag parity with the `salt` CLI**: `-E/-L/-G/--grain-pcre/-I/
  --pillar-pcre/-R/-C/-N` all map to the right `tgt_type` (`cli.py:260-337`).
  Familiar to anyone who knows `salt`; low friction.
- **`/run` uses an eauth token, not the session token** (`cli.py:755-756`,
  `libpepper.token` → `/token`; the token is injected into each lowstate at
  `cli.py:791-793`). This sidesteps the cherrypy trap where a `/login` **session
  token 401s on `/run`** (raw eauth tokens work on both modules — see
  `cherrypy_learnings.md` "/run").
- **Liveness in the polling path is computed by set difference**, not by parsing
  English: non-responders are `targeted − returned`, using the `minions` list
  from the async submit (`cli.py:706,751`). This is the correct technique
  (`minion-status-and-liveness.md`) — it's just buried behind a flag (see §2).
- **The token-cache *file* is written with good hygiene**: mode `0o600` via
  `os.open(..., 0o600)` under a zeroed umask (`cli.py:773-780`), and the cached
  blob is the `/login` return (token, expire, perms, user, eauth) — no password
  in the *cache*. (The credential story overall is still poor — the password
  lives in plaintext in the config file; see §2.9.)
- **Flexible exit-code policies**: `--fail-any / --fail-any-none / --fail-all /
  --fail-all-none` (`retcode.py`) cover "fail if any failed" vs "fail only if all
  failed", with/without treating "no retcode" as failure. A genuinely useful
  surface for scripting (the implementation has a scope bug — see §2).
- **Broad eauth backend support** including Kerberos/GSSAPI (`libpepper.py:275`),
  and a reasonable **test suite** exercising both `rest_cherrypy` and
  `rest_tornado` across Python 3.8–3.11.

---

## 2. Wrong, with an obvious fix

Each item: the problem (with refs) → the recommended approach.

### 2.1 Never uses the event bus; the SSE code is dead
`req_stream('/events')` and `req_get()` are defined (`libpepper.py:99,151`) but
**never called** by `cli.py`/`script.py` (grep-confirmed). Pepper's only "async"
is `jobs.lookup_jid` polling.
→ **Be async-first on `/events`**: subscribe *before* firing `*_async`, match
returns by `salt/job/<jid>/ret/<minion>`, and reconcile completion against
`/jobs`. Fall back to polling only when the stream is unavailable.
(`cherrypy_learnings.md` event-bus pattern; note `/events` is lossy — see §3.3.)

### 2.2 Synchronous by default; polling hidden behind a flag
The async/polling path runs *only* with `--fail-if-incomplete`
(`cli.py:827`→`poll_for_returns` `cli.py:698`). The default path is a blocking
sync `POST /` (`cli.py:831`). For long commands this is a real hazard: on
cherrypy a sync call **ties up a worker thread** for the full duration; on
tornado it returns a **misleading empty `{}`** at the ~10s gather timeout while
the job keeps running (`cherrypy_learnings.md` sync-long-command;
`tornado_learnings.md`; `gpg_pillar_learnings.md`).
→ **Default to async submit + event/JOB reconciliation**; reserve true sync for
short, interactive calls. Never infer success/failure from a sync long-command
response.

### 2.3 Response normalization is scattered shape-sniffing in the output layer
`script.py:60-108` branches inline on response shape: tornado's
`minionret["return"]` vs cherrypy's `minionret["ret"]` vs a bare fallback, plus a
separate `data.return` unfold for runner/wheel (`script.py:89-96`). The logic
lives in the *formatter* and is entangled with whether Salt is importable.
→ **Produce one internal result model in the transport/library layer** (per
client type and per netapi module), before any formatting. The headline lesson
from `cherrypy_learnings.md`: "the response envelope shape varies by client type;
a clean client should normalize these into one result model."

### 2.4 Treats the response as trustworthy; weak error classification
`req()` raises only on 401/500 and on JSON-parse failure ("Unable to parse the
server response.", `libpepper.py:257-271`); everything else is assumed to be a
valid result. But **HTTP 200 ≠ success**: a 200 routinely carries authz denials,
unknown-function errors, non-zero retcodes, failed states, and (on tornado) even
malformed-request exception strings. Auth error shapes also diverge — cherrypy
returns **HTML** 401s, tornado returns **JSON**; authz denial is a **401** on
cherrypy but a **200 with `"Failed to authenticate"`** on tornado
(`cherrypy_learnings.md` failure taxonomy; `tornado_learnings.md`). Pepper's
generic catch-all then prints a bare exception (`script.py:125-132`).
→ **Build a real response classifier** keyed on status *and* body:
authn-failure / authz-denial / not-found / minion-error / command-failure /
no-response / success. Don't assume errors are JSON. **Re-auth on 401** (tokens
expire, and unpatched 3008 evicts them — see `cherrypy_learnings.md`).

### 2.5 Retcode validation only inspects the first command
All four validators read `result[0]` only (`retcode.py:44,66,90,113`). For the
normal CLI path that's fine because `parse_cmd` emits a single-element lowstate
(`cli.py:696`) — but salt-api accepts a **command array** (reachable via
`--json`), and there's **no stop-on-failure** server-side
(`cherrypy_learnings.md` multi-command). Retcodes from commands 2..n are silently
ignored, so a multi-command job can fail yet exit `0`.
→ **Classify every command entry and every minion entry**, not just `result[0]`.

### 2.6 No connection reuse; two HTTP stacks
`req()` calls `build_opener()`/`install_opener()` per request (`libpepper.py:215`)
— a fresh connection each time, no pooling — and the code maintains **two**
transports (urllib in `req()`, requests+gssapi in `req_requests()`,
`libpepper.py:194,275`). It also **monkeypatches global SSL**
(`ssl._create_default_https_context = ssl._create_stdlib_context`,
`libpepper.py:15`) and builds a `PROTOCOL_SSLv23` context for `--ignore-ssl-errors`
(`libpepper.py:244`).
→ **One `requests.Session`** for connection reuse and a single, consistent error
path; drop the global SSL monkeypatch in favor of per-session `verify=`.

### 2.7 Ctrl-C drops the JID
`except KeyboardInterrupt: # TODO: mimic CLI and output JID on ctrl-c`
(`script.py:122-124`) returns 0 and prints nothing — the job is still running on
the master but the operator has no JID to reconnect with.
→ **Capture and print the JID** on interrupt (and on any client-side timeout) so
the user can `jobs.lookup_jid` / resubscribe later.

### 2.8 Liveness must not trust `manage.status`
Not a pepper bug today, but a trap to avoid: `manage.status/up/down` is **broken
in 3008** (reports every key-accepted minion as `up`; see
[`minion-status-and-liveness.md`](minion-status-and-liveness.md) §5 and the
upstream fix). Pepper already does the right thing in its poll path (targeted −
returned).
→ **Generalize set-diff liveness** to all paths; never derive up/down from
`manage.status`, and classify a minion alive only on an actual `true` return.

### 2.9 Password stored in plaintext in the config file
`get_login_details` reads `SALTAPI_PASS` straight out of the INI config
(`cli.py:605`), so a working `~/.pepperrc` contains the user's password in clear
text:
```ini
[main]
SALTAPI_URL=https://salt.example.com:8000
SALTAPI_USER=me
SALTAPI_PASS=<plaintext password>
SALTAPI_EAUTH=file
```
There's no mechanism to source the secret from anywhere else short of typing it
interactively every invocation.
→ **Add a `password command` config option** (an exec-based credential helper, à
la git's `credential.helper`, mbsync's `PassCmd`, isync, offlineimap): pepper runs
the command and reads the secret from stdout. That keeps the secret out of the
config file and lets the user pull it from a password manager, the OS keychain, a
GPG-encrypted file, etc. — and the helper can own the pty to prompt or require a
biometric/touch assertion to unlock. Plain `SALTAPI_PASS` can remain as a
discouraged fallback.

### 2.10 Config and cache ignore the XDG Base Directory spec
The config defaults to `~/.pepperrc` (`cli.py:73`) and the token cache to
`~/.peppercache` (`cli.py:458`) — dotfiles straight in `$HOME`. Pepper is even
inconsistent with itself: the **master** config already defaults to
`~/.config/pepper/master` (`cli.py:99`).
→ **Follow XDG**: config under `$XDG_CONFIG_HOME` (`~/.config/<app>/`), cache under
`$XDG_CACHE_HOME` (`~/.cache/<app>/`), honoring the env vars with the standard
fallbacks. Keep reading the legacy paths for back-compat.

### 2.11 Token cache isn't keyed by profile
The cache path is a single value (`-x` / `$PEPPERCACHE` / `~/.peppercache`,
`cli.py:458`) used directly as the token file (`cli.py:759`), with **no profile
component** — even though `-p/--profile` (`cli.py:85`) selects among multiple
`[profile]` sections pointing at different masters/users. So a token minted for
one profile is read back for another, against the wrong master. This is upstream
[pepper#205](https://github.com/saltstack/pepper/issues/205).
→ **Key the cache by profile** (and ideally by master URL + user + eauth): e.g. a
per-profile file under the XDG cache dir, or a single store indexed by a
profile/endpoint key. Never share one token blob across endpoints.

### 2.12 Batch vs non-batch response shapes diverge — and `full_return` doesn't fix it
Two structural divergences, both of which bite repeatedly in prod — our internal
ops runbook carries explicit warnings and a result-parsing shim to paper over
them:
1. **Array fan-out.** Non-batch `local` returns all minions in one dict —
   `return: [{m1: r1, m2: r2}]` (capture `out/17a_aggregate_test_ping.json`).
   `local_batch` returns **one `return[]` element per batch chunk** —
   `return: [{m2: r2}, {m1: r1}]` (capture `out/17b_local_batch_two_minions.json`).
2. **Envelope splice.** With `full_return: true`, non-batch wraps each minion's
   value under `ret` (alongside `retcode`/`jid`/`fun`/`out`):
   `{m1: {"ret": <retval>, "retcode": 0, …}}`. `local_batch` omits the `ret`
   wrapper and splices the raw return at top level next to `retcode`:
   `{m1: {"retcode": 0, <state_id>: {…}}}` — exactly what the skill's
   `parse_state_results` detects by presence/absence of a `ret` key.

So pepper's headline normalization (`full_return`, §1) does **not** make batched
output look like non-batched output. This also compounds §2.5: pepper's retcode
validation reads only `result[0]`, which for a batched run is just the **first
chunk** — every minion outside the first batch is silently excluded from the exit
code.

**Normalize the shape in the client — regardless of how you batch.** This is the
actual fix for the prod papercut, and it's honesty-preserving:
- **Fan-out (#1) is losslessly mergeable.** Minion ids are unique keys, so merge
  every `return[]` chunk into one `{minion: result}` map. The only thing dropped
  is "which chunk a minion landed in," which the sync response doesn't meaningfully
  convey anyway.
- **Envelope (#2): unwrap to the `ret`-wrapped form.** If a `ret` key is present
  use it; otherwise treat the dict minus injected metadata (`retcode`) as the
  return. Minor honesty caveat: the batch top-level splice merges the function's
  return with `retcode` in one dict, so it's *technically* ambiguous if a return
  key could collide with `retcode` — pathological in practice (state ids and scalar
  returns don't collide), but the client should normalize defensively. Either way,
  produce one internal result model (ties to §2.3) so callers never see the
  divergence.

**Where to batch — server-side `local_batch` vs client-side — is a separate
decision; pick it on operational grounds, not to fix the shape:**
- **Default to server-side `local_batch` when batching is needed.** The master
  orchestrates, so the run **survives client disconnect** (close the laptop, drop
  VPN — it keeps going); it's **one JID** (clean job history, not N fragments); the
  master **resolves the target natively** (no stale static minion list); and it's
  **one** API request + auth (no N× overhead / self-DDoS against your own API).
- **Client-side batching is a legitimate alternative** for interactive use on
  moderate fleets: resolve the target to a static list, chunk it, fan out
  `local`/`local_async` per chunk, merge. You get predictable shapes for free, plus
  fine-grained flow control (adaptive batch sizing, fail-fast thresholds, operator
  pauses) and accurate progress UX. Costs: a two-step target resolution with a
  **staleness window**, **JID fan-out** (N jids per logical run), the **client
  becomes the orchestrator** (the run dies with the process), and **N× API/auth
  load**. It also happens to be the only option on tornado, which has no
  `local_batch` (`tornado_learnings.md`).
- **Bottom line:** prefer server-side `local_batch` for durability/CI/large fleets,
  reach for client-side when interactive control/UX matters and the fleet is
  moderate — and **normalize the response shape in the client in both cases**.

### 2.13 Smaller, clearly-fixable items
- **Broken error message**: `PepperArgumentsException("Cannot open file: %s",
  self.options.json_file)` passes two args, so `%s` is never interpolated — the
  user sees a stringified tuple (`cli.py:635`). → use an f-string.
- **`optparse`** is deprecated → `argparse` (or click/typer).
- **Python-2 shims** (`cli.py:22-42`) and the Py2 classifiers are dead weight →
  drop them.

---

## 3. Wrong, but uncertain

Here the *problem* is clear but the best approach involves real trade-offs; the
new client needs a deliberate decision, not a copy-paste fix.

### 3.1 Cross-module portability (cherrypy vs tornado)
The two netapi modules diverge widely: tornado hardcodes only **4 clients**
(`local`, `local_async`, `runner`, `runner_async`) — **no `wheel`, `local_batch`,
`local_subset`, `ssh`**; it has **no server-side batching** (`local_batch` → 400);
it **ignores `full_return` on sync local**; it **inverts** the no-match vs
no-response signaling relative to cherrypy; and it returns **empty-200 on gather
timeout** (`tornado_learnings.md`; `gpg_pillar_learnings.md`). Pepper largely
ignores all this.
→ **Open question:** ship a capability matrix and degrade gracefully (e.g.
client-side batch fan-out via `local_async` + chunked `tgt`, key mgmt only where
`wheel` exists), or simply **declare cherrypy the supported target** and fail fast
with a clear message on tornado? Depends on whether we must support tornado at
all (we run cherrypy in prod).

### 3.2 Timeout semantics
The lowstate `timeout` does **not** bound wall-clock: salt waits `timeout`, then
issues `saltutil.find_job` and waits another `gather_job_timeout` (~10s default),
so a `timeout: 4` call can block ~14s (`cherrypy_learnings.md`). Pepper passes
`timeout` only when it has detected a salt version (`cli.py:797-800`) and uses one
global client-side timeout for the whole poll loop (`cli.py:716`), with no
per-minion budget.
→ **Open question:** how to model and present this to users — a single wall-clock
deadline the client enforces (cancel/abandon on expiry), separate
submit/gather/total budgets, or surfacing salt's own knobs? Needs a coherent
timeout model rather than ad-hoc passthrough.

### 3.3 `/events` is lossy
The SSE stream has no replay across a reconnect gap, no idle heartbeat (can't
distinguish "healthy but quiet" from "dead"), and **dies on master restart**
(`ChunkedEncodingError`) — all confirmed on both modules
(`cherrypy_learnings.md` event-bus robustness; tornado also 406s an
`Accept: text/event-stream` header).
→ **Open question:** how much to lean on `/events` vs treating it as a best-effort
hint and making `/jobs` polling the authoritative source of completion/backfill.
Likely a hybrid (subscribe for latency, reconcile via `/jobs`), but the exact
reconnect/backfill protocol is undecided.

### 3.4 GPG-pillar cold-cache storms
When many minions compile GPG-encrypted pillar with a cold cache, a single
gpg-agent serializes the decrypts and the **whole API can go unresponsive**
(40 concurrent N=1000 compiles → all read-time-out at 120s;
`gpg_pillar_learnings.md`). A naive client that fans out widely and retries
aggressively makes it worse.
→ **Open question:** is this the client's problem to mitigate (concurrency caps,
jittered fan-out, generous/adaptive timeouts, prefer async) or purely a
server/infra concern (pillar caching, secrets backend)? Probably some client-side
politeness regardless, but how much is unclear.

### 3.5 Output formatting depends on Salt being installed
Rich output reuses Salt's outputters and only works if `import salt` succeeds;
otherwise pepper prints raw JSON (`script.py:15-22,109-114`). That's a heavy
dependency for a CLI client and a sharp quality cliff when absent.
→ **Open question:** reimplement the common outputters (highstate, nested, json,
yaml) natively, depend on Salt, or ship a minimal built-in formatter with an
opt-in Salt bridge? Trade-off between fidelity, dependency weight, and
maintenance.

---

## Summary

| # | Finding | Bucket | Direction |
|---|---------|--------|-----------|
| 1 | `full_return: true` for non-wheel clients | ✅ right | keep |
| 2 | Targeting-flag parity with `salt` | ✅ right | keep |
| 3 | `/run` uses eauth token, not session token | ✅ right | keep |
| 4 | Set-diff liveness (in poll path) | ✅ right | generalize |
| 5 | Token cache 0600, no password stored | ✅ right | keep |
| 6 | Flexible exit-code policies | ✅ right | keep (fix scope, #11–12) |
| 7 | Never uses `/events` (dead SSE code) | 🔧 fix | async-first on event bus + `/jobs` backfill |
| 8 | Sync-by-default; polling behind a flag | 🔧 fix | default to async; never trust sync long-cmd result |
| 9 | Shape-sniffing in the output layer | 🔧 fix | one normalized result model in the lib |
| 10 | "200 = success"; weak error classification | 🔧 fix | classifier on status+body; re-auth on 401 |
| 11 | Retcode reads only `result[0]` | 🔧 fix | classify every command/minion entry |
| 12 | Batch vs non-batch shapes diverge (full_return doesn't fix) | 🔧 fix | normalize in client (merge fan-out + unwrap); batch location is a separate trade-off (default server-side) |
| 13 | No connection reuse; dual HTTP + SSL monkeypatch | 🔧 fix | single `requests.Session` |
| 14 | Ctrl-C drops the JID | 🔧 fix | capture & print JID |
| 15 | Liveness via `manage.status` (3008-broken) | 🔧 fix | targeted − returned everywhere |
| 16 | Password stored plaintext in config | 🔧 fix | exec-based `password command` helper |
| 17 | Config/cache ignore XDG | 🔧 fix | `$XDG_CONFIG_HOME` / `$XDG_CACHE_HOME` |
| 18 | Token cache not keyed by profile (#205) | 🔧 fix | key cache by profile/endpoint |
| 19 | `cli.py:635` format bug, optparse, py2 shims | 🔧 fix | f-string, argparse, delete |
| 20 | cherrypy vs tornado portability | ❓ uncertain | capability matrix vs cherrypy-only |
| 21 | Timeout semantics (no wall-clock bound) | ❓ uncertain | coherent client timeout model |
| 22 | `/events` lossy (gap/heartbeat/restart) | ❓ uncertain | hybrid stream + `/jobs` reconcile |
| 23 | GPG-pillar cold-cache storms | ❓ uncertain | how much client-side politeness |
| 24 | Output depends on Salt being installed | ❓ uncertain | native vs Salt-dep vs minimal+bridge |
