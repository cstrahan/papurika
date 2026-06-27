# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""Failure / outcome taxonomy: the exact shape of every logical failure.

A robust client must classify responses (auth-fail vs authz-deny vs not-found
vs minion-error vs command-failure vs malformed) rather than guess. Capture the
status + body (HTML vs JSON) for each. Needs a scoped eauth user `saltguest`
(perms test.ping + grains.* only) — see config/zz-papurika.conf + the OS user
created by the run wrapper.
"""
import json

import requests

import _common as c

token = c.login()  # saltdev superuser


def login_as(user, password, eauth="pam"):
    """Login with arbitrary creds; return the requests.Response."""
    return requests.post(
        f"{c.BASE}/login",
        headers={"Accept": "application/json", "Content-Type": "application/json"},
        json={"username": user, "password": password, "eauth": eauth},
        timeout=30, verify=c.VERIFY
    )


def raw_post(name, body_text, hdr_token=token):
    """POST a raw (possibly malformed) body string."""
    r = requests.post(
        f"{c.BASE}/",
        headers={"Accept": "application/json", "Content-Type": "application/json",
                 "X-Auth-Token": hdr_token},
        data=body_text,
        timeout=30, verify=c.VERIFY
    )
    c.record(name, {"raw_body": body_text}, r)


# ---- AUTH failures ------------------------------------------------------------
c.banner("AUTH failures")
c.record("14a_bad_token", [{"client": "local", "tgt": "*", "fun": "test.ping"}],
         c.post("0000garbage-not-a-real-token0000", [{"client": "local", "tgt": "*", "fun": "test.ping"}]))
c.record("14b_login_wrong_password", {"username": "saltdev", "password": "WRONG", "eauth": "pam"},
         login_as("saltdev", "WRONG"))
c.record("14c_login_bad_eauth_backend", {"username": "saltdev", "password": "saltdev", "eauth": "nosuchbackend"},
         login_as("saltdev", "saltdev", "nosuchbackend"))

# ---- AUTHZ (perms) ------------------------------------------------------------
c.banner("AUTHZ — scoped user saltguest (test.ping + grains.* only)")
guest_login = login_as("saltguest", "saltguest")
c.record("14d0_saltguest_login", {"username": "saltguest"}, guest_login)
guest = guest_login.json()["return"][0]["token"]
# allowed (control):
c.record("14d1_authz_allowed_test_ping", [{"client": "local", "tgt": "*", "fun": "test.ping"}],
         c.post(guest, [{"client": "local", "tgt": "*", "fun": "test.ping"}]))
# denied (cmd.run not in perms):
c.record("14d2_authz_denied_cmd_run", [{"client": "local", "tgt": "*", "fun": "cmd.run", "arg": ["id"]}],
         c.post(guest, [{"client": "local", "tgt": "*", "fun": "cmd.run", "arg": ["id"]}]))

# ---- BAD function -------------------------------------------------------------
c.banner("Unknown function")
c.record("14f_unknown_fun", [{"client": "local", "tgt": "*", "fun": "does.not.exist"}],
         c.post(token, [{"client": "local", "tgt": "*", "fun": "does.not.exist"}]))

# ---- MALFORMED requests -------------------------------------------------------
c.banner("Malformed requests")
c.record("14g_missing_fun", [{"client": "local", "tgt": "*"}],
         c.post(token, [{"client": "local", "tgt": "*"}]))
c.record("14h_missing_tgt", [{"client": "local", "fun": "test.ping"}],
         c.post(token, [{"client": "local", "fun": "test.ping"}]))
c.record("14i_body_not_a_list", {"client": "local", "tgt": "*", "fun": "test.ping"},
         c.post(token, {"client": "local", "tgt": "*", "fun": "test.ping"}))  # dict, not list
raw_post("14j_invalid_json", '{ this is not valid json ]')

# ---- Command / state FAILURE (non-zero retcode / result:false) ----------------
c.banner("Command / state failure")
c.record("14k_cmd_run_exit3", [{"client": "local", "tgt": "*", "fun": "cmd.run", "arg": ["exit 3"], "full_return": True}],
         c.post(token, [{"client": "local", "tgt": "*", "fun": "cmd.run", "arg": ["exit 3"], "full_return": True}]))
c.record("14l_state_fail", [{"client": "local", "tgt": "*", "fun": "state.single",
                             "arg": ["test.fail_without_changes"], "kwarg": {"name": "demo-fail"}, "full_return": True}],
         c.post(token, [{"client": "local", "tgt": "*", "fun": "state.single",
                         "arg": ["test.fail_without_changes"], "kwarg": {"name": "demo-fail"}, "full_return": True}]))
