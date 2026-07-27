# Minion status / liveness tools in salt-api (3008.1)

Reference for the future CLI: which salt tools report minion liveness/availability,
how each decides, what's a live ping vs a cache lookup, and which settings drive
the caches. Investigated in salt 3008.1 source (`~/src/saltstack`) and verified
against the local containers. **All of these run via the API as
`{"client":"runner","fun":"manage.<x>"}` (master-side, netapi-agnostic).**
Reproducible probe + raw captures:
`salt-test-cherrypy/scripts/19_minion_status_tools.py` → `out/19*.json` (run with
`builtin.minion` up and `builtin.minion2` down).

## TL;DR — don't trust `manage.*` for liveness

| tool | mechanism | sends a ping? | blocks | reliable for liveness? |
|------|-----------|---------------|--------|------------------------|
| `manage.status` / `up` / `down` | live `test.ping` (`_ping`) | **yes** | `timeout + gather_job_timeout` (~15s) | **NO** — reports every key-accepted minion as `up` (bug, see below) |
| `manage.present` / `alived` / `joined` / `allowed` / `reaped` (+ `not_*`) | `CkMinions.connected_ids` (cache) | no | instant | partially — reflects TCP conns to :4505, but needs grain-IP match + `minion_data_cache` |
| `manage.list_state` / `list_not_state` | same as `present` | no | instant | same caveats |
| **direct `local test.ping`** (not `manage`) | live publish + gather | **yes** | `timeout + gather_job_timeout` | **YES** — this is the authoritative check |

**Bottom line for the client: compute liveness yourself** — fire a `local`
`test.ping`, treat a minion as alive only if its return is `true` (not the
`"Minion did not return…"` string), and derive non-responders as
`targeted − responders`. Do **not** build liveness on `manage.status/up/down`
(broken in 3008.1) and don't rely solely on `present/alived` (cache + grain-IP
dependent).

---

## 1. Live-ping family: `status`, `up`, `down`

`salt/runners/manage.py`: `status()` (L68) → `_ping()` (L33); `up()` (L187) and
`down()` (L153) just return `status()`'s `up`/`down` lists. `_ping` publishes a
real `test.ping` and gathers returns:

```python
# manage.py _ping (paraphrased)
pub_data = client.run_job(tgt, "test.ping", (), tgt_type, "", timeout, "", listen=True)
returned = set()
for fn_ret in client.get_cli_event_returns(pub_data["jid"], pub_data["minions"],
                                            timeout, tgt, tgt_type, gather_job_timeout=…):
    if fn_ret:
        for mid, _ in fn_ret.items():
            returned.add(mid)          # <-- counts EVERY yielded entry
not_returned = set(pub_data["minions"]) - returned
return sorted(returned), sorted(not_returned)   # up, down
```

- **Blocks** `timeout + gather_job_timeout`. Defaults: `timeout` (master
  `timeout`, default 5) + `gather_job_timeout` (default 10) ≈ **15s**. Both are
  overridable as kwargs: `manage.status timeout=5 gather_job_timeout=10`.
- **The bug (3008.1).** `get_cli_event_returns` is invoked with
  `expect_minions=True` (its default; `salt/client/__init__.py`), so after the
  timeout it **yields a placeholder** for each non-responder:
  `{mid: {"out": "no_return", "ret": "Minion did not return. [No response]…"}}`.
  `_ping` doesn't distinguish that placeholder from a real return — it adds
  *every* yielded `mid` to `returned`. Net effect: **every targeted,
  key-accepted minion lands in `up`, and `down` is (effectively) always empty**,
  dead or not. Likely a regression from the change that made the `salt` CLI emit
  per-target timeout rows by default.
- **Verified:** with `builtin.minion2` confirmed dead (no process; a direct
  `local test.ping` to it → `"Minion did not return. [No response]"`),
  `manage.status` still returned `{"up": ["builtin.minion","builtin.minion2"],
  "down": []}` after the full ~15.6s ping. So it pays the ping cost *and* gives
  the wrong answer.

## 2. Cache family: `present` / `alived` / `joined` / `allowed` / `reaped` (+ `not_*`)

All are identical wrappers over `list_state()` (L216) →
`CkMinions.connected_ids()` (`salt/utils/minions.py` L987). On ZeroMQ transport
they collapse to one notion ("connected"); the distinct presence "states" are
not implemented for 0MQ. **No command is sent to minions.** `connected_ids`:

1. Requires **`minion_data_cache: True`** (default True) — else returns `∅`.
2. Lists cached minions from the **grains cache** (`cache.list("grains")`,
   on disk under `<cachedir>/minions/<id>/data.p` via the `cache` driver,
   default `localfs`).
3. Reads the **live OS TCP table** for the publish port:
   `local_port_tcp(publish_port)` (4505) → the set of source IPs with an
   established connection to the master (+ `remote_port_tcp(remote_minions_port)`
   if `detect_remote_minions: True`).
