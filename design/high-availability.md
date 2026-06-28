# papurika — high-availability (multi-master) considerations

> **Status: NOT yet handled in the implementation.** This document records the
> problem, the failure modes, and the menu of fixes — but papurika does **not**
> currently do anything HA-specific beyond what's noted under "Current behavior."
> Once we've ironed out the prod topology and settled on an approach, the agreed
> design should be folded back into [`async.md`](async.md), [`cli.md`](cli.md),
> and [`library.md`](library.md), and this doc updated to point at it. Until then,
> treat everything below §"Current behavior" as **proposed, not built**.

## Context

`salt-api` is frequently deployed **behind a load balancer over multiple master
nodes**. In that topology a job's identity is owned by *whichever master handled
the submit*, but subsequent requests (polls, `watch`, the event stream) can be
routed by the LB to a *different* master. Our async-first engine
([`async.md`](async.md)) leans on three salt-api interactions that all assume
they're talking to the master that owns the jid:

- the async submit (`local_async`) — publishes the job and gathers returns into
  *that master's* `local_cache` (disk-backed, per-master);
- `/jobs/<jid>` polling — the gather's reconciliation/backfill;
- the `/events` SSE stream — the low-latency return path.

If any of those lands on a different master than the submit, things break.

## Failure modes

### 1. `/jobs` polling and `watch` hit the wrong master
Submit lands on master A → A publishes the job and accumulates returns in A's job
cache. The gather then polls `GET /jobs/<jid>`, or a later `papurika watch <jid>`
runs — and the LB routes it to master B, which never saw the job. B returns
salt's explicit **`Error: Cannot contact returner or no job with this jid`**. The
client reads that as a genuine missing jid (→ `ErrJobNotFound` / `ReasonStreamFatal`)
and gives up, even though the job is running fine on A.

### 2. SSE connected to the wrong master (the nastier one)
`/events` is **per-master** — each master has its own event bus unless the masters
share a transport. If our SSE connection lands on B but the job ran on A, the
`salt/job/<jid>/ret/<minion>` events fire on **A's** bus and we never see them on
B. The run then looks like a total non-response, falling back entirely to `/jobs`
polling — which (failure mode 1) may itself hit the wrong master. The combination
can make a perfectly healthy fleet look dead.

## What determines whether this matters

This is a **non-issue** for some topologies and a real problem for others. The
deciding factors:

| Topology / config | Impact |
|---|---|
| Single salt-api/master | None — affinity is trivially satisfied. |
| LB with **sticky sessions** (pin by `X-Auth-Token` / cookie / source IP) | Largely solved *within a session*; the gap is a *new* invocation (e.g. `watch` later) that isn't sticky to the original master. |
| Shared **`master_job_cache`** (returner backed by a shared store) | Any master can resolve any jid → failure mode 1 goes away. Failure mode 2 (events) may persist unless the event bus is also shared. |
| Multi-master, no stickiness, per-master caches | Both failure modes are live. |

**Prod topology is the prerequisite question** and is currently unconfirmed: is
`salt-master.us-west1.example.com:8000` (and the AWS/Linode equivalent) a
single salt-api, or an LB VIP over several? If an LB — does it do sticky sessions,
and is there a shared `master_job_cache`? The ops notes mention a single Linode
salt-master, but the GCP side may differ. **Resolve this first** — it decides
whether any client-side work is warranted.

## Mitigations

### Server-side (the usual fix — not papurika's code)
- **LB sticky sessions** — pin a session to one backend so all of a run's requests
  (and ideally a follow-up `watch`) hit the same master. Most salt-api HA setups do
  this precisely for jid affinity.
- **Shared `master_job_cache`** — a returner backed by a shared DB/redis so jid
  resolution is master-independent. Removes failure mode 1 entirely.

### Client-side (what papurika *could* add, once warranted)
- **Pin follow-ups to the submitting endpoint.** Record on `Submission`/`Job` which
  backend served the submit, and route the gather's polls — and a later `watch`
  (e.g. `papurika watch --endpoint <url> <jid>`) — back to it. This needs a way to
  identify the backend (a header the LB/salt-api exposes, or a sticky cookie we
  capture and replay).
- **Treat a cross-master "not found" as non-fatal.** In an HA context a `/jobs`
  not-found may be a wrong-master artifact rather than a real missing jid, so the
  gather should keep leaning on events / retry instead of declaring `StreamFatal`.
  (This extends the tolerance we already added for a *missing-`info`* body to the
  *explicit-`Error`* case — but only under an HA mode, since on a single master the
  explicit Error genuinely means "aged out.")
- **Affinity-aware event subscription.** Ensure the SSE connection is pinned to the
  same backend as the submit (connection reuse / the same sticky token), or
  subscribe on all backends — the latter is a much bigger lift and probably not
  worth it vs. relying on a sticky LB.

## Current behavior (what's actually true in the code today)

- **Partial intra-`Run` stickiness, by accident.** The pooled `*http.Client` reuses
  keep-alive connections, so within a single `Run` the submit + `/jobs` polls + the
  SSE connection *tend* to stay on one backend (connection-level stickiness through
  most LBs). This is not guaranteed and is not relied upon.
- **No cross-invocation affinity.** `papurika watch <jid>` opens fresh connections
  with no endpoint pinning — an HA LB can route it anywhere.
- **`/jobs` not-found is treated literally.** A missing-`info` body is tolerated
  ("no data this poll"; see [`async.md`](async.md) §A), but salt's explicit
  `info[0].Error` maps to `ErrJobNotFound` / `ReasonStreamFatal`. On a single master
  that's correct; behind a non-sticky multi-master LB it could misfire.
- **No `--endpoint` / affinity token** on `Submission`, `Job`, or `watch`.

In short: papurika today assumes it's effectively talking to one master (or a
sticky/shared-cache LB). It will work fine in those topologies and may misbehave
(spurious not-found, missed events) behind a non-sticky multi-master LB.

## Open questions / decisions to settle

1. **Confirm prod topology** (single vs LB; sticky?; shared job cache?). Gates
   everything else.
2. If client-side work is warranted: how to identify/capture the backend for
   pinning (LB-set header? sticky cookie? a salt-api master-id header if one
   exists?).
3. Whether to add an explicit **HA mode** (`--ha` / config flag) that switches on
   not-found tolerance + endpoint pinning, vs making it automatic.
4. How `watch` recovers a jid when the submitting endpoint is unknown (e.g. a jid
   pasted from another operator's terminal) — fall back to trying all known
   backends? Require `--endpoint`? Accept best-effort.
5. Event-bus story for failure mode 2 — rely on sticky LB, or do something smarter.

Once (1) is answered and an approach is chosen, update `async.md` (gather/not-found
handling, event subscription), `cli.md` (`watch --endpoint`, any `--ha` flag), and
`library.md` (affinity fields on `Submission`/`Job`, `Client` endpoint pinning),
and replace this status banner with a pointer to the implemented design.
