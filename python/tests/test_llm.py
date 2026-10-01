"""Thư viện llm/: kế hoạch gọi, fallback, kiểm output, chi phí, quota.

Phần cuối chạy SDK anthropic thật với một server giả lập Messages API (không tốn tiền, không cần key).
"""

from __future__ import annotations

import asyncio
import json
import os
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Any

import anthropic
import pytest

from biva_worker.contracts import contracts_root
from biva_worker.llm import LLMClient, LLMRequestError, LLMUnavailable, Request, api_schema, load
from biva_worker.llm.client import AnthropicProvider, RawResponse, Usage, UsageRecord, _AttemptFailed
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
        assert [[s.model.name, s.effort] for s in CFG.plan(purpose)] == expected, purpose


def test_config_rejects_unknown_model_in_tier():
    raw = {
        "version": 1,
        "providers": {"a": {"kind": "anthropic", "api_key_env": "X"}},
        "models": {"m": {"provider": "a", "price": {"input": 1, "output": 1}}},
        "tiers": {"small": ["m"], "strong": ["khong-co"]},
        "purposes": {
            p: {"tier": "small", "tokens_per_minute": 1, "requests_per_minute": 1}
            for p in ["ingest", "knowledge", "validate", "test", "interactive"]
        },
    }
    with pytest.raises(ConfigError, match="khong-co"):
        parse(raw)
    raw["tiers"]["strong"] = ["m"]
    raw["providers"]["a"]["kind"] = "openai"
    with pytest.raises(ConfigError, match="không hợp lệ"):
        parse(raw)


def test_price_cost():
    price = CFG.models["claude-sonnet-5-5"].price
    assert price.cost(1_000_000, 0) == pytest.approx(2.0)
    assert price.cost(0, 1_000_000) == pytest.approx(10.0)
    assert price.cost(0, 0, 1_000_000) == pytest.approx(0.2)
    haiku = CFG.models["claude-haiku-4-5"].price  # không khai giá cache → tính bằng giá input
    assert haiku.cost(0, 0, 1_000_000) == pytest.approx(1.0)


def test_api_schema_strips_unsupported_constraints():
    sent = api_schema(ITEM_SCHEMA)
    assert "minLength" not in sent["properties"]["topic"]
    assert "minimum" not in sent["properties"]["price_vnd"]
    assert ITEM_SCHEMA["properties"]["topic"]["minLength"] == 2  # bản gốc không bị sửa
    with pytest.raises(ValueError, match="additionalProperties"):
        api_schema({"type": "object", "properties": {}})


# ─────────────────────────────── fallback (provider giả) ───────────────────────────────


class FakeProvider:
    """Mỗi lượt gọi lấy một phản hồi theo kịch bản: RawResponse hoặc _AttemptFailed."""

    def __init__(self, script: dict[str, list[Any]]) -> None:
        self.script = {k: list(v) for k, v in script.items()}
        self.calls: list[tuple[str, str | None, Any]] = []

    async def complete(self, step: Step, req: Request, schema: dict[str, Any] | None) -> RawResponse:
        self.calls.append((step.model.name, step.effort, schema))
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
    return LLMClient(CFG, providers={"anthropic": fake}, usage_sink=sink, **kw), fake, sink


def req(**kw) -> Request:
    base = {
        "purpose": "ingest",
        "messages": [{"role": "user", "content": "Giá vé Sài Gòn – Đà Lạt 300.000đ"}],
    }
    return Request(**{**base, **kw})


def test_primary_ok_records_usage_with_operator():
    c, fake, sink = client(
        {"claude-sonnet-5-5": [ok('{"topic": "gia_ve", "text": "300k"}', "claude-sonnet-5-5")]}
    )
    r = run(c.complete(req(schema=ITEM_SCHEMA, operator_id="phuongnam", operation_id="op-1")))
    assert r.data == {"topic": "gia_ve", "text": "300k"}
    assert r.model == "claude-sonnet-5-5" and len(r.attempts) == 1
    assert fake.calls[0][1] == "low"  # effort của model
    assert "minLength" not in json.dumps(fake.calls[0][2])  # schema gửi đi đã gỡ ràng buộc
    (rec,) = sink.records
    assert (rec.operator_id, rec.operation_id, rec.purpose, rec.ok) == ("phuongnam", "op-1", "ingest", True)
    assert rec.cost_usd == pytest.approx((100 * 2.0 + 20 * 10.0) / 1e6)


