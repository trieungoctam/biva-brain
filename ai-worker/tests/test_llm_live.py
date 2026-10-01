"""Gọi Gemini thật qua thư viện llm/ — chỉ chạy khi có BIVA_TEST_GEMINI_API_KEY (tốn vài cent mỗi lần).

BIVA_TEST_GEMINI_API_KEY=... uv run pytest -q tests/test_llm_live.py
"""

from __future__ import annotations

import asyncio
import os

import pytest

from biva_worker.llm import LLMClient, Request, load

KEY = os.environ.get("BIVA_TEST_GEMINI_API_KEY")
pytestmark = pytest.mark.skipif(not KEY, reason="đặt BIVA_TEST_GEMINI_API_KEY để gọi Gemini thật")

SCHEMA = {
    "type": "object",
    "additionalProperties": False,
    "required": ["items"],
    "properties": {
        "items": {
            "type": "array",
            "items": {
                "type": "object",
                "additionalProperties": False,
                "required": ["topic", "text", "price_vnd"],
                "properties": {
                    "topic": {"type": "string", "enum": ["fare", "schedule", "pets", "children", "other"]},
                    "text": {"type": "string", "minLength": 5},
                    "price_vnd": {"type": ["integer", "null"]},
                },
            },
        }
    },
}
MESSAGE = (
    "Từ 1/11 giá vé Sài Gòn - Đà Lạt xe giường nằm lên 320.000đ, chuyến 22h30 đổi sang 23h. "
    "Thú cưng phải để trong lồng, phụ thu 50k."
)


@pytest.mark.parametrize("purpose", ["ingest", "validate"])
def test_live_structured_extraction(purpose, monkeypatch):
    monkeypatch.setenv("GEMINI_API_KEY", KEY)

    async def t():
        return await LLMClient(load()).complete(
            Request(
                purpose=purpose,
                system="Trích xuất tri thức nhà xe từ tin nhắn thành item ngắn, giữ nguyên tiếng Việt.",
                messages=[{"role": "user", "content": MESSAGE}],
                schema=SCHEMA,
                max_tokens=4000,
            )
        )

    r = asyncio.run(t())
    assert r.attempts[-1].ok and r.usage.output_tokens > 0 and r.cost_usd > 0
    prices = {i["topic"]: i["price_vnd"] for i in r.data["items"]}
    assert prices.get("fare") == 320000
    assert prices.get("pets") == 50000
    assert any("23" in i["text"] for i in r.data["items"] if i["topic"] == "schedule")
