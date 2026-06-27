# salt-api `rest_cherrypy` — hands-on learnings

Practical notes from exercising the `rest_cherrypy` netapi against a local
salt 3008.1 master+minion (the version we run in prod). Everything here was
observed in practice; raw, untruncated request/response captures live in
`out/*.json` and the driver scripts in `scripts/` (paths in this doc are
relative to `research/salt-test-cherrypy/`).

The headline takeaway for building a client: **the response envelope shape
varies by client type** (local vs runner vs wheel vs async vs the `/minions`
and `/jobs` convenience URLs). A clean client should normalize these into one
result model. Details in [Response shapes](#response-shapes-the-important-part).

---

## Test environment

- **Image:** `ghcr.io/cdalvaro/docker-salt-master:3008.1` (matches prod's salt
  version). Runs salt-master + a builtin salt-minion (`builtin.minion`) in one
  container under supervisord. Ships salt as a relenv "onedir" with its own
  Python 3.14 at `/opt/saltstack/salt/bin/python3`.
- **netapi module:** `rest_cherrypy` (the image default, and what prod runs).
  Served as **plain HTTP on :8000** (`SALT_API_DISABLE_SSL=True`) to match
  prod's `disable_ssl: True`.
- **Auth:** the image creates an OS/PAM user from `SALT_API_USER`/`PASS`
  (`saltdev`/`saltdev`). eauth backend `pam` works here (salt-api can read
  shadow in this image). Prod uses the `file` backend, but the eauth backend
  does not change the API request/response shapes.
- **Config we had to supply** (image sets neither): `netapi_enable_clients`
  (Salt 3006+ denies all clients without it — post CVE-2024-22231) and
  `external_auth` (perms for the pam user). See `config/zz-papurika.conf`.
- **Patch applied:** the prod token/cache fix (saltstack `063aceb387`) — see
  [Token eviction bug](#token-eviction-bug-confirmed-and-fixed). Reproduce with
  `up.sh` + `apply-salt-patch.sh`.

Reproduce: `cd research/salt-test-cherrypy && bash up.sh && bash apply-salt-patch.sh`,
then `uv run scripts/NN_*.py`.

---

## 3006 → 3008 changes (cherrypy; 3006 as baseline)

We ran the full harness against a **3006.9** container in parallel (`out-3006/`)
and structurally diffed it against the 3008 `out/` (`compare_out_dirs.py`).
**Reassuring headline: the core API response schemas are unchanged 3006→3008** —
`GET /` client list and the sync-local / async / batch / runner / wheel / jobs /
failure-taxonomy / multi-command / `/run` / `mk_token` shapes are all
structurally identical. Only four real deltas:

1. **New `__pub_*` resource-targeting kwargs (3008-only, additive).** Every local
   execution's injected kwargs gain `__pub_resource_targets` (list),
   `__pub_pure_resource_target` (bool), `__pub_minion_is_target` (bool) — visible
   e.g. in `test.arg`'s returned `kwargs` and in published job data.
   Source-confirmed 3008-only (the new `salt/utils/resources.py` "resource
   registry"; **0** refs in 3006, present in 3008). A tolerant client just
   ignores unknown `__pub_*` keys.
2. **Grain additions.** 3008 adds version/packaging grains absent in 3006 —
   `ca_truststore` (source-confirmed 3008-only), `package` (e.g. `"onedir"`),
   `virtual_subtype`, `fibre_channel_host`. (`zfs_*` and similar that appear only
   in 3006 here are host-detection grains, not version-significant.) Matters only
   if you key on the grains schema.
3. **Wheel / `full_return` envelope dropped `_stamp`.** 3006 wheel responses
   include `data._stamp`; 3008 omits it (`data` = `fun/jid/tag/user/return/
   success`). Cosmetic — don't depend on `_stamp`.
4. **`manage.status`/`up`/`down` regression** (the significant behavioural one):
   3006 reports a dead minion in `down` (correct); 3008 reports it in `up`
   (`down` empty). Confirmed here (19a/b/c) and bisected in
   [`../minion-status-and-liveness.md`](../minion-status-and-liveness.md) §5.
5. **Token-eviction regression** (separate, the one prod patches): `08` token
   longevity run on both (`out/` and `out-3006/`) — a single token reused across
   ~120s of maintenance sweeps survived on **3006** (`survived_all: true`) and on
   **patched 3008**. 3008 *unpatched* evicts within ~60s (the bug fixed by the
   prod `clean_expired`/`mk_token` patch). So token retention is another 3006→
   3008 regression — already fixed in our 3008 by the patch, and absent in 3006.
   Bisected to commit `78685e5c44` (token storage moved onto `salt.cache`),
   first shipped in 3008.0 — see [Token eviction bug](#token-eviction-bug-confirmed-and-fixed).

**Debunked / not-changes:**
- The **`sleep 300` "hang"** seen mid-run on 3006 was a *harness artifact*, not a
  version difference: script 05 left `builtin.minion` **unaccepted** on 3006, so
  script 11's sync `sleep 300` targeted a half-broken minion and the gather
  stalled. With a healthy minion, 3006 sync-local behaves **identically** to 3008
  (blocks for the command's duration; the `requests` client read-timeout fires
  normally, including over HTTPS).
- The 09 / 17 / 18 capture diffs are **minion-liveness state differences** between
  the two runs (the 3006 minion was responding where the 3008 capture had it
  down), not schema changes — same shapes.

**Caveats:** the 3006 test minion was unstable in the throwaway container
(frequent drops — most likely key-churn from re-running the destructive scripts,
not necessarily a salt-version trait), so a few `out-3006/` captures are degraded;
the four deltas above are confirmed via source and the clean captures. The 3006
image has no `disable_ssl` knob, so its API is **HTTPS** — `_common` gained
`PAPURIKA_VERIFY_SSL=0` (and prod is HTTPS too, so this is reusable).

**Bottom line for the client:** schema-wise, **3006 and 3008 are
interchangeable** — the deltas are additive (`__pub_*`, grains) or cosmetic
(`_stamp`), plus the known `manage.status` regression. Build the client to
ignore unknown keys, not depend on `_stamp`, and not trust `manage.status` for
liveness, and it works on either version. The 3006-vs-3008 decision can be made
on observability/stability grounds, not API-compatibility fear.

---

## Auth (`/login`)

`POST /login` with `{username, password, eauth}` returns a **session token**:

```json
{"return": [{
  "token": "ff22c6b2...","start": 1782516808.93,"expire": 1782560008.93,
  "user": "saltdev","eauth": "pam","perms": [".*","@jobs","@runner","@wheel"]
}]}
```

- Send the token as the **`X-Auth-Token` header** on subsequent requests (also
  settable as a session cookie). `perms` reflects the eauth ACL: `.*` = all
  execution modules, `@runner`/`@wheel`/`@jobs` = those client families.
- `expire` is honored as the token lifetime (we set `token_expire: 43200` = 12h,
  matching prod).
- **`GET /`** (unauthenticated) returns `{"return": "Welcome", "clients":
  [...]}`. That `clients` list is the **statically supported** set (it includes
  `ssh` even though we didn't enable it) — it is **not** the gated
  `netapi_enable_clients` allowlist. Don't use it to discover enabled clients.

---

## The request model

`POST /` takes a **JSON array of lowstate dicts**; the response `return` is a
parallel array, one entry per command. Each command picks a `client`:

| client | tgt? | what it does | sync/async |
|--------|------|--------------|------------|
| `local` | yes | run execution fn on minions (≈ `salt`) | sync (blocks) |
| `local_async` | yes | same, returns a jid immediately | async |
| `local_batch` | yes | local, throttled via `batch` | sync (streamed) |
| `local_subset` | yes | local on a random subset | sync |
| `runner` | no | master-side runner (≈ `salt-run`) | sync |
| `runner_async` | no | runner, returns a jid | async |
| `wheel` | no | master-side wheel (keys, config) | sync |
| `wheel_async` | no | wheel, returns a jid | async |

Common fields: `tgt`, `tgt_type` (glob/list/grain/compound/pcre/…), `fun`,
`arg` (positional list), `kwarg` (named dict), `batch`, `timeout`.

---

## Response shapes (the important part)

All bodies are `{"return": [ ... ]}` at the top, but the **inner shape differs
by client**. Captured verbatim:

### `local` (sync) — `out/01*.json`
`return[0]` is `{minion_id: <function return value>}`. The return value is
whatever the function yields:
- `test.ping` → `true`
- `cmd.run` → the **stdout string**: `"blocking-cmd\ndone"`
- `cmd.run_bg` → `{"pid": 719}` (fires and returns immediately — NOT output)
- `state.apply` → the full highstate dict, keyed by `state_|-id_|-name_|-fn`,
  each with `result`, `changes`, `comment`, `duration`, `__run_num__`, `__sls__`

```json
{"return": [{"builtin.minion": "blocking-cmd\ndone"}]}            // cmd.run
{"return": [{"builtin.minion": {"pid": 719}}]}                    // cmd.run_bg
```

### `local_async` (via `/`) — `out/02a*.json`
`return[0]` is `{"jid": "...", "minions": [...]}`. HTTP 200. Results fetched
later from `/jobs/<jid>`.

### `POST /minions` — `out/02b*.json`
Forces `local_async`. **HTTP 202**, and adds a hint:
```json
{"return": [{"jid": "...", "minions": ["builtin.minion"]}],
 "_links": {"jobs": [{"href": "/jobs/20260626233617809433"}]}}
```

### `GET /jobs/<jid>` — `out/02c*.json`
While running, `return[0]` is `{}` (empty) and `info[0].Result` is `{}`. Once
complete, **both** populate:
```json
{"info": [{"jid": "...","Function":"cmd.run","Arguments":[...],"Target":"*",
           "Target-type":"glob","User":"saltdev","Minions":["builtin.minion"],
           "StartTime":"...",
           "Result": {"builtin.minion": {"return":"async-1\nasync-done",
                                          "retcode":0,"success":true}}}],
 "return": [{"builtin.minion": "async-1\nasync-done"}]}
```
**Completion check:** poll until `return[0]` is non-empty (or `info[0].Result`
populated). Caveat: each `GET /jobs/<jid>` itself spawns a `runner.jobs.list_job`
job (visible in the job cache) — polling is not free.

### `GET /jobs` — `out/02d*.json`
`return[0]` is `{jid: {Function, Arguments, Target, Target-type, User,
StartTime}}` for recent jobs (metadata only, no returns).

### `local_batch` — `out/03*.json`
Same body shape as sync `local`: `return[0]` = `{minion: <retval>}`. (The
pepper-skill warning about batch nesting differently did **not** reproduce here
— likely a `full_return`/CLI artifact, not a cherrypy-over-the-wire thing.)
`batch` accepts a count (`"1"`) or percentage (`"50%"`).

### `runner` (sync) — `out/04*.json`
`return[0]` is the runner's **bare return value, NOT wrapped**:
- `cache.grains tgt=*` → `{minion: {<grains>}}` (read from master cache, no
  minion traffic — fast host discovery)
- `manage.status` → `{"up": [...], "down": [...]}` (this one DOES ping minions)
- `manage.up` → `[ [...] ]` (a bare list)
- `jobs.list_jobs` → `{jid: {...metadata}}`

### `runner_async` — `out/04e*.json`
`return[0]` = `{"tag": "salt/run/<jid>", "jid": "<jid>"}`. Note the
**`salt/run/`** tag prefix (vs `salt/job/` for local).

### `wheel` (sync) — `out/05*.json` — **WRAPPED differently!**
`return[0]` is the full event envelope; the real payload is at
**`return[0].data.return`**:
```json
{"return": [{"tag": "salt/wheel/<jid>",
  "data": {"fun":"wheel.key.list_all","jid":"...","tag":"salt/wheel/...",
           "user":"saltdev","success": true,
           "return": {"minions":["builtin.minion"],"minions_pre":[],
                      "minions_rejected":[],"minions_denied":[],
                      "local":["master.pem","master.pub"]}}}]}
```

> **Normalization implication:** by default sync `runner` returns the value
> directly at `return[0]`, but sync `wheel` buries it at `return[0].data.return`
> with a `success` flag alongside. A client must branch on client type (or
> detect the `{tag, data:{…}}` envelope). This is exactly the kind of shape
> divergence the client should hide behind one result type.

### `full_return: true` — the consistency knob (resolved)

Adding `"full_return": true` to a lowstate command normalizes the envelope and
exposes `retcode`:

| client | default `return[0]` | with `full_return: true` |
|--------|---------------------|--------------------------|
| `local` | `{minion: <retval>}` | `{minion: {"ret": <retval>, "retcode": 0, "jid": "..."}}` |
| `runner` | `<bare retval>` | `{"tag": "salt/run/<jid>/ret", "data": {"fun","jid","user","fun_args","_stamp","return": <retval>, "success": true}}` |
| `wheel` | **already** `{"tag":"salt/wheel/<jid>","data":{...,"return":<retval>,"success":...}}` | same (wheel is always wrapped) |

So **`wheel`'s default is the `full_return` shape**; `runner`/`local` only wrap
when asked. Captured live:
```json
{"return": [{"builtin.minion": {"ret": true, "retcode": 0, "jid": "..."}}]}   // local + full_return
{"return": [{"data": {"fun":"runner.manage.status","return":{"up":[...],"down":[]},"success":true,...},
             "tag":"salt/run/<jid>/ret"}]}                                     // runner + full_return
```

**Recommendation for the client:** always send `full_return: true`. It gives a
uniform `{…, "return": <value>, "retcode"/"success": …}` envelope across local
(per-minion) and runner/wheel, and surfaces `retcode` (needed for exit-code
mapping). This is why pepper hard-codes `full_return=True` for non-wheel
clients.

---

## `cmd.run` vs `cmd.run_bg` (the two execution styles)

- **`cmd.run`** — blocking. The minion runs the command and returns its stdout;
  the value comes back as the minion's return string. The *synchronous* HTTP
  call blocks for the whole command duration.
- **`cmd.run_bg`** — fire-and-forget. The minion launches the command detached
  and immediately returns `{"pid": N}`. No output is ever captured by salt.
- In a **state**, there is no `cmd.run_bg` state function; drive it via
  `module.run` (`name: cmd.run_bg`), which returns `{"changes": {"ret":
  {"pid": N}}}`. See `srv/salt/demo.sls` and `out/01e_state_apply_demo.json`.

Client-design angle: `cmd.run_bg` is "launch and walk away" (you get a PID, not
a result); to actually track a long command you'd use `local_async` + `/jobs`
or the event bus, not `cmd.run_bg`.

---

## Key management lifecycle (`wheel key.*`) — `out/05*.json`

Accepting a pending key is a **`wheel` `key.accept`** call. (The `/keys` POST
endpoint is different — it *generates + auto-accepts a brand-new* key and
returns a tarball; it does not accept an already-pending key.)

Observed full lifecycle (script `05_wheel_keys.py`):
1. `key.list_all` → `{minions, minions_pre, minions_rejected, minions_denied,
   local}` (accepted / pending / rejected / denied / master keys).
2. `key.finger match=*` → `{minions: {id: sha256-colon-hex}, local: {...}}`.
3. `key.delete match=<id>` → `{}` (removes from accepted).
4. **Gotcha:** a *live, connected* minion does **not** re-key just because its
   key was deleted — it keeps its session. We had to **restart the minion**
   (`supervisorctl restart salt-minion`) to make it re-submit its pubkey, which
   then landed in `minions_pre` (pending).
5. `key.accept match=<id>` → `{"minions": ["<id>"]}`.
6. After accept, the minion needs a few seconds to reconnect; the first
   `test.ping` may time out the HTTP client before the minion answers (see
   below).

---

## Event bus (`/events`) — `out/06_events.json`

SSE stream (`Content-Type: text/event-stream`). Records are blank-line
separated, each with `tag:` and `data:` lines; `data` is JSON
`{"tag": ..., "data": {...}}`. The job lifecycle for one `local_async cmd.run`:

```
salt/job/<jid>/new       data.data = {jid, tgt, tgt_type, user, fun, arg, minions, missing, _stamp}
salt/job/<jid>/publish   data.data = {fun, arg, tgt, jid, ret, tgt_type, user, _stamp}
salt/job/<jid>/ret/<mid> data.data = {cmd:"_return", id, success, return, retcode, jid, fun, fun_args, user, _stamp, ts}
```

The recommended scalable async pattern (confirmed shape): **subscribe to
`/events` first, then fire `*_async` jobs, and match returns by
`salt/job/<jid>/ret/<minion>`** — no per-job synchronous connection, and you
get the actual return value in the `ret` event. Other tag prefixes:
`salt/run/<jid>` (runners), `salt/wheel/<jid>` (wheel).

---

## `/run` (stateless) — `out/07*.json`

`/run` bypasses sessions; each request self-authenticates. Otherwise identical
to `POST /`.

- **eauth trio in the body works:** `{client, tgt, fun, username, password,
  eauth}` → normal result. Good fit for CI/one-shot (prod's `gitlab_ci` user).
- **The `/login` session token does NOT work on `/run`** — neither in the body
  (`token: <session token>`) nor as an `X-Auth-Token` header; both returned
  **401**. The same token works fine as `X-Auth-Token` on `/`. Per the docs,
  `/run`'s `token` must be a *raw salt eauth token* (e.g. minted via the
  `auth.mk_token` runner), which is a different thing from the session token.
  (Minting/using a raw eauth token was not exercised.)

---

## Token eviction bug (confirmed and fixed)

Before patching, a freshly-minted token (with `token_expire: 43200`) was
evicted within ~30s, and multi-step scripts died mid-run. Master log:

```
Token '...' could not be loaded (TokenExpiredError('')); removing it from the store.
Authentication failure of type "token" occurred.
```

This is exactly the prod bug fixed by saltstack `063aceb387` (the generic
`Cache.clean_expired` fallback used file mtime as the expiry and swept all
tokens every maintenance pass). We applied the fix to the container's bundled
salt (`apply-salt-patch.sh`) and restarted the master. Confirmed fixed: a
single token reused across ~120s (multiple maintenance sweeps) kept returning
HTTP 200 (`08_token_longevity.py`):

**Root cause — introduced in 3008.0 by a single commit.** Both halves of
`token-cache-fix.patch` trace to saltstack commit
**`78685e5c44`** *"Provide token storage using the salt.cache interface"*
(Matt Phillips `<mphillips81@bloomberg.net>`, 2025-05-20), the feature commit
that re-plumbed eauth token storage onto the generic `salt.cache` interface:
- it *added* `Cache.clean_expired`'s generic fallback in `salt/cache/__init__.py`
  — already buggy, using the driver's `updated()` (file mtime, always in the
  past) as the entry's expiry (`git log -L` shows the method did not exist
  before this commit); and
- it *added* the `mk_token` branch in `salt/auth/__init__.py` calling
  `cache.store("tokens", …, expires=tdata["expire"])` — passing the absolute
  expiry timestamp where `Cache.store` wants a duration.
Routing tokens through the cache is what made the master's periodic
`clean_expired("tokens")` sweep start wiping still-valid tokens. Reachability
confirms it's a 3008-only regression: absent from v3006.9 / v3007.1 / v3007.14,
present in v3008.0 / v3008.1 — the same release boundary as the `manage.status`
regression (commit `4075aeb096`), but an unrelated commit. *Before filing
upstream, cross-check `78685e5c44` against canonical `saltproject/salt` (this
tree's history has some unusual metadata) to find the real PR.*
```
t=0    -> {'return': [{'builtin.minion': True}]}
t=+40s -> HTTP 200 ok=True
t=+40s -> HTTP 200 ok=True
t=+40s -> HTTP 200 ok=True
```
**Any realistic multi-step or batch testing must run against the patched
salt**, or you'll chase phantom 401s that are the server's bug, not the client's.

---

## Unhappy path: a targeted minion that doesn't respond — `out/09*.json`

Setup: stop the minion process but leave its key **accepted**, so salt still
targets it but never gets a reply (classic down/hung minion). Behaviors differ
sharply by call style:

### Sync `local` — blocks, then reports a *string* — `out/09b*.json`
```json
{"return": [{"builtin.minion":
  "Minion did not return. [No response]\nThe minions may not have all finished
   running ... salt-run jobs.lookup_jid 20260626235612037244"}]}
```
- We set `"timeout": 4`, but the HTTP call **blocked ~14s**. The per-command
  `timeout` bounds only the initial wait for returns; salt then issues a
  `saltutil.find_job` check and waits `gather_job_timeout` (default ~10s) before
  declaring no-show. **`timeout` alone does NOT bound wall-clock** — budget
  `timeout + gather_job_timeout`.
- The non-responder **is present** in the result, but its value is an
  **unstructured English string** ("Minion did not return. [No response]…"),
  not a structured error or retcode. A client must pattern-match this to detect
  it. (Yet another reason to prefer async.)

### `local_async` + `/jobs/<jid>` — omitted, compare targeted vs returned — `out/09c,09d*.json`
- Async returns the **targeted** set immediately: `{"jid": ..., "minions":
  ["builtin.minion"]}`.
- `/jobs/<jid>` then shows `info[0].Minions: ["builtin.minion"]` (targeted) but
  `info[0].Result: {}` and `return: [{}]` — the down minion is simply **absent
  from results, indefinitely**.
- **Non-responder detection = targeted (`minions`/`Minions`) − returned
  (`Result` keys).** This is the robust method (and what pepper's
  `--fail-if-incomplete` does). `jobs.print_job <jid>` (`out/09e*.json`) exposes
  the same `Minions` vs `Result {}` data after the fact.

### `manage.status` — ⚠️ unreliable for liveness — `out/09a*.json`
With the minion process confirmed dead (supervisor STOPPED, 0 processes),
`manage.status` returned **instantly** and **wrongly** listed it under `up`:
```json
{"return": [{"up": ["builtin.minion"], "down": []}]}
```
It reports `up` whether the minion is up **or** down (verified both ways) and
returns too fast to be doing a live ping — i.e. it's reading cached/key-derived
state, not actual liveness. `manage.alived` was no help either: it returned
`[[]]` in **both** states (presence-event tracking isn't populated in this
config). The only **authoritative** signal was a live `test.ping`: `true` when
up, "Minion did not return. [No response]" when down. **Don't trust
`manage.status`/`manage.alived` for liveness here** (3008.1, this config) — note
this contradicts the ops-skill claim that `manage.status` pings every host, so
verify against prod's config. For real liveness, use a live `test.ping` (and
compare targeted vs returned) or the event bus.

### No minions matched (vs unresponsive) — `out/09f*.json`
Targeting something that matches **no** minion (`tgt: "ghost-minion"`) returns
**immediately** (~0.03s) with `return: [{}]` (empty). Contrast with the
down-minion sync case (blocks ~14s, returns the "did not return" string). So a
client can distinguish:
- **fast + empty `[{}]`** → target matched nothing.
- **slow + `{minion: "Minion did not return…"}`** → matched but unresponsive.

### Event bus
A `salt/job/<jid>/new` fires (listing targeted minions) but **no
`salt/job/<jid>/ret/<minion>` ever arrives** for the down minion — an
event-driven client detects non-response by timing out on the expected `ret`
events (targeted set known from the `/new` event or the async response).

> **Normalization implication:** "no response" surfaces three different ways —
> a string baked into sync results, an omission in async/`/jobs`, and a missing
> `ret` event. The client should fold all three into one explicit per-minion
> status (e.g. `responded` / `no_response`) rather than leaking salt's
> inconsistency to callers.

---

## Self-killing & pathologically-long commands — `out/11*.json`

Three sync `local` cases (script `11_long_and_suicidal_cmds.py`):

### 1. `cmd.run` that kills its own minion (`pkill -9 -f salt-minion`)
The minion dies mid-job and never returns. After the gather window (~15s) the
API returns the same no-response string as a down minion:
`{"builtin.minion": "Minion did not return. [No response]… jobs.lookup_jid
<jid>"}`. supervisor (`autorestart`) brings the minion back within a few
seconds, but **the job's return is lost** (the process that would have sent it
is gone). Detect via targeted-vs-returned, not by waiting.

### 2. `cmd.run 'sleep 300'` SYNC — the connection is held for the *whole* command
**cherrypy does NOT return at a short gather timeout for a live long-running
job.** salt's `find_job` sees the job still running and keeps waiting, so the
sync HTTP call blocks for essentially the command's entire lifetime. Our client
hit its **120s** read timeout while the job kept running; `jobs.active` confirmed
it (`Running: {builtin.minion: <pid>}`, `Returned: []`). On the thread-pooled
cherrypy server this means **one long sync command pins a worker thread for the
whole duration** — a real scalability/timeout hazard. (tornado differs sharply:
it bails at ~10s with an empty body — see `tornado_learnings.md`.)

> Note the contrast with case 1: a *dead* minion returns at the gather timeout
> (~15s, find_job gets no answer); a *live but busy* minion makes the sync call
> wait indefinitely (find_job says "still running").

### 3. `cmd.run_bg 'sleep 300'` — fire-and-forget, instant
Returns in ~0.1s with `{"builtin.minion": {"pid": <pid>}}` regardless of the
5-minute command. This is the correct way to launch long commands: you get a
PID immediately and never hold the connection (but you forfeit the output —
track it via the job cache / event bus if you need it).

**Takeaway:** never run long commands synchronously. Use `cmd.run_bg` (or
`local_async` + `/jobs`/`/events`) so a slow command can't hold an HTTP
connection (and a cherrypy worker thread) hostage for minutes.

---

## jid lifecycle: find, cancel, retention — `out/12*.json`

(All netapi-agnostic except `GET /jobs/<jid>`'s envelope — see the note.)

### Finding a jid (success) — `out/12a–c`
- **`jobs.lookup_jid <jid>`** (runner) → just the per-minion return value:
  `{"return": [{"builtin.minion": true}]}`. ⚠️ **A missing jid and a
  not-yet-returned jid both return `{}`** — lookup_jid can't tell them apart.
- **`jobs.print_job <jid>`** (runner) → full metadata + result keyed by jid:
  `{<jid>: {Function, Arguments, Target, Target-type, User, Minions, StartTime,
  Result: {minion: {return, retcode, success}}}}`.
- **`GET /jobs/<jid>`** (endpoint) → cherrypy: `{info: [<that metadata+Result>],
  return: [{minion: retval}]}`. (tornado: metadata+Result in `return[0]`, no
  `info` — see `tornado_learnings.md`.)

### Cancelling a running jid — `out/12d–f`
There's no "cancel a jid" API per se; you SIGKILL the running job on the minion:
- **`saltutil.kill_job <jid>`** (local exec, targeted at the minions) →
  `{"return": [{"builtin.minion": "Signal 9 sent to job <jid> at pid <pid>"}]}`.
  `jobs.active` shows it before (`Running: [{minion: pid}]`, `Returned: []`) and
  empty after. Variants: `saltutil.term_job` (SIGTERM), `saltutil.signal_job
  <jid> <sig>`. It's a *minion* call (finds the running job by jid, signals the
  pid) — so it needs the minions reachable, not just the master.
- **Idempotent:** killing the same jid again after it's dead (`out/13*.json`)
  returns `{"builtin.minion": ""}` — the same empty string as killing a
  never-existed jid (the minion finds no running job with that jid in its proc
  cache). No error, no 5xx; the first kill is the only one that does anything.

### Non-existent jid — `out/12g–j`
- **`lookup_jid`** → `{}` (empty — indistinguishable from a pending job; not an
  error).
- **`print_job`** → `{<jid>: {"Error": "Cannot contact returner or no job with
  this jid", "StartTime": …, "Result": {}}}` — explicit Error.
- **`GET /jobs/<bogus>`** → cherrypy `{info: [{…"Error"…}], return: [{}]}`;
  tornado `{return: [{…"Error"…}]}`. **This `Error` field is the reliable way to
  detect a missing jid** (lookup_jid alone is ambiguous).
- **`saltutil.kill_job <bogus>`** → `{"builtin.minion": ""}` (empty string — no
  matching job, no error; success and "nothing to kill" are weakly
  distinguished only by the message text).

### Retention & persistence (source: `salt/config`, `salt/master.py`, `salt/returners/local_cache.py`)
- Default job cache is **`master_job_cache: local_cache`** — **disk-backed**
  under `<cachedir>/master/jobs/` (hashed jid dirs); `job_cache: True` default.
- **Max age: `keep_jobs_seconds` (default `86400` = 24h).** The master
  Maintenance loop (~`loop_interval`/`maintenance_interval`, ~60s) runs
  `clean_old_jobs`, removing job dirs whose `jid` file ctime is older than
  `keep_jobs_seconds`. **`keep_jobs_seconds: 0` = never prune** (keep forever).
  Legacy `keep_jobs` (hours) still maps to it.
- **Persistence across master restart/crash: YES** with local_cache — jobs live
  on disk, so a `salt-master` restart/crash keeps them; a handed-back jid stays
  resolvable until it ages out. (In our container the cachedir isn't mounted, so
  jobs survive `supervisorctl restart salt-master` but not `docker rm`/recreate.)
- **A handed-back jid becomes unresolvable when:** (a) it's older than
  `keep_jobs_seconds`; (b) the job cache / cachedir was cleared; (c)
  `job_cache: False`; (d) you query a *different* master without a shared job
  cache (HA/cluster without a shared `cachedir` or a shared external
  `master_job_cache` returner). Distinct from "unresolvable": the job exists but
  has no return yet (lookup_jid `{}`), or the minion never returned.

---

## Failure / outcome taxonomy — `out/14*.json`

**The central lesson for the client: HTTP status is NOT the outcome.** A `200`
routinely carries authz-denials, unknown-function errors, non-zero command
retcodes, failed states, and (on tornado) even malformed-request exceptions.
You must parse the body. Captured matrix (cherrypy | tornado):

| case | cherrypy | tornado |
|------|----------|---------|
| invalid/garbage `X-Auth-Token` on `/` | **401**, HTML body | **401**, JSON `{"status":"401 Unauthorized","return":"Please log in"}` |
| `/login` wrong password | 401, HTML | 401 |
| `/login` unknown eauth backend | 401, HTML | 401 |
| **authz denied** (valid token, perm missing) | **401**, HTML `"Authorization error occurred."` | **200**, `{"return":["Failed to authenticate"]}` |
| unknown function (`does.not.exist`) | 200, `{minion: "'…' is not available."}` | same |
| missing `fun` | **500**, `"An unexpected error occurred"` | **200**, `{"return":["Unexpected exception while handling request: run_job takes at least 2 arguments (1 given)"]}` |
| missing `tgt` | **500** | **200**, `{"return":["Unexpected exception while handling request: 'tgt'"]}` |
| body is a dict (not a list) | 200 (accepted) | 200 |
| invalid JSON body | **400**, HTML | **400** |
| `cmd.run 'exit 3'` + `full_return` | 200, `{ret:"", retcode:3, …}` | 200, `{minion: ""}` — **retcode lost** |
| failing state (`test.fail_without_changes`) | 200, `result:false`, `retcode:2` | 200, `result:false` |

### What the client must take from this
1. **A 200 ≠ success.** Inside a 200, classify by inspecting per-command /
   per-minion values: `"… is not available."` (bad fun), non-zero `retcode`,
   state `result:false`, or — on tornado — a `"Failed to authenticate"` /
   `"Unexpected exception while handling request: …"` string in the `return`
   array. Never key success off status alone.
2. **Auth error shapes diverge by module.** authn failures are 401 on both
   (cherrypy HTML; tornado JSON `"Please log in"`). **Authz denial:** cherrypy
   = 401 `"Authorization error occurred."`; tornado = **200** with
   `"Failed to authenticate"` in the array. So on tornado a permissions denial
   looks like a successful request unless you scan the body. Also note authn vs
   authz are only weakly distinguishable (cherrypy: both 401, differ by message;
   `salt/netapi/__init__.py` raises `EauthAuthenticationError` for both).
3. **Malformed lowstate:** cherrypy **500** (`"An unexpected error occurred"`);
   tornado **200** with an `"Unexpected exception while handling request: …"`
   string. Only *unparseable JSON* is a clean **400** (both). Don't rely on a
   4xx to mean "bad request."
4. **retcode is not portable via `full_return`.** cherrypy honors `full_return`
   (wraps `{ret, retcode, jid}`); **tornado ignores it on sync local** (returns
   the bare value — `cmd.run 'exit 3'` → `""`, retcode gone). To get retcode
   portably: call the function's structured variant (**`cmd.run_all`** →
   `{pid, retcode, stdout, stderr}`, works on both) or read the job cache
   (`jobs.print_job` `Result[minion]` = `{return, retcode, success}` on both).
5. **Unknown function is a 200**, surfaced per-minion as `"'x' is not
   available."` — detect by pattern (or pre-check with `sys.doc`).

---

## `/run` credentials — eauth token vs session token — `out/18*.json`

Resolves the earlier `/run` loose end (the `/login` session token 401'd on
cherrypy's `/run` but worked on tornado's).

- **Mint a raw eauth token** via the `auth.mk_token` runner (`{username,
  password, eauth}`) → 200, returns `{token, start, expire, name, eauth,
  groups}`. (Note: shape differs from `/login` — has `groups`/`name`, **no
  `perms`**.)
- **That eauth token on `/run`** (`token` kwarg) → **works on BOTH modules**.
  This is the portable `/run` credential.
- **The `/login` session token on `/run`** → cherrypy **401** (HTML); tornado
  **200**. So the session token is *not* a portable `/run` credential.

**Client guidance:** for `/run` (stateless/CI), use either the eauth trio
(`username`/`password`/`eauth`) or a raw eauth token from `auth.mk_token` — both
portable. Never use the `/login` session token on `/run` (cherrypy rejects it).

---

## Multiple minions / fleet behavior — `out/17*.json`

With two minions (`builtin.minion` + `builtin.minion2`; see `add-second-minion.sh`):

- **Aggregation** (`'*' test.ping`): a single dict with every responder —
  `{"builtin.minion": true, "builtin.minion2": true}`. Same on both modules.
- **Batch shape divergence** (`local_batch batch=1`): the `return` is a **list
  of one dict per batch**, not a single combined dict:
  `[{"builtin.minion2": "…"}, {"builtin.minion": "…"}]`. This is the
  batch-vs-non-batch nesting the pepper skill warned about — invisible with a
  single minion, real with ≥2. A client must **flatten/merge the per-batch
  dicts** back into one `{minion: result}` map. (tornado: `local_batch` → **400**,
  unsupported — no batching at all.)
- **Partial return** (kill `minion2`, then `'*' test.ping`): the responder comes
  back `true`; the down minion is handled differently by module —
  - cherrypy: included with a `"Minion did not return. [No response]…"` string;
  - tornado: **omitted entirely** (`{"builtin.minion": true}`).

  So neither module gives a clean structured "minion X failed." **Detect
  non-responders by diffing the targeted set against the returned set** (the
  `minions` list from an async submit, or the accepted-key list) — don't scan
  for the English string, and don't assume every targeted minion is a key.

---

## /events SSE robustness — `out/16*.json`

Three properties an event-driven client must handle (identical on both modules):

1. **Reconnect gap — no replay.** Events fired while you're disconnected are
   **not** replayed when you reconnect (`missed_replayed_after_reconnect:
   false`; the post-reconnect job *is* seen). So a client that relies on
   `/events` must **backfill the disconnected window via `/jobs`** (or accept
   the gap). There is no Last-Event-ID/resume.
2. **No idle heartbeat.** On an idle stream the server sends only the initial
   `retry: 400` line, then nothing (`raw_line_count: 1` over ~30s). There's no
   keepalive/comment frame, so a client can't distinguish "healthy but quiet"
   from "silently dead" via the stream alone — use a connect timeout + periodic
   liveness check, and be ready to reconnect.
3. **Stream dies on master restart.** Restarting `salt-master` drops the SSE
   connection (`ChunkedEncodingError: "Response ended prematurely"`). After the
   master is back, **reconnect succeeds and events resume** (you'll also see the
   minions' `salt/auth` re-auth events on the bus). So: expect the stream to
   break on master bounces; detect the error, reconnect, and backfill.

Net client design: treat `/events` as best-effort and **lossy across any
disconnect** (idle-death, master restart, network blip). Pair it with `/jobs`
polling for authoritative completion/backfill rather than trusting the stream to
be gap-free.

---

## Multiple lowstates in one POST — `out/15*.json`

A `/` POST body can be an array of several commands; the `return` is a
**parallel array in request order**, one entry per command, client types freely
mixed. Confirmed on both modules (identical):
- `[local test.ping, runner manage.status, local grains.get id]` →
  `[{m: true, …}, {"up":[…],"down":[]}, {m: "<id>", …}]` (order preserved).
- **No stop-on-failure:** `[local does.not.exist, local test.ping, local
  cmd.run 'exit 7']` → the unavailable first command (`"… is not available."`)
  does **not** abort the rest — `test.ping` still returns `true`, and the third
  runs (cherrypy: `{ret:"", retcode:7, jid}`; tornado: `{m: ""}` — same
  `full_return` divergence as the taxonomy section).

Client implication: when batching commands in one request, map results
positionally and classify each entry independently — one failing command is
silent w.r.t. the others (and shares the request's single `200`).

---

## Errors & status codes observed

- `POST /minions` → **202 Accepted** (everything else → 200).
- Auth failures → **401** with an **HTML body** (CherryPy error page), *not*
  JSON. A client must not assume a JSON body on non-2xx responses — check the
  status/content-type first.
- All success bodies were JSON (we requested `Accept: application/json`); the
  API also speaks YAML and urlencoded.

---

## Implications for the client (papurika)

1. **Normalize the envelope.** One internal result model over: `local`
   (`{minion: ret}`), `runner` (bare value), `wheel` (`data.return` + `success`),
   async (`{jid, minions}`), `/minions` (202 + `_links`), `/jobs/<jid>`
   (`info`+`return`, empty-until-done). Send **`full_return: true`** to collapse
   the local/runner/wheel cases toward a uniform `{return, retcode/success}`
   envelope (see the table above).
2. **Async-first.** Sync `local` blocks the HTTP connection for the whole job;
   on a thread-pooled cherrypy server that ties up a worker, and the HTTP client
   can time out before salt does (we hit this post-key-accept). Prefer
   `local_async` + `/events` (or `/jobs` polling), matching the docs' best
   practice.
3. **Don't assume JSON on errors;** surface 401/4xx/HTML cleanly and re-auth on
   401 (tokens can legitimately expire; and unpatched servers evict them).
4. **`/jobs/<jid>` polling isn't free** (spawns runner jobs) — prefer the event
   bus for completion when possible.
5. **Key accept = `wheel key.accept`**, not `/keys`. And remember a live minion
   won't re-key on delete without a restart.
6. **`/run` for stateless/CI** with the eauth trio; the session token is not a
   `/run` credential.
7. **Handle non-responders explicitly.** A sync call's `timeout` doesn't bound
   wall-clock (add `gather_job_timeout`), and "no response" surfaces three
   different ways (sync string / async omission / missing `ret` event). Track
   the targeted-vs-returned delta and expose a uniform per-minion status. Don't
   use `manage.status` for liveness (observed stale); prefer a live `test.ping`
   or the event bus.
