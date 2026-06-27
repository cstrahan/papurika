# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""Kill a jid, then kill the same jid again (and once more).

saltutil.kill_job finds the running job by jid in the minion's proc cache and
SIGKILLs it. Once it's gone, re-killing the same jid should behave like killing
a non-existent jid. Captures all three kills + jobs.active between them.
(netapi-agnostic local exec; run on cherrypy.)
"""
import time

import _common as c

token = c.login()


def kill(jid):
    ls = [{"client": "local", "tgt": "*", "fun": "saltutil.kill_job", "arg": [jid]}]
    return ls, c.post(token, ls, timeout=60)


def active():
    ls = [{"client": "runner", "fun": "jobs.active"}]
    return ls, c.post(token, ls, timeout=60)


# Launch a long job to have something to kill.
jid = c.post(token, [{"client": "local_async", "tgt": "*", "fun": "cmd.run",
                      "arg": ["sleep 120"]}]).json()["return"][0]["jid"]
print("jid:", jid)
time.sleep(2)

c.record("13a_active_before", *active())
c.record("13b_kill_1st", *kill(jid))      # expect: "Signal 9 sent to job <jid> at pid <pid>"
time.sleep(2)                              # let the job actually die / proc entry clear
c.record("13c_active_after_kill", *active())
c.record("13d_kill_2nd_same_jid", *kill(jid))   # the question: what now?
c.record("13e_kill_3rd_same_jid", *kill(jid))
