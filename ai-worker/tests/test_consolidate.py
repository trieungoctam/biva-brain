"""Job consolidate + promote (E3.2) với DB thật: gom observation, đề xuất L1 khi ≥ 3 nhà xe."""

from __future__ import annotations

import asyncio
import os
import uuid

import asyncpg
import pytest

from biva_worker import consolidate as cjob
from biva_worker import promote as pjob
from biva_worker.runner import init_connection

DB_URL = os.environ.get("BIVA_TEST_DATABASE_URL")
needs_db = pytest.mark.skipif(not DB_URL, reason="đặt BIVA_TEST_DATABASE_URL để chạy test consolidate")


@needs_db
def test_consolidate_and_promote_flow():
    async def t() -> None:
        pool = await asyncpg.create_pool(DB_URL, init=init_connection)
        ops = [f"cs{i}{uuid.uuid4().hex[:8]}" for i in range(4)]
        for op in ops:
            await pool.execute("INSERT INTO operators (id, name) VALUES ($1, 'C')", op)
        try:
            # Mỗi nhà xe một item cùng thông lệ (giống nhau về nội dung) + một câu riêng.
            texts = [
                "Không nhận chó mèo trên xe",
                "Không nhận chó mèo trên xe",
                "không nhận chó mèo trên xe ghế",
                "Xe có wifi miễn phí",
            ]
            for op, text in zip(ops, texts, strict=True):
                await pool.execute(
                    """INSERT INTO items (layer, operator_id, kind, topic, key, text, status)
                       VALUES (2, $1, 'policy', $2, $2 || '.cho_meo', $3, 'active')""",
                    op,
                    "pets_x",
                    text,
                )

            # consolidate: mỗi nhà xe 1 observation (topic pets_x), nguồn + consolidated_at.
            for op in ops:
                res = await cjob.consolidate(pool, op)
                assert res["counts"]["sources"] >= 1, res
            obs = await pool.fetch(
                """SELECT operator_id, text FROM items
                   WHERE kind = 'observation' AND layer = 2 AND operator_id = ANY($1::text[])""",
                ops,
            )
            assert len(obs) == 4
            srcs = await pool.fetchval("SELECT count(*) FROM observation_sources")
            assert srcs >= 3
            left = await pool.fetchval(
                """SELECT count(*) FROM items
                   WHERE kind <> 'observation' AND consolidated_at IS NULL AND operator_id = ANY($1::text[])""",
                ops,
            )
            assert left == 0

            # Item mới cùng topic → observation cập nhật (không tạo observation thứ hai).
            await pool.execute(
                """INSERT INTO items (layer, operator_id, kind, topic, key, text, status)
                   VALUES (2, $1, 'policy', 'pets_x', 'pets_x.them', 'Trẻ em dưới 6 tuổi miễn phí', 'active')""",
                ops[0],
            )
            await cjob.consolidate(pool, ops[0])
            n = await pool.fetchval(
                "SELECT count(*) FROM items WHERE kind = 'observation' AND operator_id = $1", ops[0]
            )
            assert n == 1

            # promote: cụm 3 nhà xe → 1 đề xuất PROMOTE kèm danh sách.
            res = await pjob.promote(pool)
            assert res["counts"]["promoted_proposals"] >= 1, res
            row = await pool.fetchrow(
                """SELECT r.id::text AS id, r.key, r.change_kind, r.risk, r.after::text AS after, r.operator_id,
                       i.status AS item_status
                   FROM review_items r JOIN items i ON i.id = r.item_id
                   WHERE r.change_kind = 'PROMOTE' AND r.operator_id = ANY($1::text[])""",
                ops,
            )
            assert row is not None and row["change_kind"] == "PROMOTE" and row["risk"] == "high"
            assert row["item_status"] == "pending"
            import json as _json

            after = _json.loads(row["after"])
            assert ops[3] not in after["operators"]  # nhà xe khác thông lệ không vào đề xuất
            assert all(op in after["operators"] for op in ops[:3])

            # Chạy lại: không tạo đề xuất trùng (đang open).
            res2 = await pjob.promote(pool)
            assert res2["counts"]["promoted_proposals"] == 0, res2

            # Duyệt PROMOTE qua apply_review → item L1 active (operator NULL).
            out = await pool.fetchval("SELECT apply_review($1::uuid, 'user:t')::text", row["id"])
            assert '"applied"' in out
            l1 = await pool.fetchrow(
                "SELECT status, operator_id, layer FROM items WHERE key = $1", row["key"]
            )
            assert l1["status"] == "active" and l1["operator_id"] is None and l1["layer"] == 1
        finally:
            for op in ops:
                await pool.execute("DELETE FROM operators WHERE id = $1", op)
            await pool.execute(
                "DELETE FROM items WHERE key LIKE 'obs.pets_x%' OR key LIKE 'l1.promoted.pets_x%'"
            )

    asyncio.run(t())


def test_clusters_can_gom_3_nhà_xe():
    obs = [
        {"id": "1", "operator": "a", "topic": "t", "embedding": [], "text": "Không nhận chó mèo trên xe"},
        {"id": "2", "operator": "b", "topic": "t", "embedding": [], "text": "Không nhận chó mèo trên xe"},
        {"id": "3", "operator": "c", "topic": "t", "embedding": [], "text": "khong nhan cho meo tren xe"},
        {"id": "4", "operator": "d", "topic": "t", "embedding": [], "text": "Xe có wifi miễn phí"},
    ]
    got = pjob.clusters(obs)
    assert len(got) == 1 and {o["operator"] for o in got[0]} == {"a", "b", "c"}
