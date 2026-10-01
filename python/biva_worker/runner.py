"""Runner: claim job từ bảng ``operations`` và chạy handler tương ứng.

- Claim bằng ``FOR UPDATE SKIP LOCKED``: nhiều worker chạy song song không giành nhau.
- Lease + heartbeat: worker chết/treo → lease hết hạn → scheduler (Go, leader) gọi
  ``operations_requeue_expired()`` để trả job về hàng đợi.
- Mất lease (heartbeat không gia hạn được) → huỷ handler, KHÔNG ghi kết quả: job đã thuộc worker khác.
- Lỗi: ``RetryableError`` hoặc exception lạ → thử lại với backoff tới ``max_attempts``;
  ``PermanentError`` → ``failed`` ngay.
- LISTEN 'biva_operations' để thức dậy khi có job; vẫn poll định kỳ nên mất NOTIFY không làm sót job.
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import logging
import os
import random
import socket
from collections.abc import Awaitable, Callable, Mapping
from dataclasses import dataclass
from typing import Any

import asyncpg

from biva_worker.contracts import validate

log = logging.getLogger(__name__)

NOTIFY_CHANNEL = "biva_operations"  # phát bởi trigger ở migration 000002


class RetryableError(Exception):
    """Lỗi tạm thời (LLM timeout, TEI bận...): job được thử lại."""

    code = "RETRYABLE"


class PermanentError(Exception):
    """Lỗi không thử lại được (payload sai, dữ liệu không hợp lệ...)."""

    def __init__(self, message: str, code: str = "PERMANENT") -> None:
        super().__init__(message)
        self.code = code


@dataclass(frozen=True)
class Job:
    id: str
    kind: str
    operator_id: str | None
    payload: dict[str, Any]
    attempts: int
    max_attempts: int
    trace_context: dict[str, Any]


Handler = Callable[[Job], Awaitable[dict[str, Any]]]


async def init_connection(conn: asyncpg.Connection) -> None:
    """JSONB ⇄ dict cho mọi kết nối trong pool."""
    await conn.set_type_codec("jsonb", encoder=json.dumps, decoder=json.loads, schema="pg_catalog")


def default_worker_id() -> str:
    return f"{socket.gethostname()}:{os.getpid()}"


def backoff_seconds(attempt: int, base: float = 2.0, cap: float = 300.0) -> float:
    """Exponential backoff có jitter: ~base^attempt, tối đa ``cap``."""
    delay = min(cap, base**attempt)
    return delay * random.uniform(0.5, 1.0)


_CLAIM = """
UPDATE operations
SET status = 'running', locked_by = $1, attempts = attempts + 1,
    lease_until = now() + make_interval(secs => $2), updated_at = now()
