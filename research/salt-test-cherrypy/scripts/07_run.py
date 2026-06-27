# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""The /run endpoint — stateless, no session token.

/run bypasses session handling: each request carries its own auth, either an
eauth trio (username/password/eauth) OR a raw salt token. Useful for CI / one
-shot scripts where you don't want to maintain a session. Otherwise identical
to POST /.
"""
import requests

import _common as c


def run(name, lowstate):
    r = requests.post(
        f"{c.BASE}/run",
        headers={"Accept": "application/json", "Content-Type": "application/json"},
        json=lowstate,
        timeout=60,
        verify=c.VERIFY,
    )
    c.record(name, lowstate, r)
    return r


# 1. /run with an eauth trio embedded in the lowstate (no prior /login).
run("07a_run_with_creds", [{
    "client": "local", "tgt": "*", "fun": "test.ping",
    "username": c.USER, "password": c.PASSWORD, "eauth": c.EAUTH,
}])

# 2. /run with a raw salt token (obtained from /login) instead of creds.
token = c.login()
run("07b_run_with_token", [{
    "client": "local", "tgt": "*", "fun": "cmd.run", "arg": ["echo via-run"],
    "token": token,
}])

# 3. /run a runner via creds (master-side, no tgt).
run("07c_run_runner_creds", [{
    "client": "runner", "fun": "manage.up",
    "username": c.USER, "password": c.PASSWORD, "eauth": c.EAUTH,
}])
