"""Thư viện llm/: kế hoạch gọi, fallback, kiểm output, chi phí, quota.

Phần cuối chạy SDK google-genai thật với một server giả lập Gemini API (không tốn tiền, không cần key).
"""

from __future__ import annotations

import asyncio
import json
import os
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Any

import pytest
from google import genai
from google.genai import types as genai_types

from biva_worker.contracts import contracts_root
from biva_worker.llm import LLMClient, LLMRequestError, LLMUnavailable, Request, api_schema, load
from biva_worker.llm.client import GeminiProvider, RawResponse, Usage, UsageRecord, _AttemptFailed
from biva_worker.llm.config import ConfigError, Step, parse
from biva_worker.llm.quota import Decision

CFG = load()
CFG_DIR = contracts_root() / "llm"

ITEM_SCHEMA = {
    "type": "object",
    "additionalProperties": False,
    "required": ["topic", "text"],
    "properties": {
        "topic": {"type": "string", "minLength": 2},
        "text": {"type": "string"},
        "price_vnd": {"type": "integer", "minimum": 0},
    },
}


def run(coro):
    return asyncio.run(coro)


# ─────────────────────────────── config ───────────────────────────────


def test_config_plan_matches_contract():
    """contracts/llm/plan_cases.json: Go (khi có) phải cho cùng thứ tự."""
    with open(CFG_DIR / "plan_cases.json", encoding="utf-8") as f:
        cases = json.load(f)
    for purpose, expected in cases.items():
        assert [[s.model.name, s.describe()] for s in CFG.plan(purpose)] == expected, purpose


def test_config_rejects_bad_config():
    raw = {
        "version": 1,
        "providers": {"g": {"kind": "gemini", "api_key_env": "X"}},
        "models": {"m": {"provider": "g", "price": {"input": 1, "output": 1}}},
        "tiers": {"small": ["m"], "strong": ["khong-co"]},
        "purposes": {
            p: {"tier": "small", "tokens_per_minute": 1, "requests_per_minute": 1}
            for p in ["ingest", "knowledge", "validate", "test", "interactive"]
        },
    }
    with pytest.raises(ConfigError, match="khong-co"):
        parse(raw)
    raw["tiers"]["strong"] = ["m"]
    parse(raw)
    raw["models"]["m"].update(thinking_level="low", thinking_budget=100)  # chỉ được chọn một
    with pytest.raises(ConfigError, match="không hợp lệ"):
        parse(raw)
    raw["models"]["m"].pop("thinking_budget")
    raw["providers"]["g"]["kind"] = "openai"
    with pytest.raises(ConfigError, match="không hợp lệ"):
        parse(raw)


def test_price_cost():
    price = CFG.models["gemini-3.5-flash"].price
    assert price.cost(1_000_000, 0) == pytest.approx(1.5)
    assert price.cost(0, 1_000_000) == pytest.approx(9.0)
    assert price.cost(0, 0, 1_000_000) == pytest.approx(0.15)
    lite = CFG.models["gemini-3.1-flash-lite"].price  # không khai giá cache → tính bằng giá input
    assert lite.cost(0, 0, 1_000_000) == pytest.approx(0.25)


def test_api_schema_strips_unsupported_constraints():
    sent = api_schema(ITEM_SCHEMA)
    assert "minLength" not in sent["properties"]["topic"]
    assert sent["properties"]["price_vnd"]["minimum"] == 0  # Gemini hỗ trợ minimum/maximum
    assert sent["additionalProperties"] is False
    assert ITEM_SCHEMA["properties"]["topic"]["minLength"] == 2  # bản gốc không bị sửa


# ─────────────────────────────── fallback (provider giả) ───────────────────────────────


class FakeProvider:
    """Mỗi lượt gọi lấy một phản hồi theo kịch bản: RawResponse hoặc _AttemptFailed."""

    def __init__(self, script: dict[str, list[Any]]) -> None:
        self.script = {k: list(v) for k, v in script.items()}
        self.calls: list[tuple[str, str, Any]] = []

    async def complete(self, step: Step, req: Request, schema: dict[str, Any] | None) -> RawResponse:
        self.calls.append((step.model.name, step.describe(), schema))
        out = self.script[step.model.name].pop(0)
        if isinstance(out, Exception):
            raise out
        return out


