# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""Unhappy path: a targeted minion that doesn't respond.

We stop the minion process but leave its key ACCEPTED, so salt still targets it
(it's in the accepted-key list) but never gets a reply. This is the classic
"minion down / hung" situation. We capture how it surfaces across:
  * sync local (does it block for the timeout? what's in the body?)
  * local_async + /jobs/<jid> (targeted vs returned)
  * runner manage.status (down list)
  * jobs.print_job (the data you need to detect non-responders)
  * a 'no minions matched' target for contrast
Then we restart the minion and confirm recovery.
"""
import subprocess
import time

import _common as c

MINION = "builtin.minion"
token = c.login()


def dexec(*args):
    subprocess.run(["docker", "exec", "papurika-salt", *args], check=True)


# Precondition: minion up & accepted.
c.banner("stopping salt-minion (key stays accepted)")
dexec("supervisorctl", "stop", "salt-minion")
time.sleep(3)

# 1. Confirm the master now sees it as down.
ls = [{"client": "runner", "fun": "manage.status"}]
c.record("09a_manage_status_down", ls, c.post(token, ls, timeout=60))

# 2. SYNC local test.ping with an explicit short gather timeout. Measure how
#    long the HTTP call blocks and what the body contains for a no-show minion.
ls = [{"client": "local", "tgt": "*", "fun": "test.ping", "timeout": 4}]
t0 = time.time()
r = c.post(token, ls, timeout=60)
elapsed = round(time.time() - t0, 2)
print(f"\n>>> sync local test.ping blocked for {elapsed}s")
c.record("09b_sync_ping_no_response", ls, r)

# 3. local_async: the targeted minion list still includes the down minion.
ls = [{"client": "local_async", "tgt": "*", "fun": "test.ping"}]
body = c.record("09c_async_targeted", ls, c.post(token, ls))
jid = body["return"][0]["jid"]

# 4. After a wait, /jobs/<jid> — Result should stay empty (never returned).
time.sleep(6)
c.record("09d_jobs_jid_no_return", None, c.get(token, f"/jobs/{jid}"))

# 5. jobs.print_job — shows Minions targeted vs Result, the basis for
#    detecting which minions did NOT respond.
ls = [{"client": "runner", "fun": "jobs.print_job", "arg": [jid]}]
c.record("09e_jobs_print_job", ls, c.post(token, ls, timeout=60))

# 6. Contrast: a target that matches NO minion at all.
ls = [{"client": "local", "tgt": "ghost-minion", "fun": "test.ping",
       "tgt_type": "glob", "timeout": 4}]
t0 = time.time()
r = c.post(token, ls, timeout=60)
print(f"\n>>> sync ping to non-existent target blocked for {round(time.time()-t0,2)}s")
c.record("09f_no_minions_matched", ls, r)

# Restore the minion.
c.banner("restarting salt-minion")
dexec("supervisorctl", "start", "salt-minion")
for attempt in range(40):
    ls = [{"client": "local", "tgt": "*", "fun": "test.ping"}]
    try:
        ret = c.post(token, ls, timeout=15).json()["return"][0]
    except Exception as e:
        ret = {"err": str(e)[:60]}
    print(f"  ping {attempt}: {ret}")
    if ret.get(MINION) is True:
        break
    time.sleep(2)
print(">>> minion restored")
