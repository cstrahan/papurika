# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""Asynchronous local execution + job tracking.

Two ways to launch async: client=local_async on `/`, and POST /minions (which
forces local_async). Both return a jid immediately; results are fetched later
from /jobs/<jid>. Also lists the job cache via GET /jobs.
"""
import time

import _common as c

token = c.login()

# 1. local_async via the root URL — returns {jid, minions} immediately.
ls = [{"client": "local_async", "tgt": "*", "fun": "cmd.run",
       "arg": ["echo async-1; sleep 3; echo async-done"]}]
body = c.record("02a_local_async_root", ls, c.post(token, ls))
jid = body["return"][0]["jid"]
print("\n>>> jid from local_async:", jid)

# 2. POST /minions — convenience endpoint that always uses local_async; note
#    the 202 status and the _links.jobs hint.
ls2 = [{"tgt": "*", "fun": "test.sleep", "arg": [2]}]
body2 = c.record("02b_post_minions", ls2, c.post(token, ls2, path="/minions"))
jid2 = body2["return"][0]["jid"]

# 3. Poll /jobs/<jid> until results arrive. NOTE the tornado shape differs from
#    cherrypy: tornado returns {"return": [{...metadata..., "Result": {...}}]}
#    (no top-level "info", no bare {minion: retval}). return[0] is ALWAYS the
#    metadata dict, so completion is detected by Result becoming non-empty.
for attempt in range(15):
    r = c.get(token, f"/jobs/{jid}")
    data = r.json()
    ret = data["return"][0]
    result = ret.get("Result", {}) if isinstance(ret, dict) else {}
    if result:  # per-minion results have arrived
        c.record("02c_jobs_jid_complete", None, r)
        break
    print(f"  poll {attempt}: Result still empty")
    time.sleep(1)

# 4. GET /jobs — the job list (metadata for recent jobs, no returns).
c.record("02d_jobs_list", None, c.get(token, "/jobs"))
