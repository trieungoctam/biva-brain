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


@needs_db
def test_promote_updates_logic_families():
    """S3.4.3 + S3.2.3: promote chạy xong gom họ logic; >= 3 nhà xe hook/custom -> promote_candidate."""

    async def t() -> None:
        pool = await asyncpg.create_pool(DB_URL, init=init_connection)
        ops = [f"fam{i}{uuid.uuid4().hex[:6]}" for i in range(3)]
        for op in ops:
            await pool.execute("INSERT INTO operators (id, name) VALUES ($1, 'F')", op)
        try:
            for op in ops:
                await pool.execute(
                    """INSERT INTO logic_specs (operator_id, capability, features, rules_text,
                           source_item_ids, implementation, created_by)
                       VALUES ($1, 'fare', $2::jsonb, ARRAY['giá theo tuyến'], '{}', $3::jsonb, 't')""",
                    op,
                    [{"id": "fare.by_route_vehicle"}, {"id": "fare.weekend_surcharge"}],
                    {"mode": "hook", "module": "fare.standard"},
                )
            res = await pjob.promote(pool)
            assert res["counts"]["logic_families"] >= 1, res
            assert res["counts"]["promote_hook_candidates"] >= 1, res
            fam = await pool.fetchrow(
                "SELECT members, promote_candidate, recommended_implementation::text AS rec"
                " FROM logic_families WHERE capability = 'fare' AND $1 = ANY(members)",
                ops[0],
            )
            assert fam is not None and fam["promote_candidate"] is True
            assert sorted(fam["members"]) == sorted(ops)
            # Cache similarity mọi cặp.
            n = await pool.fetchval("SELECT count(*) FROM logic_similarity WHERE capability = 'fare'")
            assert n == 3
            # Nhà xe rời họ (đổi spec hoàn toàn) → lần sau họ không còn thành viên đó.
            await pool.execute(
                'UPDATE logic_specs SET features = \'[{"id":"fare.parcel_only"}]\'::jsonb WHERE operator_id = $1',
                ops[2],
            )
            await pjob.promote(pool)
            fam2 = await pool.fetchrow(
                "SELECT members FROM logic_families WHERE capability = 'fare' AND $1 = ANY(members)", ops[0]
            )
            assert fam2 is not None and ops[2] not in fam2["members"]
        finally:
            for op in ops:
                await pool.execute("DELETE FROM operators WHERE id = $1", op)
            await pool.execute("DELETE FROM logic_families")
            await pool.execute("DELETE FROM logic_similarity")

    asyncio.run(t())


@needs_db
def test_apply_review_enqueues_consolidate():
    """Migration 000023: hàm SQL apply_review tự enqueue consolidate — kể cả đường auto-apply của worker."""

    async def t() -> None:
        pool = await asyncpg.create_pool(DB_URL, init=init_connection)
        op = f"ae{uuid.uuid4().hex[:8]}"
        await pool.execute("DELETE FROM operators WHERE id = $1", op)
        await pool.execute("INSERT INTO operators (id, name) VALUES ($1, 'A')", op)
        try:
            item = await pool.fetchval(
                """INSERT INTO items (layer, operator_id, kind, topic, key, text, status)
                   VALUES (2, $1, 'policy', 'luggage', 'luggage.x', '20kg miễn phí', 'pending')
                   RETURNING id::text""",
                op,
            )
            review = await pool.fetchval(
                """INSERT INTO review_items (operator_id, key, topic, change_kind, risk, item_id, proposed_by)
                   VALUES ($1, 'luggage.x', 'luggage', 'NEW', 'low', $2::uuid, 'system:ingest')
                   RETURNING id::text""",
                op,
                item,
            )
            # Đường worker: gọi thẳng hàm SQL (không qua MCP).
            out = await pool.fetchval("SELECT apply_review($1::uuid, 'system:ingest')::text", review)
            assert '"applied"' in out
            job = await pool.fetchrow(
                "SELECT kind, status FROM operations WHERE idempotency_key = $1", f"consolidate:{op}:{review}"
            )
            assert job is not None and job["kind"] == "consolidate" and job["status"] == "queued"
            # Apply lần 2 không thể (review đã applied) → không job mới.
            n = await pool.fetchval(
                "SELECT count(*) FROM operations WHERE kind = 'consolidate' AND operator_id = $1", op
            )
            assert n == 1
        finally:
            await pool.execute("DELETE FROM operators WHERE id = $1", op)

    asyncio.run(t())


@needs_db
def test_promote_khac_topic_khong_gop():
    """Regression (review mục 5): observation giống nhau nhưng khác topic không được gộp
    thành một thông lệ L1 của topic bất kỳ."""
    import asyncio

    async def t() -> None:
        pool = await asyncpg.create_pool(DB_URL, init=init_connection)
        ops = [f"tp{i}{uuid.uuid4().hex[:8]}" for i in range(3)]
        for op in ops:
            await pool.execute("INSERT INTO operators (id, name) VALUES ($1, 'T')", op)
        try:
            text = "Giá vé đã bao gồm 1 chai nước suối"
            for i, op in enumerate(ops):
                topic = "amenities" if i < 2 else "fare"
                await pool.execute(
                    """INSERT INTO items (layer, operator_id, kind, topic, text, status)
                       VALUES (2, $1, 'observation', $2, $3, 'active')""",
                    op,
                    topic,
                    text,
                )
            created = await pjob.promote(pool)
            # Chỉ 2 nhà xe cùng topic amenities — chưa đủ MIN_OPERATORS (3) → không đề xuất nào.
            assert created["counts"]["promoted_proposals"] == 0, created
        finally:
            for op in ops:
                await pool.execute("DELETE FROM operators WHERE id = $1", op)
            await pool.close()

    asyncio.run(t())


def test_promote_chi_lay_cau_chung():
    """Regression (review mục 5): L1 chỉ chứa câu được ≥3 nhà xe cùng nói — câu riêng của
    nhà xe đại diện ('trẻ em miễn phí') không được lên thông lệ ngành."""
    obs = [
        {
            "id": "a1",
            "operator": "opA",
            "topic": "pets",
            "text": "Không nhận chó mèo trên xe. Trẻ em dưới 5 tuổi miễn phí.",
            "embedding": [],
        },
        {
            "id": "a2",
            "operator": "opB",
            "topic": "pets",
            "text": "không nhận chó mèo trên xe",
            "embedding": [],
        },
        {
            "id": "a3",
            "operator": "opC",
            "topic": "pets",
            "text": "Không nhận chó mèo trên xe.",
            "embedding": [],
        },
    ]
    # Cụm đủ 3 nhà xe; câu chung đủ 3, câu riêng chỉ 1.
    common = pjob._common_sentences(obs)
    assert common == ["Không nhận chó mèo trên xe"], common

    # Không câu nào đủ 3 → trả rỗng (caller giữ nguyên văn cũ).
    lone = [
        dict(obs[0]),
        dict(obs[1]),
        {"id": "a4", "operator": "opD", "topic": "pets", "text": "Xe có wifi", "embedding": []},
    ]
    # cụm này chỉ 2 nhà xe nói câu chó mèo — nhưng cluster yêu cầu 3 ops khác nhau; gọi
    # trực tiếp hàm câu chung với 3 bản mà chỉ 2 bản cùng câu:
    assert pjob._common_sentences(lone[:2] + [lone[2]]) == [], "câu chỉ 2 nhà xe nói không lên L1"
