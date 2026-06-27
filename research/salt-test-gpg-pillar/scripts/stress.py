# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""Cold GPG-pillar decryption stress: ratchet the secret count until something
looks wonky, across BOTH netapi modules.

For each N: regenerate N gpg-encrypted pillar secrets, then on each module
trigger a fresh pillar compile (which decrypts every secret — one gpg
subprocess per secret on the master):
  * runner  pillar.show_pillar   -> master-side render+decrypt (direct)
  * local   saltutil.refresh_pillar + pillar.items -> full api->master->minion path

"wonky" = request raised (timeout/reset) | HTTP != 200 | non-JSON body |
JSON that still contains ciphertext ("BEGIN PGP MESSAGE") or an error/traceback.
Stops at the first wonky N (per the plan: provoking a weird failure == success).
"""
import json
import subprocess
import time
from pathlib import Path

import requests

import _common as c

SCHEDULE = [5, 25, 100, 250, 500, 1000, 2000, 4000]
GEN = Path(__file__).resolve().parent.parent / "gen-gpg-pillar.sh"
ERROR_MARKERS = ("BEGIN PGP MESSAGE", "Traceback", "Rendering SLS",
                 "Unable to decrypt", "CommandExecutionError", "renderer")


def regen(n):
    print(f"\n=== regenerating {n} secrets ===")
    t0 = time.time()
    subprocess.run(["bash", str(GEN), str(n)], check=True)
    print(f"    (gen took {time.time()-t0:.1f}s)")


def classify(resp_or_exc, elapsed):
    """Return a dict describing whether the response is 'wonky'."""
    if isinstance(resp_or_exc, Exception):
        return {"wonky": True, "kind": "exception", "elapsed": elapsed,
                "detail": f"{type(resp_or_exc).__name__}: {resp_or_exc}"}
    resp = resp_or_exc
    text = resp.text
    rec = {"status": resp.status_code, "elapsed": round(elapsed, 2),
           "bytes": len(text), "ctype": resp.headers.get("Content-Type")}
    if resp.status_code != 200:
        rec.update(wonky=True, kind="http_status",
                   detail=text[:400])
        return rec
    try:
        body = resp.json()
    except ValueError:
        rec.update(wonky=True, kind="non_json", detail=text[:400])
        return rec
    blob = json.dumps(body)
    hit = next((m for m in ERROR_MARKERS if m in blob), None)
    if hit:
        rec.update(wonky=True, kind="bad_body", detail=f"marker={hit!r}")
        return rec
    rec.update(wonky=False, kind="ok")
    return rec


def trigger(label, base, token):
    """Return (record_runner, record_local) for the two triggers."""
    results = {}
    # T1: runner pillar.show_pillar — master-side render+decrypt.
    t0 = time.time()
    try:
        r = c.post(base, token, [{"client": "runner", "fun": "pillar.show_pillar",
                                  "kwarg": {"minion": "builtin.minion"}}])
        results["show_pillar"] = classify(r, time.time() - t0)
    except Exception as e:
        results["show_pillar"] = classify(e, time.time() - t0)

    # T2: refresh pillar then pillar.items — full path through the minion.
    try:
        c.post(base, token, [{"client": "local", "tgt": "*",
                              "fun": "saltutil.refresh_pillar",
                              "kwarg": {"wait": True}}], timeout=180)
    except Exception as e:
        print(f"    [{label}] refresh_pillar raised: {e}")
    t0 = time.time()
    try:
        r = c.post(base, token, [{"client": "local", "tgt": "*", "fun": "pillar.items"}])
        results["pillar_items"] = classify(r, time.time() - t0)
        # stash raw body if wonky for inspection
        if results["pillar_items"]["wonky"]:
            c.save(f"_wonky_{label}_pillar_items", {"text": r.text[:5000]})
    except Exception as e:
        results["pillar_items"] = classify(e, time.time() - t0)
    return results


all_results = []
wonky_found = False
for n in SCHEDULE:
    regen(n)
    row = {"n": n, "modules": {}}
    for label, base in c.MODULES:
        token = c.login(base)
        res = trigger(label, base, token)
        row["modules"][label] = res
        for tname, rec in res.items():
            flag = "WONKY" if rec.get("wonky") else "ok"
            print(f"  [{label:8} {tname:12}] {flag:5} "
                  f"status={rec.get('status')} {rec.get('elapsed')}s "
                  f"{rec.get('kind')} {rec.get('detail','')[:80]}")
            if rec.get("wonky"):
                wonky_found = True
    all_results.append(row)
    c.save("stress_results", all_results)
    if wonky_found:
        print(f"\n>>> provoked wonky behavior at N={n}; stopping.")
        break
else:
    print(f"\n>>> reached end of schedule (N={SCHEDULE[-1]}) without wonky responses.")

print("\nfull results -> out/stress_results.json")
