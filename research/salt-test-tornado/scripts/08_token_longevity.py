# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""Confirm the token/cache patch fixed eviction.

Pre-patch, a token was swept within ~60s (TokenExpiredError) despite
token_expire=43200. Log in once, use the token, wait past a maintenance sweep,
then reuse the SAME token. With the patch it should still authenticate.
"""
import time

import _common as c

token = c.login()
print("token:", token)

ls = [{"client": "local", "tgt": "*", "fun": "test.ping"}]
print("t=0   ->", c.post(token, ls, timeout=20).json())

for wait in (40, 40, 40):  # ~120s total, crossing multiple sweeps
    time.sleep(wait)
    r = c.post(token, ls, timeout=20)
    ok = r.status_code == 200 and r.json().get("return")
    print(f"t=+{wait}s -> HTTP {r.status_code} ok={bool(ok)} {r.text[:120]}")