WHERE id = (
    SELECT id FROM operations
    WHERE status = 'queued' AND run_after <= now() AND kind = ANY($3::text[])
    ORDER BY priority, run_after
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING id::text, kind, operator_id, payload, attempts, max_attempts, trace_context
"""

_HEARTBEAT = """
UPDATE operations SET lease_until = now() + make_interval(secs => $3), updated_at = now()
WHERE id = $1 AND locked_by = $2 AND status = 'running'
"""

_DONE = """
UPDATE operations
SET status = 'done', result = $3, error = NULL, locked_by = NULL, lease_until = NULL, updated_at = now()
WHERE id = $1 AND locked_by = $2 AND status = 'running'
"""

_RETRY = """
UPDATE operations
SET status = 'queued', error = $3, locked_by = NULL, lease_until = NULL,
    run_after = now() + make_interval(secs => $4), updated_at = now()
WHERE id = $1 AND locked_by = $2 AND status = 'running'
"""

_FAIL = """
UPDATE operations
SET status = 'failed', result = $3, error = $4, locked_by = NULL, lease_until = NULL, updated_at = now()
WHERE id = $1 AND locked_by = $2 AND status = 'running'
"""

# Khi tắt êm: trả job đang chạy dở về hàng đợi, không tính là một lần thử.
_RELEASE = """
UPDATE operations
SET status = 'queued', attempts = greatest(attempts - 1, 0), locked_by = NULL, lease_until = NULL,
    run_after = now(), updated_at = now()
WHERE id = $1 AND locked_by = $2 AND status = 'running'
"""


def _failed_result(code: str, message: str, retryable: bool) -> dict[str, Any]:
    return {"status": "failed", "error": {"code": code, "message": message, "retryable": retryable}}


class Runner:
    def __init__(
        self,
        pool: asyncpg.Pool,
        handlers: Mapping[str, Handler],
        *,
        worker_id: str | None = None,
        lease_seconds: float = 60.0,
        heartbeat_seconds: float | None = None,
        poll_seconds: float = 5.0,
        concurrency: int = 1,
        shutdown_grace_seconds: float = 10.0,
    ) -> None:
        if not handlers:
            raise ValueError("cần ít nhất một handler")
        self.pool = pool
        self.handlers = dict(handlers)
        self.worker_id = worker_id or default_worker_id()
        self.lease_seconds = lease_seconds
        self.heartbeat_seconds = heartbeat_seconds or lease_seconds / 3
        self.poll_seconds = poll_seconds
        self.concurrency = concurrency
        self.shutdown_grace_seconds = shutdown_grace_seconds
        self._wake = asyncio.Event()

    # ───────────────────────────── vòng chính ─────────────────────────────

    async def run(self, stop: asyncio.Event) -> None:
        """Chạy tới khi ``stop`` được set; job dở dang được chờ rồi trả về hàng đợi."""
        listener = await self.pool.acquire()
        try:
            await listener.add_listener(NOTIFY_CHANNEL, self._on_notify)
            slots = [asyncio.create_task(self._slot(stop)) for _ in range(self.concurrency)]
            await stop.wait()
            self._wake.set()
            await asyncio.gather(*slots)
        finally:
            with contextlib.suppress(Exception):
                await listener.remove_listener(NOTIFY_CHANNEL, self._on_notify)
            await self.pool.release(listener)

    def _on_notify(self, _conn: Any, _pid: int, _channel: str, kind: str) -> None:
        if kind in self.handlers:
            self._wake.set()

    async def _slot(self, stop: asyncio.Event) -> None:
        while not stop.is_set():
            try:
                job = await self.claim()
            except (OSError, asyncpg.PostgresError) as exc:
                log.warning("claim lỗi: %s", exc)
                job = None
            if job is not None:
                await self._process(job, stop)
                continue
            self._wake.clear()
            with contextlib.suppress(TimeoutError):
                await asyncio.wait_for(self._wake.wait(), self.poll_seconds)

    async def run_once(self) -> bool:
        """Claim và xử lý tối đa một job (dùng cho test / CLI). Trả về True nếu có job."""
        job = await self.claim()
        if job is None:
            return False
        await self._process(job, asyncio.Event())
        return True

    # ───────────────────────────── một job ─────────────────────────────

    async def claim(self) -> Job | None:
        row = await self.pool.fetchrow(_CLAIM, self.worker_id, self.lease_seconds, list(self.handlers))
        if row is None:
            return None
        return Job(
            id=row["id"],
            kind=row["kind"],
            operator_id=row["operator_id"],
            payload=row["payload"] or {},
            attempts=row["attempts"],
            max_attempts=row["max_attempts"],
            trace_context=row["trace_context"] or {},
        )

    async def _process(self, job: Job, stop: asyncio.Event) -> None:
        lost = asyncio.Event()
        work = asyncio.create_task(self.handlers[job.kind](job))
        beat = asyncio.create_task(self._heartbeat(job, lost))
        lost_wait = asyncio.create_task(lost.wait())
        stop_wait = asyncio.create_task(stop.wait())
        try:
            await asyncio.wait({work, lost_wait, stop_wait}, return_when=asyncio.FIRST_COMPLETED)
            if not work.done() and stop.is_set() and not lost.is_set():
                # Tắt êm: cho handler thêm thời gian, quá hạn thì huỷ và trả job về hàng đợi.
                await asyncio.wait({work, lost_wait}, timeout=self.shutdown_grace_seconds)
            if not work.done():
                work.cancel()
                with contextlib.suppress(asyncio.CancelledError, Exception):
                    await work
                if lost.is_set():
                    log.warning("mất lease job %s (%s), bỏ kết quả", job.id, job.kind)
                else:
                    await self.pool.execute(_RELEASE, job.id, self.worker_id)
                    log.info("trả job %s (%s) về hàng đợi khi tắt", job.id, job.kind)
                return
            if lost.is_set():
                log.warning("job %s xong nhưng đã mất lease, bỏ kết quả", job.id)
                return
            await self._finish(job, work)
        finally:
            for t in (beat, lost_wait, stop_wait):
                t.cancel()
            await asyncio.gather(beat, lost_wait, stop_wait, return_exceptions=True)

    async def _heartbeat(self, job: Job, lost: asyncio.Event) -> None:
        while True:
            await asyncio.sleep(self.heartbeat_seconds)
            try:
                status = await self.pool.execute(_HEARTBEAT, job.id, self.worker_id, self.lease_seconds)
            except (OSError, asyncpg.PostgresError) as exc:
                log.warning("heartbeat job %s lỗi: %s", job.id, exc)
                continue  # lease còn tới lần gia hạn sau; hết hẳn thì lần sau trả về UPDATE 0
            if status == "UPDATE 0":
                lost.set()
                return

    async def _finish(self, job: Job, work: asyncio.Task[dict[str, Any]]) -> None:
        exc = work.exception()
        if exc is None:
            result = work.result()
            errors = validate("operation_result", result)
            if errors or result.get("status") != "done":
                exc = PermanentError(
                    f"handler trả kết quả không hợp lệ: {errors or result.get('status')}",
                    code="INVALID_RESULT",
                )
            else:
                await self.pool.execute(_DONE, job.id, self.worker_id, result)
                log.info("job %s (%s) xong", job.id, job.kind)
                return

        if isinstance(exc, PermanentError):
            await self._fail(job, exc.code, str(exc), retryable=False)
            return
        if not isinstance(exc, RetryableError):
            log.error("job %s (%s) lỗi không lường trước", job.id, job.kind, exc_info=exc)
        code = getattr(exc, "code", "UNEXPECTED")
        message = f"{type(exc).__name__}: {exc}"
        if job.attempts >= job.max_attempts:
            await self._fail(job, code, message, retryable=True)
            return
        delay = backoff_seconds(job.attempts)
        await self.pool.execute(_RETRY, job.id, self.worker_id, message, delay)
        log.warning(
            "job %s (%s) lần %d lỗi, thử lại sau %.1fs: %s", job.id, job.kind, job.attempts, delay, message
        )

    async def _fail(self, job: Job, code: str, message: str, *, retryable: bool) -> None:
        await self.pool.execute(
            _FAIL, job.id, self.worker_id, _failed_result(code, message, retryable), message
        )
        log.error("job %s (%s) thất bại: %s", job.id, job.kind, message)
