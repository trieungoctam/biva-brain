"""S3.4.1: sandbox chặn mạng, giới hạn CPU/RAM/thời gian; hàm thuần chạy đúng."""

from __future__ import annotations

import sys

import pytest

from biva_worker.sandbox import run_code

FARE = """
def calculate(base_fares, seat, **kw):
    price = base_fares[seat]
    surcharge = kw.get("holiday_surcharge") or {}
    for factor in surcharge.values():
        price = int(price * (1 + factor))
    return price
"""


def test_hàm_thuần_chạy_đúng():
    res = run_code(
        FARE,
        "calculate",
        {"base_fares": {"sleeper": 350000}, "seat": "sleeper", "holiday_surcharge": {"tet": 0.2}},
    )
    assert res["ok"] and res["value"] == 420000, res


def test_chặn_gọi_mạng_socket():
    res = run_code("import socket\ndef f():\n    return socket.socket()\n", "f")
    assert not res["ok"] and "bị cấm" in res["error"], res


def test_chặn_urllib():
    res = run_code("import urllib.request\ndef f():\n    return 1\n", "f")
    assert not res["ok"] and "bị cấm" in res["error"], res


def test_chặn_os_system():
    res = run_code("import os\ndef f():\n    return os.system('echo hi')\n", "f")
    assert not res["ok"] and "bị cấm" in res["error"], res


def test_chặn_subprocess():
    res = run_code("import subprocess\ndef f():\n    return 1\n", "f")
    assert not res["ok"] and "bị cấm" in res["error"], res


def test_chặn_file_io():
    res = run_code("def f():\n    return open('/etc/passwd').read()\n", "f")
    assert not res["ok"], res


def test_vòng_lặp_vô_hạn_timeout():
    res = run_code("def f():\n    while True:\n        pass\n", "f", timeout_s=1.5)
    assert not res["ok"] and "vượt thời gian" in res["error"], res


@pytest.mark.skipif(sys.platform == "darwin", reason="macOS bỏ qua RLIMIT_AS; Linux (CI/prod) bắt")
def test_ram_quá_giới_hạn():
    res = run_code(
        "def f():\n    x = bytearray(1024 * 1024 * 1024)\n    return len(x)\n", "f", timeout_s=6, mem_mb=64
    )
    assert not res["ok"], res  # MemoryError hoặc SIGKILL do RLIMIT_AS


def test_thiếu_hàm():
    res = run_code("def other():\n    return 1\n", "f")
    assert not res["ok"] and "không có hàm" in res["error"], res


def test_module_cho_phép_chạy_dc():
    res = run_code(
        "import decimal\nimport datetime\nimport json\ndef f():\n"
        "    return {'d': str(datetime.date(2026, 10, 2)), 'x': str(decimal.Decimal('1.5'))}\n",
        "f",
    )
    assert res["ok"] and res["value"]["d"] == "2026-10-02", res


def _fn(*stmts: str) -> str:
    body = "\n".join("    " + st for st in stmts)
    return f"def __biva_entry():\n{body}\n"


def test_builtin_bypass_bi_chan():
    """_io/_socket/marshal/pickle là builtin: import lấy từ sys.modules cache, KHÔNG qua
    finder — phải purge khỏi cache và deny ở nhánh builtin (đường từng lọt, vòng fix sau)."""
    from biva_worker.security import sandbox_builtin_bypass_cases

    for code in sandbox_builtin_bypass_cases():
        r = run_code(_fn(code), "__biva_entry")
        assert r["ok"] is False and "SANDBOX_BLOCKED" in r["error"], (code, r)


def test_module_hop_le_van_chay():
    """Toàn bộ whitelist vẫn import được sau khi _io bị chặn (machinery đọc .py qua _io —
    nên phải nạp sẵn TRƯỚC khi đăng ký finder + purge)."""
    r = run_code(
        _fn(
            "import json, datetime, math, re, decimal, statistics, fractions, "
            "textwrap, hashlib, random, collections, itertools, functools, typing, string",
            "return 'ok'",
        ),
        "__biva_entry",
    )
    assert r["ok"] is True and r["value"] == "ok", r
