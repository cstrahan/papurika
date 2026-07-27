# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""Async gather liveness & `saltutil.find_job` — the evidence behind the client's
find_job re-check (see design/async.md §B and minion-status-and-liveness.md §6).

Three findings, each captured to out/24_*.json:

  A. **salt-api gives NO "missing" signal for an async gather.** A down-but-
     key-accepted minion shows `missing: []` in the `salt/job/<jid>/new` event and
     NEVER appears in `/jobs/<jid>`. So there is no server signal that marks it
     terminal — the client must self-bound. (Without this, dropping the settle
     window would hang forever on any offline-but-accepted key.)

  B. **`saltutil.find_job <jid>` is the liveness oracle.** Per minion:
     running the jid -> a non-empty job-info map; up-but-idle -> `{}`; unreachable
     -> the "Minion did not return. [No response]" string. Captured both raw and
     under `full_return: true` (what the papurika client sends). NOTE the
     full_return trap: EVERY minion value is wrapped `{"ret": …, "retcode": …}`, so
     even the dead minion is a non-empty map at the wrapper level
     (`{out:no_return, ret:"<no-response string>", retcode:1}`). The client
     distinguishes them by the **inner `ret`**: a running minion's `ret` is a map,
     a dead minion's `ret` is a string. So "still running" == `ret` unwraps to a
     non-empty map (which is exactly what decodeMinionValue + findRunning do).

  C. **find_job must be bounded.** On a dead minion it otherwise blocks the
     master's `timeout + gather_job_timeout` (~15s). Passing BOTH `timeout` and
     `gather_job_timeout` (=2) as top-level lowstate fields — honored by salt-api's
     local client — cuts it to ~4s; live minions answer in <1s.

