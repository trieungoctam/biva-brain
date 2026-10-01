"""S3.4.2: chạy ví dụ nhà xe trên code ứng viên qua sandbox — config/hook/custom, api bỏ qua."""

from __future__ import annotations

import asyncio
import os
import uuid
from pathlib import Path

import asyncpg
import pytest

from biva_worker import examples
from biva_worker.runner import PermanentError, init_connection

DB_URL = os.environ.get("BIVA_TEST_DATABASE_URL")
needs_db = pytest.mark.skipif(not DB_URL, reason="đặt BIVA_TEST_DATABASE_URL")

MODULE = """\
def calculate(base_fares, seat, adjust_price=None, **kw):
    if seat not in base_fares:
        raise ValueError("không có giá cho loại ghế")
    price = base_fares[seat]
    if adjust_price is not None:
        price = adjust_price(price, {"seat": seat})
    return price
"""

HOOK = "def add_fee(price, ctx):\n    return price + 30000\n"

CASES = [
    {
        "input": {"base_fares": {"sleeper": 350000}, "seat": "sleeper"},
        "expected": {"out": 350000},
        "note": "thường",
    },
    {
        "input": {"base_fares": {"sleeper": 350000}, "seat": "sleeper"},
        "expected": {"out": 380000},
        "note": "phí cuối tuần",
    },
    {
        "input": {"base_fares": {}, "seat": "vip"},
        "expected": {"error": "không có giá cho loại ghế"},
        "note": "ghế lạ",
    },
]


def make_repo(root: Path, hook_op: str, custom_op: str) -> None:
    (root / "modules" / "fare" / "mini").mkdir(parents=True)
    (root / "modules" / "fare" / "mini" / "calc.py").write_text(MODULE, encoding="utf-8")
    hb = root / "operators" / hook_op / "hooks"
    hb.mkdir(parents=True)
    (hb / "fee.py").write_text(HOOK, encoding="utf-8")
    cb = root / "operators" / custom_op / "custom"
    cb.mkdir(parents=True)
    (cb / "own.py").write_text(
        "def handle(base_fares, seat, **kw):\n    return base_fares[seat] * 2\n", encoding="utf-8"
    )


@needs_db
def test_run_examples_against_candidates(tmp_path, monkeypatch):
    async def t() -> None:
        pool = await asyncpg.create_pool(DB_URL, init=init_connection)
        me, a, b, c = (f"ex{i}{uuid.uuid4().hex[:6]}" for i in range(4))
        for op in (me, a, b, c):
            await pool.execute("INSERT INTO operators (id, name) VALUES ($1, 'E')", op)
        make_repo(tmp_path, hook_op=b, custom_op=c)
        monkeypatch.setenv("BIVA_INTEGRATIONS_DIR", str(tmp_path))
        try:
            await pool.execute(
                """INSERT INTO logic_modules (id, version, layer, capability, summary, entrypoint,
                       repo, path, commit) VALUES ('fare.minitest', 1, 'L1', 'fare', 'mini',
                       'calc.py:calculate', 'test', 'modules/fare/mini', 'c')"""
            )
            for c_ in CASES:
                await pool.execute(
                    """INSERT INTO logic_tests (operator_id, capability, input, expected, note, commit)
                       VALUES ($1, 'fare', $2::jsonb, $3::jsonb, $4, 'c')""",
                    me,
                    c_["input"],
                    c_["expected"],
                    c_["note"],
                )
            # cand_plain: config thuần — chỉ pass case thường (380k cần hook).
            await pool.execute(
                """INSERT INTO logic_profiles (operator_id, capability, mode, module_id, module_version,
                       params, commit) VALUES ($1, 'fare', 'config', 'fare.minitest', 1, '{}', 'c')""",
                a,
            )
            # cand_hook: hook add_fee 30k — pass cả case cuối tuần; ghế lạ error.
            await pool.execute(
                """INSERT INTO logic_profiles (operator_id, capability, mode, module_id, module_version,
                       hooks, commit) VALUES ($1, 'fare', 'hook', 'fare.minitest', 1,
                       $2::jsonb, 'c')""",
                b,
                {"adjust_price": "hooks/fee.py:add_fee"},
            )
            # cand_custom: nhân đôi giá — fail mọi case thường.
            await pool.execute(
                """INSERT INTO logic_profiles (operator_id, capability, mode, module_id, module_version,
                       entrypoint, decision_id, commit) VALUES ($1, 'fare', 'custom', '', 1,
                       'custom/own.py:handle', NULL, 'c')""",
                c,
            )
            res = await examples.run_examples(pool, me, "fare", None)
            by = {r["operator"]: r for r in res["results"]}
            # plain: pass thường + ghế lạ; hook (+30k mọi giá): pass cuối tuần + ghế lạ; custom x2: chỉ ghế lạ.
            assert by[a]["pass_rate"] == round(2 / 3, 3)
            assert by[a]["failed"][0]["note"] == "phí cuối tuần"
            assert by[b]["pass_rate"] == round(2 / 3, 3)
            assert by[c]["pass_rate"] == 0.0  # nhân đôi giá + lỗi không đúng thông điệp
            # Ứng viên tốt nhất đứng đầu (2/3, hòa thì theo tên).
            assert res["results"][0]["pass_rate"] == round(2 / 3, 3)
            assert res["summary"].startswith("tốt nhất: ")
            # Giới hạn candidates.
            res2 = await examples.run_examples(pool, me, "fare", [a])
            assert [r["operator"] for r in res2["results"]] == [a]
            # Chưa có ví dụ → lỗi rõ.
            with pytest.raises(PermanentError, match="chưa có ví dụ"):
                await examples.run_examples(pool, a, "fare", None)
        finally:
            for op in (me, a, b, c):
                await pool.execute("DELETE FROM operators WHERE id = $1", op)
            await pool.execute("DELETE FROM logic_modules WHERE id = 'fare.minitest'")

    asyncio.run(t())