def ok(text: str, model: str, stop: str = "end_turn", usage: Usage | None = None) -> RawResponse:
    return RawResponse(text=text, stop_reason=stop, served_model=model, usage=usage or Usage(100, 20))


class Sink:
    def __init__(self) -> None:
        self.records: list[UsageRecord] = []

    async def __call__(self, r: UsageRecord) -> None:
        self.records.append(r)


def client(script, **kw):
    fake = FakeProvider(script)
    sink = Sink()
    return LLMClient(CFG, providers={"gemini": fake}, usage_sink=sink, **kw), fake, sink


def req(**kw) -> Request:
    base = {
        "purpose": "ingest",
        "messages": [{"role": "user", "content": "Giá vé Sài Gòn – Đà Lạt 300.000đ"}],
    }
    return Request(**{**base, **kw})


FLASH, LITE, PRO, PRO_GA = (
    "gemini-3.5-flash",
    "gemini-3.1-flash-lite",
    "gemini-3.1-pro-preview",
    "gemini-2.5-pro",
)


def test_primary_ok_records_usage_with_operator():
    c, fake, sink = client({FLASH: [ok('{"topic": "gia_ve", "text": "300k"}', FLASH)]})
    r = run(c.complete(req(schema=ITEM_SCHEMA, operator_id="phuongnam", operation_id="op-1")))
    assert r.data == {"topic": "gia_ve", "text": "300k"}
    assert r.model == FLASH and len(r.attempts) == 1
    assert fake.calls[0][1] == "level:low"
    assert "minLength" not in json.dumps(fake.calls[0][2])  # schema gửi đi đã gỡ ràng buộc
    (rec,) = sink.records
    assert (rec.operator_id, rec.operation_id, rec.purpose, rec.ok) == ("phuongnam", "op-1", "ingest", True)
    assert rec.cost_usd == pytest.approx((100 * 1.5 + 20 * 9.0) / 1e6)


@pytest.mark.parametrize("code", ["RATE_LIMIT", "SERVER", "TIMEOUT", "CONNECTION"])
def test_fallback_on_transient_error(code):
    """AC S1.1.1: model/provider chính lỗi → tự chuyển."""
    c, fake, sink = client({FLASH: [_AttemptFailed(code, True, "lỗi")], LITE: [ok("xin chào", LITE)]})
    r = run(c.complete(req(operator_id="phuongnam")))
    assert r.model == LITE and r.text == "xin chào"
    assert [x[0] for x in fake.calls] == [FLASH, LITE]
    assert [(x.model, x.ok, x.error_code) for x in sink.records] == [(FLASH, False, code), (LITE, True, None)]
    assert all(x.operator_id == "phuongnam" for x in sink.records)


def test_fallback_on_block_and_invalid_output():
    c, _, sink = client(
        {
            FLASH: [ok("", FLASH, stop="refusal")],
            LITE: [
                ok('{"topic": "x", "text": "y"}', LITE)
            ],  # topic quá ngắn (minLength chỉ kiểm phía client)
        }
    )
    with pytest.raises(LLMUnavailable) as e:
        run(c.complete(req(schema=ITEM_SCHEMA)))
    assert [a.error_code for a in e.value.attempts] == ["REFUSAL", "INVALID_OUTPUT"]
    assert len(sink.records) == 2  # lượt lỗi vẫn ghi usage (đã tốn token)


def test_bad_request_everywhere_is_permanent():
    c, _, _ = client(
        {
            FLASH: [_AttemptFailed("BAD_REQUEST", False, "400")],
            LITE: [_AttemptFailed("BAD_REQUEST", False, "400")],
        }
    )
    with pytest.raises(LLMRequestError):
        run(c.complete(req()))


