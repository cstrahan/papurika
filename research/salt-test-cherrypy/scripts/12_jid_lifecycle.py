# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""jid lifecycle: find / cancel, for existing and non-existent jids.

find   = jobs.lookup_jid / jobs.print_job (runner) + GET /jobs/<jid> (endpoint)
cancel = saltutil.kill_job (local exec; SIGKILLs the running job by jid)

Captures the response for each, in both the success and the bogus-jid cases.
(Job-cache retention/persistence is master-side and netapi-agnostic — see the
"jid retention" notes in the learnings doc.)
"""
import time

import _common as c

token = c.login()
BOGUS = "20200101000000000000"  # well-formed but never-issued jid


def runner(fun, *arg):
    ls = [{"client": "runner", "fun": fun, "arg": list(arg)}]
    return ls, c.post(token, ls, timeout=60)


def local(fun, *arg):
    ls = [{"client": "local", "tgt": "*", "fun": fun, "arg": list(arg)}]
    return ls, c.post(token, ls, timeout=60)


# ---- FIND: existing jid -------------------------------------------------------
c.banner("FIND an existing jid")
jid = c.post(token, [{"client": "local_async", "tgt": "*", "fun": "test.ping"}]).json()["return"][0]["jid"]
print("issued jid:", jid)
time.sleep(3)  # let the return land in the cache
c.record("12a_lookup_jid_found", *runner("jobs.lookup_jid", jid))
c.record("12b_print_job_found", *runner("jobs.print_job", jid))
c.record("12c_get_jobs_jid_found", None, c.get(token, f"/jobs/{jid}"))

# ---- CANCEL: a running jid ----------------------------------------------------
c.banner("CANCEL a running jid (saltutil.kill_job)")
jid_long = c.post(token, [{"client": "local_async", "tgt": "*", "fun": "cmd.run",
                           "arg": ["sleep 120"]}]).json()["return"][0]["jid"]
print("long jid:", jid_long)
time.sleep(2)
c.record("12d_jobs_active_running", *runner("jobs.active"))
c.record("12e_kill_job_success", *local("saltutil.kill_job", jid_long))
time.sleep(2)
c.record("12f_jobs_active_after_kill", *runner("jobs.active"))

# ---- non-existent jid ---------------------------------------------------------
c.banner("FIND / CANCEL a non-existent jid")
c.record("12g_lookup_jid_missing", *runner("jobs.lookup_jid", BOGUS))
c.record("12h_print_job_missing", *runner("jobs.print_job", BOGUS))
c.record("12i_get_jobs_jid_missing", None, c.get(token, f"/jobs/{BOGUS}"))
c.record("12j_kill_job_missing", *local("saltutil.kill_job", BOGUS))
