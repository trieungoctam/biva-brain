"""Job ``ingest``: nội dung nhà xe → item ứng viên → diff → review_items → tự apply phần rủi ro thấp.

Hai đầu vào, chung một pipeline từ bước 4:
- ``content`` (raw: tin Zalo, ghi chú, bảng dán từ Excel…) → ai-worker trích item bằng LLM.
- ``items`` (AI phía builder — ChatGPT, coding agent — đã đọc file/tin và trích sẵn) → không gọi LLM;
  ``content`` khi đó (nếu có) chỉ là trích đoạn nguồn làm bằng chứng.

Luồng (docs/architecture.md §3.1):
1. Kiểm payload (contracts jobs.ingest); cùng nội dung đã ingest (content_hash) → bỏ qua.
2. Đọc item active của nhà xe (key, text, facts) làm ngữ cảnh để LLM dùng lại key.
3. Item ứng viên: từ ``items`` của payload, hoặc LLM trích (purpose ingest, structured output).
4. Diff theo key → NEW / CHANGE / REMOVE / DUPLICATE / CONFLICT.
5. MỘT transaction: lưu document, item pending, review_items. Gọi LLM xong mới ghi → job retry không để lại
   document "đã thấy" mà chưa có item.
6. Review rủi ro thấp → apply_review(…, 'system:ingest');
   rủi ro cao (giá, giờ, huỷ, xoá, mâu thuẫn) chờ người duyệt.
7. Enqueue index.items cho nhà xe.
"""

from __future__ import annotations

import hashlib
import json
import os
from datetime import date, datetime, time
from typing import Any
from zoneinfo import ZoneInfo

import asyncpg

from biva_worker import kbtemplate
from biva_worker.contracts import validate
from biva_worker.ingest.diff import Decision, diff
from biva_worker.ingest.extract import OTHER_TOPIC, Candidate, ExistingItem, extract, normalize_key
from biva_worker.llm import LLMClient
from biva_worker.runner import Job, PermanentError

VN = ZoneInfo("Asia/Ho_Chi_Minh")
ACTOR = "system:ingest"


def auto_apply_enabled() -> bool:
    """BIVA_INGEST_AUTO_APPLY=false: không tự áp dụng gì, mọi đề xuất (kể cả rủi ro thấp) chờ người duyệt.
    Rủi ro cao không bao giờ tự áp dụng, bất kể cấu hình."""
    return os.environ.get("BIVA_INGEST_AUTO_APPLY", "true").strip().lower() not in ("0", "false", "no", "off")


def content_hash(content: str) -> str:
    return hashlib.sha256(content.strip().encode()).hexdigest()


def payload_hash(payload: dict[str, Any]) -> str:
    """Raw: hash nội dung. Có items: hash cả items lẫn trích đoạn (trích khác → lần ingest khác)."""
    if "items" not in payload:
        return content_hash(payload["content"])
    blob = json.dumps(
        {"content": payload.get("content", ""), "items": payload["items"]}, sort_keys=True, ensure_ascii=False
    )
    return hashlib.sha256(blob.encode()).hexdigest()


def candidates_from_items(items: list[dict[str, Any]], topics: list[str]) -> list[Candidate]:
    """Item do AI client gửi → Candidate (cùng chuẩn hoá key như đường LLM)."""
    unknown = sorted({i["topic"] for i in items if i["topic"] not in topics and i["topic"] != OTHER_TOPIC})
    if unknown:
        raise PermanentError(
            f"topic không có trong template: {', '.join(unknown)} (dùng: {', '.join(topics)}, other)",
            code="INVALID_TOPIC",
        )
    return [
        Candidate(
            action=i.get("action", "upsert"),
            kind=i["kind"],
            topic=i["topic"],
            key=normalize_key(i["topic"], i["key"]),
            text=i["text"].strip(),
            facts={k.strip(): v.strip() for k, v in (i.get("facts") or {}).items() if k.strip()},
            valid_from=date.fromisoformat(i["valid_from"]) if i.get("valid_from") else None,
            valid_to=date.fromisoformat(i["valid_to"]) if i.get("valid_to") else None,
        )
        for i in items
    ]


def _day_start(d: date | None) -> datetime | None:
    return datetime.combine(d, time.min, tzinfo=VN) if d else None


