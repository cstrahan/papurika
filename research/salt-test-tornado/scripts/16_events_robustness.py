# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""/events SSE robustness — backs the async-first design.

(1) reconnect gap: events fired while disconnected are NOT replayed on
    reconnect (client must backfill via /jobs).
(2) idle/heartbeat: what (if anything) the server sends on an idle stream.
(3) master restart: does the stream survive a salt-master restart, and do
    events resume after reconnect?

Container-name-hardcoded for the master-restart step (swapped in the tornado copy).
"""
import json
import subprocess
import threading
import time

import requests

import _common as c

CONTAINER = "papurika-salt-tornado"
token = c.login()


def dump(name, obj):
    (c.OUT / f"{name}.json").write_text(json.dumps(obj, indent=2))
    print(f"  saved {name}: {json.dumps(obj)[:160]}")


class Streamer:
    """Background SSE consumer. Omit Accept (tornado 406s on text/event-stream)."""
    def __init__(self, tok):
        self.tok = tok
        self.events = []   # (t, tag)
        self.raw = []      # (t, line)
        self.err = None
        self._stop = False
        self._resp = None
        self._t = threading.Thread(target=self._run, daemon=True)

    def start(self):
        self._t.start()
        time.sleep(1.5)  # let the subscription establish
        return self

    def _run(self):
        try:
            self._resp = requests.get(f"{c.BASE}/events",
                                      headers={"X-Auth-Token": self.tok},
                                      stream=True, timeout=60)
            cur = {}
            for raw in self._resp.iter_lines(decode_unicode=True):
                if self._stop:
                    break
                self.raw.append((round(time.time(), 2), raw))
                if raw == "":
                    if "data" in cur:
                        self.events.append((round(time.time(), 2), cur.get("tag")))
                    cur = {}
                elif raw.startswith("tag: "):
                    cur["tag"] = raw[5:]
                elif raw.startswith("data: "):
                    cur["data"] = raw[6:]
        except Exception as e:
            self.err = f"{type(e).__name__}: {e}"

    def tags(self):
        return [t for _, t in self.events]

    def close(self):
        self._stop = True
        try:
            self._resp.close()
        except Exception:
            pass
        time.sleep(0.3)


def fire(tok):
    return c.post(tok, [{"client": "local_async", "tgt": "*", "fun": "test.ping"}]).json()["return"][0]["jid"]


# (1) reconnect gap ------------------------------------------------------------
c.banner("(1) reconnect gap — events fired while disconnected are not replayed")
s1 = Streamer(token).start()
time.sleep(1)
s1.close()                       # disconnect
jid_missed = fire(token)         # fired with nobody listening
time.sleep(2)
s2 = Streamer(token).start()     # reconnect
time.sleep(0.5)
jid_seen = fire(token)
time.sleep(3)
s2.close()
dump("16a_reconnect_gap", {
    "jid_missed": jid_missed, "jid_seen": jid_seen,
    "s2_tags": s2.tags(),
    "missed_replayed_after_reconnect": any(jid_missed in (t or "") for t in s2.tags()),
    "seen_after_reconnect": any(jid_seen in (t or "") for t in s2.tags()),
})

# (2) idle / heartbeat ---------------------------------------------------------
c.banner("(2) idle stream for ~30s (heartbeat?)")
s = Streamer(token).start()
t0 = time.time()
time.sleep(30)                   # no jobs fired
s.close()
dump("16b_idle_heartbeat", {
    "idle_seconds": round(time.time() - t0, 1),
    "raw_lines": s.raw,          # full raw frames seen while idle
    "raw_line_count": len(s.raw),
    "events_during_idle": s.tags(),
    "stream_error": s.err,
})

# (3) master restart -----------------------------------------------------------
c.banner("(3) master restart while streaming")
s = Streamer(token).start()
time.sleep(0.5)
fire(token)                      # confirm events flow pre-restart
time.sleep(2)
pre_tags = list(s.tags())
subprocess.run(["docker", "exec", CONTAINER, "supervisorctl", "restart", "salt-master"], check=True)
time.sleep(8)                    # master down→up
stream_error_on_restart = s.err
s.close()
# reconnect after restart — salt-api may be briefly bounced by the watchdog, so
# retry login until it comes back.
tok2 = None
for _ in range(30):
    try:
        tok2 = c.login()
        break
    except Exception:
        time.sleep(2)
s3 = Streamer(tok2).start()
time.sleep(0.5)
jid_after = fire(tok2)
time.sleep(3)
s3.close()
dump("16c_master_restart", {
    "pre_restart_event_tags": pre_tags,
    "stream_error_on_restart": stream_error_on_restart,
    "reconnected_login_ok": bool(tok2),
    "jid_after_restart": jid_after,
    "events_resumed_after_reconnect": any(jid_after in (t or "") for t in s3.tags()),
    "s3_tags": s3.tags(),
})
