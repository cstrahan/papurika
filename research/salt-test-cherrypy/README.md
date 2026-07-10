# salt-test-cherrypy

A self-contained harness for exploring salt-api's `rest_cherrypy` netapi against
a local Salt 3008.1 master + builtin minion (the version we run in prod).

**Findings:** [`cherrypy_learnings.md`](cherrypy_learnings.md) — the main write-up;
[`state-apply-shapes.md`](state-apply-shapes.md) — `state.apply` return-shape
matrix + concurrency (the state-run lock);
[`highstate-formatter.md`](highstate-formatter.md) — how salt's own `highstate`
outputter renders states (reference for our pretty-printer).
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
out/*.json                captured responses (the numbered suite; version-parallel in out-3006/)
out-state-shapes/*.json   captured responses for the state.apply shape + concurrency probes
out-highstate-cli/*.txt   salt's rendered highstate output across its formatting modes
up.sh                     launch the container
apply-salt-patch.sh       apply the prod token/cache fix to the running container
token-cache-fix.patch     the fix (saltstack 063aceb387), for reference
```

## Run

```bash
bash up.sh                 # start ghcr.io/cdalvaro/docker-salt-master:3008.1 (master+minion, cherrypy, :8000)
bash apply-salt-patch.sh   # apply the token/cache fix (needs ~/src/saltstack); restarts the master
uv run scripts/01_sync_local.py   # ... and the rest; each is independent
docker rm -f papurika-salt        # tear down
```

Notes:
- API is **plain HTTP on :8000** (`SALT_API_DISABLE_SSL=True`), eauth user
  `saltdev`/`saltdev` (pam). These are throwaway local-container credentials.
- `apply-salt-patch.sh` copies the fixed `salt/auth` + `salt/cache` from
  `~/src/saltstack` (HEAD = v3008.1 + fix) over the container's bundled salt;
  it's ephemeral (re-run after recreating the container). Without it, eauth
  tokens get swept within ~60s — see the learnings doc.
- Scripts use `uv` (PEP 723 inline deps); no venv needed.
