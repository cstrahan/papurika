# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""rest_tornado supports FEWER clients than rest_cherrypy.

tornado's saltnado.py hardcodes self.saltclients = {local, local_async, runner,
runner_async} (saltnado.py:458) and rejects anything else with HTTP 400 in
_verify_client (saltnado.py:434) -- regardless of netapi_enable_clients. So
wheel/wheel_async (key mgmt), local_batch/local_subset (batching), and ssh are
simply unavailable here. We capture the exact rejection for each.

Consequences for a client targeting tornado:
  * key management -> not via this API (use rest_cherrypy on the same master,
    or salt-key); there is no wheel handler.
  * batching -> must be done client-side (chunk the minion list, fan out
    local_async with explicit tgt subsets).
"""
import _common as c

token = c.login()

# What tornado advertises as supported.
c.record("10a_get_root_clients", None, c.get(token, "/"))

# Each unsupported client -> HTTP 400 "Invalid Client".
for client in ("wheel", "wheel_async", "local_batch", "local_subset", "ssh"):
    ls = [{"client": client, "fun": "key.list_all", "tgt": "*"}]
    c.record(f"10_unsupported_{client}", ls, c.post(token, ls, timeout=15))
