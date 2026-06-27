"""Shared helpers for the rest_cherrypy exploration scripts.

Everything writes raw, UNtruncated responses to ../out/<name>.json so we can
inspect exact shapes after the fact. Auth uses the pam user saltdev/saltdev.
"""
from __future__ import annotations

import json
import os
import sys
import time
from pathlib import Path

import requests

BASE = os.environ.get("SALTAPI_URL", "http://localhost:8000")
USER = "saltdev"
PASSWORD = "saltdev"
EAUTH = "pam"

# Output dir is overridable (PAPURIKA_OUT) so the same scripts can capture a
# parallel run against another salt version — e.g. PAPURIKA_OUT=out-3006.
OUT = Path(__file__).resolve().parent.parent / os.environ.get("PAPURIKA_OUT", "out")
OUT.mkdir(parents=True, exist_ok=True)

# Container name for the few docker-exec steps (overridable for other versions).
CONTAINER = os.environ.get("SALT_CONTAINER", "papurika-salt")

# TLS verification. Some images (e.g. the 3006 docker image, and prod) serve the
# API over HTTPS with a self-signed cert; set PAPURIKA_VERIFY_SSL=0 to skip
# verification. Exposed as VERIFY so scripts making direct requests calls match.
VERIFY = os.environ.get("PAPURIKA_VERIFY_SSL", "1") not in ("0", "false", "False")
if not VERIFY:
    requests.packages.urllib3.disable_warnings()  # quiet the InsecureRequestWarning


def login() -> str:
    """POST /login and return the session token."""
    r = requests.post(
        f"{BASE}/login",
        headers={"Accept": "application/json", "Content-Type": "application/json"},
        json={"username": USER, "password": PASSWORD, "eauth": EAUTH},
        timeout=30,
        verify=VERIFY,
    )
    r.raise_for_status()
    return r.json()["return"][0]["token"]


def post(token: str, lowstate, path: str = "/", timeout: int = 60):
    """POST a lowstate (list of dicts) to `path`; return the parsed response."""
    r = requests.post(
        f"{BASE}{path}",
        headers={
            "Accept": "application/json",
            "Content-Type": "application/json",
            "X-Auth-Token": token,
        },
        json=lowstate,
        timeout=timeout,
        verify=VERIFY,
    )
    return r


def get(token: str, path: str, timeout: int = 60):
    r = requests.get(
        f"{BASE}{path}",
        headers={"Accept": "application/json", "X-Auth-Token": token},
        timeout=timeout,
        verify=VERIFY,
    )
    return r


def record(name: str, payload, response: requests.Response):
    """Dump request payload + full response (status, headers, body) to a file
    and echo it to stdout, untruncated."""
    try:
        body = response.json()
    except ValueError:
        body = response.text
    blob = {
        "request": {"path": response.request.path_url, "body": payload},
        "status_code": response.status_code,
        "response_headers": dict(response.headers),
        "response_body": body,
    }
    out_file = OUT / f"{name}.json"
    out_file.write_text(json.dumps(blob, indent=2, sort_keys=False))
    print(f"\n{'='*78}\n# {name}  ->  HTTP {response.status_code}\n{'='*78}")
    print("REQUEST:", json.dumps(payload, indent=2) if payload is not None else "(GET)")
    print("RESPONSE BODY:")
    print(json.dumps(body, indent=2) if not isinstance(body, str) else body)
    return body


def banner(msg: str):
    print(f"\n\n########## {msg} ##########")
