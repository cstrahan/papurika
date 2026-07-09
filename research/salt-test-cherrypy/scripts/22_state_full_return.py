# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""How `full_return: true` wraps state.apply results.

papurika ALWAYS injects full_return:true, so these are the exact bytes the client
decodes — not the bare shapes captured in 21_state_shapes.py. full_return turns
each minion value from `<states>` into `{"ret": <states>, "retcode": N, "out":
..., ...}`, which is how we recover the per-minion retcode (0 ok / non-zero on
any failed state) without scanning every sub-state's `result`.

The interesting question this pins down: does full_return also wrap the
LIST-of-strings error shapes (render error / no-such-sls / concurrency conflict),
and what retcode do they carry? Writes to ../out-state-shapes/fullret_*.json.
"""
import os

os.environ.setdefault("PAPURIKA_OUT", "out-state-shapes")

import subprocess

import _common as c

TGT = "builtin.minion"


def sh(cmd: str):
    subprocess.run(["docker", "exec", c.CONTAINER, "sh", "-c", cmd],
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)


def apply(name: str, sls: str, test: bool = False, timeout: int = 120):
    # full_return is a TOP-LEVEL lowstate field (a sibling of client/tgt/fun),
    # NOT a function kwarg — in kwarg it's silently ignored and no {ret,retcode}
    # wrapper appears. This mirrors command.go's lowstate() injection.
    ls = [{"client": "local", "tgt": TGT, "fun": "state.apply",
           "arg": [sls], "kwarg": {"test": test}, "full_return": True}]
    c.record(name, ls, c.post(token, ls, timeout=timeout))


token = c.login()

c.banner("full_return wrapping — success (all states pass), retcode should be 0")
sh("mkdir -p /tmp/shapes && printf 'line one\\nline two\\nline three\\n' > /tmp/shapes/managed.txt")
apply("fullret_success_nochange", "shapes.file_managed", test=False)

c.banner("full_return wrapping — a runtime failure, retcode should be non-zero")
apply("fullret_fail_cmd", "shapes.fail_cmd", test=False)

c.banner("full_return wrapping — mixed test.* (some fail), retcode reflects worst")
apply("fullret_test_states", "shapes.test_states", test=False)

c.banner("full_return wrapping — LIST error shapes (render error, no-such-sls)")
apply("fullret_render_error", "shapes.render_error", test=False)
apply("fullret_nonexistent_sls", "shapes.nonexistent", test=False)

c.banner("done")
