# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""Mint a real eauth token (auth.mk_token runner) and use it on /run.

Completes the /run story: prior finding was that the /login *session* token
worked on tornado's /run but 401'd on cherrypy's. A *raw eauth token* (from the
auth.mk_token runner) is the documented credential for /run — verify it works
on both, and contrast with the session token on the same module.
"""
import requests

import _common as c

session_token = c.login()

# Mint a raw eauth token via the auth runner.
ls = [{"client": "runner", "fun": "auth.mk_token",
       "kwarg": {"username": c.USER, "password": c.PASSWORD, "eauth": c.EAUTH}}]
mk = c.post(session_token, ls, timeout=30)
c.record("18a_auth_mk_token", ls, mk)
eauth_token = mk.json()["return"][0].get("token")
print("minted eauth token:", eauth_token)


def run(name, lowstate):
    r = requests.post(f"{c.BASE}/run",
                      headers={"Accept": "application/json", "Content-Type": "application/json"},
                      json=lowstate, timeout=60, verify=c.VERIFY)
    c.record(name, lowstate, r)


# /run with the raw eauth token (no session) — expect success on both modules.
run("18b_run_with_eauth_token",
    [{"client": "local", "tgt": "*", "fun": "test.ping", "token": eauth_token}])

# Contrast: the /login session token on /run (cherrypy 401'd; tornado accepted).
run("18c_run_with_session_token",
    [{"client": "local", "tgt": "*", "fun": "test.ping", "token": session_token}])
