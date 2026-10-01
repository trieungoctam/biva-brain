"""Điểm vào ai-worker.

M0: chỉ kiểm tra kết nối Postgres rồi chờ. Vòng claim job (SKIP LOCKED, lease, retry)
thuộc story S0.2.2 và sẽ thay thế phần chờ này.
"""

from __future__ import annotations

import asyncio
import logging
import os
import signal

import asyncpg

from biva_worker import __version__

log = logging.getLogger("biva_worker")


async def main() -> None:
    url = os.environ.get("BIVA_DATABASE_URL")
    if not url:
        raise SystemExit("thiếu BIVA_DATABASE_URL")
    conn = await asyncpg.connect(url)
    try:
        version = await conn.fetchval("SHOW server_version")
        log.info("ai-worker %s đã kết nối Postgres %s; chưa bật runner (S0.2.2)", __version__, version)
    finally:
        await conn.close()

    stop = asyncio.Event()
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, stop.set)
    await stop.wait()
    log.info("ai-worker dừng")


if __name__ == "__main__":
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")
    asyncio.run(main())