@pytest.mark.parametrize("code", ["RATE_LIMIT", "SERVER", "TIMEOUT", "CONNECTION"])
def test_fallback_on_transient_error(code):
    """AC S1.1.1: provider/model chính lỗi → tự chuyển."""
    c, fake, sink = client(
        {
            "claude-sonnet-5-5": [_AttemptFailed(code, True, "lỗi")],
            "claude-haiku-4-5": [ok("xin chào", "claude-haiku-4-5")],
        }
    )
    r = run(c.complete(req(operator_id="phuongnam")))
    assert r.model == "claude-haiku-4-5" and r.text == "xin chào"
    assert [c[0] for c in fake.calls] == ["claude-sonnet-5-5", "claude-haiku-4-5"]
    assert fake.calls[1][1] is None  # Haiku 4.5: không gửi effort
    assert [(x.model, x.ok, x.error_code) for x in sink.records] == [
        ("claude-sonnet-5-5", False, code),
        ("claude-haiku-4-5", True, None),
    ]
    assert all(x.operator_id == "phuongnam" for x in sink.records)


def test_fallback_on_refusal_and_invalid_output():
    c, _, sink = client(
        {
            "claude-sonnet-5-5": [ok("", "claude-sonnet-5-5", stop="refusal")],
            "claude-haiku-4-5": [ok('{"topic": "x", "text": "y"}', "claude-haiku-4-5")],  # topic quá ngắn
        }
    )
    with pytest.raises(LLMUnavailable) as e:
        run(c.complete(req(schema=ITEM_SCHEMA)))
    assert [a.error_code for a in e.value.attempts] == ["REFUSAL", "INVALID_OUTPUT"]
    assert "minLength" in e.value.attempts[1].detail or "short" in e.value.attempts[1].detail
    assert len(sink.records) == 2  # lượt lỗi vẫn ghi usage (đã tốn token)


def test_bad_request_everywhere_is_permanent():
    c, _, _ = client(
        {
            "claude-sonnet-5-5": [_AttemptFailed("BAD_REQUEST", False, "400")],
            "claude-haiku-4-5": [_AttemptFailed("BAD_REQUEST", False, "400")],
        }
    )
    with pytest.raises(LLMRequestError):
        run(c.complete(req()))


def test_auth_error_skips_rest_of_provider():
    c, fake, _ = client({"claude-opus-5-5": [_AttemptFailed("AUTH", False, "401")]})
    with pytest.raises(LLMRequestError):
        run(c.complete(req(purpose="validate")))
    assert [x[0] for x in fake.calls] == ["claude-opus-5-5"]  # sonnet cùng provider không bị gọi


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


def test_purpose_effort_override():
    c, fake, _ = client({"claude-opus-5-5": [ok("ok", "claude-opus-5-5")]})
    run(c.complete(req(purpose="interactive")))
    assert fake.calls[0][1] == "high"


def test_unknown_purpose():
    c, _, _ = client({})
    with pytest.raises(ConfigError):
        run(c.complete(req(purpose="chat")))


# ─────────────────────────────── SDK thật + server giả lập ───────────────────────────────


class FakeMessagesAPI:
    """HTTP server trả lời /v1/messages theo kịch bản (status, body); ghi lại request nhận được."""

    def __init__(self, script: list[tuple[int, dict[str, Any]]]) -> None:
        self.script = list(script)
        self.requests: list[tuple[dict[str, str], dict[str, Any]]] = []
        outer = self

        class H(BaseHTTPRequestHandler):
            def do_POST(self):  # noqa: N802
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                outer.requests.append(({k.lower(): v for k, v in self.headers.items()}, body))
                status, payload = outer.script.pop(0)
                data = json.dumps(payload).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("request-id", f"req_test_{len(outer.requests)}")
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


def message(model: str, text: str, stop: str = "end_turn") -> dict[str, Any]:
    return {
        "id": "msg_1",
        "type": "message",
        "role": "assistant",
        "model": model,
        "content": [{"type": "text", "text": text}],
        "stop_reason": stop,
        "stop_sequence": None,
        "usage": {"input_tokens": 50, "output_tokens": 10, "cache_read_input_tokens": 30},
    }


