"""Job ``bot.reflect`` (M3, E3.6.1): reflect — agent phân tích tri thức cho builder.

LLM (purpose ``interactive``) nhận câu hỏi phân tích của builder + knowledge pack rút gọn
(policy/lesson/data của nhà xe + thông lệ L1) và **phải trích dẫn [[id]]** cho mỗi nhận định —
kiểm sau: mọi [[id]] phải là item active của đúng scope, câu nhận định thiếu trích dẫn bị đánh dấu.
Kết quả là markdown cho builder đọc; không ghi lại vào tri thức (ai đọc, người quyết định).

compare_with_industry (cùng file, không cần LLM): chỗ nhà xe khác thông lệ — quan sát ở E3.2
(observation của scope) so với thông lệ L1 cùng topic.
"""

from __future__ import annotations

import re
from typing import Any

import asyncpg

from biva_worker.llm import LLMClient
from biva_worker.llm.client import Request
from biva_worker.runner import Job, PermanentError

ACTOR = "ai:reflect"
MAX_ITEMS = 60
CITE_RE = re.compile(r"\[\[([0-9a-fA-F-]{36})\]\]")

SYSTEM = """\
Bạn là trợ lý phân tích tri thức của BIVA Brain cho builder. Trả lời câu hỏi phân tích dựa CHÍNH XÁC
vào tri thức được cung cấp; mỗi nhận định về nhà xe/ngành phải kèm trích dẫn [[<id>]] ngay sau nó.
Không có dữ liệu thì nói rõ chưa có — không đoán. Trả về markdown tiếng Việt, ngắn gọn, có mục."""


async def load_knowledge(pool: asyncpg.Pool, operator: str) -> str:
    rows = await pool.fetch(
        """SELECT id::text, layer, topic, text FROM items
           WHERE status = 'active' AND kind <> 'observation'
             AND ((operator_id = $1 AND layer = 2) OR (operator_id IS NULL AND layer <= 1))
           ORDER BY layer, topic LIMIT $2""",
        operator,
        MAX_ITEMS,
    )
    parts = ["TRI THỨC (id | tầng | topic | nội dung):"]
    parts += [f"- {r['id']} | L{r['layer']} | {r['topic']} | {r['text']}" for r in rows]
    return "\n".join(parts)


async def reflect(
    pool: asyncpg.Pool, llm: LLMClient, operator: str, question: str, operation_id: str | None = None
) -> dict[str, Any]:
    question = question.strip()
    if not question:
        raise PermanentError("thiếu câu hỏi phân tích", code="INVALID_PAYLOAD")
    knowledge = await load_knowledge(pool, operator)
    result = await llm.complete(
        Request(
            purpose="interactive",
            system=SYSTEM,
            messages=[
                {"role": "user", "content": f"NHÀ XE: {operator}\n\n{knowledge}\n\nCÂU HỎI: {question}"}
            ],
            max_tokens=4000,
            operator_id=operator,
            operation_id=operation_id,
        )
    )
    answer = (result.text or "").strip()
    # Kiểm trích dẫn: [[id]] phải là item active của scope; câu nhận định dài mà thiếu trích dẫn → cảnh báo.
    valid = {
        r["id"]
        for r in await pool.fetch(
            """SELECT id::text FROM items
           WHERE status = 'active'
             AND ((operator_id = $1 AND layer = 2) OR (operator_id IS NULL AND layer <= 1))""",
            operator,
        )
    }
    bad_cites = sorted({c for c in CITE_RE.findall(answer) if c not in valid})
    warnings = []
    if bad_cites:
        warnings.append("trích dẫn không hợp lệ: " + ", ".join(bad_cites))
    for line in answer.splitlines():
        s = line.strip().lstrip("-#>* ").strip()
        informative = len(s.split()) >= 8 and any(ch.isdigit() for ch in s)
        if informative and not s.endswith("?") and not CITE_RE.search(s):
            warnings.append(f'câu mang số liệu nhưng chưa có trích dẫn: "{s[:60]}…"')
            break
    return {
        "status": "done",
        "answer": answer,
        "warnings": warnings,
        "counts": {"citations": len(set(CITE_RE.findall(answer)))},
    }


async def compare_with_industry(pool: asyncpg.Pool, operator: str) -> dict[str, Any]:
    """Chỗ nhà xe khác thông lệ: observation của nhà xe vs observation L1 theo topic."""
    own = await pool.fetch(
        """SELECT topic, text FROM items
           WHERE operator_id = $1 AND kind = 'observation' AND layer = 2 AND status = 'active'""",
        operator,
    )
    defaults = {
        r["topic"]: r["text"]
        for r in await pool.fetch(
            """SELECT topic, text FROM items
           WHERE operator_id IS NULL AND kind = 'observation' AND layer = 1 AND status = 'active'"""
        )
    }
    rows = []
    for r in own:
        rows.append(
            {
                "topic": r["topic"],
                "operator": [line[2:] for line in r["text"].splitlines() if line.startswith("- ")],
                "industry": [line[2:] for line in defaults[r["topic"]].splitlines() if line.startswith("- ")]
                if r["topic"] in defaults
                else None,
            }
        )
    return {
        "status": "done",
        "summary": f"{len(rows)} topic có observation của nhà xe" if rows else "chưa có observation",
        "topics": rows,
    }


def handler(pool: asyncpg.Pool, llm: LLMClient):
    async def run(job: Job) -> dict[str, Any]:
        operator = job.payload.get("operator_id") or job.operator_id
        if job.kind == "bot.reflect":
            return await reflect(pool, llm, operator, job.payload.get("question") or "", operation_id=job.id)
        if job.kind == "bot.compare":
            return await compare_with_industry(pool, operator)
        raise PermanentError(f"kind lạ {job.kind}", code="INVALID_PAYLOAD")

    return run
