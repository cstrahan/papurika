# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""Wheel client: minion key management + a full accept lifecycle.

To demonstrate *accepting* a pending key we drive the key through its whole
lifecycle: ensure it's deleted, restart the minion so it re-submits its pubkey
(landing in minions_pre / pending), then accept it and verify it responds.

Two practical gotchas this encodes:
  * A live, connected minion will NOT re-key just because its key was deleted
    on the master -- it must restart / re-auth. We force that with supervisorctl.
  * key acceptance is the WHEEL `key.accept` call. The /keys POST endpoint only
    generates + auto-accepts brand-new keys (returns a tarball); it does not
    accept an already-pending key.

Wheel responses are wrapped: the real payload is at return[0].data.return.
"""
import subprocess
import time

import _common as c

MINION = "builtin.minion"
token = c.login()


def wheel(fun, **kw):
    ls = [{"client": "wheel", "fun": fun, **kw}]
    return ls, c.post(token, ls)


def key_state():
    _, r = wheel("key.list_all")
    return r.json()["return"][0]["data"]["return"]


# 1. Current state.
ls, r = wheel("key.list_all")
c.record("05a_key_list_all", ls, r)

# 2. Fingerprints.
ls, r = wheel("key.finger", match="*")
c.record("05b_key_finger", ls, r)

# 3. Ensure the key is deleted (idempotent if already gone).
ls, r = wheel("key.delete", match=MINION)
c.record("05c_key_delete", ls, r)

# 4. Restart the minion so it re-submits its public key -> pending.
print("\n>>> restarting salt-minion to force re-key ...")
subprocess.run(
    ["docker", "exec", "papurika-salt", "supervisorctl", "restart", "salt-minion"],
    check=True,
)

# 5. Wait for it to land in minions_pre (pending).
print(">>> waiting for PENDING ...")
for attempt in range(60):
    st = key_state()
    print(f"  poll {attempt}: minions={st['minions']} minions_pre={st['minions_pre']}")
    if MINION in st["minions_pre"]:
        (c.OUT / "05d_key_pending.json").write_text(
            __import__("json").dumps(st, indent=2)
        )
        break
    time.sleep(1)

# 6. Accept the pending key.
ls, r = wheel("key.accept", match=MINION)
c.record("05e_key_accept", ls, r)

# 7. Confirm accepted.
ls, r = wheel("key.list_all")
c.record("05f_key_list_after_accept", ls, r)

# 8. Wait for the minion to reconnect, then prove it responds.
print("\n>>> waiting for minion to reconnect, then test.ping ...")
for attempt in range(40):
    ls = [{"client": "local", "tgt": "*", "fun": "test.ping"}]
    r = c.post(token, ls, timeout=15)
    ret = r.json()["return"][0]
    print(f"  ping {attempt}: {ret}")
    if ret.get(MINION) is True:
        c.record("05g_test_ping_recovered", ls, r)
        break
    time.sleep(2)