def error(kind: str, msg: str) -> dict[str, Any]:
    return {"type": "error", "error": {"type": kind, "message": msg}}


@pytest.fixture
def api():
    servers = []

    def make(script):
        s = FakeMessagesAPI(script)
        servers.append(s)
        return s

    yield make
    for s in servers:
        s.close()


def sdk_client(api_server) -> LLMClient:
    sdk = anthropic.AsyncAnthropic(api_key="test", base_url=api_server.url, max_retries=0)
    return LLMClient(CFG, providers={"anthropic": AnthropicProvider(CFG.providers["anthropic"], client=sdk)})


def test_sdk_request_shape_and_fallback_on_429(api):
    server = api(
        [
            (429, error("rate_limit_error", "chậm lại")),
            (200, message("claude-haiku-4-5", '{"topic": "gia_ve", "text": "300k"}')),
        ]
    )
    r = run(
        sdk_client(server).complete(
            req(schema=ITEM_SCHEMA, system="Bạn trích xuất tri thức.", cache_system=True)
        )
    )
    assert r.data["topic"] == "gia_ve" and r.model == "claude-haiku-4-5"
    assert r.attempts[0].error_code == "RATE_LIMIT" and "req_test_1" in r.attempts[0].detail
    assert r.usage.cache_read_tokens == 30

    (h1, b1), (h2, b2) = server.requests
    # Sonnet 5.5: effort + structured output + fallback phía server (beta).
    assert b1["model"] == "claude-sonnet-5-5"
    assert b1["output_config"]["effort"] == "low"
    assert b1["output_config"]["format"]["type"] == "json_schema"
    assert "minLength" not in json.dumps(b1["output_config"]["format"]["schema"])
    assert b1["fallbacks"] == "default" and "server-side-fallback-2026-07-01" in h1.get("anthropic-beta", "")
    assert b1["system"][0]["cache_control"] == {"type": "ephemeral"}
    # Haiku 4.5: không effort, không fallback phía server.
    assert b2["model"] == "claude-haiku-4-5"
    assert "effort" not in b2["output_config"] and "fallbacks" not in b2
    assert "anthropic-beta" not in h2


def test_sdk_server_error_then_bad_request(api):
    server = api([(529, error("overloaded_error", "quá tải")), (400, error("invalid_request_error", "sai"))])
    with pytest.raises(LLMUnavailable) as e:  # còn một lượt retryable (529) → job thử lại sau
        run(sdk_client(server).complete(req()))
    assert [a.error_code for a in e.value.attempts] == ["SERVER", "BAD_REQUEST"]


def test_sdk_served_by_fallback_model_is_priced_by_served_model(api):
    server = api([(200, message("claude-opus-5-5", "ok"))])  # gọi sonnet, server fallback trả opus
    r = run(sdk_client(server).complete(req()))
    assert r.model == "claude-sonnet-5-5" and r.served_model == "claude-opus-5-5"
    assert r.cost_usd == pytest.approx((50 * 4.0 + 10 * 20.0 + 30 * 0.2) / 1e6)


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
            assert (await q.acquire("anthropic", "ingest", 1000, 2)).allowed
            assert (await q.acquire("anthropic", "ingest", 1000, 2)).allowed
            d = await q.acquire("anthropic", "ingest", 1000, 2)
            assert not d.allowed and 0 < d.retry_after_s <= 30
            # Hết token sau khi trừ → bị chặn dù còn request; purpose khác không bị ảnh hưởng.
            await q.debit("anthropic", "validate", 5000, 1000, 100)
            assert not (await q.acquire("anthropic", "validate", 1000, 100)).allowed
            assert (await q.acquire("anthropic", "knowledge", 1000, 100)).allowed
        finally:
            await r.aclose()

    run(t())


def test_redis_down_fails_open():
    import redis.asyncio as aioredis

    from biva_worker.llm.quota import RedisQuota

    async def t():
        r = aioredis.from_url("redis://127.0.0.1:1", socket_connect_timeout=0.2)
        try:
            assert (await RedisQuota(r).acquire("anthropic", "ingest", 1, 1)).allowed
        finally:
            await r.aclose()

    run(t())
