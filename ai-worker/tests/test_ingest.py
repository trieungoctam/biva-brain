"""Ingest (S1.1.2 + S1.2.1–S1.2.3): diff thuần + luồng đầy đủ với Postgres thật và LLM giả lập."""

from __future__ import annotations

import asyncio
import json
import os
import uuid
from datetime import date, datetime
from typing import Any

import asyncpg
import pytest

from biva_worker.contracts import validate
from biva_worker.ingest import job as ingest_job
from biva_worker.ingest.diff import diff
from biva_worker.ingest.extract import Candidate, ExistingItem, build_prompt, normalize_key, schema
from biva_worker.llm import LLMClient, load
from biva_worker.llm.client import RawResponse, Usage
from biva_worker.runner import Job, PermanentError, init_connection

# ─────────────────────────────── thuần ───────────────────────────────


def cand(key, text="x x x", action="upsert", topic=None, facts=None, vf=None, kind="data"):
    topic = topic or key.split(".")[0]
    return Candidate(action, kind, topic, key, text, facts or {}, vf, None)


def item(key, text="x x x", facts=None, vf=None):
    return ExistingItem(f"id-{key}", key, key.split(".")[0], text, facts or {}, vf, None)


def test_normalize_key():
    assert normalize_key("fare", "fare.Sài Gòn - Đà Lạt.Giường nằm") == "fare.sai_gon_da_lat.giuong_nam"
    assert normalize_key("fare", "sai_gon-da_lat.giuong_nam") == "fare.sai_gon_da_lat.giuong_nam"
    assert normalize_key("pets", "Điều kiện") == "pets.dieu_kien"
    assert normalize_key("pets", "..") == "pets"


def test_diff_kinds_and_risk():
    existing = {
        "fare.a": item("fare.a", "Giá 300.000đ", {"gia_ve": "300000"}),
        "pets.dieu_kien": item("pets.dieu_kien", "Thú cưng để trong lồng"),
        "luggage.mien_phi": item("luggage.mien_phi", "20kg miễn phí"),
        "children.mien_ve": item("children.mien_ve", "Dưới 5 tuổi miễn vé"),
    }
    out = {
        d.key: (d.change_kind, d.risk)
        for d in diff(
            [
                cand("fare.a", "Giá vé nay là 320.000đ", facts={"gia_ve": "320000"}),  # CHANGE, giá → high
                cand("pets.dieu_kien", "thú cưng để trong LỒNG"),  # cùng text (chuẩn hoá) → DUPLICATE
                cand("luggage.mien_phi", "bỏ", action="remove"),  # REMOVE → high
                cand("route.moi", "Thêm tuyến Sài Gòn - Phan Thiết"),  # NEW, route → low
                cand("fare.b", "Giá mới 1", facts={"gia_ve": "1"}),
                cand("fare.b", "Giá mới 2", facts={"gia_ve": "2"}),  # cùng tin, 2 nội dung → CONFLICT
                cand("contact.khong_co", "bỏ", action="remove"),  # remove key chưa có → bỏ qua
                cand("children.mien_ve", "bỏ cũ", action="remove"),
                cand("children.mien_ve", "Dưới 6 tuổi miễn vé"),  # upsert thắng remove → CHANGE (luôn duyệt)
            ],
            existing,
        )
    }
    assert out == {
        "fare.a": ("CHANGE", "high"),
        "pets.dieu_kien": ("DUPLICATE", "low"),
        "luggage.mien_phi": ("REMOVE", "high"),
        "route.moi": ("NEW", "low"),
        "fare.b": ("CONFLICT", "high"),
        "children.mien_ve": ("CHANGE", "high"),
    }
    assert (
        len([d for d in diff([cand("fare.b", facts={"g": "1"}), cand("fare.b", facts={"g": "2"})], {})]) == 2
    )


def test_diff_same_facts_different_wording_is_duplicate_but_new_validity_is_change():
    e = {"fare.a": item("fare.a", "Giá 300k", {"gia_ve": "300000"})}
    assert (
        diff([cand("fare.a", "Vé 300.000 đồng", facts={"Gia_ve": "300000"})], e)[0].change_kind == "DUPLICATE"
    )
    d = diff([cand("fare.a", "Từ 1/11 giá 300k", facts={"gia_ve": "300000"}, vf=date(2026, 11, 1))], e)
    assert d[0].change_kind == "CHANGE"


