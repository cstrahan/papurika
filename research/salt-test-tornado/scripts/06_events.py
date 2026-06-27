# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""The /events SSE bus — the recommended async pattern.

Subscribe to /events FIRST, then fire async jobs, and watch the job lifecycle
flow by as events: salt/job/<jid>/new + /publish + /ret/<mid>. This is the
lightweight, scalable way to get real-time job returns without a synchronous
connection per job.

The SSE framing is identical to rest_cherrypy (`retry:`/`tag:`/`data:`, blank
-line separated). One operational note vs cherrypy: tornado does NOT replay
events that occurred before the subscription is fully established, so we
*establish the stream first*, THEN fire — exactly what the docs prescribe
("Subscribe to the SSE stream before creating any events"). We collect in the
main thread and fire from a background thread.

Captures the raw SSE records (tag + data) verbatim to out/06_events.json.
"""
import json
import threading
import time

import requests

import _common as c

token = c.login()

# 1. Establish the /events subscription FIRST (synchronously).
# NOTE: do NOT send `Accept: text/event-stream` — tornado's content negotiation
# rejects it with 406 (cherrypy tolerated it). Omit Accept (the stream is always
# SSE regardless).
stream = requests.get(
    f"{c.BASE}/events",
    headers={"X-Auth-Token": token},
    stream=True,
    timeout=30,
)
print("events stream:", stream.status_code, stream.headers.get("Content-Type"))


# 2. Once subscribed, fire async jobs from a background thread.
def fire():
    time.sleep(1.0)
    c.banner("firing local_async cmd.run + test.ping")
    ls = [{"client": "local_async", "tgt": "*", "fun": "cmd.run",
           "arg": ["echo via-event-bus; sleep 2; echo done"]}]
    print("fired jid:", c.post(token, ls).json()["return"][0]["jid"])
    c.post(token, [{"client": "local_async", "tgt": "*", "fun": "test.ping"}])


threading.Thread(target=fire, daemon=True).start()

# 3. Collect events in the main thread for a fixed window.
events = []
cur = {}
start = time.time()
for raw in stream.iter_lines(decode_unicode=True):
    if raw == "":  # record boundary
        if "data" in cur:
            rec = {"t": round(time.time(), 3), "tag": cur.get("tag"),
                   "data": json.loads(cur["data"])}
            events.append(rec)
            print(f"  [event] {rec['tag']}")
        cur = {}
    elif raw.startswith("tag: "):
        cur["tag"] = raw[5:]
    elif raw.startswith("data: "):
        cur["data"] = raw[6:]
    if time.time() - start > 8:
        break
stream.close()

(c.OUT / "06_events.json").write_text(json.dumps(events, indent=2))
c.banner(f"captured {len(events)} events")
print(json.dumps(events, indent=2))
