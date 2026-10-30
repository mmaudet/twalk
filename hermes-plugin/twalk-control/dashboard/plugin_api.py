"""Twalk control tower: backend for the desktop pane, mounted at /api/plugins/twalk-control/.

Read-only. Shells out to the Twalk `trieur feed` binary (NATS account `hermes`, which can
only read the bus and publish suggestions) and returns its JSON. Results are cached a few
seconds so several open windows do not multiply bus reads. A handler never raises.
"""
from __future__ import annotations

import json
import os
import subprocess
import threading
import time
from pathlib import Path

from fastapi import APIRouter

router = APIRouter()

TRIEUR = Path(os.environ.get("TWALK_TRIEUR_BIN", "/home/hermes/work/twalk2/bin/trieur"))
CACHE_SECONDS = 4
_ALLOWED_SINCE = {"2h", "6h", "12h", "24h", "48h"}
_lock = threading.Lock()
_cache: dict[tuple[str, int], tuple[float, dict]] = {}


def _run_feed(since: str, limit: int) -> dict:
    env = {k: v for k, v in os.environ.items() if not k.startswith("NATS_")}
    env.setdefault("HERMES_HOME", str(Path.home() / "hermes-new"))
    proc = subprocess.run(
        [str(TRIEUR), "feed", "--since", since, "--limit", str(limit), "--account", "hermes"],
        capture_output=True, text=True, timeout=25, env=env,
    )
    if proc.returncode != 0:
        return {"ok": False, "error": "feed_failed"}  # stderr may name contacts: not returned
    data = json.loads(proc.stdout)
    data["ok"] = True
    return data


@router.get("/feed")
async def feed(since: str = "12h", limit: int = 150):
    since = since if since in _ALLOWED_SINCE else "12h"
    limit = max(10, min(int(limit or 150), 400))
    key = (since, limit)
    try:
        with _lock:
            hit = _cache.get(key)
            if hit and time.monotonic() - hit[0] < CACHE_SECONDS:
                return hit[1]
        if not TRIEUR.exists():
            return {"ok": False, "error": "trieur_missing"}
        data = await _to_thread(_run_feed, since, limit)
        with _lock:
            _cache[key] = (time.monotonic(), data)
        return data
    except Exception as exc:  # plugin contract: never raise out of the handler
        return {"ok": False, "error": type(exc).__name__}


async def _to_thread(fn, *args):
    import asyncio

    return await asyncio.to_thread(fn, *args)
