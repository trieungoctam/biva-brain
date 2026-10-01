"""Job validate (S3.3.1): mâu thuẫn artifact ⇔ tri thức.

- Unit: LLM scripted — pipeline đầy đủ (claim, prompt, ghi CONTRADICTION + hạ invalid, audit);
  không mâu thuẫn → chỉ đánh dấu đã kiểm.
- Live: GEMINI thật (BIVA_TEST_GEMINI_API_KEY) trên bộ mâu thuẫn cài sẵn — AC: recall ≥ 90%
  và không báo oan các câu hợp lệ. Skip trong CI.
"""

from __future__ import annotations

import asyncio
import json
import os
import uuid

import asyncpg
import pytest

from biva_worker import validate_llm as v
from biva_worker.llm import LLMClient
from biva_worker.llm import load as llm_load
from biva_worker.llm.client import RawResponse, Usage
from biva_worker.runner import Job, PermanentError, init_connection

DB_URL = os.environ.get("BIVA_TEST_DATABASE_URL")
needs_db = pytest.mark.skipif(not DB_URL, reason="đặt BIVA_TEST_DATABASE_URL để chạy test validate")


class ScriptedLLM:
    def __init__(self, data: dict) -> None:
        self.data = data
        self.prompt = ""

    async def complete(self, step, req, schema) -> RawResponse:
        self.prompt = req.messages[0]["content"]
        return RawResponse(json.dumps(self.data), "end_turn", step.model.name, Usage(100, 50))


def client_with(fake) -> LLMClient:
    return LLMClient(llm_load(), providers={"gemini": fake})


@needs_db
def test_validate_job_records_contradiction_and_clean_case():
    async def t() -> None:
        pool = await asyncpg.create_pool(DB_URL, init=init_connection)
        op = f"vl{uuid.uuid4().hex[:8]}"
        await pool.execute("DELETE FROM operators WHERE id = $1", op)  # dọn lần chạy dở dang
        await pool.execute("INSERT INTO operators (id, name) VALUES ($1, 'V')", op)
        await pool.execute(
            "INSERT INTO bots (id, operator_id, channel) VALUES ($1, $2, 'zalo')", op + ":zalo", op
        )
        try:
            item = await pool.fetchval(
                """INSERT INTO items (layer, operator_id, kind, topic, text, status)
                   VALUES (2, $1, 'policy', 'pets', 'Không nhận chó mèo trên xe', 'active')
                   RETURNING id::text""",
                op,
            )
            art = await pool.fetchval(
                """INSERT INTO bot_artifacts (bot_id, operator_id, kind, version, content, content_hash,
                       status, author, validation)
                   VALUES ($1, $2, 'faq', 1, $3, 'h1', 'valid', 'ai:t', '{"valid": true}'::jsonb)
                   RETURNING id::text""",
                op + ":zalo",
                op,
                "### Chó mèo?\nNhà xe nhận chó mèo nhỏ có lồng. [[" + item + "]]",
            )
            await pool.execute(
                """INSERT INTO artifact_citations (artifact_id, item_id, line)
                   VALUES ($1::uuid, $2::uuid, 2)""",
                art,
                item,
            )

            fake = ScriptedLLM(
                {
                    "contradictions": [
                        {
                            "quote": "Nhà xe nhận chó mèo nhỏ có lồng.",
                            "item_id": item,
                            "reason": "artifact cho phép, tri thức cấm",
                            "confident": True,
                        },
                        {"quote": "gì đó", "item_id": item, "reason": "không chắc", "confident": False},
                    ]
                }
            )
            run = v.handler(pool, client_with(fake))
            res = await run(
                Job(str(uuid.uuid4()), "validate", op, {"operator_id": op, "artifact_id": art}, 1, 5, {})
            )
            assert res["counts"]["contradictions"] == 1  # confident=false bị bỏ
            assert "Không nhận chó mèo" in fake.prompt
            row = await pool.fetchrow(
                "SELECT status, validation::text AS val FROM bot_artifacts WHERE id = $1::uuid", art
            )
            assert row["status"] == "invalid"
            errors = json.loads(row["val"])["errors"]
            assert errors[0]["code"] == "CONTRADICTION" and errors[0]["item"] == item
            assert errors[0]["line"] == 2

            # Không mâu thuẫn → giữ valid, đánh dấu đã kiểm.
            art2 = await pool.fetchval(
                """INSERT INTO bot_artifacts (bot_id, operator_id, kind, version, content, content_hash,
                       status, author, validation)
                   VALUES ($1, $2, 'faq', 2, $3, 'h2', 'valid', 'ai:t', '{"valid": true}'::jsonb)
                   RETURNING id::text""",
                op + ":zalo",
                op,
                "### Chó mèo?\nNhà xe không nhận chó mèo ạ. [[" + item + "]]",
            )
            await pool.execute(
                """INSERT INTO artifact_citations (artifact_id, item_id, line)
                   VALUES ($1::uuid, $2::uuid, 2)""",
                art2,
                item,
            )
            clean = ScriptedLLM({"contradictions": []})
            res = await v.handler(pool, client_with(clean))(
                Job(str(uuid.uuid4()), "validate", op, {"operator_id": op, "artifact_id": art2}, 1, 5, {})
            )
            assert res["counts"]["contradictions"] == 0
            row = await pool.fetchrow(
                "SELECT status, validation::text AS val FROM bot_artifacts WHERE id = $1::uuid", art2
            )
            assert row["status"] == "valid"
            assert json.loads(row["val"])["contradiction_checked_at"] is True

            # Artifact không tồn tại → lỗi rõ.
            with pytest.raises(PermanentError):
                await v.handler(pool, client_with(clean))(
                    Job(
                        str(uuid.uuid4()),
                        "validate",
                        op,
                        {"operator_id": op, "artifact_id": str(uuid.uuid4())},
                        1,
                        5,
                        {},
                    )
                )
        finally:
            await pool.execute("DELETE FROM operators WHERE id = $1", op)

    asyncio.run(t())


