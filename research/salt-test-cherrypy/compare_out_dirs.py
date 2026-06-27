# /// script
# requires-python = ">=3.11"
# ///
"""Structurally diff two capture dirs (e.g. out/ vs out-3006/) to surface
schema/behaviour changes between salt versions, ignoring volatile noise
(minion ids, jids, timestamps, pids, tokens, ips).

Usage: uv run compare_out_dirs.py <baseline_dir> <other_dir>
       (baseline = 3006, other = 3008, by convention here)
"""
import json
import re
import sys
from pathlib import Path

JID = re.compile(r"^\d{17,20}$")
HEX = re.compile(r"^[0-9a-f]{40,64}$")
MINIONS = {"builtin.minion", "builtin.minion2"}
VOLATILE_KEYS = {"_stamp", "start", "expire", "StartTime", "start_time", "duration",
                 "ts", "jid", "pid", "fqdn", "machine_id", "server_id", "Date"}


def nkey(k):
    if k in MINIONS:
        return "<MINION>"
    if JID.match(str(k)) or HEX.match(str(k)):
        return "<ID>"
    return k


def paths(obj, prefix=""):
    """Set of (path, leaf-type) — the structural signature."""
    out = set()
    if isinstance(obj, dict):
        if not obj:
            out.add((prefix, "{}"))
        for k, v in obj.items():
            out |= paths(v, f"{prefix}.{nkey(k)}")
    elif isinstance(obj, list):
        if not obj:
            out.add((prefix, "[]"))
        for el in obj[:4]:
            out |= paths(el, prefix + "[]")
    else:
        out.add((prefix, type(obj).__name__))
    return out


def body_and_status(path):
    d = json.loads(path.read_text())
    if isinstance(d, dict) and "response_body" in d:
        return d.get("response_body"), d.get("status_code")
    return d, None  # custom dumps (16*, etc.)


base_dir, other_dir = Path(sys.argv[1]), Path(sys.argv[2])
base_files = {p.name for p in base_dir.glob("*.json")}
other_files = {p.name for p in other_dir.glob("*.json")}

print(f"# baseline={base_dir.name}  other={other_dir.name}\n")
only_base = sorted(base_files - other_files)
only_other = sorted(other_files - base_files)
if only_base:
    print(f"files only in {base_dir.name}: {only_base}")
if only_other:
    print(f"files only in {other_dir.name}: {only_other}")
print()

for name in sorted(base_files & other_files):
    b_body, b_status = body_and_status(base_dir / name)
    o_body, o_status = body_and_status(other_dir / name)
    b_paths, o_paths = paths(b_body), paths(o_body)
    only_b = sorted(p for p in b_paths - o_paths)
    only_o = sorted(p for p in o_paths - b_paths)
    diffs = []
    if b_status != o_status:
        diffs.append(f"  STATUS: {base_dir.name}={b_status}  {other_dir.name}={o_status}")
    if only_b:
        diffs.append(f"  paths only in {base_dir.name}: {only_b}")
    if only_o:
        diffs.append(f"  paths only in {other_dir.name}: {only_o}")
    if diffs:
        print(f"### {name}")
        print("\n".join(diffs))
        print()
