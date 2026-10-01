"""Job logic.spec (S2.5.3) với DB thật + LLM scripted: dựng spec từ tri thức, feature lạ bị chặn."""

from __future__ import annotations

import asyncio
import json
import os
import uuid

import asyncpg
import pytest

from biva_worker import logic_spec
from biva_worker.llm import LLMClient
from biva_worker.llm.client import RawResponse, Usage
from biva_worker.llm.config import load as llm_load
from biva_worker.runner import Job, PermanentError, init_connection

DB_URL = os.environ.get("BIVA_TEST_DATABASE_URL")
needs_db = pytest.mark.skipif(not DB_URL, reason="đặt BIVA_TEST_DATABASE_URL để chạy test logic_spec")


class ScriptedLLM:
    """Provider giả trả JSON spec theo kịch bản; ghi prompt để kiểm."""

    def __init__(self, data: dict) -> None:
        self.data = data
        self.prompt = ""

    async def complete(self, step, req, schema) -> RawResponse:
        self.prompt = req.messages[0]["content"]
        return RawResponse(json.dumps(self.data), "end_turn", step.model.name, Usage(100, 50))


class FakeEmbedder:
    async def embed(self, texts: list[str]) -> list[list[float]]:
        return [[0.1, 0.2, 0.3] for _ in texts]


def client_with(fake) -> LLMClient:
    return LLMClient(llm_load(), providers={"gemini": fake})


@needs_db
def test_extract_spec_and_validate_unknown_feature():
    async def t() -> None:
        pool = await asyncpg.create_pool(DB_URL, init=init_connection)
        op = f"spc{uuid.uuid4().hex[:8]}"
        try:
            await pool.execute("INSERT INTO operators (id, name) VALUES ($1, 'Spec test')", op)
            for cap in ("fare", "booking"):
                await pool.execute(
                    """INSERT INTO logic_features (id, capability, description, status)
                       VALUES ($1, $2, $3, 'active')
                       ON CONFLICT (id) DO UPDATE SET status = 'active',
                           capability = EXCLUDED.capability, description = EXCLUDED.description""",
                    f"{cap}.seed_{cap}",
                    cap,
                    f"feature seed {cap}",
                )
            item = await pool.fetchval(
                """INSERT INTO items (layer, operator_id, kind, topic, text, status)
                   VALUES (2, $1, 'policy', 'fare', 'Tết phụ thu 20%', 'active') RETURNING id::text""",
                op,
            )

            fake = ScriptedLLM(
                {
                    "specs": [
                        {
                            "capability": "fare",
                            "features": [{"id": "fare.seed_fare", "params": {"tet": 0.2}}],
                            "rules_text": ["Phụ thu Tết 20% theo ngày đi"],
                            "source_item_ids": [item],
                        },
                        {
                            "capability": "booking",
                            "features": [{"id": "booking.seed_booking"}],
                            "rules_text": ["Giữ chỗ 15 phút"],
                            "source_item_ids": [],
                        },
                    ],
                    "proposed_features": [
                        {
                            "id": "fare.vip_surcharge",
                            "capability": "fare",
                            "description": "Phụ thu ghế VIP",
                            "reason": "tri thức nhắc ghế VIP",
                        },
                        {
                            "id": "fare.seed_fare",
                            "capability": "fare",
                            "description": "đã có",
                            "reason": "bịa",
                        },
                    ],
                }
            )
            run = logic_spec.handler(pool, client_with(fake), FakeEmbedder())
            res = await run(Job(str(uuid.uuid4()), "logic.spec", op, {"operator_id": op}, 1, 5, {}))
            assert res["counts"]["specs"] == 2
            # proposed chỉ giữ feature chưa có trong catalog (vip_surcharge), bỏ cái bịa trùng.
            assert [p["id"] for p in res["proposed_features"]] == ["fare.vip_surcharge"]
            assert "fare.seed_fare" in fake.prompt and item in fake.prompt

            spec = await pool.fetchrow(
                "SELECT features::text AS f, rules_text, source_item_ids, embedding IS NOT NULL AS emb"
                " FROM logic_specs WHERE operator_id=$1 AND capability='fare'",
                op,
            )
            assert json.loads(spec["f"])[0]["id"] == "fare.seed_fare"
            assert spec["rules_text"] == ["Phụ thu Tết 20% theo ngày đi"]
            assert spec["source_item_ids"] == [uuid.UUID(item)]
            assert spec["emb"] is True

            # Chạy lại: idempotent — vẫn 2 dòng spec.
            await run(Job(str(uuid.uuid4()), "logic.spec", op, {"operator_id": op}, 1, 5, {}))
            n = await pool.fetchval("SELECT count(*) FROM logic_specs WHERE operator_id=$1", op)
            assert n == 2
        finally:
            await pool.execute("DELETE FROM operators WHERE id = $1", op)

    asyncio.run(t())


@needs_db
def test_extract_rejects_unknown_feature_and_empty_knowledge():
    async def t() -> None:
        pool = await asyncpg.create_pool(DB_URL, init=init_connection)
        op = f"spc{uuid.uuid4().hex[:8]}"
        try:
            await pool.execute("INSERT INTO operators (id, name) VALUES ($1, 'Spec test 2')", op)
            await pool.execute(
                """INSERT INTO logic_features (id, capability, description, status)
                   VALUES ('fare.seed_fare', 'fare', 'seed', 'active')
                   ON CONFLICT (id) DO UPDATE SET status = 'active'"""
            )
            await pool.execute(
                """INSERT INTO items (layer, operator_id, kind, topic, text, status)
                   VALUES (2, $1, 'policy', 'fare', 'Tết phụ thu 20%', 'active')""",
                op,
            )

            bad = ScriptedLLM(
                {
                    "specs": [
                        {
                            "capability": "fare",
                            "features": [{"id": "fare.khong_co"}],
                            "rules_text": ["quy tắc đủ dài để hợp lệ"],
                            "source_item_ids": [],
                        }
                    ],
                    "proposed_features": [],
                }
            )
            run = logic_spec.handler(pool, client_with(bad), FakeEmbedder())
            with pytest.raises(PermanentError, match="không có trong danh mục"):
                await run(Job(str(uuid.uuid4()), "logic.spec", op, {"operator_id": op}, 1, 5, {}))

            # Nhà xe chưa có tri thức → từ chối rõ ràng, không gọi LLM.
            op2 = op + "x"
            await pool.execute("INSERT INTO operators (id, name) VALUES ($1, 'Trống')", op2)
            none = ScriptedLLM({"specs": [], "proposed_features": []})
            run2 = logic_spec.handler(pool, client_with(none), FakeEmbedder())
            with pytest.raises(PermanentError, match="chưa có tri thức"):
                await run2(Job(str(uuid.uuid4()), "logic.spec", op2, {"operator_id": op2}, 1, 5, {}))
        finally:
            await pool.execute("DELETE FROM operators WHERE id = $1", op)
            await pool.execute("DELETE FROM operators WHERE id = $1", op + "x")

    asyncio.run(t())
