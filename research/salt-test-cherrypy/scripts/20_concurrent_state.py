# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""What does salt-api return when a SECOND state.apply lands while one is still
running on the same minion?

Salt guards its state subsystem with a per-minion run lock: only one state run
(state.apply / state.sls / state.highstate / ...) may execute at a time. A second
state function submitted while one is in flight is REJECTED — the minion returns
a *list* containing a single string naming the PID/jid of the run that holds the
lock, with retcode 1 / success False.

Two important, non-obvious properties this probe pins down:
  1. The lock is global to the minion's state subsystem — it rejects ANY
     state.apply (even a different SLS), not just a duplicate of the running one.
  2. The rejection surfaces as the SAME minion-value shape as a render error or a
     "no matching sls" error: a list-of-strings, not the {state_id: {...}} dict.
     A normalizer that assumes state.apply always yields a dict WILL break here.

We also capture the `queue=True` escape hatch, which makes the second run wait
for the lock instead of being rejected.

Writes to ../out-state-shapes/. Target is a single minion for clean captures.
"""
import os

os.environ.setdefault("PAPURIKA_OUT", "out-state-shapes")

import time

import _common as c

TGT = "builtin.minion"


def local_async(token, sls, kwarg=None):
    ls = [{"client": "local_async", "tgt": TGT, "fun": "state.apply", "arg": [sls]}]
    if kwarg:
        ls[0]["kwarg"] = kwarg
    return ls


def local_sync(token, sls, kwarg=None):
    ls = [{"client": "local", "tgt": TGT, "fun": "state.apply", "arg": [sls]}]
    if kwarg:
        ls[0]["kwarg"] = kwarg
    return ls


token = c.login()

c.banner("Start a long state run (slow.sls sleeps 20s) to hold the state lock")
ls = local_async(token, "slow")
body = c.record("concurrency_00_submit_slow_async", ls, c.post(token, ls))
jid_a = body["return"][0].get("jid")
print("holder jid:", jid_a)

time.sleep(3)  # give the minion time to acquire the state-run lock

c.banner("SYNC state.apply while the run is in flight -> expect rejection")
ls = local_sync(token, "slow")
c.record("concurrency_01_sync_conflict", ls, c.post(token, ls, timeout=60))

c.banner("ASYNC state.apply while in flight -> jid is issued, rejection lands in /jobs")
ls = local_async(token, "slow")
body = c.record("concurrency_02_async_conflict_submit", ls, c.post(token, ls))
jid_c = body["return"][0].get("jid")
time.sleep(3)
c.record("concurrency_03_async_conflict_jobs", None,
         c.get(token, f"/jobs/{jid_c}", timeout=30))

c.banner("A DIFFERENT sls is rejected too -> lock is global to the state subsystem")
ls = local_sync(token, "demo")
c.record("concurrency_04_sync_conflict_other_sls", ls, c.post(token, ls, timeout=60))

c.banner("queue=True makes the second run WAIT for the lock instead of failing")
# This blocks until jid_a releases the lock (~remaining sleep) and then runs its
# own slow state, so it can take up to ~40s total. timeout is generous.
ls = local_sync(token, "slow", kwarg={"queue": True})
c.record("concurrency_05_sync_queue_true", ls, c.post(token, ls, timeout=120))

c.banner("done; minion left idle")
