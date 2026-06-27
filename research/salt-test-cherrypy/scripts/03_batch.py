# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""Batched local execution (local_batch).

Batch limits how many minions run at once. With a single test minion we can't
show real throttling, but we CAN capture the response *shape* the batch client
produces, which differs from the plain local client (relevant for result
normalization). Captures a batched cmd.run and a batched state.apply.
"""
import _common as c

token = c.login()

# Batched cmd.run. `batch` may be a count ("1") or percentage ("50%").
ls = [{"client": "local_batch", "tgt": "*", "fun": "cmd.run",
       "arg": ["echo batched"], "batch": "1"}]
c.record("03a_local_batch_cmd_run", ls, c.post(token, ls, timeout=120))

# Batched state.apply — compare the per-state result nesting vs the sync
# (non-batch) state.apply captured in 01e.
ls = [{"client": "local_batch", "tgt": "*", "fun": "state.apply",
       "arg": ["demo"], "batch": "50%"}]
c.record("03b_local_batch_state_apply", ls, c.post(token, ls, timeout=120))
