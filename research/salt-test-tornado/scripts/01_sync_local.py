# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""Synchronous `local` client: the bread-and-butter blocking calls.

Exercises test.ping, test.arg (arg/kwarg shapes), cmd.run (blocking, captures
output) vs cmd.run_bg (fire-and-forget), and a synchronous state.apply.
"""
import _common as c

token = c.login()
print("token:", token)

# 1. test.ping — simplest possible local call.
ls = [{"client": "local", "tgt": "*", "fun": "test.ping"}]
c.record("01a_test_ping", ls, c.post(token, ls))

# 2. test.arg — see how arg (positional) and kwarg (named) echo back.
ls = [{
    "client": "local", "tgt": "*", "fun": "test.arg",
    "arg": ["one", "two"], "kwarg": {"name": "papurika", "flag": True},
}]
c.record("01b_test_arg", ls, c.post(token, ls))

# 3. cmd.run — BLOCKING: the HTTP response waits for the command and returns
#    its stdout as the minion return value.
ls = [{
    "client": "local", "tgt": "*", "fun": "cmd.run",
    "arg": ["echo blocking-cmd; sleep 1; echo done"],
}]
c.record("01c_cmd_run_blocking", ls, c.post(token, ls))

# 4. cmd.run_bg — FIRE-AND-FORGET: the command is launched in the background
#    on the minion; the return is NOT the command output. Compare the shape to
#    cmd.run above.
ls = [{
    "client": "local", "tgt": "*", "fun": "cmd.run_bg",
    "arg": ["sleep 10 && echo bg-finished > /tmp/papurika-direct-bg.txt"],
}]
c.record("01d_cmd_run_bg", ls, c.post(token, ls))

# 5. state.apply (synchronous) — apply the demo highstate; full state return.
ls = [{"client": "local", "tgt": "*", "fun": "state.apply", "arg": ["demo"]}]
c.record("01e_state_apply_demo", ls, c.post(token, ls, timeout=120))
