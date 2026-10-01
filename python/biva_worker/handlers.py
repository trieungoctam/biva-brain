"""Handler theo ``kind``. ``system.ping`` để kiểm tra queue đầu-cuối; ``index.items`` từ M1."""

from __future__ import annotations

import asyncio
from typing import Any

import asyncpg

from biva_worker import index
from biva_worker.embed import Embedder
from biva_worker.runner import Handler, Job, PermanentError


async def ping(job: Job) -> dict[str, Any]:
    delay = job.payload.get("sleep_seconds", 0)
    if not isinstance(delay, int | float) or not 0 <= delay <= 600:
        raise PermanentError("sleep_seconds phải trong [0, 600]", code="INVALID_PAYLOAD")
    await asyncio.sleep(delay)
    return {"status": "done", "summary": "pong"}


def build(pool: asyncpg.Pool, embedder: Embedder) -> dict[str, Handler]:
    return {
        "system.ping": ping,
        "index.items": index.handler(pool, embedder),
    }
