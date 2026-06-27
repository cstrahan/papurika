# salt-api `rest_tornado` — hands-on learnings (vs `rest_cherrypy`)

Practical notes from exercising `rest_tornado` against the same local salt
3008.1 master+minion, framed as a **diff against the cherrypy findings** (see
`../salt-test-cherrypy/cherrypy_learnings.md`). Raw captures in `out/*.json`,
scripts in `scripts/` (paths here are relative to `research/salt-test-tornado/`).

## Test environment

Same image/master/minion/patch as the cherrypy harness, except how the API is
served:

- The image only auto-configures `rest_cherrypy` (:8000). We add a
  `rest_tornado` block on **:8001** via `config/zz-papurika.conf`; `salt-api`
  serves **both modules at once**. This harness targets tornado on :8001 (only
  that port is published).
- tornado config keys differ slightly: `address` (not `host`), plus
  `disable_ssl`, `cors_origin`, `websockets`.
- Same eauth (`pam`, `saltdev`/`saltdev`), same `netapi_enable_clients`, same
  `token_expire`, same token/cache patch (`apply-salt-patch.sh`).

---

## TL;DR — what differs from cherrypy

| area | cherrypy | tornado |
|------|----------|---------|
| **client types** | local, local_async, **local_batch, local_subset**, runner, runner_async, **wheel, wheel_async, ssh** | **only** local, local_async, runner, runner_async |
| key management (`wheel`) | ✅ via `client=wheel` and `/keys` | ❌ **HTTP 400** — not implemented |
| batching (`local_batch`/`subset`) | ✅ | ❌ **HTTP 400** |
| `GET /` clients list | 9 clients | 4 clients |
| `POST /minions` status | **202** + `_links` | **200**, no `_links` |
| `GET /jobs/<jid>` shape | `{info:[meta+Result], return:[{minion:retval}]}` | `{return:[{…meta…, Result:{…}}]}` (no `info`, no bare return) |
| `/run` with `/login` token | **401** (needs raw eauth token) | **200** (session token works) |
| `/events` + `Accept: text/event-stream` | tolerated | **406** — omit the Accept header |
| sync local, no response | `{minion: "Minion did not return…"}` (string) | `{}` (non-responder omitted) |
| no minions matched | `{}` (empty) | `"No minions matched the target…"` (string) |
| **sync `cmd.run 'sleep 300'`** | **blocks for the whole command** (held our 120s client timeout; job keeps running) | **returns empty `{}` at ~10s** (gather timeout; job keeps running) |
| `cmd.run` self-kill (minion suicide) | "Minion did not return…" string @~15s | empty `{}` @~10s |
| `GET /jobs/<jid>` missing jid | `{info: [{…Error…}], return: [{}]}` | `{return: [{…Error…}]}` |
| invalid token on `/` | 401, HTML | 401, JSON `{"return":"Please log in"}` |
| **authz denied** (perm missing) | 401, HTML `"Authorization error occurred."` | **200**, `{"return":["Failed to authenticate"]}` |
| malformed lowstate (missing `fun`/`tgt`) | **500** `"An unexpected error occurred"` | **200**, `"Unexpected exception while handling request: …"` string |
| `full_return` (retcode metadata) | honored (`{ret,retcode,jid}`) | **ignored** on sync local (bare value; retcode lost) |
| extra endpoints | `/keys`, `/logout`, `/stats`, `/ws` | none of those; has `/all_events`, `/formatted_events` (websockets, untested) |

**What's identical:** login/token shape & perms, sync `local` result shapes
(test.ping/cmd.run/cmd.run_bg/state.apply), `runner` result shapes
(cache.grains, manage.status/up, jobs.list_jobs, runner_async `salt/run/` tag),
`/run` with the eauth trio, the event-bus tag/data shapes & job lifecycle, the
token-eviction bug + fix, and `manage.status` being stale for liveness.

---

## The headline: tornado supports far fewer clients

`rest_tornado` keeps its **own hardcoded client registry**, separate from
cherrypy and **not gated by `netapi_enable_clients`**:

```python
# salt/netapi/rest_tornado/saltnado.py:458
self.saltclients = {
    "local": ..., "local_async": ...,
    "runner": ..., "runner_async": ...,
}
```

