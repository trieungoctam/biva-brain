"""Test runner với Postgres thật (cần BIVA_TEST_DATABASE_URL, DB đã migrate). Thiếu thì skip.

Mỗi test dùng ``kind`` riêng nên không claim nhầm job của test khác.
"""

from __future__ import annotations

import asyncio
import os
import uuid
from collections.abc import Awaitable, Callable
from typing import Any

import asyncpg
import pytest

from biva_worker.handlers import ping
from biva_worker.runner import Job, PermanentError, RetryableError, Runner, init_connection

DB_URL = os.environ.get("BIVA_TEST_DATABASE_URL")
pytestmark = pytest.mark.skipif(not DB_URL, reason="đặt BIVA_TEST_DATABASE_URL để chạy test runner")


def run(fn: Callable[[asyncpg.Pool, str], Awaitable[None]]) -> None:
    """Chạy một test async với pool mới và một kind riêng; dọn job sau khi xong."""

    async def wrapper() -> None:
        pool = await asyncpg.create_pool(DB_URL, min_size=1, max_size=12, init=init_connection)
        kind = f"test.{uuid.uuid4().hex[:12]}"
        try:
            await fn(pool, kind)
        finally:
            await pool.execute("DELETE FROM operations WHERE kind = $1", kind)
            await pool.close()

    asyncio.run(wrapper())


async def enqueue(pool: asyncpg.Pool, kind: str, payload: dict[str, Any] | None = None, **cols: Any) -> str:
    max_attempts = cols.get("max_attempts", 5)
    return await pool.fetchval(
        "INSERT INTO operations (kind, payload, max_attempts) VALUES ($1, $2, $3) RETURNING id::text",
        kind,
        payload or {},
        max_attempts,
    )


async def row(pool: asyncpg.Pool, job_id: str) -> asyncpg.Record:
    return await pool.fetchrow(
        "SELECT status, attempts, result, error, locked_by, run_after > now() AS delayed "
        "FROM operations WHERE id = $1",
        job_id,
    )


def test_done():
    async def t(pool: asyncpg.Pool, kind: str) -> None:
        job_id = await enqueue(pool, kind)
        assert await Runner(pool, {kind: ping}).run_once()
        r = await row(pool, job_id)
        assert (r["status"], r["attempts"], r["locked_by"]) == ("done", 1, None)
        assert r["result"] == {"status": "done", "summary": "pong"}
        assert not await Runner(pool, {kind: ping}).run_once()  # hết job

    run(t)


def test_retryable_then_success():
    calls = 0

    async def flaky(job: Job) -> dict[str, Any]:
        nonlocal calls
        calls += 1
        if calls == 1:
            raise RetryableError("TEI bận")
        return {"status": "done"}

    async def t(pool: asyncpg.Pool, kind: str) -> None:
        job_id = await enqueue(pool, kind, max_attempts=3)
        runner = Runner(pool, {kind: flaky})
        await runner.run_once()
        r = await row(pool, job_id)
        assert (r["status"], r["attempts"], r["delayed"]) == ("queued", 1, True)
        assert "TEI bận" in r["error"]
        assert not await runner.run_once()  # còn trong backoff
        await pool.execute("UPDATE operations SET run_after = now() WHERE id = $1", job_id)
        assert await runner.run_once()
        r = await row(pool, job_id)
        assert (r["status"], r["attempts"], r["error"]) == ("done", 2, None)

    run(t)


def test_permanent_error_fails_immediately():
    async def bad(job: Job) -> dict[str, Any]:
        raise PermanentError("payload sai", code="INVALID_PAYLOAD")

    async def t(pool: asyncpg.Pool, kind: str) -> None:
        job_id = await enqueue(pool, kind)
        await Runner(pool, {kind: bad}).run_once()
        r = await row(pool, job_id)
        assert (r["status"], r["attempts"]) == ("failed", 1)
        assert r["result"]["error"] == {
            "code": "INVALID_PAYLOAD",
            "message": "payload sai",
            "retryable": False,
        }

    run(t)


def test_unexpected_error_exhausts_attempts():
    async def boom(job: Job) -> dict[str, Any]:
        raise ValueError("bug")

    async def t(pool: asyncpg.Pool, kind: str) -> None:
        job_id = await enqueue(pool, kind, max_attempts=1)
        await Runner(pool, {kind: boom}).run_once()
        r = await row(pool, job_id)
        assert r["status"] == "failed"
        assert r["result"]["error"]["code"] == "UNEXPECTED"
        assert r["result"]["error"]["retryable"] is True

    run(t)


def test_invalid_result_fails():
    async def sloppy(job: Job) -> dict[str, Any]:
        return {"status": "done", "counts": {"items": -1}}

    async def t(pool: asyncpg.Pool, kind: str) -> None:
        job_id = await enqueue(pool, kind)
        await Runner(pool, {kind: sloppy}).run_once()
        r = await row(pool, job_id)
        assert r["status"] == "failed"
        assert r["result"]["error"]["code"] == "INVALID_RESULT"

    run(t)


