"""Handler theo ``kind``. M0 chỉ có ``system.ping`` để kiểm tra queue đầu-cuối;
ingest, consolidate... được thêm từ M1."""

from __future__ import annotations

import asyncio
from typing import Any

from biva_worker.runner import Handler, Job, PermanentError


async def ping(job: Job) -> dict[str, Any]:
    delay = job.payload.get("sleep_seconds", 0)
    if not isinstance(delay, int | float) or not 0 <= delay <= 600:
        raise PermanentError("sleep_seconds phải trong [0, 600]", code="INVALID_PAYLOAD")
    await asyncio.sleep(delay)
    return {"status": "done", "summary": "pong"}


HANDLERS: dict[str, Handler] = {"system.ping": ping}
