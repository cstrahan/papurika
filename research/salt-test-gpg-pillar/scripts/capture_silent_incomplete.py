# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""Capture the tornado silent-incomplete vs cherrypy slow-but-correct case.

At a secret count whose compile exceeds tornado's local gather window (~10s),
tornado's `local pillar.items` returns HTTP 200 with an EMPTY body while
cherrypy waits and returns the full pillar. Run after generating a large N
(e.g. `gen-gpg-pillar.sh 2000`).
"""
import json
import time

import _common as c

out = []
for label, base in c.MODULES:
    tok = c.login(base)
    t0 = time.time()
    r = c.post(base, tok, [{"client": "local", "tgt": "*", "fun": "pillar.items"}], timeout=90)
    el = round(time.time() - t0, 1)
    ret = r.json()["return"][0]
    nsec = (len(ret.get("builtin.minion", {}).get("secrets", {}))
            if isinstance(ret.get("builtin.minion"), dict) else 0)
    rec = {"module": label, "status": r.status_code, "elapsed_s": el,
           "return_empty": ret == {}, "secrets_returned": nsec,
           "interpretation": "SILENT INCOMPLETE (200 OK, empty body)" if ret == {}
                             else f"complete ({nsec} secrets)"}
    print(json.dumps(rec))
    out.append({**rec, "raw_return": ret if ret == {} else "<full pillar omitted>"})

c.save("silent_incomplete_pillar_items_N2000", out)
print("saved -> out/silent_incomplete_pillar_items_N2000.json")
