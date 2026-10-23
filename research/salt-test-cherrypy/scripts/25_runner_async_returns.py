# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""How an async runner's RESULT comes back — the gap behind `papurika jobs --active`.

`out/04e_runner_async.json` already records what `runner_async` returns at
submit time ({tag, jid}). What was never captured is how you get the runner's
actual *return*, which is what a client needs in order to run a runner
async-first instead of holding a synchronous connection open across a
fleet-wide fan-out.

Three questions, each a decision a client has to make:

  1. What event carries the return, and what is its payload shape? (Local jobs
     use salt/job/<jid>/ret/<minion>; runners use the salt/run/ prefix.)
  2. Is there a /new-style event at publish time, as local jobs have?
  3. Does GET /jobs/<jid> resolve a RUNNER jid? If so a client can reconcile a
     missed event by polling, exactly as it does for local jobs — if not, the
     event bus is the only way to ever see the result.

Captures verbatim to out/25a_runner_async_events.json and
out/25b_runner_async_job_lookup.json.
"""
import json
import threading
import time

import requests

import _common as c

token = c.login()
events = []
stop = threading.Event()
ready = threading.Event()


def listen():
    """Stream /events, recording every record until told to stop."""
    r = requests.get(
        f"{c.BASE}/events",
        headers={"X-Auth-Token": token, "Accept": "text/event-stream"},
        stream=True,
        timeout=60,
        verify=c.VERIFY,
    )
    ready.set()
    cur = {}
    for raw in r.iter_lines(decode_unicode=True):
        if stop.is_set():
            break
        if raw == "":
            if "data" in cur:
                events.append({
                    "t": round(time.time(), 3),
                    "tag": cur.get("tag"),
                    "data": json.loads(cur["data"]),
                })
                print(f"  [event] {cur.get('tag')}")
            cur = {}
            continue
        if ":" in raw:
            k, _, v = raw.partition(":")
            cur[k.strip()] = v.strip()


t = threading.Thread(target=listen, daemon=True)
t.start()
ready.wait(timeout=10)
time.sleep(1)  # let the stream settle before publishing

# Subscribe first, THEN submit — the ordering a client must use to avoid
# racing the return.
print("submitting runner_async jobs.active ...")
sub = requests.post(
    f"{c.BASE}/",
    headers={"X-Auth-Token": token, "Accept": "application/json"},
    json=[{"client": "runner_async", "fun": "jobs.active"}],
    timeout=30,
    verify=c.VERIFY,
).json()
print("  submit ->", json.dumps(sub))
jid = sub["return"][0]["jid"]

# Also submit one that FAILS, so the error shape is on record too: a client
# has to tell "the runner ran and said no" from "the runner blew up".
bad = requests.post(
    f"{c.BASE}/",
    headers={"X-Auth-Token": token, "Accept": "application/json"},
    json=[{"client": "runner_async", "fun": "jobs.lookup_jid"}],  # missing required arg
    timeout=30,
    verify=c.VERIFY,
).json()
print("  bad submit ->", json.dumps(bad))
bad_jid = bad["return"][0].get("jid")

time.sleep(20)  # jobs.active fans out to the fleet; give it room
stop.set()

run_events = [e for e in events if (e["tag"] or "").startswith("salt/run/")]
(c.OUT / "25a_runner_async_events.json").write_text(json.dumps({
    "submit": sub,
    "submit_failing": bad,
    "jid": jid,
    "failing_jid": bad_jid,
    "salt_run_events": run_events,
    "all_tags_seen": sorted({e["tag"] for e in events if e["tag"]}),
}, indent=2))

# Question 3: can a runner jid be reconciled through /jobs like a local one?
lookups = {}
for label, j in (("ok", jid), ("failing", bad_jid)):
    if not j:
        continue
    r = requests.get(
        f"{c.BASE}/jobs/{j}",
        headers={"X-Auth-Token": token, "Accept": "application/json"},
        timeout=30,
        verify=c.VERIFY,
    )
    lookups[label] = {"jid": j, "status_code": r.status_code, "body": r.json()}
    print(f"  /jobs/{j} ({label}) -> HTTP {r.status_code}")

(c.OUT / "25b_runner_async_job_lookup.json").write_text(json.dumps(lookups, indent=2))
print("\ndone — see out/25a_runner_async_events.json, out/25b_runner_async_job_lookup.json")