def test_schema_and_prompt():
    s = schema(["fare", "pets"])
    assert s["properties"]["items"]["items"]["properties"]["topic"]["enum"] == ["fare", "pets", "other"]
    p = build_prompt("Giá vé 300k", date(2026, 10, 1), [item("fare.a", "Giá 300k")])
    assert "2026-10-01 (thứ Năm)" in p and "- fare.a: Giá 300k" in p and "<<<\nGiá vé 300k\n>>>" in p


# ─────────────────────────────── luồng đầy đủ với DB ───────────────────────────────

DB_URL = os.environ.get("BIVA_TEST_DATABASE_URL")
needs_db = pytest.mark.skipif(not DB_URL, reason="đặt BIVA_TEST_DATABASE_URL để chạy test ingest")


class ScriptedLLM:
    """Provider giả: mỗi lượt gọi trả JSON items kế tiếp trong kịch bản."""

    def __init__(self, outputs: list[list[dict[str, Any]]]) -> None:
        self.outputs = list(outputs)
        self.prompts: list[str] = []

    async def complete(self, step, req, schema) -> RawResponse:
        self.prompts.append(req.messages[0]["content"])
        return RawResponse(
            json.dumps({"items": self.outputs.pop(0)}), "end_turn", step.model.name, Usage(100, 50)
        )


def it(action, kind, topic, key, text, facts=None, vf=None):
    return {
        "action": action,
        "kind": kind,
        "topic": topic,
        "key": key,
        "text": text,
        "facts": [{"name": k, "value": v} for k, v in (facts or {}).items()],
        "valid_from": vf,
        "valid_to": None,
    }


