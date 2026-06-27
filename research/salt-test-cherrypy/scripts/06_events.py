# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""The /events SSE bus — the recommended async pattern.

Subscribe to /events FIRST, then fire async jobs, and watch the job lifecycle
flow by as events: salt/job/<jid>/new (published) and salt/job/<jid>/ret/<mid>
(minion returned). This is the lightweight, scalable way to get real-time job
returns without holding a synchronous connection per job.

We capture the raw SSE records (tag + data) verbatim to out/06_events.json.
"""
import json
import threading
import time

import requests

import _common as c

token = c.login()
events = []
stop = threading.Event()


def listen():
    """Stream /events and parse the SSE records (blank-line separated, with
    `tag:` and `data:` fields)."""
    r = requests.get(
        f"{c.BASE}/events",
        headers={"X-Auth-Token": token, "Accept": "text/event-stream"},
        stream=True,
        timeout=30,
    )
    cur = {}
    for raw in r.iter_lines(decode_unicode=True):
        if stop.is_set():
            break
        if raw == "":  # record boundary
            if "data" in cur:
                rec = {"t": round(time.time(), 3), "tag": cur.get("tag"),
                       "data": json.loads(cur["data"])}
                events.append(rec)
                print(f"  [event] {rec['tag']}")
            cur = {}
            continue
        if raw.startswith("tag: "):
            cur["tag"] = raw[5:]
        elif raw.startswith("data: "):
            cur["data"] = raw[6:]
        elif raw.startswith("retry: "):
            pass
    r.close()


listener = threading.Thread(target=listen, daemon=True)
listener.start()
time.sleep(2)  # let the stream establish before firing jobs

# Fire a couple of async jobs and let their events flow through the stream.
c.banner("firing local_async cmd.run")
ls = [{"client": "local_async", "tgt": "*", "fun": "cmd.run",
       "arg": ["echo via-event-bus; sleep 2; echo done"]}]
resp = c.post(token, ls).json()
fired_jid = resp["return"][0]["jid"]
print("fired jid:", fired_jid)

ls = [{"client": "local_async", "tgt": "*", "fun": "test.ping"}]
c.post(token, ls)

time.sleep(8)  # collect events
stop.set()
time.sleep(0.5)

(c.OUT / "06_events.json").write_text(json.dumps(events, indent=2))
c.banner(f"captured {len(events)} events")
print(json.dumps(events, indent=2))
