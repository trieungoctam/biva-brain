"""Job ``validate`` (M3, S3.3.1): kiểm mâu thuẫn artifact ⇔ tri thức bằng LLM, chạy nền.

Phần tĩnh (trích dẫn, locked, hardcode…) chạy đồng bộ ở brain-api; CONTRADICTION cần hiểu ngữ nghĩa
nên là job: sau khi validate_artifact tĩnh PASS, brain-api enqueue job này cho artifact đó.

LLM (purpose ``validate``, tier strong) nhận từng câu mang thông tin của artifact + các item tri thức
liên quan (được trích dẫn hoặc cùng topic); chỉ báo mâu thuẫn khi chắc chắn (confident) về khác biệt
fact: con số, giờ, cho/không cho, điều kiện áp dụng. Kết quả ghi vào ``bot_artifacts.validation``
(mã CONTRADICTION, dòng tìm từ câu gốc) và hạ ``valid → invalid`` — có audit; không có mâu thuẫn thì
chỉ ghi ``contradiction_checked_at``. Bộ test mâu thuẫn sống ở ``tests/test_validate_llm.py`` (live
với GEMINI key thật, skip trong CI).
"""

from __future__ import annotations

import json
from typing import Any

import asyncpg

from biva_worker.llm import LLMClient
from biva_worker.llm.client import Request
from biva_worker.runner import Job, PermanentError

ACTOR = "ai:validate"
MAX_ITEMS = 20

SYSTEM = """\
Bạn kiểm tra mâu thuẫn giữa câu trong artifact của bot chatbot nhà xe và tri thức đã duyệt của Brain.
CHỈ báo mâu thuẫn khi hai bên nói về CÙNG điều và KHÁC nhau về fact: con số (giá, phụ thu, kg), giờ,
cho phép / không cho phép, điều kiện áp dụng (tuổi, tuyến, ngày), thời hạn. Câu mang thông tin đúng
hoặc không liên quan tới item → không báo. Không chắc chắn → confident: false. Trả JSON theo schema."""

OUTPUT_SCHEMA = {
    "type": "object",
    "additionalProperties": False,
    "required": ["contradictions"],
    "properties": {
        "contradictions": {
            "type": "array",
            "items": {
                "type": "object",
                "additionalProperties": False,
                "required": ["quote", "item_id", "reason", "confident"],
                "properties": {
                    "quote": {"type": "string", "description": "câu trong artifact (nguyên văn)"},
                    "item_id": {"type": "string"},
                    "reason": {"type": "string", "description": "vì sao mâu thuẫn (fact nào, giá trị nào)"},
                    "confident": {"type": "boolean"},
                },
            },
        }
    },
}


def build_prompt(claims: list[str], items: list[dict]) -> str:
    parts = ["CÁC CÂU TRONG ARTIFACT:"]
    parts += [f"{i + 1}. {c}" for i, c in enumerate(claims)]
    parts.append("\nTRI THỨC ĐÃ DUYỆT (id | nội dung):")
    parts += [f"- {it['id']} | {it['text']}" for it in items]
    parts.append("\nTìm mâu thuẫn fact giữa câu artifact và tri thức. Không báo khi cùng ý/không liên quan.")
    return "\n".join(parts)


def informative_claims(content: str) -> list[str]:
    """Các câu mang thông tin (có số, hoặc ≥ 8 từ, không phải câu hỏi)."""
    out = []
    for line in content.splitlines():
        s = line.strip().lstrip("-#>* ").strip()
        if not s or s.endswith("?") or s.startswith("```"):
            continue
        if any(ch.isdigit() for ch in s) or len(s.split()) >= 8:
            out.append(s)
    return out[:30]


async def detect(
    llm: LLMClient,
    claims: list[str],
    items: list[dict],
    operator_id: str | None = None,
    operation_id: str | None = None,
) -> list[dict]:
    """Trả mâu thuẫn confident (quote, item_id, reason) — dùng chung cho job và live test."""
    if not claims or not items:
        return []
    result = await llm.complete(
        Request(
            purpose="validate",
            system=SYSTEM,
            messages=[{"role": "user", "content": build_prompt(claims, items)}],
            schema=OUTPUT_SCHEMA,
            max_tokens=4000,
            operator_id=operator_id,
            operation_id=operation_id,
        )
    )
    known = {it["id"] for it in items}
    out = []
    for c in result.data.get("contradictions", []):
        if c.get("confident") and c.get("item_id") in known and c.get("quote"):
            out.append({"quote": c["quote"], "item_id": c["item_id"], "reason": c["reason"]})
    return out


