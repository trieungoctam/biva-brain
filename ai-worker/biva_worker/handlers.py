"""Handler theo ``kind``: ping, index.items/code, logic.spec/propose, consolidate, promote, ingest."""

from __future__ import annotations

import asyncio
from typing import Any

import asyncpg

from biva_worker import consolidate, index, logic_propose, logic_spec, logic_sync, promote
from biva_worker.embed import Embedder
from biva_worker.ingest import job as ingest_job
from biva_worker.llm import LLMClient
from biva_worker.runner import Handler, Job, PermanentError


async def ping(job: Job) -> dict[str, Any]:
    delay = job.payload.get("sleep_seconds", 0)
    if not isinstance(delay, int | float) or not 0 <= delay <= 600:
        raise PermanentError("sleep_seconds phải trong [0, 600]", code="INVALID_PAYLOAD")
    await asyncio.sleep(delay)
    return {"status": "done", "summary": "pong"}


def build(pool: asyncpg.Pool, embedder: Embedder, llm: LLMClient) -> dict[str, Handler]:
    return {
        "system.ping": ping,
        "index.items": index.handler(pool, embedder),
        "index.code": logic_sync.handler(pool, embedder),
        "logic.spec": logic_spec.handler(pool, llm, embedder),
        "logic.propose": logic_propose.handler(pool),
        "consolidate": consolidate.handler(pool),
        "promote": promote.handler(pool),
        "ingest": ingest_job.handler(pool, llm),
    }
