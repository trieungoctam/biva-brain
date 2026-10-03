"""Reference executor (M3, E3.5.1): chạy snapshot của bot như bot thật — chỉ cho test/sandbox UAT.

LLM đóng vai bot (system prompt ghép sẵn từ snapshot, không còn [[id]]), được gọi **tool tra tri thức
và data của Brain** như runtime thật sẽ làm (không bịa). Mỗi lượt chat:

1. LLM nhận system prompt (system_prompt + persona + flows + fallbacks + tool_spec) và câu khách.
2. LLM có thể gọi tool: ``query_data`` (giá/lịch/điểm đón hiệu lực theo ngày) — thực hiện bằng SQL
   trên items của nhà xe; kết quả đưa lại cho LLM trả lời cuối.
3. Trả về {reply, tools_called[]}.

Verdict theo test case (S3.5.2/S3.5.3): must_mention / must_not_say / must_call_tool.
Tool-call dùng vòng lặp LLM có cấu trúc đơn giản: lần 1 trả về {"tool": "query_data", "args": {...}}
hoặc {"reply": ...}; nếu có tool → gọi thật rồi gọi lần 2 với kết quả.
"""

from __future__ import annotations

import json
from typing import Any

import asyncpg

from biva_worker.llm import LLMClient
from biva_worker.llm.client import Request
from biva_worker.runner import PermanentError

SYSTEM_SUFFIX = (
    "\n\nBạn là bot chăm sóc khách hàng của nhà xe. Khi cần giá, lịch, điểm đón hay "
    "số liệu vận hành, hãy gọi tool query_data thay vì đoán. Trả lời ngắn, đúng knowledge đã cho."
)

TOOL_SCHEMA = {
    "type": "object",
    "additionalProperties": False,
    "required": ["reply"],
    "properties": {
        "reply": {"type": "string"},
        "tool": {"type": "string", "enum": ["query_data"]},
        "args": {"type": "object"},
    },
}


def build_system(definition: dict) -> str:
    arts = definition.get("artifacts", {})
    parts = []
    for kind in ("system_prompt", "persona", "flows", "fallbacks", "tool_spec"):
        a = arts.get(kind) or {}
        content = (a.get("content") or "").strip()
        if content:
            parts.append(f"# {kind}\n{content}")
    if not parts:
        raise PermanentError("snapshot không có artifact nào để chạy", code="EMPTY_SNAPSHOT")
    return "\n\n".join(parts) + SYSTEM_SUFFIX


async def query_data(pool: asyncpg.Pool, operator: str, args: dict) -> dict:
    """Tra data vận hành hiệu lực: topic (fare/schedule/pickup…), date (mặc định hôm nay), match."""
    topic = (args.get("topic") or "fare").strip()
    date = (args.get("date") or "").strip()
    match = (args.get("match") or "").strip()
    import datetime as _dt
    from zoneinfo import ZoneInfo

    # Ngày mặc định theo giờ Việt Nam (container chạy UTC: 00:00–06:59 VN còn là hôm qua);
    # cast ngày theo Asia/Ho_Chi_Minh (cast theo session UTC làm giá từ 00:00+07 lệch 1 ngày);
    # khoảng hiệu lực NỬA MỎ [valid_from, valid_to): ngày valid_to không còn thuộc bản cũ.
    tz = "Asia/Ho_Chi_Minh"
    as_of = _dt.date.fromisoformat(date) if date else _dt.datetime.now(ZoneInfo(tz)).date()
    # Giao KHOẢNG theo timestamp (không cast date): valid_to trong hệ thống có HAI dạng —
    # ingest ghi "D 23:59:59+07" (ngày cuối còn hiệu lực), apply_review ghi nửa đêm
    # (exclusive). Điều kiện valid_to > ĐẦU NGÀY (strict) đúng cho cả hai: 23:59:59 của ngày
    # D > D 00:00 (còn), midnight của ngày sau > D 00:00 (hết từ D).
    rows = await pool.fetch(
        """SELECT text FROM items
           WHERE operator_id = $1 AND status = 'active' AND kind = 'data' AND topic = $2
             AND (valid_from IS NULL OR valid_from < (($3::date + 1)::timestamp AT TIME ZONE $4))
             AND (valid_to IS NULL OR valid_to > ($3::date::timestamp AT TIME ZONE $4))
           ORDER BY updated_at DESC LIMIT 10""",
        operator,
        topic,
        as_of,
        tz,
    )
    if match:
        m = match.lower()
        rows = [r for r in rows if m in r["text"].lower()]
    return {"topic": topic, "date": date, "results": [r["text"] for r in rows]}


async def chat_once(
    pool: asyncpg.Pool, llm: LLMClient, definition: dict, message: str, operator_id: str | None = None
) -> dict[str, Any]:
    """Một lượt chat: trả {reply, tools_called:[{tool, args}]}."""
    system = build_system(definition)
    operator = definition.get("operator") or operator_id or ""
    tools: list[dict] = []
    context = ""
    for turn in range(2):  # tối đa 1 vòng tool
        user = message + ("\n\n[tool query_data trả về]\n" + context if context else "")
        system_full = system + (
            "\n\nBạn có thể gọi tool: trả JSON "
            '{"tool": "query_data", "args": {...}} để tra trước khi trả lời, '
            'hoặc {"reply": "..."} để trả lời trực tiếp.'
        )
        result = await llm.complete(
            Request(
                purpose="interactive",
                system=system_full,
                messages=[{"role": "user", "content": user}],
                schema=TOOL_SCHEMA,
                max_tokens=2000,
                operator_id=operator,
            )
        )
        data = result.data or {}
        if data.get("tool") == "query_data" and turn == 0:
            args = data.get("args") or {}
            tools.append({"tool": "query_data", "args": args})
            context = json.dumps(await query_data(pool, operator, args), ensure_ascii=False)[:2000]
            continue
        return {"reply": data.get("reply") or "", "tools_called": tools}
    return {"reply": "", "tools_called": tools}


def verdict(reply: str, tools_called: list[dict], expected: dict) -> tuple[bool, str]:
    """Đánh verdict theo kỳ vọng: must_mention / must_not_say / must_call_tool."""
    low = reply.lower()
    called = {t["tool"] for t in tools_called}
    for phrase in expected.get("must_mention", []):
        if str(phrase).lower() not in low:
            return False, f"thiếu '{phrase}' trong câu trả lời"
    for phrase in expected.get("must_not_say", []):
        if str(phrase).lower() in low:
            return False, f"không được nói '{phrase}'"
    for tool in expected.get("must_call_tool", []):
        if tool not in called:
            return False, f"phải gọi tool {tool} (đã gọi: {sorted(called) or 'không'})"
    return True, "ok"
