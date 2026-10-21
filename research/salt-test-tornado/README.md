# salt-test-tornado

A self-contained harness for exploring salt-api's `rest_tornado` netapi against
a local Salt 3008.1 master + builtin minion, run as a **diff against the
cherrypy harness**.

**Findings:** [`tornado_learnings.md`](tornado_learnings.md) — framed as
differences from `../salt-test-cherrypy/cherrypy_learnings.md`.
**Captures:** `out/*.json` — verbatim, untruncated request/response pairs.

## How it's served

The image only auto-configures `rest_cherrypy` (:8000). `config/zz-papurika.conf`
adds a `rest_tornado` block on **:8001**; `salt-api` serves both modules at once
and this harness targets tornado on **:8001** (the only published port). The
scripts default to `http://localhost:8001`.

## Layout

```
config/zz-papurika.conf   master overrides (rest_tornado :8001, netapi_enable_clients, external_auth, token_expire)
srv/                      demo states + pillar (same as cherrypy harness)
scripts/_common.py        shared helpers (BASE = :8001)
scripts/NN_*.py           probe scripts (uv run); numbered to parallel the cherrypy harness
scripts/10_unsupported_clients.py   captures the HTTP 400s for wheel/batch/subset/ssh
out/*.json                captured responses
up.sh
```

Note there is **no `03_batch` or `05_wheel`** here (cherrypy had them) —
tornado doesn't implement those clients; `10_unsupported_clients.py` documents
the rejection instead.

## Run

```bash
bash up.sh                 # master+minion; cherrypy :8000 (internal) + tornado :8001 (published)
uv run scripts/01_sync_local.py   # ... and the rest; each is independent
docker rm -f papurika-salt-tornado
```

Credentials and patch notes are the same as the cherrypy harness.