`GET /` returns exactly these keys (`out/10a_get_root_clients.json`):
```json
{"clients": ["local", "local_async", "runner", "runner_async"], "return": "Welcome"}
```
Every POST runs `_verify_client` (saltnado.py:434); anything not in that dict is
rejected **before dispatch** with `HTTP 400 "Invalid Client: Client not found
in salt clients"`. Confirmed live for `wheel`, `wheel_async`, `local_batch`,
`local_subset`, `ssh` (`out/10_unsupported_*.json`). The source comment
(saltnado.py:253) is explicit: it mimics cherrypy's interface but `wheel (need
asynchronous api…)` was never implemented.

**Consequences (no config or indirect path changes this — it's hardcoded):**
- **Key management is unavailable via tornado.** No `wheel`, and tornado has no
  `/keys` endpoint either. To manage keys you must use `rest_cherrypy` on the
  same master (it supports `wheel`), or `salt-key` CLI / master-side.
- **No server-side batching.** A client must batch itself — chunk the minion
  list and fan out `local_async` with explicit `tgt` subsets.

---

## Endpoint-by-endpoint differences

### `POST /minions` — `out/02b*.json`
Returns **HTTP 200** (cherrypy: 202) and has **no `_links`**. Body is the same
`{"return": [{"jid": …, "minions": […]}]}`.

### `GET /jobs/<jid>` — `out/02c*.json`, `out/09d*.json`
Different structure. tornado puts metadata **and** results together in
`return[0]`, with **no top-level `info`** and **no bare `{minion: retval}`**:
```json
{"return": [{"jid": "...", "Function": "cmd.run", "Arguments": [...],
  "Target": "*", "Target-type": "glob", "User": "saltdev",
  "Minions": ["builtin.minion"], "StartTime": "...",
  "Result": {"builtin.minion": {"return": "async-1\nasync-done",
                                 "retcode": 0, "success": true}}}]}