def test_crashed_worker_job_rerun_after_lease():
    """AC S0.2.2: worker chết giữa chừng → job chạy lại sau khi hết lease."""

    async def t(pool: asyncpg.Pool, kind: str) -> None:
        job_id = await enqueue(pool, kind)
        dead = Runner(pool, {kind: ping}, worker_id="w-dead", lease_seconds=0.5)
        assert await dead.claim() is not None  # claim rồi "chết": không heartbeat, không ghi kết quả
        assert (await row(pool, job_id))["locked_by"] == "w-dead"

        assert await pool.fetchval("SELECT operations_requeue_expired()") == 0  # lease còn hạn
        await asyncio.sleep(0.7)
        assert await pool.fetchval("SELECT operations_requeue_expired()") == 1

        assert await Runner(pool, {kind: ping}, worker_id="w-alive").run_once()
        r = await row(pool, job_id)
        assert (r["status"], r["attempts"]) == ("done", 2)

    run(t)


def test_lost_lease_discards_result():
    started = asyncio.Event()

    async def slow(job: Job) -> dict[str, Any]:
        started.set()
        await asyncio.sleep(5)
        return {"status": "done"}

    async def t(pool: asyncpg.Pool, kind: str) -> None:
        job_id = await enqueue(pool, kind)
        runner = Runner(pool, {kind: slow}, worker_id="w-a", lease_seconds=5, heartbeat_seconds=0.1)
        task = asyncio.create_task(runner.run_once())
        await started.wait()
        # Job bị trả về và worker khác đã nhận.
        await pool.execute("UPDATE operations SET locked_by = 'w-b' WHERE id = $1", job_id)
        await asyncio.wait_for(task, 2)
        r = await row(pool, job_id)
        assert (r["status"], r["locked_by"], r["result"]) == ("running", "w-b", None)

    run(t)


def test_parallel_runners_no_double_processing():
    seen: list[str] = []

    async def record(job: Job) -> dict[str, Any]:
        seen.append(job.id)
        await asyncio.sleep(0.01)
        return {"status": "done"}

    async def t(pool: asyncpg.Pool, kind: str) -> None:
        ids = {await enqueue(pool, kind) for _ in range(30)}
        stop = asyncio.Event()
        runners = [
            Runner(pool, {kind: record}, worker_id=f"w{i}", concurrency=3, poll_seconds=0.1) for i in range(2)
        ]
        tasks = [asyncio.create_task(r.run(stop)) for r in runners]
        for _ in range(100):
            done = await pool.fetchval(
                "SELECT count(*) FROM operations WHERE kind = $1 AND status = 'done'", kind
            )
            if done == len(ids):
                break
            await asyncio.sleep(0.05)
        stop.set()
        await asyncio.gather(*tasks)
        assert sorted(seen) == sorted(ids)  # mỗi job đúng một lần
        assert await pool.fetchval("SELECT max(attempts) FROM operations WHERE kind = $1", kind) == 1

    run(t)


def test_notify_wakes_idle_runner():
    async def t(pool: asyncpg.Pool, kind: str) -> None:
        stop = asyncio.Event()
        runner = Runner(pool, {kind: ping}, poll_seconds=30)  # poll chậm: chỉ NOTIFY mới kịp
        task = asyncio.create_task(runner.run(stop))
        await asyncio.sleep(0.3)  # runner đã ngủ
        job_id = await enqueue(pool, kind)
        for _ in range(40):
            if (await row(pool, job_id))["status"] == "done":
                break
            await asyncio.sleep(0.05)
        stop.set()
        await asyncio.wait_for(task, 5)
        assert (await row(pool, job_id))["status"] == "done"

    run(t)


def test_graceful_shutdown_releases_job():
    started = asyncio.Event()

    async def slow(job: Job) -> dict[str, Any]:
        started.set()
        await asyncio.sleep(30)
        return {"status": "done"}

    async def t(pool: asyncpg.Pool, kind: str) -> None:
        job_id = await enqueue(pool, kind)
        stop = asyncio.Event()
        runner = Runner(pool, {kind: slow}, shutdown_grace_seconds=0.1, poll_seconds=0.1)
        task = asyncio.create_task(runner.run(stop))
        await started.wait()
        stop.set()
        await asyncio.wait_for(task, 3)
        r = await row(pool, job_id)
        assert (r["status"], r["attempts"], r["locked_by"]) == ("queued", 0, None)

    run(t)


def test_ping_rejects_bad_payload():
    async def t(pool: asyncpg.Pool, kind: str) -> None:
        job_id = await enqueue(pool, kind, {"sleep_seconds": "lâu"})
        await Runner(pool, {kind: ping}).run_once()
        assert (await row(pool, job_id))["status"] == "failed"

    run(t)


def test_job_span_continues_enqueue_trace():
    """AC S0.4.3: span của job là con của span enqueue (traceparent do brain-api ghi)."""
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import SimpleSpanProcessor
    from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

    from biva_worker import runner as runner_mod

    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    original = runner_mod.tracer
    runner_mod.tracer = provider.get_tracer("test")
    trace_id, parent_span = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"

    async def t(pool: asyncpg.Pool, kind: str) -> None:
        await pool.execute(
            "INSERT INTO operations (kind, trace_context) VALUES ($1, $2)",
            kind,
            {"traceparent": f"00-{trace_id}-{parent_span}-01"},
        )
        assert await Runner(pool, {kind: ping}).run_once()

    try:
        run(t)
    finally:
        runner_mod.tracer = original
    (span,) = exporter.get_finished_spans()
    assert span.name.startswith("job test.")
    assert format(span.context.trace_id, "032x") == trace_id
    assert format(span.parent.span_id, "016x") == parent_span