4. A minion counts as connected iff one of its **cached grain `ipv4`/`ipv6`**
   addresses is in that connected-IP set.

So it's "minions whose cached grain IP currently holds a TCP connection to
:4505" — live-ish (the socket table is read each call) but gated by the grains
cache and by IP matching.

- **Caveat seen in our container:** the builtin minion's `ipv4`/`ipv6` grains
  are `[]` (OrbStack networking), so `connected_ids` matches nothing →
  `present`/`alived` return `[]` even with the minion fully up. In a real fleet
  (minions with routable IPs in grains) it reflects current connections.
- **Freshness:** the IP→connection check is live each call; the grains (and
  their IPs) are only as fresh as the grains cache — updated when a minion
  authenticates / refreshes grains / pillar is compiled, not continuously. A
  minion whose IP changed without a grains refresh can be mis-classified.

## 3. Settings that matter

| setting | default | effect |
|---------|---------|--------|
| `timeout` | 5 | initial wait for `test.ping` returns (live family) |
| `gather_job_timeout` | 10 | extra wait + `find_job` window (live family); the "~10s tail" seen elsewhere |
| `minion_data_cache` | True | gates `connected_ids` (cache family); off → `present/alived` return `∅` |
| `cache` | `localfs` | backing store for the grains cache `connected_ids` reads |
| `publish_port` | 4505 | the port whose TCP table `connected_ids` inspects |
| `detect_remote_minions` / `remote_minions_port` | False / 4505 | also count minions connected to a remote port |
| `presence_events` | False | if True, master emits `salt/presence/{present,change}` events on `loop_interval` — a separate, event-driven presence signal a client could subscribe to (not used by `manage.*`) |
| `con_cache` | False | runs a `ConCache` process tracking connected minion ids for fast master-side minion-list checks (prod sets this True); orthogonal to `connected_ids`, which reads the live TCP table directly |

## 4. Recommendation for the CLI

- **Authoritative liveness = a direct `local test.ping`** (or any cheap exec),
  classifying each minion by its actual return: `true` = alive; the
  `"Minion did not return…"` string (cherrypy) or omission (tornado) = not
  responding. Derive non-responders as **targeted − responders** using the
  published `minions` list (from a `local_async` submit) or the accepted-key
  list as the target set. (See the unhappy-path findings in the harness docs.)
- **Treat `manage.status/up/down` as unusable for liveness** on 3008.1 (they
  report all key-accepted minions up). They're still fine as "what minions does
  the master know about" if you ignore the up/down split.
- **`manage.present`/`alived` are a cheap, no-traffic "is it TCP-connected"**
  signal *if* `minion_data_cache` is on and grain IPs are populated — usable as
  a fast pre-filter, but confirm with a real ping before acting.
- Consider subscribing to **`presence_events`** (if enabled on the master) for a
  push-based connect/disconnect signal instead of polling.

Source refs: `salt/runners/manage.py` (`_ping` L33, `status` L68, `up` L187,
`down` L153, `list_state` L216, `present`/`alived`/… L279–460);
`salt/utils/minions.py` `connected_ids` L987; `salt/client/__init__.py`
`get_cli_event_returns` (placeholder yield, `expect_minions` default True).

---

## 5. Regression analysis — introduced in 3008.0 (PR candidate)

This is a **regression**, not longstanding behavior. The trigger is the default
of `expect_minions` in `LocalClient.get_cli_event_returns`
(`salt/client/__init__.py`), which `manage._ping` calls. When `True`, the
gather injects a `{"out":"no_return","ret":"Minion did not return…"}` placeholder
for every non-responder; `_ping` counts every yielded entry as a return, so
non-responders land in `up`.

Confirmed by version (release tags **and** the matching public
`ghcr.io/cdalvaro/docker-salt-master:<ver>` images, **and** live behavior):

| version | `get_cli_event_returns` `expect_minions` default | `manage.status` w/ dead, key-accepted minion |
|---------|--------------------------------------------------|----------------------------------------------|
| 3006.9  | `False` | correct → minion in `down` |
| 3007.14 | `False` | correct → minion in `down` |
| 3008.0 / 3008.1 | **`True`** | **broken → minion in `up`, `down` empty** |

(All tested live: stop the minion via `supervisorctl stop salt-minion`, then
`salt-run manage.status` — 3006/3007 report `down`, 3008 reports `up`, while a
direct `salt '*' test.ping` always correctly shows no response.)

**Introducing change:** the `expect_minions` default flipped `False → True`
between v3007.14 and v3008.0, bundled into the 3008 "resource registry /
resource-id targeting" work (in this tree, commit `4075aeb096`, with the comment
*"Default True: `salt` must still emit per-target timeout rows … even without
`-v`"*). That change is correct for the `salt` **CLI** (it wants timeout rows),
but it silently broke `manage._ping`, which was never updated to ignore the
placeholders. (`_ping` itself is byte-identical 3007→3008.)
*Before filing upstream, cross-check the canonical `saltproject/salt` commit/PR
for this hunk — the local history here has unusual metadata.*