@needs_db
def test_ingest_flow_end_to_end():
    async def t() -> None:
        pool = await asyncpg.create_pool(DB_URL, init=init_connection)
        op = f"ing{uuid.uuid4().hex[:8]}"
        fake = ScriptedLLM(
            [
                [  # tin 1
                    it("upsert", "data", "fare", "fare.sai_gon-da_lat.giuong_nam", "Giá vé SG–ĐL giường nằm 300.000đ",
                       {"gia_ve": "300000"}),
                    it("upsert", "policy", "pets", "pets.dieu_kien", "Thú cưng phải để trong lồng"),
                ],
                [  # tin 2
                    it("upsert", "data", "fare", "fare.sai_gon_da_lat.giuong_nam", "Từ 1/11 giá SG–ĐL giường nằm 320.000đ",
                       {"gia_ve": "320000"}, "2026-11-01"),
                    it("upsert", "policy", "pets", "pets.dieu_kien", "Thú cưng phải để trong lồng."),
                    it("remove", "policy", "luggage", "luggage.mien_phi", "Bỏ quy định hành lý"),
                ],
                [  # tin 3: lại đổi giá (đề xuất song song với tin 2)
                    it("upsert", "data", "fare", "fare.sai_gon_da_lat.giuong_nam", "Giá SG–ĐL giường nằm 310.000đ",
                       {"gia_ve": "310000"}),
                ],
            ]
        )  # fmt: skip
        llm = LLMClient(load(), providers={"gemini": fake})
        run = ingest_job.handler(pool, llm)

        def job(content: str) -> Job:
            payload = {"operator_id": op, "source": "zalo", "received_at": "2026-10-01T09:00:00+07:00",
                       "content": content}  # fmt: skip
            return Job(str(uuid.uuid4()), "ingest", op, payload, 1, 5, {})

        async def review(key: str, status: str = "open") -> asyncpg.Record:
            return await pool.fetchrow(
                "SELECT * FROM review_items WHERE operator_id=$1 AND key=$2 AND status=$3 ORDER BY created_at DESC",
                op, key, status,
            )  # fmt: skip

        async def active(key: str) -> asyncpg.Record | None:
            return await pool.fetchrow(
                "SELECT * FROM items WHERE operator_id=$1 AND key=$2 AND status='active'", op, key
            )

        try:
            await pool.execute("INSERT INTO operators (id, name) VALUES ($1, 'Ingest test')", op)

            # ── Tin 1: giá (rủi ro cao) chờ duyệt; thú cưng (thấp) tự áp dụng.
            r1 = await run(job("Giá vé SG-ĐL giường nằm 300k. Thú cưng để trong lồng."))
            assert validate("operation_result", r1) == []
            assert (
                r1["counts"]["new"] == 2
                and r1["counts"]["auto_applied"] == 1
                and r1["counts"]["waiting_review"] == 1
            )
            fare_key = "fare.sai_gon_da_lat.giuong_nam"
            assert await active(fare_key) is None and await active("pets.dieu_kien") is not None
            rv = await review(fare_key)
            assert (rv["change_kind"], rv["risk"]) == ("NEW", "high")
            res = json.loads(await pool.fetchval("SELECT apply_review($1, 'user:tam')::text", rv["id"]))
            assert res["status"] == "applied"
            old = await active(fare_key)
            assert old["value"] == {"facts": {"gia_ve": "300000"}}
            assert await pool.fetchval(
                "SELECT count(*) FROM operations WHERE kind='index.items' AND operator_id=$1", op) == 1  # fmt: skip

            # ── Tin trùng: không gọi LLM.
            calls = len(fake.prompts)
            dup = await run(job("Giá vé SG-ĐL giường nằm 300k. Thú cưng để trong lồng."))
            assert dup["counts"] == {"duplicate_document": 1} and len(fake.prompts) == calls

            # ── Tin 2: LLM thấy key đã có; đổi giá → CHANGE chờ duyệt; thú cưng → DUPLICATE tự áp dụng.
            r2 = await run(job("Từ 1/11 giá lên 320k. Thú cưng vẫn để trong lồng. Bỏ hành lý."))
            assert f"- {fare_key}: Giá vé SG–ĐL giường nằm 300.000đ" in fake.prompts[-1]
            assert r2["counts"]["change"] == 1 and r2["counts"]["duplicate"] == 1
            assert "remove" not in r2["counts"]  # luggage chưa từng có → bỏ qua
            assert (await active("pets.dieu_kien"))["proof_count"] == 2

            # ── Tin 3 đề xuất giá khác cho cùng key; duyệt tin 3 → đề xuất của tin 2 thành stale.
            await run(job("Giá SG-ĐL giường nằm 310k."))
            open_changes = await pool.fetch(
                "SELECT id, after FROM review_items WHERE operator_id=$1 AND key=$2 AND status='open' "
                "ORDER BY created_at",
                op, fare_key,
            )  # fmt: skip
            assert len(open_changes) == 2
            res = json.loads(
                await pool.fetchval("SELECT apply_review($1, 'user:tam')::text", open_changes[1]["id"])
            )
            assert res["status"] == "applied"
            new = await active(fare_key)
            assert new["value"] == {"facts": {"gia_ve": "310000"}}
            superseded = await pool.fetchrow("SELECT status, superseded_by FROM items WHERE id=$1", old["id"])
            assert (superseded["status"], superseded["superseded_by"]) == ("superseded", new["id"])
            stale = await pool.fetchrow(
                "SELECT status, item_id FROM review_items WHERE id=$1", open_changes[0]["id"]
            )
            assert stale["status"] == "stale"
            assert await pool.fetchval("SELECT status FROM items WHERE id=$1", stale["item_id"]) == "rejected"
            with pytest.raises(asyncpg.PostgresError, match="stale"):
                await pool.fetchval("SELECT apply_review($1, 'user:tam')", open_changes[0]["id"])

            # audit ghi người duyệt.
            assert await pool.fetchval(
                "SELECT count(*) FROM audit_log WHERE action='review.apply' AND actor='user:tam' "
                "AND payload->>'operator_id'=$1",
                op,
            ) == 2  # fmt: skip
        finally:
            await pool.execute("DELETE FROM audit_log WHERE payload->>'operator_id' = $1", op)
            await pool.execute("DELETE FROM operators WHERE id = $1", op)
            await pool.close()

    asyncio.run(t())


