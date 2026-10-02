"""Sandbox chạy hàm thuần của repo logic (M3, S3.4.1): không mạng, giới hạn CPU/RAM/thời gian.

Code của nhà xe là dữ liệu không tin cậy → chạy trong **process riêng** (``sandbox_worker.py``)
với: import whitelist (không socket/ssl/subprocess/os-system/urllib...), chặn ``socket`` ở tầng
bootstrap, ``RLIMIT_CPU`` + ``RLIMIT_AS``, timeout của cha (kill cả nhóm process), giới hạn
output. Kết quả về qua JSON một dòng trên stdout.

Cha không tin output để thực thi — chỉ đọc (max 64 KB) và parse JSON.
"""

from __future__ import annotations

import json
import os
import resource
import signal
import subprocess
import sys
import time
from pathlib import Path

WORKER = Path(__file__).with_name("sandbox_worker.py")
DEFAULT_TIMEOUT_S = 5.0
DEFAULT_MEM_MB = 128
DEFAULT_CPU_S = 4
MAX_OUTPUT = 64 * 1024


def _limits(cpu_s: int, mem_mb: int):  # chạy trong process con trước khi exec (POSIX).
    def apply() -> None:  # noqa: ANN202 — preexec_fn
        os.setsid()  # nhóm riêng để kill cả cây khi timeout
        resource.setrlimit(resource.RLIMIT_CPU, (cpu_s, cpu_s + 1))
        try:
            resource.setrlimit(resource.RLIMIT_AS, (mem_mb * 1024 * 1024,) * 2)  # macOS có thể bỏ qua
        except (ValueError, OSError):
            pass
        resource.setrlimit(resource.RLIMIT_NOFILE, (64, 64))
        resource.setrlimit(resource.RLIMIT_NPROC, (32, 32))
        resource.setrlimit(resource.RLIMIT_FSIZE, (1024 * 1024, 1024 * 1024))  # ghi tối đa 1MB/file
        resource.setrlimit(resource.RLIMIT_CORE, (0, 0))

    return apply


def run_code(
    code: str,
    entry: str,
    payload: dict | None = None,
    *,
    timeout_s: float = DEFAULT_TIMEOUT_S,
    mem_mb: int = DEFAULT_MEM_MB,
    cpu_s: int = DEFAULT_CPU_S,
) -> dict:
    """Chạy ``entry(**payload)`` trong sandbox. Trả {ok, value|error, elapsed_ms}."""
    job = json.dumps({"code": code, "entry": entry, "payload": payload or {}}, ensure_ascii=False)
    start = time.monotonic()
    try:
        proc = subprocess.run(
            [sys.executable, "-I", str(WORKER)],
            input=job.encode(),
            capture_output=True,
            timeout=timeout_s,
            preexec_fn=_limits(cpu_s, mem_mb),
        )
    except subprocess.TimeoutExpired:
        return {"ok": False, "error": f"vượt thời gian {timeout_s}s", "elapsed_ms": _ms(start)}
    elapsed = _ms(start)
    out = proc.stdout[:MAX_OUTPUT]
    if proc.returncode in (-signal.SIGKILL, 137):
        # CPU/RA M đạt giới hạn của RLIMIT → SIGKILL; phân biệt với timeout đã bắt ở trên.
        return {"ok": False, "error": "vượt giới hạn CPU/RAM", "elapsed_ms": elapsed}
    if proc.returncode != 0:
        return {
            "ok": False,
            "error": _tail(proc.stderr.decode("utf-8", "replace")) or f"thoát với mã {proc.returncode}",
            "elapsed_ms": elapsed,
        }
    try:
        return {"ok": True, **json.loads(out.decode("utf-8")), "elapsed_ms": elapsed}
    except (json.JSONDecodeError, UnicodeDecodeError):
        return {
            "ok": False,
            "error": "output không phải JSON (hàm không được in ra stdout)",
            "elapsed_ms": elapsed,
        }


def _ms(start: float) -> int:
    return int((time.monotonic() - start) * 1000)


def _tail(s: str, n: int = 400) -> str:
    s = s.strip()
    return s[-n:]
