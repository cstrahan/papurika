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