def test_auth_error_skips_rest_of_provider():
    c, fake, _ = client({PRO: [_AttemptFailed("AUTH", False, "API key sai")]})
    with pytest.raises(LLMRequestError):
        run(c.complete(req(purpose="validate")))
    assert [x[0] for x in fake.calls] == [PRO]  # model cùng provider không bị gọi


def test_quota_denied_skips_and_reports_retry_after():
    class Deny:
        async def acquire(self, provider, purpose, tpm, rpm):
            return Decision(False, 12.5)

        async def debit(self, *a):
            raise AssertionError("không gọi model thì không trừ token")

    c, fake, _ = client({}, quota=Deny())
    with pytest.raises(LLMUnavailable) as e:
        run(c.complete(req()))
    assert fake.calls == [] and e.value.retry_after_s == 12.5


def test_unknown_purpose():
    c, _, _ = client({})
    with pytest.raises(ConfigError):
        run(c.complete(req(purpose="chat")))


# ─────────────────────────────── SDK google-genai thật + server giả lập ───────────────────────────────


class FakeGeminiAPI:
    """HTTP server trả lời …/models/{model}:generateContent theo kịch bản; ghi lại request nhận được."""

    def __init__(self, script: list[tuple[int, dict[str, Any]]]) -> None:
        self.script = list(script)
        self.requests: list[tuple[str, dict[str, str], dict[str, Any]]] = []
        outer = self

        class H(BaseHTTPRequestHandler):
            def do_POST(self):  # noqa: N802
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                outer.requests.append((self.path, {k.lower(): v for k, v in self.headers.items()}, body))
                status, payload = outer.script.pop(0)
                data = json.dumps(payload).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def log_message(self, *a):
                pass

        self.server = HTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.url = f"http://127.0.0.1:{self.server.server_port}"

    def close(self):
        self.server.shutdown()


def response(model: str, text: str, finish: str = "STOP") -> dict[str, Any]:
    return {
        "candidates": [{"content": {"role": "model", "parts": [{"text": text}]}, "finishReason": finish}],
        "usageMetadata": {
            "promptTokenCount": 80,
            "cachedContentTokenCount": 30,
            "candidatesTokenCount": 10,
            "thoughtsTokenCount": 5,
            "totalTokenCount": 95,
        },
        "modelVersion": model,
    }


def error(code: int, status: str, msg: str) -> dict[str, Any]:
    return {"error": {"code": code, "status": status, "message": msg}}


@pytest.fixture
def api():
    servers = []

    def make(script):
        s = FakeGeminiAPI(script)
        servers.append(s)
        return s

    yield make
    for s in servers:
        s.close()


def sdk_client(api_server) -> LLMClient:
    cfg = CFG.providers["gemini"]
    sdk = genai.Client(
        api_key="test-key",
        http_options=genai_types.HttpOptions(
            base_url=api_server.url, timeout=5000, retry_options=genai_types.HttpRetryOptions(attempts=1)
        ),
    )
    return LLMClient(CFG, providers={"gemini": GeminiProvider(cfg, client=sdk)})


def test_sdk_request_shape_and_fallback_on_429(api):
    server = api(
        [
            (429, error(429, "RESOURCE_EXHAUSTED", "quá hạn mức")),
            (200, response(LITE, '{"topic": "gia_ve", "text": "300k"}')),
        ]
    )
    r = run(sdk_client(server).complete(req(schema=ITEM_SCHEMA, system="Bạn trích xuất tri thức.")))
    assert r.data["topic"] == "gia_ve" and r.model == LITE
    assert r.attempts[0].error_code == "RATE_LIMIT"
    # promptTokenCount gồm cả phần cache; output gồm cả token suy luận.
    assert r.usage == Usage(input_tokens=50, output_tokens=15, cache_read_tokens=30)

    (p1, h1, b1), (p2, _, b2) = server.requests
    assert p1.endswith(f"/models/{FLASH}:generateContent") and p2.endswith(f"/models/{LITE}:generateContent")
    assert h1.get("x-goog-api-key") == "test-key"
    gc1 = b1["generationConfig"]
    assert gc1["responseMimeType"] == "application/json"
    assert "minLength" not in json.dumps(gc1["responseJsonSchema"])
    # SDK gửi field này dạng snake_case; proto JSON của Gemini API nhận cả hai dạng tên.
    assert list(gc1["thinkingConfig"].values()) == ["LOW"]
    assert b1["systemInstruction"]["parts"][0]["text"] == "Bạn trích xuất tri thức."
    assert b1["contents"][0]["parts"][0]["text"].startswith("Giá vé")
    assert "thinkingConfig" not in b2["generationConfig"]  # flash-lite: để mặc định