**Proposed fix (surgical, preserves the CLI behavior):** have `_ping` request
only real returns by passing `expect_minions=False` to `get_cli_event_returns`:

```python
for fn_ret in client.get_cli_event_returns(
    pub_data["jid"], pub_data["minions"], client._get_timeout(timeout),
    tgt, tgt_type, gather_job_timeout=gather_job_timeout,
    expect_minions=False,          # <-- runner wants actual returns, not timeout placeholders
):
```

Alternative (if the placeholders are wanted for logging): skip them when
counting — `if isinstance(data, dict) and data.get("out") == "no_return": continue`.
Either restores 3006/3007 behavior: dead minions correctly report in `down`.

### 5a. Fix implemented + verified (2026-06-27)

The surgical fix (pass `expect_minions=False` from `_ping`) is implemented in
`~/src/saltstack` on branch `cstrahan/fix-manage-status-expect-minions`
(branched off `v3008.1`), commit `bf0d873f5c` — `salt/runners/manage.py`, +8
lines (the kwarg plus an explanatory comment). `get_cli_event_returns` pops
`expect_minions` from `**kwargs` with a default of `True`
(`salt/client/__init__.py:1860`), and since `_ping` passes neither `verbose` nor
`show_timeout`, `False` propagates through cleanly — no other call site touched.

Verified live against `papurika-salt` (3008.1, fixed `manage.py` copied in over
the byte-identical v3008.1 baseline + master restart), with `builtin.minion` up
and `builtin.minion2` key-accepted:

| scenario (authoritative `test.ping`) | `manage.status` before fix | `manage.status` after fix |
|---|---|---|
| minion2 **dead** | `up: [minion, minion2]`, `down: []` ❌ | `up: [minion]`, `down: [minion2]` ✅ |
| minion2 **alive** | `up: [minion, minion2]` | `up: [minion, minion2]`, `down: []` ✅ |

`manage.up`/`manage.down` (thin wrappers over `status()`) inherit the fix —
confirmed `up → [minion]`, `down → [minion2]` in the dead case. The reverse case
confirms we restored correct behavior rather than merely inverting the bug.
(Patched container only; re-copy after recreating it, like the token/cache fix.)

## 6. Async gather liveness — no "missing" signal; use `find_job` (2026-07-27)

How the papurika client itself decides a minion is down during a `local_async`
gather (distinct from the `manage.*` runners above). Verified against
`papurika-salt` (3008.1) with `builtin.minion` up and `builtin.minion2`
key-accepted but its process dead:

- **salt-api gives no "missing" signal for async.** The `salt/job/<jid>/new`
  event reports `minions: [builtin.minion, builtin.minion2]` and **`missing: []`**
  — the dead-but-accepted minion is *not* flagged. Polling `/jobs/<jid>` over 18s,
  its `Result` only ever lists `builtin.minion`; `builtin.minion2` never appears in
  any form. So there is **nothing** from the server that marks a down-but-accepted
  minion terminal — the client must self-bound.
- **`saltutil.find_job <jid>` is the liveness oracle.** A sync `local`
  `saltutil.find_job <jid>` targeting the pending minions returns, per minion
  (with `full_return: true`):
  - still running it → a **non-empty job-info map** `{"ret":{"jid":…,"pid":…,…},"retcode":0}`;
  - up but not running it → **`{"ret":{},"retcode":0}`** (empty map);
  - unreachable → the **`"Minion did not return. [No response]…"` string**
    (`{"out":"no_return","ret":"…","retcode":1}`).
  So "still running" ⇔ the return normalizes to a **non-empty map**.
- **Bounding.** find_job on a dead minion otherwise blocks the master's full
  `timeout + gather_job_timeout` (~15s). Passing **both** `timeout` and
  `gather_job_timeout` (e.g. 2) as top-level lowstate fields is honored by the
  local client and cuts it to ~4s; live minions answer in <1s.

**Client design (see `design/async.md` §B):** when the settle window goes quiet,
re-probe the still-pending minions with `find_job`; keep waiting on the ones still
running the job, settle the rest as `NoResponse`. This is why a *slow-but-alive*
minion is never mislabeled — the whole point of the gather.

**Captured evidence:** `research/salt-test-cherrypy/scripts/24_async_liveness_findjob.py`
→ `out/24a`–`24f`: the empty `missing` list (`24a`), the down minion never
appearing in `/jobs` (`24b`), the `find_job` shapes raw and under `full_return`
(`24c`/`24d`/`24e`), and the timing ladder — default ~15.2s, `timeout=2` ~12.2s,
`timeout=2,gather_job_timeout=2` ~4.2s (`24f`). Note the `full_return` trap in
`24d`: a dead minion is a non-empty *wrapper* map `{out,ret,retcode}`; it's the
inner `ret` (a string, not a map) that marks it down — which is what the client's
`ret`-unwrap keys off.