@needs_db
def test_reject_and_payload_errors():
    async def t() -> None:
        pool = await asyncpg.create_pool(DB_URL, init=init_connection)
        op = f"rej{uuid.uuid4().hex[:8]}"
        fake = ScriptedLLM(
            [[it("upsert", "data", "schedule", "schedule.sg_dl.22h30", "Chuyến 22h30 đổi sang 23h")]]
        )
        run = ingest_job.handler(pool, LLMClient(load(), providers={"gemini": fake}))
        payload = {
            "operator_id": op,
            "source": "zalo",
            "received_at": "2026-10-01T09:00:00+07:00",
            "content": "x",
        }
        try:
            await pool.execute("INSERT INTO operators (id, name) VALUES ($1, 'Reject test')", op)
            await run(Job(str(uuid.uuid4()), "ingest", op, payload, 1, 5, {}))
            rid = await pool.fetchval("SELECT id FROM review_items WHERE operator_id=$1", op)
            await pool.execute("SELECT reject_review($1, 'user:tam', 'nhà xe nhắn nhầm')", rid)
            row = await pool.fetchrow(
                "SELECT r.status, r.reason, i.status AS item_status "
                "FROM review_items r JOIN items i ON i.id = r.item_id WHERE r.id = $1",
                rid,
            )
            assert (row["status"], row["reason"], row["item_status"]) == (
                "rejected",
                "nhà xe nhắn nhầm",
                "rejected",
            )

            with pytest.raises(PermanentError, match="không có nhà xe"):
                await run(
                    Job(
                        str(uuid.uuid4()),
                        "ingest",
                        "khong-co",
                        {**payload, "operator_id": "khong-co"},
                        1,
                        5,
                        {},
                    )
                )
            with pytest.raises(PermanentError, match="chưa hỗ trợ"):
                file_payload = {k: v for k, v in payload.items() if k != "content"}
                file_payload["file_ref"] = {"bucket": "b", "key": "gia.xlsx"}
                await run(Job(str(uuid.uuid4()), "ingest", op, file_payload, 1, 5, {}))
        finally:
            await pool.execute("DELETE FROM audit_log WHERE payload->>'operator_id' = $1", op)
            await pool.execute("DELETE FROM operators WHERE id = $1", op)
            await pool.close()

    asyncio.run(t())


def test_diff_candidate_without_dates_keeps_validity():
    e = {"pets.a": item("pets.a", "Thú cưng để trong lồng", vf=date(2026, 10, 1))}
    assert diff([cand("pets.a", "Thú cưng để trong lồng.")], e)[0].change_kind == "DUPLICATE"


@needs_db
def test_future_dated_change_keeps_current_version_until_then():
    """Giá mới từ một ngày tương lai: giá hiện tại vẫn đúng tới ngày đó (không supersede sớm)."""

    async def t() -> None:
        pool = await asyncpg.create_pool(DB_URL, init=init_connection)
        op = f"fut{uuid.uuid4().hex[:8]}"
        key = "fare.sg_dl.giuong_nam"
        fake = ScriptedLLM(
            [
                [it("upsert", "data", "fare", key, "Giá giường nằm 300.000đ", {"gia_ve": "300000"})],
                [
                    it(
                        "upsert",
                        "data",
                        "fare",
                        key,
                        "Từ 1/1/2099 giá 320.000đ",
                        {"gia_ve": "320000"},
                        "2099-01-01",
                    )
                ],
            ]
        )
        run = ingest_job.handler(pool, LLMClient(load(), providers={"gemini": fake}))

        def job(content: str) -> Job:
            payload = {
                "operator_id": op,
                "source": "zalo",
                "received_at": "2026-10-01T09:00:00+07:00",
                "content": content,
            }
            return Job(str(uuid.uuid4()), "ingest", op, payload, 1, 5, {})

        async def approve_open() -> None:
            for row in await pool.fetch(
                "SELECT id FROM review_items WHERE operator_id=$1 AND status='open'", op
            ):
                await pool.fetchval("SELECT apply_review($1, 'user:tam')", row["id"])

        async def price_at(iso: str) -> str:
            ts = datetime.fromisoformat(iso)
            return await pool.fetchval(
                "SELECT value->'facts'->>'gia_ve' FROM items WHERE operator_id=$1 AND key=$2 AND status='active' "
                "AND (valid_from IS NULL OR valid_from <= $3) AND (valid_to IS NULL OR valid_to > $3)",
                op, key, ts,
            )  # fmt: skip

        try:
            await pool.execute("INSERT INTO operators (id, name) VALUES ($1, 'Future test')", op)
            await run(job("giá 300k"))
            await approve_open()
            r = await run(job("từ 1/1/2099 giá 320k"))
            assert r["counts"]["change"] == 1 and r["counts"]["waiting_review"] == 1  # CHANGE luôn chờ duyệt
            await approve_open()

            rows = await pool.fetch(
                "SELECT value->'facts'->>'gia_ve' AS gia, status, valid_to FROM items "
                "WHERE operator_id=$1 AND key=$2 ORDER BY created_at",
                op, key,
            )  # fmt: skip
            assert [(x["gia"], x["status"]) for x in rows] == [("300000", "active"), ("320000", "active")]
            new_from = await pool.fetchval(
                "SELECT valid_from FROM items WHERE operator_id=$1 AND key=$2 ORDER BY created_at DESC LIMIT 1",
                op,
                key,
            )
            assert rows[0]["valid_to"] == new_from  # bản cũ hết hiệu lực đúng lúc bản mới bắt đầu
            assert await price_at("2026-12-31T12:00:00+07:00") == "300000"
            assert await price_at("2099-01-02T12:00:00+07:00") == "320000"

            # Ràng buộc DB: hai bản active cùng key không được chồng hiệu lực.
            with pytest.raises(asyncpg.exceptions.ExclusionViolationError):
                await pool.execute(
                    "INSERT INTO items (layer, operator_id, kind, topic, key, text, status, valid_from) "
                    "VALUES (2, $1, 'data', 'fare', $2, 'chồng hiệu lực', 'active', '2098-06-01')",
                    op, key,
                )  # fmt: skip
        finally:
            await pool.execute("DELETE FROM audit_log WHERE payload->>'operator_id' = $1", op)
            await pool.execute("DELETE FROM operators WHERE id = $1", op)
            await pool.close()

    asyncio.run(t())


