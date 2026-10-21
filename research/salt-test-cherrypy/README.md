# salt-test-cherrypy

A self-contained harness for exploring salt-api's `rest_cherrypy` netapi against
a local Salt 3008.1 master + builtin minion (the version we run in prod).

**Findings:** [`cherrypy_learnings.md`](cherrypy_learnings.md) — the main write-up;
[`state-apply-shapes.md`](state-apply-shapes.md) — `state.apply` return-shape
matrix + concurrency (the state-run lock);
[`highstate-formatter.md`](highstate-formatter.md) — how salt's own `highstate`
outputter renders states (reference for our pretty-printer);
[`../minion-status-and-liveness.md`](../minion-status-and-liveness.md) §6 —
async-gather liveness (no `missing` signal; `saltutil.find_job` is the oracle),
captured by `scripts/24`.
**Captures:** `out/*.json` and `out-state-shapes/*.json` — verbatim, untruncated
request/response pairs; `out-highstate-cli/*.txt` — salt's rendered highstate text.

## Layout

```
config/zz-papurika.conf   master overrides (netapi_enable_clients, external_auth, token_expire)
srv/salt/                 demo states (top.sls + demo.sls); slow.sls; shapes/*.sls (state-shape matrix)
srv/pillar/               demo pillar
scripts/_common.py        shared login/post/record helpers
scripts/NN_*.py           probe scripts, runnable with `uv run` (20/21/22 = state.apply shapes)
scripts/23_highstate_render.sh   captures salt's own highstate text renders (salt-call --local)
scripts/24_async_liveness_findjob.py   async-gather liveness + saltutil.find_job (needs a 2nd minion; leaves it DEAD)
out/*.json                captured responses (the numbered suite; version-parallel in out-3006/)
out-state-shapes/*.json   captured responses for the state.apply shape + concurrency probes
out-highstate-cli/*.txt   salt's rendered highstate output across its formatting modes
up.sh                     launch the container

```

## Run

```bash
bash up.sh                 # start ghcr.io/cdalvaro/docker-salt-master:3008.1 (master+minion, cherrypy, :8000)
uv run scripts/01_sync_local.py   # ... and the rest; each is independent
docker rm -f papurika-salt        # tear down
```

Notes:
- API is **plain HTTP on :8000** (`SALT_API_DISABLE_SSL=True`), eauth user
  `saltdev`/`saltdev` (pam). These are throwaway local-container credentials.
- Runs stock **3008.3**; no patching. On 3008.0/3008.1 eauth tokens were swept
  within ~60s and `manage.status` reported dead minions as `up`; both are fixed
  upstream in **3008.2** (`fc21fca`, `79e8bb9db7`), so the former
  `apply-salt-patch.sh` is retired — see the learnings doc.
- Scripts use `uv` (PEP 723 inline deps); no venv needed.
- Override the endpoint with `SALTAPI_URL` (e.g. `SALTAPI_URL=http://127.0.0.1:8000`
  if `localhost` resolves to an IPv6 address something else is squatting on).
- `24_async_liveness_findjob.py` needs a **second minion** (it runs
  `add-second-minion.sh` if the key isn't accepted) and deliberately **leaves
  `builtin.minion2` dead** — re-run `add-second-minion.sh` to bring it back for
  other multi-minion scripts.
