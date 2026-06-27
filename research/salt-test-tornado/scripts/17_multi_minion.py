# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""Real fleet behavior with two minions (builtin.minion + builtin.minion2).

Run add-second-minion.sh first. Tests:
  (a) aggregation: '*' test.ping returns BOTH minions
  (b) batch: local_batch batch=1 across 2 minions (cherrypy; tornado -> 400)
  (c) partial return: kill minion2, '*' test.ping -> one returns, one missing

Container-name-hardcoded for the kill step (swapped in the tornado copy).
"""
import subprocess
import time

import _common as c

CONTAINER = "papurika-salt-tornado"
token = c.login()

# (a) aggregation
ls = [{"client": "local", "tgt": "*", "fun": "test.ping"}]
c.record("17a_aggregate_test_ping", ls, c.post(token, ls, timeout=30))

# (b) batch one-at-a-time across the two minions (cherrypy only; tornado 400s)
ls = [{"client": "local_batch", "tgt": "*", "fun": "cmd.run",
       "arg": ["echo $(hostname) batched"], "batch": "1"}]
c.record("17b_local_batch_two_minions", ls, c.post(token, ls, timeout=120))

# (c) partial return: stop minion2, then target '*'
c.banner("killing builtin.minion2 for a partial-return test")
subprocess.run(["docker", "exec", "-u", "root", CONTAINER, "bash", "-c",
                "kill $(cat /var/run/salt-minion2.pid) 2>/dev/null || true"], check=True)
time.sleep(6)  # let the master notice it's gone
ls = [{"client": "local", "tgt": "*", "fun": "test.ping", "timeout": 4}]
c.record("17c_partial_return_one_down", ls, c.post(token, ls, timeout=60))
print(">>> builtin.minion2 left stopped; re-run add-second-minion.sh to restore")