def test_diff_past_start_dates_are_not_a_change():
    """LLM có thể gán valid_from = ngày nhận tin; nhắc lại điều đang đúng ở tin sau không thành CHANGE."""
    e = {"luggage.a": item("luggage.a", "20kg miễn phí", vf=date(2026, 10, 1))}
    again = cand("luggage.a", "20kg miễn phí", vf=date(2026, 10, 15))
    assert diff([again], e, today=date(2026, 10, 15))[0].change_kind == "DUPLICATE"
    future = cand("luggage.a", "20kg miễn phí", vf=date(2026, 12, 1))
    assert diff([future], e, today=date(2026, 10, 15))[0].change_kind == "CHANGE"


def test_diff_fewer_facts_with_same_values_is_duplicate():
    e = {
        "fare.l": item(
            "fare.l", "Limousine SG–ĐL 450.000đ", {"tuyen": "Sài Gòn - Đà Lạt", "gia_ve": "450000"}
        )
    }
    assert (
        diff([cand("fare.l", "Limousine giữ nguyên 450k", facts={"gia_ve": "450000"})], e)[0].change_kind
        == "DUPLICATE"
    )
    assert diff([cand("fare.l", "Limousine 480k", facts={"gia_ve": "480000"})], e)[0].change_kind == "CHANGE"
    assert (
        diff([cand("fare.l", "Thêm wifi", facts={"gia_ve": "450000", "wifi": "co"})], e)[0].change_kind
        == "CHANGE"
    )


def test_normalize_key_shared_fixture():
    """Cùng fixture với Go (textnorm.NormalizeKey): key do ingest và propose_item sinh ra phải trùng nhau."""
    from biva_worker.contracts import contracts_root

    for line in (contracts_root() / "textnorm" / "keys.jsonl").read_text(encoding="utf-8").splitlines():
        c = json.loads(line)
        assert normalize_key(c["topic"], c["key"]) == c["expected"], c


@needs_db
def test_auto_apply_can_be_disabled(monkeypatch):
    """AC S1.2.2: rủi ro thấp chỉ tự áp dụng theo cấu hình; tắt → mọi đề xuất chờ duyệt."""
    monkeypatch.setenv("BIVA_INGEST_AUTO_APPLY", "false")

    async def t() -> None:
        pool = await asyncpg.create_pool(DB_URL, init=init_connection)
        op = f"noauto{uuid.uuid4().hex[:6]}"
        fake = ScriptedLLM([[it("upsert", "policy", "pets", "pets.dieu_kien", "Thú cưng để trong lồng")]])
        run = ingest_job.handler(pool, LLMClient(load(), providers={"gemini": fake}))
        payload = {
            "operator_id": op,
            "source": "zalo",
            "received_at": "2026-10-01T09:00:00+07:00",
            "content": "x",
        }
        try:
            await pool.execute("INSERT INTO operators (id, name) VALUES ($1, 'No auto')", op)
            r = await run(Job(str(uuid.uuid4()), "ingest", op, payload, 1, 5, {}))
            assert (r["counts"]["auto_applied"], r["counts"]["waiting_review"]) == (0, 1)
            assert await pool.fetchval("SELECT status FROM review_items WHERE operator_id=$1", op) == "open"
        finally:
            await pool.execute("DELETE FROM operators WHERE id = $1", op)
            await pool.close()

    asyncio.run(t())


