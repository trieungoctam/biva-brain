"""E3.5: sinh test từ tri thức, run_tests + sandbox_chat qua executor (LLM scripted)."""

from __future__ import annotations

import asyncio
import json
import os
import uuid

import asyncpg
import pytest

from biva_worker import bot_tests
from biva_worker import reflect as reflect_mod
from biva_worker.executor import verdict
from biva_worker.llm import LLMClient
from biva_worker.llm import load as llm_load
from biva_worker.llm.client import RawResponse, Usage
from biva_worker.runner import Job, PermanentError, init_connection

DB_URL = os.environ.get("BIVA_TEST_DATABASE_URL")
needs_db = pytest.mark.skipif(not DB_URL, reason="đặt BIVA_TEST_DATABASE_URL")

DEFINITION = {
    "bot_id": None,
    "operator": None,
    "channel": "zalo",
    "snapshot_version": 1,
    "artifacts": {
        "system_prompt": {"content": "Bạn là bot nhà xe. Giá vé luôn gọi tool query_data."},
        "faq": {"content": "### Chó mèo?\nNhà xe không nhận chó mèo."},
    },
}


class ScriptedLLM:
    """Lượt 1 gọi tool khi câu hỏi chứa 'giá'; lượt sau trả lời dùng kết quả tool."""

    def __init__(self) -> None:
        self.turn = 0

    async def complete(self, step, req, schema) -> RawResponse:
        has_tool_ctx = "tool query_data trả về" in req.messages[0]["content"]
        wants_tool = "giá" in req.messages[0]["content"].lower() and not has_tool_ctx
        if wants_tool:
            data = {"reply": "", "tool": "query_data", "args": {"topic": "fare"}}
        else:
            extra = "350.000đ" if has_tool_ctx else "Dạ nhà xe không nhận chó mèo ạ."
            data = {"reply": extra}
        self.turn += 1
        return RawResponse(json.dumps(data), "end_turn", step.model.name, Usage(50, 30))


def client_with(fake) -> LLMClient:
    return LLMClient(llm_load(), providers={"gemini": fake})


@needs_db
def test_run_tests_and_sandbox_chat(tmp_path):
    async def t() -> None:
        pool = await asyncpg.create_pool(DB_URL, init=init_connection)
        op = f"bt{uuid.uuid4().hex[:8]}"
        await pool.execute("DELETE FROM operators WHERE id = $1", op)
        await pool.execute("INSERT INTO operators (id, name) VALUES ($1, 'B')", op)
        try:
            await pool.execute(
                """INSERT INTO items (layer, operator_id, kind, topic, text, status)
                   VALUES (2, $1, 'policy', 'pets', 'Không nhận chó mèo trên xe', 'active')""",
                op,
            )
            await pool.execute(
                """INSERT INTO items (layer, operator_id, kind, topic, text, status)
                   VALUES (2, $1, 'data', 'fare', 'Giá Sài Gòn Đà Lạt giường nằm 350000đ', 'active')""",
                op,
            )
            # Snapshot.
            await pool.execute(
                "INSERT INTO bots (id, operator_id, channel) VALUES ($1, $2, 'zalo')", op + ":zalo", op
            )
            snap = dict(DEFINITION, bot_id=op + ":zalo", operator=op)
            await pool.execute(
                """INSERT INTO snapshots (bot_id, operator_id, version, artifact_versions, definition, created_by)
                   VALUES ($1, $2, 1, '{}', $3::jsonb, 't')""",
                op + ":zalo",
                op,
                snap,
            )
            run = bot_tests.handler(pool, client_with(ScriptedLLM()))

            # run_tests: sinh test từ tri thức (policy pets + data fare) rồi chạy qua executor.
            res = await run(Job(str(uuid.uuid4()), "bot.tests", op, {"operator_id": op}, 1, 5, {}))
            assert res["counts"]["total"] >= 2, res
            # Case data phải gọi tool; case policy phải nhắc từ khoá.
            assert res["counts"]["passed"] == res["counts"]["total"], res["failed_cases"]

            # test_runs ghi nhận kết quả.
            row = await pool.fetchrow(
                "SELECT total, passed FROM test_runs WHERE operator_id = $1 ORDER BY created_at DESC LIMIT 1",
                op,
            )
            assert row["total"] == res["counts"]["total"] and row["passed"] == res["counts"]["passed"]

            # Sinh lại idempotent: retire bản cũ, không đúp.
            n_active = await pool.fetchval(
                "SELECT count(*) FROM test_cases WHERE operator_id = $1 AND status = 'active'", op
            )
            assert n_active == res["counts"]["total"]

            # sandbox_chat: 1 lượt — câu giá đi qua tool, câu thường không.
            chat = await run(
                Job(
                    str(uuid.uuid4()),
                    "bot.chat",
                    op,
                    {"operator_id": op, "message": "Giá xe đi Đà Lạt bao nhiêu?"},
                    1,
                    5,
                    {},
                )
            )
            assert "350.000đ" in chat["reply"] and chat["tools_called"], chat
            chat2 = await run(
                Job(
                    str(uuid.uuid4()),
                    "bot.chat",
                    op,
                    {"operator_id": op, "message": "Cho em chó lên xe được không?"},
                    1,
                    5,
                    {},
                )
            )
            assert "chó mèo" in chat2["reply"] and not chat2["tools_called"], chat2

            # Chưa có snapshot → lỗi rõ (channel khác).
            with pytest.raises(PermanentError, match="snapshot"):
                await run(
                    Job(
                        str(uuid.uuid4()),
                        "bot.chat",
                        op,
                        {"operator_id": op, "channel": "web", "message": "hi"},
                        1,
                        5,
                        {},
                    )
                )
        finally:
            await pool.execute("DELETE FROM operators WHERE id = $1", op)

    asyncio.run(t())