def test_sdk_safety_block_then_server_error(api):
    server = api([(200, response(FLASH, "", finish="SAFETY")), (503, error(503, "UNAVAILABLE", "quá tải"))])
    with pytest.raises(LLMUnavailable) as e:
        run(sdk_client(server).complete(req()))
    assert [a.error_code for a in e.value.attempts] == ["REFUSAL", "SERVER"]


def test_sdk_invalid_api_key_is_auth(api):
    server = api([(400, error(400, "INVALID_ARGUMENT", "API key not valid. Please pass a valid API key."))])
    with pytest.raises(LLMRequestError) as e:
        run(sdk_client(server).complete(req()))
    assert [a.error_code for a in e.value.attempts] == [
        "AUTH",
        "AUTH",
    ]  # model thứ hai cùng provider bị bỏ qua
    assert len(server.requests) == 1


def test_sdk_max_tokens_is_not_retryable(api):
    server = api(
        [(200, response(FLASH, '{"topic": "gi', "MAX_TOKENS")), (200, response(LITE, "", "MAX_TOKENS"))]
    )
    with pytest.raises(LLMRequestError):
        run(sdk_client(server).complete(req(schema=ITEM_SCHEMA)))


def test_sdk_priced_by_served_model(api):
    server = api([(200, response("gemini-3.1-pro-preview", "ok"))])  # alias trỏ sang model khác
    r = run(sdk_client(server).complete(req()))
    assert r.model == FLASH and r.served_model == PRO
    assert r.cost_usd == pytest.approx((50 * 2.0 + 15 * 12.0 + 30 * 2.0) / 1e6)


# ─────────────────────────────── quota Redis ───────────────────────────────

REDIS_URL = os.environ.get("BIVA_TEST_REDIS_URL")


@pytest.mark.skipif(not REDIS_URL, reason="đặt BIVA_TEST_REDIS_URL để chạy test quota với Redis thật")
def test_redis_quota_token_bucket():
    import redis.asyncio as aioredis

    from biva_worker.llm.quota import RedisQuota

    async def t():
        r = aioredis.from_url(REDIS_URL)
        q = RedisQuota(r, prefix=f"test:{os.getpid()}")
        try:
            # 2 request/phút: lần 3 bị chặn, có thời gian chờ.
            assert (await q.acquire("gemini", "ingest", 1000, 2)).allowed
            assert (await q.acquire("gemini", "ingest", 1000, 2)).allowed
            d = await q.acquire("gemini", "ingest", 1000, 2)
            assert not d.allowed and 0 < d.retry_after_s <= 30
            # Hết token sau khi trừ → bị chặn dù còn request; purpose khác không bị ảnh hưởng.
            await q.debit("gemini", "validate", 5000, 1000, 100)
            assert not (await q.acquire("gemini", "validate", 1000, 100)).allowed
            assert (await q.acquire("gemini", "knowledge", 1000, 100)).allowed
        finally:
            await r.aclose()

    run(t())


def test_redis_down_fails_open():
    import redis.asyncio as aioredis

    from biva_worker.llm.quota import RedisQuota

    async def t():
        r = aioredis.from_url("redis://127.0.0.1:1", socket_connect_timeout=0.2)
        try:
            assert (await RedisQuota(r).acquire("gemini", "ingest", 1, 1)).allowed
        finally:
            await r.aclose()

    run(t())