@needs_db
def test_structured_items_skip_llm():
    """AI phía builder (ChatGPT, coding agent) gửi items đã trích: không gọi LLM, cùng diff/review."""

    class NoLLM:
        async def complete(self, *a):
            raise AssertionError("đường items không được gọi LLM")

    async def t() -> None:
        pool = await asyncpg.create_pool(DB_URL, init=init_connection)
        op = f"str{uuid.uuid4().hex[:8]}"
        run = ingest_job.handler(pool, LLMClient(load(), providers={"gemini": NoLLM()}))

        def job(items, content=None, source="image"):
            payload = {
                "operator_id": op,
                "source": source,
                "received_at": "2026-10-01T09:00:00+07:00",
                "items": items,
            }
            if content:
                payload["content"] = content
            return Job(str(uuid.uuid4()), "ingest", op, payload, 1, 5, {})

        try:
            await pool.execute("INSERT INTO operators (id, name) VALUES ($1, 'Structured')", op)
            r = await run(job(
                [
                    {"kind": "data", "topic": "fare", "key": "Fare.Sài Gòn - Vũng Tàu.16 chỗ", "text": "Xe 16 chỗ SG–VT 180.000đ",
                     "facts": {"gia_ve": "180000"}},
                    {"kind": "policy", "topic": "pets", "key": "pets.dieu_kien", "text": "Không nhận thú cưng"},
                ],
                content="[ảnh bảng giá] 16 chỗ 180k; không nhận thú cưng",
            ))  # fmt: skip
            assert validate("operation_result", r) == []
            assert "(client)" in r["summary"] and r["counts"]["new"] == 2 and r["counts"]["auto_applied"] == 1
            keys = {x["key"] for x in await pool.fetch("SELECT key FROM items WHERE operator_id=$1", op)}
            assert keys == {"fare.sai_gon_vung_tau.16_cho", "pets.dieu_kien"}  # key chuẩn hoá như đường LLM
            doc = await pool.fetchrow("SELECT source, content FROM documents WHERE operator_id=$1", op)
            assert (doc["source"], doc["content"]) == (
                "image",
                "[ảnh bảng giá] 16 chỗ 180k; không nhận thú cưng",
            )

            # Mọi kênh nguồn trong contract đều ghi được vào documents.
            for src in ["call", "chat", "other", "excel"]:
                await run(job([{"kind": "lesson", "topic": "contact", "key": f"contact.{src}", "text": f"Ghi chú kênh {src}"}],
                              source=src))  # fmt: skip

            # Gửi lại đúng như cũ → trùng; topic lạ → lỗi rõ ràng để AI sửa.
            again = await run(
                job(
                    [
                        {
                            "kind": "policy",
                            "topic": "pets",
                            "key": "pets.dieu_kien",
                            "text": "Không nhận thú cưng",
                        }
                    ]
                )
            )
            assert again["counts"].get("duplicate") == 1
            with pytest.raises(PermanentError, match="karaoke"):
                await run(job([{"kind": "data", "topic": "karaoke", "key": "k", "text": "Có karaoke"}]))
        finally:
            await pool.execute("DELETE FROM audit_log WHERE payload->>'operator_id' = $1", op)
            await pool.execute("DELETE FROM operators WHERE id = $1", op)
            await pool.close()

    asyncio.run(t())