def test_verdict():
    ok, why = verdict(
        "Giá vé là 350.000đ ạ.",
        [{"tool": "query_data", "args": {}}],
        {"must_call_tool": ["query_data"], "must_mention": ["350.000"]},
    )
    assert ok, why
    ok, why = verdict("350.000đ", [], {"must_call_tool": ["query_data"]})
    assert not ok and "query_data" in why
    ok, why = verdict("giá 400k nhé", [], {"must_not_say": ["400k"]})
    assert not ok and "400k" in why


@needs_db
def test_reflect_and_compare(tmp_path):
    async def t() -> None:
        pool = await asyncpg.create_pool(DB_URL, init=init_connection)
        op = f"rf{uuid.uuid4().hex[:8]}"
        await pool.execute("DELETE FROM operators WHERE id = $1", op)
        await pool.execute("INSERT INTO operators (id, name) VALUES ($1, 'R')", op)
        try:
            item = await pool.fetchval(
                """INSERT INTO items (layer, operator_id, kind, topic, text, status)
                   VALUES (2, $1, 'policy', 'pets', 'Nhà xe nhận chó nhỏ có lồng', 'active')
                   RETURNING id::text""",
                op,
            )

            class ReflectLLM:
                async def complete(self, step, req, schema=None):
                    text = (
                        f"Nhà xe cho phép chó có lồng [[{item}]].\n"
                        "Hiện vận hành 3 chuyến mỗi ngày với tổng 45 ghế các loại."
                    )
                    return RawResponse(text, "end_turn", step.model.name, Usage(50, 30))

            run = reflect_mod.handler(pool, client_with(ReflectLLM()))
            res = await run(
                Job(
                    str(uuid.uuid4()),
                    "bot.reflect",
                    op,
                    {"operator_id": op, "question": "chính sách chó mèo?"},
                    1,
                    5,
                    {},
                )
            )
            assert "[[" + item + "]]" in res["answer"]
            assert res["counts"]["citations"] == 1
            assert any("chưa có trích dẫn" in w for w in res["warnings"]), res  # câu 50k thiếu trích dẫn

            # compare_with_industry: observation L2 + L1 cùng topic.
            await pool.execute(
                """INSERT INTO items (layer, operator_id, kind, topic, key, text, status)
                   VALUES (2, $1, 'observation', 'pets', 'obs.pets', '- Nhà xe nhận chó nhỏ có lồng', 'active')""",
                op,
            )
            await pool.execute(
                """INSERT INTO items (layer, operator_id, kind, topic, key, text, status)
                   VALUES (1, NULL, 'observation', 'pets', 'obs.pets', '- Không nhận chó mèo', 'active')"""
            )
            cmp = await run(Job(str(uuid.uuid4()), "bot.compare", op, {"operator_id": op}, 1, 5, {}))
            assert cmp["topics"][0]["topic"] == "pets"
            assert cmp["topics"][0]["operator"] == ["Nhà xe nhận chó nhỏ có lồng"]
            assert cmp["topics"][0]["industry"] == ["Không nhận chó mèo"]
        finally:
            await pool.execute("DELETE FROM operators WHERE id = $1", op)
            await pool.execute("DELETE FROM items WHERE operator_id IS NULL AND kind = 'observation'")

    asyncio.run(t())


