"""S3.5.2 sinh test từ tri thức + S3.5.3 job run_tests / sandbox_chat (dùng executor)."""

from __future__ import annotations

import json
from typing import Any

import asyncpg

from biva_worker import executor
from biva_worker.llm import LLMClient
from biva_worker.runner import Job, PermanentError

MAX_CASES_PER_RUN = 50

# ───────────────────────── S3.5.2: sinh test không cần LLM ─────────────────────────

# Topic data phải gọi tool (không ghi cứng số); topic policy → câu hỏi khách tự nhiên.
QUESTION_TEMPLATES = {
    "policy": "Nhà xe cho phép {what} không ạ?",
    "data": "Cho em hỏi {what} nhé?",
}


def cases_from_knowledge(rows: list[dict]) -> list[dict]:
    """Sinh test case từ item active: policy → must_mention từ khóa chính; data → must_call_tool."""
    out = []
    for r in rows:
        if r["kind"] == "data":
            expected = {"must_call_tool": ["query_data"]}
            name = f"data:{r['topic']}"
        else:
            # must_mention: 1–2 từ khoá đặc trưng của câu (từ ≥ 4 ký tự, không phải stopwords).
            words = [w for w in r["text"].split() if len(w) >= 4][:2]
            if not words:
                continue
            expected = {"must_mention": words}
            name = f"policy:{r['topic']}"
        out.append(
            {
                "layer": 2,
                "name": name,
                "input": r["text"],
                "expected": expected,
                "source_item_id": r["id"],
                "origin": "policy" if r["kind"] != "data" else "data",
            }
        )
    return out


async def generate_tests(pool: asyncpg.Pool, operator: str) -> dict[str, Any]:
    """Sinh test từ tri thức active của nhà xe (thay thế test cũ origin=generated)."""
    rows = await pool.fetch(
        """SELECT id::text, kind, topic, text FROM items
           WHERE operator_id = $1 AND status = 'active' AND kind IN ('policy', 'data')
           ORDER BY topic LIMIT $2""",
        operator,
        MAX_CASES_PER_RUN,
    )
    fresh = cases_from_knowledge([dict(r) for r in rows])
    async with pool.acquire() as con:
        async with con.transaction():
            retired = await con.fetchval(
                "UPDATE test_cases SET status = 'retired' WHERE operator_id = $1"
                " AND origin IN ('generated', 'policy', 'data') RETURNING id",
                operator,
            )
            existing = {
                r["source_item_id"]
                for r in await con.fetch(
                    """SELECT source_item_id::text FROM test_cases
                   WHERE operator_id = $1 AND status = 'active'""",
                    operator,
                )
            }
            added = 0
            for c in fresh:
                if c["source_item_id"] in existing:
                    continue
                await con.execute(
                    """INSERT INTO test_cases (operator_id, layer, name, input, expected,
                           source_item_id, origin)
                       VALUES ($1, $2, $3, $4, $5::jsonb, $6::uuid, $7)""",
                    operator,
                    c["layer"],
                    c["name"],
                    c["input"],
                    c["expected"],
                    c["source_item_id"],
                    c["origin"],
                )
                added += 1
    return {
        "status": "done",
        "summary": f"sinh {added} test case mới (thay {retired} bản cũ)",
        "counts": {"generated": added, "retired": retired},
    }


# ───────────────────────── S3.5.3: run_tests / sandbox_chat ─────────────────────────


async def load_definition(pool: asyncpg.Pool, operator: str, channel: str) -> tuple[dict | None, str | None]:
    """Trả (definition, snapshot_id) của snapshot MỚI NHẤT — run phải gắn với đúng bản này."""
    row = await pool.fetchrow(
        """SELECT definition::text, s.id::text AS sid FROM snapshots s
           JOIN bots b ON b.id = s.bot_id AND b.operator_id = $1 AND b.channel = $2
           ORDER BY s.version DESC LIMIT 1""",
        operator,
        channel or "zalo",
    )
    return (json.loads(row["definition"]), row["sid"]) if row else (None, None)


async def run_tests(pool: asyncpg.Pool, llm: LLMClient, operator: str, channel: str) -> dict[str, Any]:
    definition, snapshot_id = await load_definition(pool, operator, channel)
    if definition is None:
        raise PermanentError(
            f"chưa có snapshot cho bot {operator}/{channel or 'zalo'} — export_bot trước", code="NO_SNAPSHOT"
        )
    await generate_tests(pool, operator)
    cases = await pool.fetch(
        """SELECT id::text, name, input, expected::text FROM test_cases
           WHERE operator_id = $1 AND status = 'active' ORDER BY layer, name LIMIT $2""",
        operator,
        MAX_CASES_PER_RUN,
    )
    report = []
    passed = 0
    for c in cases:
        try:
            got = await executor.chat_once(pool, llm, definition, c["input"], operator)
            ok, reason = executor.verdict(got["reply"], got["tools_called"], json.loads(c["expected"]))
        except Exception as exc:  # noqa: BLE001 — lỗi 1 case không dừng cả bộ
            got, ok, reason = {"reply": "", "tools_called": []}, False, f"executor lỗi: {exc}"
        passed += ok
        report.append(
            {
                "case": c["name"],
                "input": c["input"],
                "expected": json.loads(c["expected"]),
                "got": got["reply"][:300],
                "tools_called": got["tools_called"],
                "pass": ok,
                "reason": reason,
            }
        )
    rate = round(passed / len(cases), 3) if cases else 0.0
    run_id = await pool.fetchval(
        """INSERT INTO test_runs (operator_id, bot_channel, snapshot_id, total, passed, report)
           VALUES ($1, $2, $3::uuid, $4, $5, $6::jsonb) RETURNING id::text""",
        operator,
        channel or "zalo",
        snapshot_id,
        len(cases),
        passed,
        report,
    )
    fails = [r for r in report if not r["pass"]]
    return {
        "status": "done",
        "summary": f"{passed}/{len(cases)} pass ({rate:.0%})",
        "run_id": run_id,
        "counts": {"total": len(cases), "passed": passed, "failed": len(fails)},
        "failed_cases": fails[:10],
    }


async def sandbox_chat(
    pool: asyncpg.Pool, llm: LLMClient, operator: str, message: str, channel: str
) -> dict[str, Any]:
    definition, snapshot_id = await load_definition(pool, operator, channel)
    if definition is None:
        raise PermanentError(
            f"chưa có snapshot cho bot {operator}/{channel or 'zalo'} — export_bot trước", code="NO_SNAPSHOT"
        )
    got = await executor.chat_once(pool, llm, definition, message, operator)
    return {"status": "done", "reply": got["reply"], "tools_called": got["tools_called"]}


def handler(pool: asyncpg.Pool, llm: LLMClient):
    async def run(job: Job) -> dict[str, Any]:
        operator = job.payload.get("operator_id") or job.operator_id
        channel = job.payload.get("channel") or "zalo"
        if job.kind == "bot.tests":
            return await run_tests(pool, llm, operator, channel)
        if job.kind == "bot.chat":
            message = (job.payload.get("message") or "").strip()
            if not message:
                raise PermanentError("thiếu message", code="INVALID_PAYLOAD")
            return await sandbox_chat(pool, llm, operator, message, channel)
        raise PermanentError(f"kind lạ {job.kind}", code="INVALID_PAYLOAD")

    return run