Precondition (the script sets it up): builtin.minion UP, builtin.minion2
key-accepted but its process DEAD. We run add-second-minion.sh (idempotent, also
accepts the key) then kill minion2, leaving it down-but-accepted.
"""
import json
import subprocess
import threading
import time
from pathlib import Path

import requests

import _common as c

HERE = Path(__file__).resolve().parent
UP = "builtin.minion"
DOWN = "builtin.minion2"


def dsh(cmd: str):
    subprocess.run(["docker", "exec", c.CONTAINER, "sh", "-c", cmd],
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)


def key_accepted() -> bool:
    r = subprocess.run(["docker", "exec", c.CONTAINER, "salt-key", "-L", "--out=json"],
                       capture_output=True, text=True, check=False)
    try:
        return DOWN in (json.loads(r.stdout).get("minions") or [])
    except Exception:
        return DOWN in r.stdout


def minion2_responds(token) -> bool:
    """Authoritative liveness: a bounded direct test.ping (never process-grep —
    `pgrep -f minion2` false-positives on its own command line)."""
    ls = [{"client": "local", "tgt": DOWN, "tgt_type": "list", "fun": "test.ping",
           "timeout": 3, "gather_job_timeout": 3}]
    return c.post(token, ls, timeout=15).json()["return"][0].get(DOWN) is True


def setup_down_minion(token):
    """Leave builtin.minion2 key-accepted but its process DEAD. Only launch it (to
    generate+accept a key) if the key isn't already there — otherwise we'd just be
    reviving the minion we want dead. Then pkill it and confirm down via test.ping."""
    if not key_accepted():
        subprocess.run(["bash", str(HERE.parent / "add-second-minion.sh"), c.CONTAINER],
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
    for _ in range(4):
        dsh("pkill -9 -f minion2; true")
        time.sleep(3)  # let it die and its master-side connection drop
        if not minion2_responds(token):
            print(f"setup: {DOWN} is key-accepted but DEAD")
            return
    raise SystemExit(f"setup: {DOWN} still responds to test.ping — cannot capture "
                     "the down-but-accepted case")


token = c.login()
setup_down_minion(token)

# ---------------------------------------------------------------------------
# A. No "missing" signal: /new says missing=[]; /jobs never lists the down minion
# ---------------------------------------------------------------------------
c.banner("A. async test.ping '*' — the /new event's missing list (down minion accepted)")

new_frames = []
stop = threading.Event()


def listen():
    r = requests.get(f"{c.BASE}/events",
                     headers={"X-Auth-Token": token, "Accept": "text/event-stream"},
                     stream=True, timeout=30, verify=c.VERIFY)
    cur = {}
    for raw in r.iter_lines(decode_unicode=True):
        if stop.is_set():
            break
        if raw == "":
            if "data" in cur:
                try:
                    data = json.loads(cur["data"])
                except ValueError:
                    data = cur["data"]
                new_frames.append({"tag": cur.get("tag"), "data": data})
            cur = {}
        elif raw.startswith("tag: "):
            cur["tag"] = raw[5:]
        elif raw.startswith("data: "):
            cur["data"] = raw[6:]
    r.close()


listener = threading.Thread(target=listen, daemon=True)
listener.start()
time.sleep(2)  # establish the stream before submitting

ls = [{"client": "local_async", "tgt": "*", "fun": "test.ping"}]
sub = c.post(token, ls).json()
jid = sub["return"][0]["jid"]
matched = sub["return"][0]["minions"]
print("submitted jid:", jid, "matched:", matched)

time.sleep(4)
stop.set()

# Pull out the /new event for our jid and record its (empty) missing list.
new_ev = next((f for f in new_frames
               if f["tag"] == f"salt/job/{jid}/new"
               or (isinstance(f.get("data"), dict) and f["data"].get("data", {}).get("jid") == jid
                   and f["tag"].endswith("/new"))), None)
new_data = (new_ev or {}).get("data", {})
new_inner = new_data.get("data", new_data) if isinstance(new_data, dict) else {}
capA1 = {
    "note": "async local_async test.ping to '*' with builtin.minion2 down-but-accepted",
    "submit_return": sub["return"][0],
    "new_event": new_ev,
    "finding": {
        "matched_minions": new_inner.get("minions"),
        "missing": new_inner.get("missing"),
        "down_minion_in_missing": DOWN in (new_inner.get("missing") or []),
    },
}
(c.OUT / "24a_new_event_missing.json").write_text(json.dumps(capA1, indent=2))
print(json.dumps(capA1["finding"], indent=2))

# Poll /jobs/<jid> a few times: the down minion never appears as returned.
polls = []
for wait in (2, 4, 6):
    time.sleep(wait if not polls else wait - polls[-1]["t"])
    info = c.get(token, f"/jobs/{jid}").json().get("info", [{}])[0]
    polls.append({"t": wait, "returned": sorted((info.get("Result") or {}).keys()),
                  "targeted": info.get("Minions")})
capA2 = {
    "note": "/jobs/<jid> over ~6s — the down-but-accepted minion never returns "
            "and is never flagged; only the client's find_job re-check can classify it",
    "jid": jid, "polls": polls,
}
(c.OUT / "24b_jobs_down_never_returns.json").write_text(json.dumps(capA2, indent=2))
print(json.dumps(polls, indent=2))

# ---------------------------------------------------------------------------
# B. saltutil.find_job shapes — running vs idle vs dead; raw and full_return
# ---------------------------------------------------------------------------
c.banner("B. saltutil.find_job <jid> shapes (running / idle / dead)")

# Start a long job on the LIVE minion so find_job has something 'running' to see.
longjob = c.post(token, [{"client": "local_async", "tgt": UP, "fun": "cmd.run",
                          "arg": ["sleep 30"]}]).json()
long_jid = longjob["return"][0]["jid"]
print("long jid (running on", UP, "):", long_jid)
time.sleep(3)

# find_job targeting BOTH minions — bounded so the dead one doesn't stall it ~15s.
# Raw (no full_return): running -> job-info map, dead -> the no-response string.
ls = [{"client": "local", "tgt": f"{UP},{DOWN}", "tgt_type": "list",
       "fun": "saltutil.find_job", "arg": [long_jid], "timeout": 2, "gather_job_timeout": 2}]
c.record("24c_findjob_running_raw", ls, c.post(token, ls, timeout=30))

# full_return:true — what the papurika client sends. 'running' normalizes to a
# non-empty map after the ret-unwrap; dead becomes the no-response string.
ls = [{"client": "local", "tgt": f"{UP},{DOWN}", "tgt_type": "list",
       "fun": "saltutil.find_job", "arg": [long_jid], "full_return": True,
       "timeout": 2, "gather_job_timeout": 2}]
c.record("24d_findjob_running_fullreturn", ls, c.post(token, ls, timeout=30))

# A jid nobody is running -> {} (up-but-idle) on the live minion.
ls = [{"client": "local", "tgt": UP, "tgt_type": "list",
       "fun": "saltutil.find_job", "arg": ["20000101000000000000"],
       "timeout": 2, "gather_job_timeout": 2}]
c.record("24e_findjob_not_running", ls, c.post(token, ls, timeout=30))

# ---------------------------------------------------------------------------
# C. find_job must be bounded (timeout + gather_job_timeout, honored by salt-api)
# ---------------------------------------------------------------------------
c.banner("C. find_job latency on a dead minion — default vs bounded")

variants = [
    ("default", {}),
    ("timeout=2", {"timeout": 2}),
    ("timeout=2,gather_job_timeout=2", {"timeout": 2, "gather_job_timeout": 2}),
]
timings = []
for label, extra in variants:
    ls = {"client": "local", "tgt": DOWN, "tgt_type": "list",
          "fun": "saltutil.find_job", "arg": ["20000101000000000000"]}
    ls.update(extra)
    t0 = time.time()
    resp = c.post(token, [ls], timeout=60).json()
    dt = round(time.time() - t0, 1)
    val = resp["return"][0].get(DOWN)
    timings.append({"variant": label, "extra": extra, "seconds": dt,
                    "down_value_is_no_response_string": isinstance(val, str)})
    print(f"  {label:32s} {dt:5.1f}s")
capC = {
    "note": "find_job to a dead minion blocks the master's timeout+gather_job_timeout "
            "(~15s) unless BOTH are passed; salt-api honors them for the local client.",
    "timings": timings,
}
(c.OUT / "24f_findjob_timing.json").write_text(json.dumps(capC, indent=2))

c.banner("done — captures in out/24*.json")