def test_diff_target_theo_khoang_hieu_luc():
    """Regression (review mục 5): đã có bản lên lịch cho tương lai, tin sửa giá HÔM NAY
    phải nhắm bản hiện tại làm target — không phải bản valid_from lớn nhất."""
    from datetime import date

    # Hai bản đúng khoảng hiệu lực: hiện tại (→01/11) và Tết (từ 01/11).
    cur = ExistingItem(
        "id-cur", "fare.sg_dl", "fare", "Giá 320k", {"gia_ve": "320000"}, date(2026, 1, 1), date(2026, 11, 1)
    )
    tet = ExistingItem(
        "id-tet", "fare.sg_dl", "fare", "Giá Tết 450k", {"gia_ve": "450000"}, date(2026, 11, 1), None
    )
    existing = {"fare.sg_dl": [cur, tet]}

    # Tin hôm nay (không nói ngày hiệu lực) → CHANGE nhắm bản hiện tại.
    d = diff(
        [cand("fare.sg_dl", "Giá 350k", facts={"gia_ve": "350000"})], existing, today=date(2026, 10, 15)
    )[0]
    assert d.change_kind == "CHANGE" and d.target.id == "id-cur", (d.change_kind, d.target.id)

    # Tin lên lịch cho Tết → nhắm bản Tết (khoảng chứa 01/11).
    d = diff(
        [cand("fare.sg_dl", "Giá Tết 500k", facts={"gia_ve": "500000"}, vf=date(2026, 11, 1))],
        existing,
        today=date(2026, 10, 15),
    )[0]
    assert d.target.id == "id-tet", d.target.id


@needs_db
def test_ingest_rollback_khi_enqueue_loi():
    """Regression (review mục 2): mọi side effect của ingest trong MỘT transaction — lỗi ở bước
    enqueue index.items phải rollback cả document + review, để retry xử lý lại (không kẹt
    ở 'tin đã ingest' với nửa pipeline)."""

    class Boom(Exception):
        pass

    class ProxyConn:
        """Forward mọi lời gọi; raise với đúng INSERT index.items."""

        def __init__(self, inner):
            self._inner = inner

        def __getattr__(self, name):
            return getattr(self._inner, name)

        async def execute(self, query, *args):
            if "index.items" in query:
                raise Boom("mô phỏng worker chết trước enqueue")
            return await self._inner.execute(query, *args)

    class ProxyPool:
        def __init__(self, inner):
            self._inner = inner

        def acquire(self):
            inner = self._inner

            class _Ctx:
                async def __aenter__(_s):  # noqa: N805
                    _s._cm = inner.acquire()
                    return ProxyConn(await _s._cm.__aenter__())

                async def __aexit__(_s, *exc):
                    return await _s._cm.__aexit__(*exc)

            return _Ctx()

        def __getattr__(self, name):
            return getattr(self._inner, name)

    async def t() -> None:
        import hashlib

        pool = await asyncpg.create_pool(DB_URL, init=init_connection)
        op = f"rb{uuid.uuid4().hex[:8]}"
        await pool.execute("INSERT INTO operators (id, name) VALUES ($1, 'RB')", op)
        content = "Thú cưng phải để trong lồng khi lên xe."
        try:
            payload = {"operator_id": op, "source": "zalo",
                       "received_at": "2026-10-01T09:00:00+07:00", "content": content}  # fmt: skip
            job = Job(str(uuid.uuid4()), "ingest", op, payload, 1, 5, {})
            scripted = [it("upsert", "policy", "pets", "pets.dieu_kien", "Thú cưng phải để trong lồng")]
            fake = ScriptedLLM([scripted, scripted])  # lần 1 (bị rollback) + lần 2 (thành công)
            llm = LLMClient(load(), providers={"gemini": fake})
            # Lần 1: enqueue lỗi → MỌI thứ phải rollback.
            try:
                await ingest_job.ingest(ProxyPool(pool), llm, job)
                raised = False
            except Boom:
                raised = True
            assert raised, "phải ném lỗi enqueue"
            h = hashlib.sha256(content.encode()).hexdigest()
            n_doc = await pool.fetchval(
                "SELECT count(*) FROM documents WHERE operator_id=$1 AND content_hash=$2", op, h
            )
            assert n_doc == 0, "document phải bị rollback theo enqueue"
            assert await pool.fetchval("SELECT count(*) FROM items WHERE operator_id=$1", op) == 0, (
                "item pending phải rollback"
            )
            # Lần 2 (không lỗi): ingest lại từ đầu thành công — không kẹt duplicate.
            r2 = await ingest_job.ingest(pool, llm, job)
            assert r2["status"] == "done" and r2["counts"]["auto_applied"] >= 1, r2
        finally:
            await pool.execute("DELETE FROM operators WHERE id = $1", op)
            await pool.close()

    asyncio.run(t())