def _day_end(d: date | None) -> datetime | None:
    # valid_to là ngày cuối còn hiệu lực → hết hiệu lực từ 0h ngày hôm sau.
    return datetime.combine(d, time.max, tzinfo=VN) if d else None


def _facts(value: Any) -> dict[str, str]:
    if isinstance(value, dict) and isinstance(value.get("facts"), dict):
        return {str(k): str(v) for k, v in value["facts"].items()}
    return {}


def _snapshot(e: ExistingItem | None) -> dict[str, Any] | None:
    if e is None:
        return None
    return {
        "id": e.id,
        "text": e.text,
        "facts": e.facts,
        "valid_from": e.valid_from.isoformat() if e.valid_from else None,
        "valid_to": e.valid_to.isoformat() if e.valid_to else None,
    }


def _proposal(c: Candidate) -> dict[str, Any]:
    return {
        "action": c.action,
        "kind": c.kind,
        "text": c.text,
        "facts": c.facts,
        "valid_from": c.valid_from.isoformat() if c.valid_from else None,
        "valid_to": c.valid_to.isoformat() if c.valid_to else None,
    }


async def load_existing(conn: asyncpg.Connection, operator_id: str) -> dict[str, ExistingItem]:
    """Bản active mới nhất theo key (một key có thể có bản hiện tại + bản có hiệu lực từ ngày tương lai)."""
    rows = await conn.fetch(
        """SELECT DISTINCT ON (key) id::text, key, topic, text, value, valid_from, valid_to FROM items
           WHERE operator_id = $1 AND layer = 2 AND status = 'active' AND key IS NOT NULL
           ORDER BY key, valid_from DESC NULLS LAST""",
        operator_id,
    )
    return {
        r["key"]: ExistingItem(
            id=r["id"],
            key=r["key"],
            topic=r["topic"],
            text=r["text"],
            facts=_facts(r["value"]),
            valid_from=r["valid_from"].astimezone(VN).date() if r["valid_from"] else None,
            valid_to=r["valid_to"].astimezone(VN).date() if r["valid_to"] else None,
        )
        for r in rows
    }


async def _write(
    conn: asyncpg.Connection, payload: dict[str, Any], job: Job, h: str, decisions: list[Decision]
) -> tuple[str | None, list[tuple[str, str]]]:
    """Ghi document + item pending + review_items.

    Trả (document_id, [(review_id, risk)]); document_id = None nếu tin vừa bị job khác ingest.
    """
    received = datetime.fromisoformat(payload["received_at"])
    async with conn.transaction():
        doc_id = await conn.fetchval(
            """INSERT INTO documents (layer, operator_id, source, content, content_hash, submitted_by,
                                     received_at, metadata)
               VALUES (2, $1, $2, $3, $4, $5, $6, $7)
               ON CONFLICT (COALESCE(operator_id, ''), layer, content_hash) DO NOTHING
               RETURNING id::text""",
            payload["operator_id"],
            payload["source"],
            payload.get("content") or json.dumps(payload["items"], ensure_ascii=False, indent=1),
            h,
            payload.get("submitted_by"),
            received,
            payload.get("metadata") or {},
        )
        if doc_id is None:  # job khác vừa ingest đúng tin này
            return None, []
        reviews: list[tuple[str, str]] = []
        for d in decisions:
            item_id = None
            c = d.candidate
            if d.change_kind in ("NEW", "CHANGE", "CONFLICT"):
                assert c is not None
                item_id = await conn.fetchval(
                    """INSERT INTO items (layer, operator_id, kind, topic, key, text, value, status,
                                          valid_from, valid_to, mentioned_at, document_id, metadata)
                       VALUES (2, $1, $2, $3, $4, $5, $6, 'pending', $7, $8, $9, $10::uuid, $11)
                       RETURNING id::text""",
                    payload["operator_id"],
                    c.kind,
                    c.topic,
                    c.key,
                    c.text,
                    {"facts": c.facts} if c.facts else None,
                    _day_start(c.valid_from),
                    _day_end(c.valid_to),
                    received,
                    doc_id,
                    {"source": payload["source"], "extracted_by": "client" if "items" in payload else "llm"},
                )
            review_id = await conn.fetchval(
                """INSERT INTO review_items (operator_id, key, topic, change_kind, risk, item_id,
                                             target_item_id, before, after, reason, document_id,
                                             operation_id, proposed_by)
                   VALUES ($1, $2, $3, $4, $5, $6::uuid, $7::uuid, $8, $9, $10, $11::uuid, $12::uuid, $13)
                   RETURNING id::text""",
                payload["operator_id"],
                d.key,
                d.topic,
                d.change_kind,
                d.risk,
                item_id,
                d.target.id if d.target else None,
                _snapshot(d.target),
                _proposal(c) if c else None,
                d.reason or None,
                doc_id,
                job.id,
                ACTOR,
            )
            reviews.append((review_id, d.risk))
    return doc_id, reviews


