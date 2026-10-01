"""Trích item ứng viên từ nội dung nhà xe gửi bằng LLM (structured output) — S1.1.2."""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import date
from pathlib import Path
from typing import Any

from biva_worker import textnorm
from biva_worker.llm import LLMClient, Request, Result

OTHER_TOPIC = "other"

SYSTEM = (Path(__file__).resolve().parents[1] / "prompts" / "ingest_extract.md").read_text(encoding="utf-8")


def schema(topics: list[str]) -> dict[str, Any]:
    return {
        "type": "object",
        "additionalProperties": False,
        "required": ["items"],
        "properties": {
            "items": {
                "type": "array",
                "items": {
                    "type": "object",
                    "additionalProperties": False,
                    "required": ["action", "kind", "topic", "key", "text", "facts", "valid_from", "valid_to"],
                    "properties": {
                        "action": {"enum": ["upsert", "remove"]},
                        "kind": {"enum": ["data", "policy", "lesson", "persona"]},
                        "topic": {"enum": [*topics, OTHER_TOPIC]},
                        "key": {"type": "string", "minLength": 3},
                        "text": {"type": "string", "minLength": 5},
                        "facts": {
                            "type": "array",
                            "items": {
                                "type": "object",
                                "additionalProperties": False,
                                "required": ["name", "value"],
                                "properties": {"name": {"type": "string"}, "value": {"type": "string"}},
                            },
                        },
                        "valid_from": {"type": ["string", "null"], "format": "date"},
                        "valid_to": {"type": ["string", "null"], "format": "date"},
                    },
                },
            }
        },
    }


@dataclass(frozen=True)
class Candidate:
    action: str
    kind: str
    topic: str
    key: str
    text: str
    facts: dict[str, str] = field(default_factory=dict)
    valid_from: date | None = None
    valid_to: date | None = None


@dataclass(frozen=True)
class ExistingItem:
    id: str
    key: str
    topic: str
    text: str
    facts: dict[str, str]
    valid_from: date | None = None
    valid_to: date | None = None


def normalize_key(topic: str, key: str) -> str:
    """Key ổn định: các phần tách bằng "."; mỗi phần = token textnorm (không dấu, chữ thường) nối bằng "_".
    Luôn bắt đầu bằng topic. "fare.Sài Gòn - Đà Lạt" và "sai_gon-da_lat" cùng cho "fare.sai_gon_da_lat"."""
    parts = [seg for raw in key.split(".") if (seg := "_".join(textnorm.tokens(raw.replace("_", " "))))]
    if not parts or parts[0] != topic:
        parts.insert(0, topic)
    return ".".join(parts)


def _date(v: str | None) -> date | None:
    return date.fromisoformat(v) if v else None


def build_prompt(content: str, received: date, existing: list[ExistingItem], max_existing: int = 300) -> str:
    lines = [f"- {e.key}: {e.text}" for e in existing[:max_existing]]
    known = "\n".join(lines) if lines else "(chưa có)"
    weekday = ["thứ Hai", "thứ Ba", "thứ Tư", "thứ Năm", "thứ Sáu", "thứ Bảy", "Chủ nhật"][received.weekday()]
    return (
        f"Ngày nhận tin: {received.isoformat()} ({weekday}).\n\n"
        f"Key đã có của nhà xe:\n{known}\n\n"
        f"Tin nhắn:\n<<<\n{content}\n>>>"
    )


async def extract(
    llm: LLMClient,
    *,
    content: str,
    received: date,
    existing: list[ExistingItem],
    topics: list[str],
    operator_id: str,
    operation_id: str | None = None,
) -> tuple[list[Candidate], Result]:
    result = await llm.complete(
        Request(
            purpose="ingest",
            system=SYSTEM,
            messages=[{"role": "user", "content": build_prompt(content, received, existing)}],
            schema=schema(topics),
            max_tokens=16000,
            operator_id=operator_id,
            operation_id=operation_id,
        )
    )
    candidates = [
        Candidate(
            action=i["action"],
            kind=i["kind"],
            topic=i["topic"],
            key=normalize_key(i["topic"], i["key"]),
            text=i["text"].strip(),
            facts={f["name"].strip(): f["value"].strip() for f in i["facts"] if f["name"].strip()},
            valid_from=_date(i["valid_from"]),
            valid_to=_date(i["valid_to"]),
        )
        for i in result.data["items"]
    ]
    return candidates, result
