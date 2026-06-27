# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""Minion status/liveness tools: manage.* vs a direct test.ping.

Run with builtin.minion UP and builtin.minion2 DOWN to expose the contrast:
manage.status/up/down (live ping, but buggy) report BOTH up, while a direct
local test.ping correctly shows minion2 not responding; manage.present/alived
(cache) depend on grain-IP matching. See ../minion-status-and-liveness.md.
Captures each call with its elapsed time.
"""
import time

import _common as c

token = c.login()


def runner(name, fun):
    ls = [{"client": "runner", "fun": fun}]
    t0 = time.time()
    r = c.post(token, ls, timeout=60)
    print(f"  {fun:16} {round(time.time()-t0,2)}s")
    c.record(name, ls, r)


c.banner("manage.* status/liveness tools (runner)")
runner("19a_manage_status", "manage.status")
runner("19b_manage_up", "manage.up")
runner("19c_manage_down", "manage.down")
runner("19d_manage_present", "manage.present")
runner("19e_manage_alived", "manage.alived")
runner("19f_manage_list_state", "manage.list_state")

c.banner("authoritative: direct local test.ping")
ls = [{"client": "local", "tgt": "*", "fun": "test.ping", "timeout": 5}]
t0 = time.time()
r = c.post(token, ls, timeout=60)
print(f"  direct test.ping {round(time.time()-t0,2)}s")
c.record("19g_direct_test_ping", ls, r)
