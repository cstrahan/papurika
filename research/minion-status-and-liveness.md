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

> Two independent reasons, both field-confirmed: the `status` family
> mis-reports dead minions as up on 3008.0/3008.1 (§5), and the `present`
> family silently omits any minion whose connection address differs from its
> grains — NAT, floating IPs, cross-network masters (§2). papurika computes
> liveness itself (`targeted − returned`) and asks salt for neither.

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

### The IP match breaks completely across NAT — confirmed in a real fleet

Step 4 is not a rounding error, it is a **structural failure for any fleet
where a minion connects from an address different from the one in its
grains** — NAT, floating IPs, or a master in one network managing minions in
another. The master sees the translated source address on :4505; the minion's
grains report its private address; they never match, so the minion is absent
from `present`/`alived`/`joined` *while being perfectly reachable*.

Observed on a production master managing minions across two networks:
`manage.present` returned only the handful of minions **local to the master's
own network**, while `salt -C '<remote-minion>' test.ping` answered `true`
immediately. Thousands of reachable minions simply did not appear.

The failure is **silent and one-directional** — it under-reports, so it reads
as "those minions are down" rather than as a broken check. Upstream discussion:
[saltstack/salt#68081](https://github.com/saltstack/salt/discussions/68081),
where `mattp-` identifies the same cause (the `ss`-vs-grains IP comparison).

`detect_remote_minions` does **not** fix this: it defaults to `False` and keys
on `remote_minions_port`, default **22** — a salt-ssh scenario, not NAT.

⇒ **`present` is therefore not a usable "is this minion alive" check in
general**, on top of the `status` family already being unusable. To count
connected minions without a fleet gather, read the master's socket table
directly (`ss -tn state established '( sport = :4505 )'`) — that is the same
data `connected_ids` starts from, minus the grains-matching step that breaks.
It yields a count, not minion ids, which is enough for sizing a fan-out.

### 2b. Why this is not fixed upstream — and why the obvious fix does not exist

Upstream bug: **[saltstack/salt#66295 — "[BUG] Minion behind NAT incorrectly
shows not connected"](https://github.com/saltstack/salt/issues/66295)**. Open
since April 2024, severity-medium, milestone pushed repeatedly (Argon v3008.5
as of Oct 2026), **no maintainer response and no PR**. Same diagnosis: the
master compares `ss` output against minion-reported interface IPs.

**Every id-level presence path bottoms out in the same check.** There is no
alternative source to fall back on:

| entry point | source |
|---|---|
| `manage.present` / `alived` / `joined` / `allowed` / `reaped` | `connected_ids()` |
| `wheel minions.connected` | `con_cache` if enabled, **else** `connected_ids()` |
| `con_cache` itself | `CacheWorker.run()` → `connected_ids()`, **wholesale replace** |
| `presence_events` | `connected_ids()` on `loop_interval` |

`con_cache` deserves spelling out because it looks like a workaround and is
not. The master *does* record the correct id at authentication —
`master.py`, `self.cache_cli.put_cache([load["id"]])` — which is NAT-safe,
because auth carries the id. But `CacheWorker.run()`
(`salt/utils/master.py`) then does
`list(CkMinions(self.opts).connected_ids())` and **replaces the whole list**
on a timer. Correct ids go in; the broken matcher sweeps them out again.

**The obvious fix — "record the source IP seen at auth and match on that too"
— is not implementable on the 0MQ transport.** Checked: the auth handler
receives only the decoded payload (`handle_message(self, stream, payload)` →
`self.message_handler(payload)` in `salt/transport/zeromq.py`), and the ROUTER
socket sits behind a `zmq_device` proxying to workers over an internal DEALER,
so the original peer is several hops gone by then. A ROUTER identity frame is
a ZeroMQ identity, not an address. **The master has the minion id and no
address at auth time.**

That is the real reason the bug has sat for years: on 0MQ pub/sub, 4505
connections are anonymous at the protocol level, so IP matching is the only
handle anyone has found. Don't spend time rediscovering the auth-time-IP fix.

Nor is "stop `CacheWorker` clobbering the auth-recorded ids" sufficient on its
own: nothing would ever *remove* an entry, so a minion that died an hour ago
stays "connected" forever. That trades silent under-reporting for silent
over-reporting — the same failure mode as the `manage.status` regression in
§5, which is worse, not better.

#### The TCP transport already does this correctly

The IP-matching problem is specific to **ZeroMQ**. On a `transport: tcp`
master, presence is tracked *in the transport*, by authenticated minion id,
per connection — and the maintenance loop deliberately skips its
`connected_ids()` polling in that case:

```python
# salt/master.py — Maintenance.__init__
if self.opts.get("presence_events", False):
    tcp_only = all(t == "tcp" for t, _ in iter_transport_opts(self.opts))
    if not tcp_only:
        # For a TCP only transport, the presence events will be
        # handled in the transport code.
        self.presence_events = True
```

```python
# salt/channel/server.py — presence_callback
if not self.aes_funcs.verify_minion(load["id"], load["tok"]):
    return
subscriber.id_ = load["id"]
self._add_client_present(subscriber)        # self.present[id_] = {client}
```

The subscriber announces its **id** over the authenticated channel on connect,
and `remove_presence_callback` fires on disconnect — so `present` is a real,
id-keyed, connect/disconnect-driven set. No grains, no addresses, NAT
irrelevant.

⇒ So "correct presence" is not architecturally impossible in salt; it is
**absent from the 0MQ transport specifically**, because 0MQ pub/sub gives the
master no per-subscriber identity to hang it on. Switching transport is a
large change with its own consequences and is **not** a casual recommendation
— but it is the one existing configuration where presence is trustworthy, and
it is the shape any 0MQ fix would have to imitate.

**Candidate fixes, if anyone takes this up** (none attempted here):

1. *Configurable match grains.* `connected_ids` hardcodes `grains["ipv4"]` /
   `["ipv6"]`. An option listing which grain keys to match would let a NAT'd
   fleet surface its public address from cloud metadata and have presence
   work. Contained and backward-compatible, but needs operator setup — a
   mitigation with a config story, not a cure.
2. *Make the failure loud.* `connected_ids` can see it has established
   connections matching no cached minion. Logging that count turns a
   confidently wrong answer into a detectable one. No behaviour change;
   highest value per unit of risk.
3. *id-keyed `last_seen`.* Track when the master last heard from each id on
   4506 (auth, returns, mine, pings) and define presence as "heard from within
   N seconds" — never touches IPs, so NAT-proof. But it is only meaningful
   with `ping_interval` enabled (default **0**), so an idle minion looks dead.
   A design change needing maintainer buy-in before anyone writes it.

⇒ None of this changes papurika's position: compute liveness as
`targeted − returned` (§4). It is noted here so the next person does not
re-derive the dead end.

## 3. Settings that matter

| setting | default | effect |
|---------|---------|--------|
| `timeout` | 5 | initial wait for `test.ping` returns (live family) |
| `gather_job_timeout` | 10 | extra wait + `find_job` window (live family); the "~10s tail" seen elsewhere |
| `minion_data_cache` | True | gates `connected_ids` (cache family); off → `present/alived` return `∅` |
| `cache` | `localfs` | backing store for the grains cache `connected_ids` reads |
| `publish_port` | 4505 | the port whose TCP table `connected_ids` inspects |
| `detect_remote_minions` / `remote_minions_port` | False / **22** | also count minions connected to a remote port. The default is **22** — this targets a salt-ssh-style setup, **not** NAT; it does not help with §2b |
| `presence_events` | False | if True, master emits `salt/presence/{present,change}` events on `loop_interval` — a separate, event-driven presence signal a client could subscribe to (not used by `manage.*`) |
| `con_cache` | False | runs a `ConCache` process caching connected minion **ids**. **NOT orthogonal to `connected_ids`** — see §2b: it is periodically rebuilt *from* `connected_ids`, so it inherits the same IP-matching bug on a timer |

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
| 3008.2 / 3008.3 | **`True`** (unchanged) | correct → minion in `down` (**fixed in the runner**) |

(All tested live: stop the minion via `supervisorctl stop salt-minion`, then
`salt-run manage.status` — 3006/3007 report `down`, 3008.0/3008.1 report `up`,
while a direct `salt '*' test.ping` always correctly shows no response.)

**Fixed in 3008.2** by saltstack `79e8bb9db7` *"Fix manage.status reporting all
minions as up"* (`git tag --contains` → v3008.2, v3008.3). Verified live on
stock 3008.3: with the minion stopped, `manage.status` → `{"up": [], "down":
["builtin.minion"]}`.

Note *where* it was fixed: the `get_cli_event_returns` `expect_minions` default
is **still `True`** — the `salt` CLI still wants its per-target timeout rows.
The runner was fixed instead: `manage._ping` now passes `expect_minions=False`
explicitly and additionally skips any `out == "no_return"` row that sneaks
through. So the underlying synthesized-row behavior this doc describes is
unchanged; only `manage.*` stopped mistaking those rows for returns.

**This does not change the recommendation.** papurika still computes liveness as
`targeted − returned`: the fix only exists on ≥3008.2, the client has to work
against 3006/3007/3008.1 masters too, and a computed answer needs no runner call
at all.

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