# Regression: khoảng hiệu lực NỬA MỞ [valid_from, valid_to) + cast ngày theo giờ Việt Nam.
def test_query_data_khoang_nua_mo_va_timezone_vn():
    import asyncio
    import os

    import asyncpg

    from biva_worker.executor import query_data
    from biva_worker.runner import init_connection

    db_url = os.environ.get("BIVA_TEST_DATABASE_URL")
    if not db_url:
        pytest.skip("đặt BIVA_TEST_DATABASE_URL để chạy test query_data")

    async def t() -> None:
        import datetime as dt
        import uuid

        pool = await asyncpg.create_pool(db_url, init=init_connection)
        op = "exe" + uuid.uuid4().hex[:8]
        await pool.execute("INSERT INTO operators (id, name) VALUES ($1, 'E')", op)
        try:

            async def ins(key: str, text: str, vf: str, vt: str | None) -> None:
                vf_dt = dt.datetime.fromisoformat(vf)
                vt_dt = dt.datetime.fromisoformat(vt) if vt else None
                await pool.execute(
                    """INSERT INTO items (layer, operator_id, kind, topic, key, text, value, status,
                       valid_from, valid_to)
                       VALUES (2, $1, 'data', 'fare', $2, $3, '{}', 'active', $4::timestamptz, $5::timestamptz)""",
                    op,
                    key,
                    text,
                    vf_dt,
                    vt_dt,
                )

            # HAI dạng valid_to thực tế trong hệ thống:
            # (a) ingest ghi ngày cuối 23:59:59 (giá thường hết 31/10);
            # (b) apply_review cắt midnight-exclusive (giá Tết từ 04/02/2027).
            await ins("fare.sg_dl.old", "SG-DL 320k", "2026-01-01 00:00:07+07", "2026-10-31 23:59:59+07")
            await ins("fare.sg_dl.new", "SG-DL 350k", "2026-11-01 00:00:07+07", None)
            await ins("fare.sg_dl.tet", "SG-DL Tết 450k", "2027-02-04 00:00:07+07", "2027-02-12 23:59:59+07")

            async def only(day: str) -> list[str]:
                r = await query_data(pool, op, {"topic": "fare", "date": day})
                return sorted(r["results"])

            assert await only("2026-10-31") == ["SG-DL 320k"]
            assert await only("2026-11-01") == ["SG-DL 350k"], "mốc chuyển chỉ còn giá mới"
            assert await only("2026-12-01") == ["SG-DL 350k"]
            # Biên Tết (giá 350k vô hạn nên luôn có; Tết chỉ trong khoảng của nó):
            assert await only("2027-02-03") == ["SG-DL 350k"]
            assert await only("2027-02-04") == ["SG-DL 350k", "SG-DL Tết 450k"]
            assert await only("2027-02-12") == ["SG-DL 350k", "SG-DL Tết 450k"]
            assert await only("2027-02-13") == ["SG-DL 350k"]
        finally:
            await pool.execute("DELETE FROM items WHERE operator_id = $1", op)
            await pool.execute("DELETE FROM operators WHERE id = $1", op)
            await pool.close()

    asyncio.run(t())
