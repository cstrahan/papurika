# /// script
# requires-python = ">=3.11"
# dependencies = ["requests>=2.31"]
# ///
"""Concurrency dimension: simulate many minions hitting a cold GPG pillar at once.

A single sequential pillar compile only gets *slow* (linear in secret count).
The realistic prod trigger is many minions compiling pillar simultaneously: each
compile forks one `gpg` per secret, so K concurrent compiles fork ~K*N gpg
processes on the master at once -> CPU/fork/thread-pool pressure that can surface
as 5xx / HTML / timeouts / dropped connections.

Usage: stress_concurrent.py [N] [K]   (regenerates N secrets, fires K concurrent
runner pillar.show_pillar requests per module).
"""
import json
import subprocess
import sys
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

import requests

import _common as c

N = int(sys.argv[1]) if len(sys.argv) > 1 else 1000
K = int(sys.argv[2]) if len(sys.argv) > 2 else 40
GEN = Path(__file__).resolve().parent.parent / "gen-gpg-pillar.sh"


def one_call(base, token, i):
    t0 = time.time()
    try:
        r = c.post(base, token, [{"client": "runner", "fun": "pillar.show_pillar",
                                  "kwarg": {"minion": "builtin.minion"}}], timeout=120)
        ok = r.status_code == 200
        try:
            r.json()
            json_ok = True
        except ValueError:
            json_ok = False
        return {"i": i, "status": r.status_code, "elapsed": round(time.time() - t0, 2),
                "ok": ok and json_ok, "json": json_ok, "bytes": len(r.text),
                "snippet": "" if json_ok else r.text[:200]}
    except Exception as e:
        return {"i": i, "status": None, "elapsed": round(time.time() - t0, 2),
                "ok": False, "json": None, "error": f"{type(e).__name__}: {e}"}


print(f"=== regenerating {N} secrets ===")
subprocess.run(["bash", str(GEN), str(N)], check=True)

summary = {"n": N, "k": K, "modules": {}}
for label, base in c.MODULES:
    token = c.login(base)
    print(f"\n=== {label}: firing {K} concurrent pillar.show_pillar (N={N}) ===")
    t0 = time.time()
    with ThreadPoolExecutor(max_workers=K) as ex:
        results = list(ex.map(lambda i: one_call(base, token, i), range(K)))
    wall = round(time.time() - t0, 2)
    oks = [r for r in results if r["ok"]]
    bad = [r for r in results if not r["ok"]]
    statuses = {}
    for r in results:
        key = r.get("error", str(r["status"]))
        statuses[key] = statuses.get(key, 0) + 1
    print(f"  wall={wall}s  ok={len(oks)}/{K}  bad={len(bad)}  status/error breakdown: {statuses}")
    for r in bad[:5]:
        print(f"    BAD i={r['i']} status={r.get('status')} {r.get('error','')}{r.get('snippet','')[:120]}")
    summary["modules"][label] = {"wall": wall, "ok": len(oks), "bad": len(bad),
                                 "breakdown": statuses, "results": results}

c.save(f"concurrent_N{N}_K{K}", summary)
print(f"\nsaved -> out/concurrent_N{N}_K{K}.json")