def find_line(content: str, quote: str) -> int:
    q = quote.strip().lower()
    for i, line in enumerate(content.splitlines(), start=1):
        if q and q[:40] in line.lower():
            return i
    return 1


async def validate(
    pool: asyncpg.Pool, llm: LLMClient, operator: str, artifact_id: str, operation_id: str | None = None
) -> dict[str, Any]:
    async with pool.acquire() as con:
        art = await con.fetchrow(
            """SELECT id::text, kind, version, content, status FROM bot_artifacts
               WHERE id = $1::uuid AND operator_id = $2 AND status IN ('valid', 'invalid')""",
            artifact_id,
            operator,
        )
        if art is None:
            raise PermanentError("artifact không tồn tại hoặc không ở trạng thái đã kiểm", code="NO_ARTIFACT")
        cited = await con.fetch(
            """SELECT DISTINCT i.id::text, i.text FROM artifact_citations c
               JOIN items i ON i.id = c.item_id
               WHERE c.artifact_id = $1::uuid AND i.status = 'active'""",
            artifact_id,
        )
        topics = [
            r["topic"]
            for r in await con.fetch(
                """SELECT DISTINCT i.topic FROM artifact_citations c
               JOIN items i ON i.id = c.item_id WHERE c.artifact_id = $1::uuid""",
                artifact_id,
            )
        ]
        by_topic = []
        if topics:
            by_topic = await con.fetch(
                """SELECT id::text, text FROM items
                   WHERE status = 'active' AND topic = ANY($2::text[])
                     AND (operator_id = $1 OR operator_id IS NULL) AND kind <> 'observation'
                   LIMIT $3""",
                operator,
                topics,
                MAX_ITEMS,
            )
        items = {r["id"]: {"id": r["id"], "text": r["text"]} for r in list(cited) + list(by_topic)}
        item_list = list(items.values())[:MAX_ITEMS]

    claims = informative_claims(art["content"])
    contradictions = await detect(llm, claims, item_list, operator, operation_id)

    async with pool.acquire() as con:
        async with con.transaction():
            if contradictions:
                val = await con.fetchval(
                    "SELECT validation::text FROM bot_artifacts WHERE id = $1::uuid", artifact_id
                )
                doc = json.loads(val) if val else {}
                errors = doc.get("errors") or []
                for c in contradictions:
                    errors.append(
                        {
                            "code": "CONTRADICTION",
                            "line": find_line(art["content"], c["quote"]),
                            "item": c["item_id"],
                            "message": c["reason"],
                            "quote": c["quote"][:120],
                        }
                    )
                doc["errors"] = errors
                doc["valid"] = False
                doc["contradiction_checked_at"] = True
                await con.execute(
                    """UPDATE bot_artifacts SET status = 'invalid', validation = $2::jsonb,
                           updated_at = now()
                       WHERE id = $1::uuid AND status = 'valid'""",
                    artifact_id,
                    doc,
                )
                await con.execute(
                    """INSERT INTO audit_log (actor, action, target, payload)
                       VALUES ($1, 'artifact.contradiction', $2, $3::jsonb)""",
                    ACTOR,
                    "artifact:" + artifact_id,
                    {"operator_id": operator, "contradictions": contradictions},
                )
            else:
                await con.execute(
                    """UPDATE bot_artifacts
                       SET validation = validation || '{"contradiction_checked_at": true}'::jsonb
                       WHERE id = $1::uuid""",
                    artifact_id,
                )
    return {
        "status": "done",
        "summary": f"{len(contradictions)} mâu thuẫn" if contradictions else "không có mâu thuẫn",
        "counts": {"contradictions": len(contradictions)},
        "contradictions": contradictions,
    }


def handler(pool: asyncpg.Pool, llm: LLMClient):
    async def run(job: Job) -> dict[str, Any]:
        operator = job.payload.get("operator_id") or job.operator_id
        artifact = job.payload.get("artifact_id")
        if not operator or not artifact:
            raise PermanentError("thiếu operator_id hoặc artifact_id", code="INVALID_PAYLOAD")
        return await validate(pool, llm, operator, artifact, operation_id=job.id)

    return run