```
- **Completion detection differs:** `return[0]` is *always* the metadata dict, so
  poll `return[0]["Result"]` becoming non-empty (cherrypy: poll `return[0]`
  itself, which is `{}` until done).
- Result values are `{return, retcode, success}` per minion (cherrypy exposes
  both this under `info[0].Result` *and* the bare value under `return[0]`).

### `/run` — `out/07*.json`
- eauth trio in the body works (same as cherrypy).
- **The `/login` session token WORKS on `/run`** here (`out/07b`, HTTP 200,
  executed) — whereas cherrypy **401'd** the same token. tornado treats `/run`
  almost identically to `/` (both go through `disbatch`, which just needs a
  `token` or creds), and tornado's `/login` token is directly usable as the
  `token` kwarg.

### `/events` — `out/06_events.json`
SSE framing is **identical** to cherrypy (`retry:`/`tag:`/`data:` lines, blank
-line separated; `data` is `{"tag":…, "data":{…}}`). Two operational notes:
- **Do NOT send `Accept: text/event-stream`** — tornado content-negotiates it to
  **406**. Omit `Accept` (the stream is SSE regardless). cherrypy tolerated it.
- tornado does **not replay** events from before the subscription is fully
  established — establish `/events` *first*, then fire (the docs say so). The
  job lifecycle (`…/new` → `…/publish` → `…/ret/<minion>`) is the same.

### Missing endpoints
No `/keys`, `/logout`, `/stats` (cherrypy has all three). tornado instead has
websocket endpoints `/all_events` and `/formatted_events` (enabled via
`rest_tornado: websockets: True`; not exercised here).

---

## Unhappy paths — `out/09*.json`

Same harness as cherrypy (stop the minion, key stays accepted). Differences:

- **Sync `local` to a down minion → `{"return": [{}]}`** (empty; the
  non-responder is simply **omitted**). cherrypy instead returned
  `{minion: "Minion did not return. [No response]…"}` (a string). So on tornado
  you detect non-response purely by **targeted − returned**, never by a string.
  It blocked ~10s here (cherrypy ~14s) — still longer than the `timeout: 4`.
- **No minions matched → `{"return": ["No minions matched the target. No command
  was sent, no jid was assigned."]}`** (a **string** in the slot). cherrypy
  returned `{}`. So the two modules **invert** how they signal these:

  | situation | cherrypy | tornado |
  |-----------|----------|---------|
  | target matched nothing | `{}` | string "No minions matched…" |
  | matched, no response | string "Minion did not return…" | `{}` |

- `manage.status` is **stale** (reports `up` while the minion is dead) — same
  unreliability as cherrypy. Async `/jobs/<jid>` keeps the down minion out of
  `Result` (consistent with the tornado job shape above).

---

## Self-killing & long-running commands — `out/11*.json`

Same three sync `local` cases as the cherrypy harness; the **sleep-300 case is
the big divergence**.

- **`cmd.run` self-kill** (`pkill -9 -f salt-minion`): tornado returns
  **empty `{}` at ~10s** (cherrypy: the "Minion did not return…" string at
  ~15s). Minion auto-restarts; the job's return is lost either way.
- **sync `cmd.run 'sleep 300'`**: tornado returns **empty `{}` at ~10s** — it
  caps the local gather at ~`gather_job_timeout` and gives up, while the job
  keeps running on the minion (`jobs.active` shows it). cherrypy instead
  **blocks for the entire command** (held our 120s client timeout). So:
  - cherrypy = *slow but truthful* (connection held until the job finishes — a
    worker-thread hazard);
  - tornado = *fast but lossy* (a silent empty `{}` at ~10s; the long command is
    silently orphaned as a running job).
  This is the same ~10s gather-timeout behavior seen in the GPG-pillar
  silent-incomplete finding (`../salt-test-gpg-pillar/`).
- **`cmd.run_bg 'sleep 300'`**: identical to cherrypy — instant return (~0.2s)
  with `{minion: {pid}}`.

**Takeaway:** on tornado, a sync long/blocking command yields a misleading empty
200 after ~10s (not an error, not the result), with the job still running. Use
`cmd.run_bg` or `local_async` + `/jobs`/`/events`; never infer success/failure
from a sync long-command response.

---

## Failure / outcome taxonomy — `out/14*.json`

Full matrix + client guidance in `cherrypy_learnings.md` ("Failure / outcome
taxonomy"). tornado-specific behavior worth calling out:
- **Invalid token** → 401 with **JSON** `{"status":"401 Unauthorized","return":
  "Please log in"}` (cherrypy serves HTML).
- **Authz denial is a `200`** with `{"return":["Failed to authenticate"]}` — a
  status-only client treats a permissions denial as success. (cherrypy 401s.)
  Note tornado labels *both* authn and authz failures inside `disbatch` as
  `"Failed to authenticate"`.
- **Malformed lowstate** (missing `fun`/`tgt`) → `200` with `"Unexpected
  exception while handling request: …"` in the `return` array (cherrypy 500s).
- **`full_return` is ignored** on sync local (the bare minion value comes back;
  `cmd.run 'exit 3'` → `""`, retcode gone). Use `cmd.run_all` (`{retcode,
  stdout, stderr}`, works on both) or the job cache for retcode.
- Unknown function and failed-state shapes match cherrypy (200 + per-minion
  `"'x' is not available."` / `result:false`).

---

## jid lifecycle — `out/12*.json`

Mostly netapi-agnostic — see the full writeup in `cherrypy_learnings.md`
("jid lifecycle: find, cancel, retention"). Verified identical on tornado:
`jobs.lookup_jid` (bare per-minion return; `{}` for both missing *and* pending),
`jobs.print_job` (metadata+Result; explicit `Error` for missing),
`saltutil.kill_job` (`"Signal 9 sent to job <jid> at pid <pid>"`; `""` for
missing), and job-cache retention (`keep_jobs_seconds`, disk-backed local_cache,
survives master restart).

The only difference is the `GET /jobs/<jid>` envelope (as everywhere): tornado
returns `{return: [{…metadata…, Result}]}` (and `{return: [{…Error…}]}` for a
missing jid), with no top-level `info`.

---

## Same as cherrypy (verified, not re-detailed)

- **Auth:** `/login` returns the same `{token, expire, start, user, eauth,
  perms}` shape; `X-Auth-Token` header auth identical.
- **Sync `local`:** `test.ping`→`true`, `cmd.run`→stdout string,
  `cmd.run_bg`→`{pid}`, `state.apply`→full highstate — byte-for-byte the same
  shapes (`out/01*.json`).
- **`runner`:** `cache.grains`, `manage.status` (`{up,down}`), `manage.up`
  (bare list), `jobs.list_jobs`; `runner_async`→`{tag:"salt/run/<jid>", jid}`
  (`out/04*.json`). (`full_return` knob behaves the same — see the cherrypy doc.)
- **Token eviction bug:** present identically; the patch fixes it (token
  survived ~120s across sweeps, `08_token_longevity.py`).

---

## Implications for the client (papurika)

1. **The two netapi modules are NOT interchangeable.** tornado lacks
   wheel/batch/subset/ssh entirely. A client supporting tornado must (a) route
   key management to cherrypy or CLI, and (b) implement batching client-side.
   Worth surfacing a capability matrix and failing fast with a clear message on
   `400 Invalid Client`.
2. **Normalize `/jobs/<jid>` across both shapes** (cherrypy `info`+bare `return`
   vs tornado `return[0].Result`), and detect completion per-module.
3. **`/minions` status differs (202 vs 200)** — don't key logic on 202.
4. **`/events`: don't send `Accept: text/event-stream`** (406 on tornado), and
   subscribe before firing.
5. **Non-response/no-match signaling is inconsistent across modules** (string vs
   empty, and *inverted* between them) — fold into one explicit status, never
   parse the human strings.
6. **`/run` token semantics differ** — the session token works on tornado but
   not cherrypy; for portability use the eauth trio on `/run`.
