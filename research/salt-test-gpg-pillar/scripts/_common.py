"""Helpers for the GPG-pillar stress harness — hits BOTH netapi modules.

cherrypy on :8000, tornado on :8001. Auth: pam saltdev/saltdev.
"""
from __future__ import annotations

import json
from pathlib import Path

import requests

CHERRYPY = "http://localhost:8000"
TORNADO = "http://localhost:8001"
MODULES = [("cherrypy", CHERRYPY), ("tornado", TORNADO)]

USER, PASSWORD, EAUTH = "saltdev", "saltdev", "pam"
OUT = Path(__file__).resolve().parent.parent / "out"
OUT.mkdir(exist_ok=True)


def login(base: str) -> str:
    r = requests.post(
        f"{base}/login",
        headers={"Accept": "application/json", "Content-Type": "application/json"},
        json={"username": USER, "password": PASSWORD, "eauth": EAUTH},
        timeout=30,
    )
    r.raise_for_status()
    return r.json()["return"][0]["token"]


def post(base: str, token: str, lowstate, timeout: int = 180):
    return requests.post(
        f"{base}/",
        headers={
            "Accept": "application/json",
            "Content-Type": "application/json",
            "X-Auth-Token": token,
        },
        json=lowstate,
        timeout=timeout,
    )


def save(name: str, blob):
    (OUT / f"{name}.json").write_text(json.dumps(blob, indent=2, default=str))
