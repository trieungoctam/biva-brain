"""Điểm vào ai-worker: chạy Runner tới khi nhận SIGINT/SIGTERM.

Biến môi trường: BIVA_DATABASE_URL (bắt buộc), BIVA_WORKER_CONCURRENCY (mặc định 4),
BIVA_WORKER_LEASE_SECONDS (mặc định 60).
"""

from __future__ import annotations

import asyncio
import logging
import os
import signal

import asyncpg

from biva_worker import __version__, telemetry
from biva_worker.handlers import HANDLERS
from biva_worker.runner import Runner, init_connection

log = logging.getLogger("biva_worker")


async def main() -> None:
    url = os.environ.get("BIVA_DATABASE_URL")
    if not url:
        raise SystemExit("thiếu BIVA_DATABASE_URL")
    concurrency = int(os.environ.get("BIVA_WORKER_CONCURRENCY", "4"))
    lease = float(os.environ.get("BIVA_WORKER_LEASE_SECONDS", "60"))

    tracing = telemetry.setup("ai-worker", __version__)

    # +1 kết nối cho LISTEN, +concurrency cho heartbeat chạy song song với handler.
    pool = await asyncpg.create_pool(url, min_size=1, max_size=2 * concurrency + 1, init=init_connection)
    try:
        runner = Runner(pool, HANDLERS, lease_seconds=lease, concurrency=concurrency)
        stop = asyncio.Event()
        loop = asyncio.get_running_loop()
        for sig in (signal.SIGINT, signal.SIGTERM):
            loop.add_signal_handler(sig, stop.set)
        log.info(
            "ai-worker %s (%s) chạy; kinds=%s concurrency=%d lease=%.0fs",
            __version__,
            runner.worker_id,
            sorted(HANDLERS),
            concurrency,
            lease,
        )
        await runner.run(stop)
        log.info("ai-worker dừng")
    finally:
        await pool.close()
        tracing.shutdown()  # flush span còn trong batch


if __name__ == "__main__":
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")
    asyncio.run(main())
