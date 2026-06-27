# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""Unhappy paths: self-killing and pathologically-long commands (sync local).

1. cmd.run that kills the very minion executing it (pkill -9 salt-minion) ->
   the minion dies mid-job and never returns; supervisor (autorestart) brings
   it back. Observe what the API returns for the vanished job.
2. cmd.run 'sleep 300' (SYNC): the HTTP call can't wait 5 minutes — observe how
   long it actually blocks and what it returns while the job keeps running.
3. cmd.run_bg 'sleep 300' (fire-and-forget): should return immediately with a
   PID regardless of the 5-minute command.

Container-agnostic (recovery relies on supervisor autorestart), so identical in
the cherrypy and tornado harnesses; only _common.BASE differs.
"""
import json
import time

import _common as c

token = c.login()


def save_exc(name, elapsed, exc):
    blob = {"elapsed": elapsed, "exception": f"{type(exc).__name__}: {exc}"}
    (c.OUT / f"{name}.json").write_text(json.dumps(blob, indent=2))
    print(f"  raised after {elapsed}s: {type(exc).__name__}: {exc}")


def wait_minion_back(tries=45):
    for i in range(tries):
        try:
            r = c.post(token, [{"client": "local", "tgt": "*", "fun": "test.ping"}], timeout=15)
            if r.status_code == 200 and r.json()["return"][0].get("builtin.minion") is True:
                print(f"  minion recovered after ~{i*2}s")
                return True
        except Exception:
            pass
        time.sleep(2)
    print("  minion did NOT recover in time")
    return False


# ---- Test 1: cmd.run that kills its own minion --------------------------------
c.banner("Test 1: cmd.run 'pkill -9 -f salt-minion' (minion suicide mid-command)")
ls = [{"client": "local", "tgt": "*", "fun": "cmd.run", "arg": ["pkill -9 -f salt-minion"]}]
t0 = time.time()
try:
    r = c.post(token, ls, timeout=90)
    print(f"  returned after {round(time.time()-t0,1)}s")
    c.record("11a_cmd_run_minion_suicide", ls, r)
except Exception as e:
    save_exc("11a_cmd_run_minion_suicide", round(time.time()-t0, 1), e)
token = c.login()
wait_minion_back()

# ---- Test 2: sync cmd.run 'sleep 300' -----------------------------------------
c.banner("Test 2: sync cmd.run 'sleep 300' (pathologically long, blocking)")
ls = [{"client": "local", "tgt": "*", "fun": "cmd.run", "arg": ["sleep 300"]}]
t0 = time.time()
try:
    r = c.post(token, ls, timeout=120)
    print(f"  returned after {round(time.time()-t0,1)}s")
    c.record("11b_cmd_run_sleep300_sync", ls, r)
except Exception as e:
    save_exc("11b_cmd_run_sleep300_sync", round(time.time()-t0, 1), e)
# the sleep keeps running even though the API gave up — show it as an active job
r = c.post(token, [{"client": "runner", "fun": "jobs.active"}], timeout=30)
c.record("11b2_jobs_active_after_sleep", [{"client": "runner", "fun": "jobs.active"}], r)

# ---- Test 3: cmd.run_bg 'sleep 300' -------------------------------------------
c.banner("Test 3: cmd.run_bg 'sleep 300' (fire-and-forget)")
ls = [{"client": "local", "tgt": "*", "fun": "cmd.run_bg", "arg": ["sleep 300"]}]
t0 = time.time()
r = c.post(token, ls, timeout=60)
print(f"  returned after {round(time.time()-t0,1)}s")
c.record("11c_cmd_run_bg_sleep300", ls, r)
