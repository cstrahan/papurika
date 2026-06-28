# pepper design analysis (language-agnostic)

The **architectural, behavioral, and protocol-level** lessons from the official
SaltStack `pepper` CLI that apply to *any* alternative salt-api client —
including ours (papurika), which we're writing in **Go**. This is a distillation
of [`pepper_analysis.md`](pepper_analysis.md) with the Python-implementation
critiques removed: argument-parser choice, the dual urllib/requests transport,
the global SSL monkeypatch, Python-2 vestiges, a string-format bug, and the reuse
of Salt's Python outputters *as a library* are all real, but they only matter if
we contribute fixes back to pepper itself — they don't carry over to a Go client.
Keep `pepper_analysis.md` for that source-grounded detail.

Findings are sorted into three buckets:

1. **[What pepper gets right](#1-what-pepper-gets-right)**
2. **[Wrong, with an obvious best-in-class fix](#2-wrong-with-an-obvious-fix)**
3. **[Wrong, but the best approach is still uncertain](#3-wrong-but-uncertain)**

The salt-api behaviours referenced here are documented in our harness learnings:
[`salt-test-cherrypy/cherrypy_learnings.md`](salt-test-cherrypy/cherrypy_learnings.md),
[`salt-test-tornado/tornado_learnings.md`](salt-test-tornado/tornado_learnings.md),
[`salt-test-gpg-pillar/gpg_pillar_learnings.md`](salt-test-gpg-pillar/gpg_pillar_learnings.md),
and [`minion-status-and-liveness.md`](minion-status-and-liveness.md).

---

## Behavioral shape

Pepper is a thin CLI over salt-api's `POST /` lowstate interface. It logs in
(session token, or an eauth token for `/run`), builds a single lowstate command
from CLI args, POSTs it, then formats the response and maps minion `retcode`s to
a process exit code. It is **synchronous request/response by default**, with an
**opt-in polling mode** for waiting on all minion returns, and it reuses Salt's
own output formatters when Salt is present. It never touches the `/events`
stream. Those four shape choices (sync-default, opt-in polling, no event bus,
formatter-coupled output) drive most of the findings below.

---

## 1. What pepper gets right

- **Always sends `full_return: true`** (for non-wheel clients). This normalizes
  `local`/`runner` into the same `{…, "return": <value>, "retcode"/"success": …}`
  envelope `wheel` already uses, and surfaces `retcode` for exit-code mapping.
  (See "full_return" in `cherrypy_learnings.md`.)
- **Targeting parity with the `salt` CLI**: the full set of target selectors
  (glob / PCRE / list / grain / grain-PCRE / pillar / pillar-PCRE / range /
  compound / nodegroup) maps to the right `tgt_type`. Familiar, low friction.
- **`/run` uses an eauth token, not the session token.** This avoids the cherrypy
  trap where a `/login` session token 401s on `/run` (raw eauth tokens work on
  both modules — `cherrypy_learnings.md` "/run").
- **Liveness (in the polling path) is computed by set difference**: non-responders
  = `targeted − returned`, using the `minions` list from the async submit — not by
  scraping English strings. This is the correct technique
  (`minion-status-and-liveness.md`); it's just gated behind a flag (§2.2).
- **The token-cache file is written with secure permissions** (owner-only). Good
  hygiene any client should copy; the broader secret story is still poor (§2.9).
- **Flexible exit-code policies**: "fail if any failed" vs "fail only if all
  failed", with/without treating "no retcode" as failure. A useful scripting
  surface (its implementation only covers the first command — §2.5).
- **Broad eauth backend support** (pam/ldap/file/kerberos/…), and a **test suite
  that exercises both `rest_cherrypy` and `rest_tornado`** — the right instinct,
  since the two modules diverge (§3.1).

---

## 2. Wrong, with an obvious fix

### 2.1 Never uses the event bus
Pepper never subscribes to `/events`; its only "async" is job-cache polling.
→ **Be async-first on `/events`**: subscribe *before* firing `*_async`, match
returns by `salt/job/<jid>/ret/<minion>`, and reconcile completion against
`/jobs`. Polling is the fallback, not the primary mechanism.
(`cherrypy_learnings.md` event-bus pattern; `/events` is lossy — see §3.3.)

### 2.2 Synchronous by default; waiting is opt-in
The default path is a blocking sync `POST /`; the polling/“wait for all returns”
path is only enabled by a flag. For long commands this is a real hazard: on
cherrypy a sync call ties up a server worker for the full duration; on tornado it
returns a **misleading empty `{}`** at the ~10s gather timeout while the job keeps
running (`cherrypy_learnings.md`; `tornado_learnings.md`; `gpg_pillar_learnings.md`).
→ **Default to async submit + event/job reconciliation**; reserve true sync for
short interactive calls, and never infer success/failure from a sync
long-command response.

### 2.3 Response normalization lives in the output layer
Pepper branches on response shape inline while formatting (tornado's nested
`return` vs cherrypy's `ret` vs bare, plus a separate runner/wheel unfold), so the
normalization is entangled with output and with whether Salt is importable.
→ **Produce one internal result model in a transport/result layer**, before any
formatting — keyed on client type and netapi module. The headline lesson:
"the response envelope shape varies by client type; a clean client should
normalize these into one result model" (`cherrypy_learnings.md`).

### 2.4 Treats the response as trustworthy; weak error classification
Pepper raises only on a couple of HTTP statuses and otherwise assumes the body is
a valid result. But **HTTP 200 ≠ success**: a 200 routinely carries authz
denials, unknown-function errors, non-zero retcodes, failed states, and (on
tornado) malformed-request exception strings. Error shapes also diverge — cherrypy
returns HTML 401s, tornado returns JSON; authz denial is a 401 on cherrypy but a
**200 with `"Failed to authenticate"`** on tornado (`cherrypy_learnings.md`
failure taxonomy; `tornado_learnings.md`).
→ **Build a real response classifier** keyed on status *and* body — authn-failure
/ authz-denial / not-found / minion-error / command-failure / no-response /
success — and don't assume errors are JSON. **Re-authenticate on 401** (tokens
expire; unpatched 3008 evicts them).

### 2.5 Exit-code logic only inspects the first command
salt-api accepts a command *array* with no stop-on-failure, but pepper's retcode
validation only looks at the first command's result. A multi-command job can fail
yet exit `0`.
→ **Classify every command entry and every minion entry**, not just the first.

### 2.6 No connection reuse; global TLS toggle
Pepper opens a fresh connection per request (no pooling) and its
“ignore-SSL-errors” switch flips *process-global* TLS state rather than scoping to
the one connection.
→ **Use a connection-pooling HTTP client** with **per-client/per-connection TLS
config** (never global). One consistent transport and error path.

### 2.7 Ctrl-C / timeout drops the JID
On interrupt, pepper exits without printing the job id — the job is still running
on the master, but the operator has nothing to reconnect with.
→ **Capture and print the JID** on interrupt and on any client-side timeout, so
the user can look it up (`jobs.lookup_jid`) or resubscribe later.

### 2.8 Don't trust `manage.status` for liveness
`manage.status/up/down` is broken in 3008 (reports every key-accepted minion as
`up`; see [`minion-status-and-liveness.md`](minion-status-and-liveness.md) §5).
Pepper already does the right thing in its poll path.
→ **Use targeted − returned everywhere**; treat a minion alive only on an actual
`true` return, never on `manage.status`.

### 2.9 Password stored in plaintext in the config file
The config holds `SALTAPI_PASS` in clear text; there's no way to source the secret
elsewhere short of typing it every invocation.
→ **Add a `password command` option** (an exec-based credential helper, à la git's
`credential.helper`, mbsync's `PassCmd`): the client runs the command and reads
the secret from its output. That keeps the secret out of the config file and lets
it come from a password manager, the OS keychain, a GPG-encrypted file, etc., and
the helper can prompt or require a biometric/touch assertion to unlock. A plain
inline password can remain as a discouraged fallback.

### 2.10 Config and cache ignore the XDG Base Directory spec
Config and token cache are dotfiles directly in `$HOME` (and pepper is even
inconsistent with itself — its *master* config already lives under `~/.config/`).
→ **Follow XDG**: config under `$XDG_CONFIG_HOME`, cache under `$XDG_CACHE_HOME`,
honoring the env vars with standard fallbacks; keep reading legacy paths for
back-compat.

### 2.11 Token cache isn't keyed by profile
The cache is a single fixed path used regardless of which config *profile* is
selected — even though profiles point at different masters/users. A token minted
for one profile is read back for another, against the wrong master. (Upstream
[pepper#205](https://github.com/saltstack/pepper/issues/205).)
→ **Key the cache by profile** (ideally master URL + user + eauth) — a per-profile
entry, or a single store indexed by an endpoint key. Never share one token across
endpoints.

---

## 3. Wrong, but uncertain

The problem is clear; the best approach involves real trade-offs and needs a
deliberate decision.

### 3.1 Cross-module portability (cherrypy vs tornado)
The two netapi modules diverge widely: tornado supports only `local`/
`local_async`/`runner`/`runner_async` (no `wheel`, `local_batch`, `local_subset`,
`ssh`), has no server-side batching, ignores `full_return` on sync local, inverts
no-match vs no-response signaling, and returns empty-200 on gather timeout
(`tornado_learnings.md`; `gpg_pillar_learnings.md`).
→ **Open question:** ship a capability matrix and degrade gracefully (client-side
batch fan-out via `local_async` + chunked `tgt`; key management only where `wheel`
exists), or **declare cherrypy the supported target** and fail fast on tornado?
We run cherrypy in prod, which argues for the latter — but worth deciding
explicitly.

### 3.2 Timeout semantics
The lowstate `timeout` does **not** bound wall-clock: salt waits `timeout`, then
issues `saltutil.find_job` and waits another `gather_job_timeout` (~10s), so a
short `timeout` can still block ~14s (`cherrypy_learnings.md`).
→ **Open question:** what timeout model to expose — a single client-enforced
wall-clock deadline (cancel/abandon on expiry), separate submit/gather/total
budgets, or surfacing salt's own knobs? Needs a coherent model, not ad-hoc
passthrough.

### 3.3 `/events` is lossy
The SSE stream has no replay across a reconnect gap, no idle heartbeat (can't tell
"healthy but quiet" from "dead"), and dies on master restart — on both modules
(`cherrypy_learnings.md` event-bus robustness).
→ **Open question:** how much to lean on `/events` for latency vs treating it as a
best-effort hint and making `/jobs` polling the authoritative source of
completion/backfill. Likely a hybrid, but the reconnect/backfill protocol is
undecided.

### 3.4 GPG-pillar cold-cache storms
When many minions compile GPG-encrypted pillar with a cold cache, a single
gpg-agent serializes the decrypts and the whole API can go unresponsive
(`gpg_pillar_learnings.md`). A client that fans out widely and retries hard makes
it worse.
→ **Open question:** how much is the client's job to mitigate (concurrency caps,
jittered fan-out, generous/adaptive timeouts, prefer async) vs a server/infra
concern (pillar caching, secrets backend)? Probably some client-side politeness
regardless, but how much is unclear.

### 3.5 Output formatting: fidelity vs dependency
Pepper gets rich output by reusing Salt's own (Python) outputters, with a sharp
quality cliff when Salt isn't present. A non-Python client can't reuse them at
all.
→ **Open question:** which outputters to reimplement natively (highstate, nested,
json, yaml, …) and how closely to match Salt's formatting — i.e. how much fidelity
is worth the maintenance. The dependency dilemma is forced on us; the scope
decision isn't.

---

## Summary

| # | Finding | Bucket | Direction |
|---|---------|--------|-----------|
| 1 | Always sends `full_return: true` | ✅ right | keep |
| 2 | Targeting parity with `salt` | ✅ right | keep |
| 3 | `/run` uses eauth token, not session token | ✅ right | keep |
| 4 | Set-diff liveness (in poll path) | ✅ right | generalize |
| 5 | Secure token-cache file permissions | ✅ right | keep |
| 6 | Flexible exit-code policies | ✅ right | keep (fix scope, #10) |
| 7 | Tests both netapi modules | ✅ right | keep |
| 8 | Never uses `/events` | 🔧 fix | async-first on event bus + `/jobs` backfill |
| 9 | Sync-by-default; waiting opt-in | 🔧 fix | default to async; never trust sync long-cmd result |
| 10 | Normalization in the output layer | 🔧 fix | one normalized result model in a transport layer |
| 11 | "200 = success"; weak error handling | 🔧 fix | classify on status+body; re-auth on 401 |
| 12 | Exit code reads only first command | 🔧 fix | classify every command/minion entry |
| 13 | No connection reuse; global TLS toggle | 🔧 fix | pooled HTTP client; per-connection TLS |
| 14 | Ctrl-C / timeout drops the JID | 🔧 fix | capture & print JID |
| 15 | Liveness via `manage.status` (3008-broken) | 🔧 fix | targeted − returned everywhere |
| 16 | Password plaintext in config | 🔧 fix | exec-based `password command` helper |
| 17 | Config/cache ignore XDG | 🔧 fix | `$XDG_CONFIG_HOME` / `$XDG_CACHE_HOME` |
| 18 | Token cache not keyed by profile (#205) | 🔧 fix | key cache by profile/endpoint |
| 19 | cherrypy vs tornado portability | ❓ uncertain | capability matrix vs cherrypy-only |
| 20 | Timeout semantics (no wall-clock bound) | ❓ uncertain | coherent client timeout model |
| 21 | `/events` lossy (gap/heartbeat/restart) | ❓ uncertain | hybrid stream + `/jobs` reconcile |
| 22 | GPG-pillar cold-cache storms | ❓ uncertain | how much client-side politeness |
| 23 | Output formatting fidelity vs dependency | ❓ uncertain | which outputters to reimplement natively |