async def ingest(pool: asyncpg.Pool, llm: LLMClient, job: Job) -> dict[str, Any]:
    payload = dict(job.payload)
    if job.operator_id and "operator_id" not in payload:
        payload["operator_id"] = job.operator_id
    errors = validate("jobs.ingest", payload)
    if errors:
        raise PermanentError("; ".join(errors), code="INVALID_PAYLOAD")
    if "file_ref" in payload:
        raise PermanentError(
            "ingest file chưa hỗ trợ: AI phía builder đọc file rồi gửi items (submit_knowledge) hoặc text",
            code="UNSUPPORTED",
        )
    operator_id = payload["operator_id"]
    h = payload_hash(payload)

    async with pool.acquire() as conn:
        if not await conn.fetchval("SELECT EXISTS (SELECT 1 FROM operators WHERE id = $1)", operator_id):
            raise PermanentError(f"không có nhà xe {operator_id}", code="UNKNOWN_OPERATOR")
        dup = await conn.fetchval(
            "SELECT id::text FROM documents WHERE operator_id = $1 AND layer = 2 AND content_hash = $2",
            operator_id,
            h,
        )
        if dup:
            return {
                "status": "done",
                "summary": "tin đã ingest trước đó",
                "counts": {"duplicate_document": 1},
                "refs": [{"type": "document", "id": dup}],
            }
        existing = await load_existing(conn, operator_id)

    received = datetime.fromisoformat(payload["received_at"]).astimezone(VN).date()
    if "items" in payload:
        candidates = candidates_from_items(payload["items"], kbtemplate.topics())
        extracted_by = "client"
    else:
        candidates, llm_result = await extract(
            llm,
            content=payload["content"],
            received=received,
            existing=list(existing.values()),
            topics=kbtemplate.topics(),
            operator_id=operator_id,
            operation_id=job.id,
        )
        extracted_by = llm_result.served_model
    decisions = diff(candidates, existing, today=received)

    async with pool.acquire() as conn:
        doc_id, reviews = await _write(conn, payload, job, h, decisions)
    if doc_id is None:
        return {"status": "done", "summary": "tin đã ingest trước đó", "counts": {"duplicate_document": 1}}

    applied, stale = 0, 0
    auto = auto_apply_enabled()
    async with pool.acquire() as conn:
        for review_id, risk in reviews:
            if risk != "low" or not auto:
                continue
            async with conn.transaction():
                res = json.loads(
                    await conn.fetchval("SELECT apply_review($1::uuid, $2)::text", review_id, ACTOR)
                )
            applied += res["status"] == "applied"
            stale += res["status"] == "stale"
        await conn.execute(
            "INSERT INTO operations (kind, operator_id, payload) VALUES ('index.items', $1, $2)",
            operator_id,
            {"operator_id": operator_id},
        )

    counts: dict[str, int] = {"candidates": len(candidates), "auto_applied": applied, "stale": stale}
    for d in decisions:
        counts[d.change_kind.lower()] = counts.get(d.change_kind.lower(), 0) + 1
    waiting = len(reviews) - applied - stale
    counts["waiting_review"] = waiting
    summary = f"{len(candidates)} item ứng viên ({extracted_by}): tự áp dụng {applied}, chờ duyệt {waiting}"
    next_actions = ["list_review_queue"] if waiting else []
    return {
        "status": "done",
        "summary": summary,
        "counts": counts,
        "refs": [{"type": "document", "id": doc_id}] + [{"type": "review_item", "id": r} for r, _ in reviews],
        "next_actions": next_actions,
    }


def handler(pool: asyncpg.Pool, llm: LLMClient):
    async def run(job: Job) -> dict[str, Any]:
        return await ingest(pool, llm, job)

    return run
