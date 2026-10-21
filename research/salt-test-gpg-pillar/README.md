# salt-test-gpg-pillar

Cross-cutting unhappy-path harness: does **cold GPG-encrypted-pillar
decryption** cause flakiness? Runs one master+minion serving **both** netapi
modules (cherrypy :8000, tornado :8001) so the GPG pillar can be triggered via
each.

**Findings:** [`gpg_pillar_learnings.md`](gpg_pillar_learnings.md) — short
version: yes. A tornado **silent incomplete** (200 OK + empty) on slow compiles,
and **total request timeout** under concurrent compiles; single-minion
sequential is just linearly slow.

**Captures:** `out/*.json`.

## Layout

```
config/zz-papurika.conf        rest_tornado :8001, decrypt_pillar + gpg_keydir, pillar_cache: False
srv/pillar/{top,secrets}.sls   secrets.sls is GENERATED + gitignored
gen-gpg-pillar.sh <N>          (re)generate a throwaway gpg key + N encrypted secrets in the container
_gen_secrets_incontainer.sh    runs inside the container (invoked by the above)
scripts/_common.py             dual-base helpers (cherrypy :8000 + tornado :8001)
scripts/stress.py              sequential ratchet (N=5..4000) until wonky
scripts/capture_silent_incomplete.py   the tornado empty-200 case
scripts/stress_concurrent.py <N> <K>   K concurrent compiles (the minion-herd simulation)
up.sh
```

GPG key material and `secrets.sls` are **gitignored** (throwaway; avoids secret
-scanner false alarms). See `gpg_pillar_learnings.md` for the run sequence.