# ─────────────────── Bộ mâu thuẫn cài sẵn (live test với Gemini thật) ───────────────────

CASES = [
    # (claim trong artifact, item tri thức, có mâu thuẫn không)
    ("Nhà xe nhận chó mèo có lồng.", "Không nhận chó mèo trên xe.", True),
    ("Hành lý miễn phí 20kg.", "Mỗi khách được mang 40kg hành lý miễn phí.", True),
    ("Chuyến 22h30 khởi hành mỗi tối.", "Chuyến 22h30 khởi hành mỗi tối.", False),
    (
        "Trẻ em dưới 6 tuổi ngồi chung miễn phí.",
        "Trẻ em dưới 6 tuổi ngồi chung ghế với bố mẹ miễn phí.",
        False,
    ),
    ("Giá tuyến Sài Gòn – Đà Lạt 350.000đ.", "Giá tuyến Sài Gòn – Đà Lạt giường nằm 320.000đ.", True),
    ("Đổi vé miễn phí trước 24 giờ.", "Đổi vé trước 24 giờ mất 10% giá vé.", True),
    ("Xe có wifi miễn phí.", "Không nhận chó mèo trên xe.", False),
    ("Khách đến trước 30 phút để làm thủ tục.", "Khách nên có mặt trước 30 phút giờ khởi hành.", False),
    ("Huỷ vé trước 6 giờ được hoàn 100%.", "Huỷ vé trước 6 giờ hoàn 80% giá vé.", True),
    ("Xe giường nằm chạy ban đêm.", "Chuyến giường nằm Sài Gòn – Đà Lạt khởi hành 21h30.", False),
]

KEY = os.environ.get("BIVA_TEST_GEMINI_API_KEY")
live = pytest.mark.skipif(not KEY, reason="đặt BIVA_TEST_GEMINI_API_KEY để đo recall trên Gemini thật")


@live
def test_contradiction_recall_live():
    """AC S3.3.1: bắt ≥ 90% mâu thuẫn cài sẵn; không báo oan quá 1 câu hợp lệ."""

    async def t() -> None:
        client = LLMClient(llm_load())
        hits = 0
        total = 0
        false_alarm = 0
        for idx, (claim, fact, expect) in enumerate(CASES):
            items = [{"id": f"it_{idx}", "text": fact}]
            got = await v.detect(client, [claim], items)
            if expect:
                total += 1
                hits += bool(got)
            else:
                false_alarm += bool(got)
        recall = hits / total
        assert recall >= 0.9, f"recall {recall:.2f} ({hits}/{total})"
        assert false_alarm <= 1, f"báo oan {false_alarm} câu hợp lệ"

    asyncio.run(t())
