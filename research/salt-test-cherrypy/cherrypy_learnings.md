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
- **Patch applied:** the prod token/cache fix (saltstack `70178c9671`) — see
  [Token eviction bug](#token-eviction-bug-confirmed-and-fixed). Reproduce with
  `up.sh` + `apply-salt-patch.sh`.

Reproduce: `cd research/salt-test-cherrypy && bash up.sh && bash apply-salt-patch.sh`,
then `uv run scripts/NN_*.py`.

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

This is exactly the prod bug fixed by saltstack `70178c9671` (the generic
`Cache.clean_expired` fallback used file mtime as the expiry and swept all
tokens every maintenance pass). We applied the fix to the container's bundled
salt (`apply-salt-patch.sh`) and restarted the master. Confirmed fixed: a
single token reused across ~120s (multiple maintenance sweeps) kept returning
HTTP 200 (`08_token_longevity.py`):
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
