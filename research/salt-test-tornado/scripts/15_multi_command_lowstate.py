# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""Multiple lowstates in a single POST — run serially, no stop-on-failure.

We've only ever sent single-element arrays. Verify: (a) a mix of client types
in one request returns a parallel response array in order; (b) a failing
command mid-array does NOT abort the rest.
"""
import _common as c

token = c.login()

# (a) mixed client types, one request -> ordered response array
ls = [
    {"client": "local", "tgt": "*", "fun": "test.ping"},
    {"client": "runner", "fun": "manage.status"},
    {"client": "local", "tgt": "*", "fun": "grains.get", "arg": ["id"]},
]
c.record("15a_multi_mixed_clients", ls, c.post(token, ls, timeout=60))

# (b) failure mid-array -> later commands still run
ls = [
    {"client": "local", "tgt": "*", "fun": "does.not.exist"},          # errors
    {"client": "local", "tgt": "*", "fun": "test.ping"},               # should still run
    {"client": "local", "tgt": "*", "fun": "cmd.run", "arg": ["exit 7"], "full_return": True},  # non-zero
]
c.record("15b_multi_with_failure", ls, c.post(token, ls, timeout=60))
