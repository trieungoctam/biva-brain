"""Thư viện llm/: gọi model theo purpose, fallback, quota, đo chi phí — docs/system-architecture.md §3.4.

    client = LLMClient(config.load(), quota=RedisQuota(redis), usage_sink=PgUsageSink(pool))
    result = await client.complete(Request(purpose="ingest", messages=[...], schema=ITEM_SCHEMA,
                                           operator_id="phuongnam", operation_id=job.id))
    result.data   # dict đã kiểm theo schema

Kế hoạch gọi = danh sách model của tier (config.plan). Mỗi lượt:
quota → gọi → kiểm output → ghi usage. Lượt lỗi được ghi lại rồi chuyển lượt kế tiếp:

- 429, ≥ 500, timeout, mất kết nối, hết quota, model từ chối, output sai schema → model kế tiếp;
  mọi lượt đều vậy → ``LLMUnavailable`` (retryable: job thử lại theo backoff).
- 401/403 → bỏ mọi model cùng provider.
- 400/404/422, hết ``max_tokens`` → model kế tiếp (có thể do tham số riêng của model);
  không lượt nào retryable → ``LLMRequestError`` (không retry).
"""

from __future__ import annotations

import copy
import json
import logging
import time
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from typing import Any, Protocol

import anthropic
from jsonschema import Draft202012Validator, FormatChecker
from opentelemetry import metrics, trace

from biva_worker.llm.config import LLMConfig, Provider, Step
from biva_worker.llm.quota import NoQuota, Quota
from biva_worker.runner import PermanentError, RetryableError

log = logging.getLogger(__name__)
tracer = trace.get_tracer(__name__)
meter = metrics.get_meter(__name__)
_tokens = meter.create_counter("biva.llm.tokens", unit="{token}", description="token LLM theo hướng in/out")
_cost = meter.create_counter("biva.llm.cost", unit="USD", description="chi phí LLM ước tính")
_latency = meter.create_histogram("biva.llm.latency", unit="ms", description="thời gian một lượt gọi model")

DEFAULT_MAX_TOKENS = 16000  # non-streaming: đủ chỗ cho output, dưới ngưỡng timeout HTTP của SDK


# ─────────────────────────────── kiểu dữ liệu ───────────────────────────────


@dataclass(frozen=True)
class Request:
    purpose: str
    messages: list[dict[str, Any]]
    system: str | None = None
    schema: dict[str, Any] | None = None  # JSON Schema → structured output, kết quả ở Result.data
    max_tokens: int = DEFAULT_MAX_TOKENS
    cache_system: bool = False  # đánh dấu cache cho system prompt dài, dùng lặp lại (prompt caching)
    operator_id: str | None = None
    operation_id: str | None = None


@dataclass(frozen=True)
class Usage:
    input_tokens: int = 0
    output_tokens: int = 0
    cache_read_tokens: int = 0
    cache_write_tokens: int = 0


@dataclass(frozen=True)
class RawResponse:
    text: str
    stop_reason: str | None
    served_model: str
    usage: Usage


@dataclass(frozen=True)
class Attempt:
    model: str
    provider: str
    ok: bool
    error_code: str | None = None
    retryable: bool = True
    detail: str = ""


@dataclass(frozen=True)
class Result:
    text: str
    data: Any
    model: str
    served_model: str
    provider: str
    usage: Usage
    cost_usd: float
    latency_ms: int
    attempts: list[Attempt] = field(default_factory=list)


@dataclass(frozen=True)
class UsageRecord:
    operator_id: str | None
    operation_id: str | None
    purpose: str
    provider: str
    model: str
    served_model: str | None
    ok: bool
    error_code: str | None
    usage: Usage
    cost_usd: float
    latency_ms: int


UsageSink = Callable[[UsageRecord], Awaitable[None]]


class LLMUnavailable(RetryableError):
    code = "LLM_UNAVAILABLE"

    def __init__(self, message: str, attempts: list[Attempt], retry_after_s: float = 0.0) -> None:
        super().__init__(message)
        self.attempts = attempts
        self.retry_after_s = retry_after_s


class LLMRequestError(PermanentError):
    def __init__(self, message: str, attempts: list[Attempt]) -> None:
        super().__init__(message, code="LLM_REQUEST")
        self.attempts = attempts


class _AttemptFailed(Exception):
    def __init__(
        self, code: str, retryable: bool, detail: str, usage: Usage | None = None, served: str | None = None
    ):
        super().__init__(detail)
        self.code, self.retryable, self.detail = code, retryable, detail
        self.usage, self.served = usage or Usage(), served


# ─────────────────────────────── provider ───────────────────────────────


class ProviderAdapter(Protocol):
    async def complete(self, step: Step, req: Request, schema: dict[str, Any] | None) -> RawResponse: ...


class AnthropicProvider:
    """Claude API qua SDK ``anthropic`` (AsyncAnthropic)."""

    SERVER_FALLBACK_BETA = "server-side-fallback-2026-07-01"

    def __init__(self, cfg: Provider, client: anthropic.AsyncAnthropic | None = None) -> None:
        if client is None:
            kwargs: dict[str, Any] = {"timeout": cfg.timeout_s, "max_retries": cfg.max_retries}
            if cfg.api_key:
                kwargs["api_key"] = cfg.api_key
            if cfg.base_url:
                kwargs["base_url"] = cfg.base_url
            client = anthropic.AsyncAnthropic(**kwargs)
        self.client = client

    async def complete(self, step: Step, req: Request, schema: dict[str, Any] | None) -> RawResponse:
        kwargs: dict[str, Any] = {
            "model": step.model.name,
            "max_tokens": req.max_tokens,
            "messages": req.messages,
        }
        if req.system:
            block: dict[str, Any] = {"type": "text", "text": req.system}
            if req.cache_system:
                block["cache_control"] = {"type": "ephemeral"}
            kwargs["system"] = [block]
        output_config: dict[str, Any] = {}
        if step.effort:
            output_config["effort"] = step.effort
        if schema is not None:
            output_config["format"] = {"type": "json_schema", "schema": schema}
        if output_config:
            kwargs["output_config"] = output_config

        try:
            if step.model.server_fallback:
                msg = await self.client.beta.messages.create(
                    betas=[self.SERVER_FALLBACK_BETA], fallbacks="default", **kwargs
                )
            else:
                msg = await self.client.messages.create(**kwargs)
        except (anthropic.AuthenticationError, anthropic.PermissionDeniedError) as exc:
            raise _AttemptFailed("AUTH", False, _err(exc)) from exc
        except (
            anthropic.BadRequestError,
            anthropic.NotFoundError,
            anthropic.UnprocessableEntityError,
        ) as exc:
            raise _AttemptFailed("BAD_REQUEST", False, _err(exc)) from exc
        except anthropic.RateLimitError as exc:
            raise _AttemptFailed("RATE_LIMIT", True, _err(exc)) from exc
        except anthropic.APITimeoutError as exc:
            raise _AttemptFailed("TIMEOUT", True, _err(exc)) from exc
        except anthropic.APIConnectionError as exc:
            raise _AttemptFailed("CONNECTION", True, _err(exc)) from exc
        except anthropic.APIStatusError as exc:
            raise _AttemptFailed(
                "SERVER" if exc.status_code >= 500 else "HTTP", exc.status_code >= 500, _err(exc)
            ) from exc

        u = msg.usage
        usage = Usage(
            input_tokens=u.input_tokens or 0,
            output_tokens=u.output_tokens or 0,
            cache_read_tokens=getattr(u, "cache_read_input_tokens", None) or 0,
            cache_write_tokens=getattr(u, "cache_creation_input_tokens", None) or 0,
        )
        text = next((b.text for b in msg.content if b.type == "text"), "")
        return RawResponse(text=text, stop_reason=msg.stop_reason, served_model=msg.model, usage=usage)


def _err(exc: Exception) -> str:
    req_id = (
        getattr(getattr(exc, "response", None), "headers", {}).get("request-id", "")
        if hasattr(exc, "response")
        else ""
    )
    msg = getattr(exc, "message", None) or str(exc)
    return f"{type(exc).__name__}: {msg}" + (f" (request-id {req_id})" if req_id else "")


# ─────────────────────────────── schema ───────────────────────────────

# Ràng buộc structured output của Claude API không hỗ trợ: gỡ khỏi schema gửi đi, kiểm lại phía client.
_UNSUPPORTED = {
    "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf",
    "minLength", "maxLength", "pattern", "minItems", "maxItems", "uniqueItems",
    "minProperties", "maxProperties", "$schema", "$id",
}  # fmt: skip
_FORMATS = {"date-time", "time", "date", "duration", "email", "hostname", "uri", "ipv4", "ipv6", "uuid"}


def api_schema(schema: dict[str, Any]) -> dict[str, Any]:
    """Bản schema gửi cho API: bỏ ràng buộc không hỗ trợ; object phải có additionalProperties: false."""

    def walk(node: Any, path: str) -> Any:
        if isinstance(node, list):
            return [walk(x, path) for x in node]
        if not isinstance(node, dict):
            return node
        out = {}
        for k, v in node.items():
            if k in _UNSUPPORTED or (k == "format" and v not in _FORMATS):
                continue
            out[k] = walk(v, f"{path}/{k}")
        if out.get("type") == "object" and out.get("additionalProperties") is not False:
            raise ValueError(f"schema {path or '/'}: object phải có additionalProperties: false")
        return out

    return walk(copy.deepcopy(schema), "")


# ─────────────────────────────── client ───────────────────────────────


class LLMClient:
    def __init__(
        self,
        config: LLMConfig,
        *,
        quota: Quota | None = None,
        usage_sink: UsageSink | None = None,
        providers: dict[str, ProviderAdapter] | None = None,
    ) -> None:
        self.config = config
        self.quota = quota or NoQuota()
        self.usage_sink = usage_sink
        self.providers: dict[str, ProviderAdapter] = providers or {
            name: AnthropicProvider(p) for name, p in config.providers.items() if p.kind == "anthropic"
        }

    async def complete(self, req: Request) -> Result:
        plan = self.config.plan(req.purpose)
        purpose = self.config.purposes[req.purpose]
        schema_to_send = api_schema(req.schema) if req.schema is not None else None
        validator = Draft202012Validator(req.schema, format_checker=FormatChecker()) if req.schema else None

        attempts: list[Attempt] = []
        dead_providers: set[str] = set()
        min_retry_after = 0.0
        with tracer.start_as_current_span(
            f"llm {req.purpose}",
            attributes={"biva.llm.purpose": req.purpose, "biva.operator_id": req.operator_id or ""},
        ) as span:
            for step in plan:
                model, provider = step.model.name, step.provider.name
                if provider in dead_providers:
                    attempts.append(
                        Attempt(model, provider, False, "AUTH", False, "provider đã lỗi xác thực")
                    )
                    continue
                decision = await self.quota.acquire(
                    provider, req.purpose, purpose.tokens_per_minute, purpose.requests_per_minute
                )
                if not decision.allowed:
                    attempts.append(
                        Attempt(model, provider, False, "QUOTA", True, f"chờ {decision.retry_after_s:.1f}s")
                    )
                    min_retry_after = (
                        decision.retry_after_s
                        if not min_retry_after
                        else min(min_retry_after, decision.retry_after_s)
                    )
                    continue

                started = time.monotonic()
                try:
                    raw = await self.providers[provider].complete(step, req, schema_to_send)
                    data = self._check(raw, validator)
                except _AttemptFailed as fail:
                    latency = int((time.monotonic() - started) * 1000)
                    await self._record(req, step, fail.served, False, fail.code, fail.usage, latency)
                    attempts.append(Attempt(model, provider, False, fail.code, fail.retryable, fail.detail))
                    log.warning("llm %s: %s lỗi %s: %s", req.purpose, model, fail.code, fail.detail)
                    if fail.code == "AUTH":
                        dead_providers.add(provider)
                    continue

                latency = int((time.monotonic() - started) * 1000)
                cost = await self._record(req, step, raw.served_model, True, None, raw.usage, latency)
                attempts.append(Attempt(model, provider, True))
                span.set_attributes(
                    {
                        "gen_ai.request.model": model,
                        "gen_ai.response.model": raw.served_model,
                        "gen_ai.usage.input_tokens": raw.usage.input_tokens,
                        "gen_ai.usage.output_tokens": raw.usage.output_tokens,
                        "biva.llm.cost_usd": cost,
                        "biva.llm.attempts": len(attempts),
                    }
                )
                return Result(
                    text=raw.text,
                    data=data,
                    model=model,
                    served_model=raw.served_model,
                    provider=provider,
                    usage=raw.usage,
                    cost_usd=cost,
                    latency_ms=latency,
                    attempts=attempts,
                )

            summary = "; ".join(f"{a.model}: {a.error_code}" for a in attempts)
            span.set_attribute("biva.llm.attempts", len(attempts))
            if any(a.retryable for a in attempts):
                raise LLMUnavailable(f"mọi model đều lỗi ({summary})", attempts, min_retry_after)
            raise LLMRequestError(f"yêu cầu LLM không hợp lệ ({summary})", attempts)

    @staticmethod
    def _check(raw: RawResponse, validator: Draft202012Validator | None) -> Any:
        if raw.stop_reason == "refusal":
            raise _AttemptFailed("REFUSAL", True, "model từ chối", raw.usage, raw.served_model)
        if raw.stop_reason == "max_tokens":
            raise _AttemptFailed(
                "MAX_TOKENS", False, "output bị cắt (max_tokens)", raw.usage, raw.served_model
            )
        if validator is None:
            return None
        try:
            data = json.loads(raw.text)
        except json.JSONDecodeError as exc:
            raise _AttemptFailed(
                "INVALID_OUTPUT", True, f"không phải JSON: {exc}", raw.usage, raw.served_model
            ) from exc
        errors = sorted(validator.iter_errors(data), key=lambda e: list(e.path))
        if errors:
            detail = "; ".join(f"/{'/'.join(map(str, e.path))}: {e.message}" for e in errors[:5])
            raise _AttemptFailed("INVALID_OUTPUT", True, detail, raw.usage, raw.served_model)
        return data

    async def _record(
        self,
        req: Request,
        step: Step,
        served: str | None,
        ok: bool,
        code: str | None,
        usage: Usage,
        latency_ms: int,
    ) -> float:
        # Giá theo model thực sự trả lời (fallback phía server có thể đổi model).
        priced = self.config.models.get(served or "", step.model)
        cost = priced.price.cost(usage.input_tokens, usage.output_tokens, usage.cache_read_tokens)
        cost += usage.cache_write_tokens * priced.price.input * 1.25 / 1e6  # ghi cache 5 phút = 1.25× input
        attrs = {
            "purpose": req.purpose,
            "model": served or step.model.name,
            "operator_id": req.operator_id or "",
        }
        _tokens.add(
            usage.input_tokens + usage.cache_read_tokens + usage.cache_write_tokens,
            {**attrs, "direction": "in"},
        )
        _tokens.add(usage.output_tokens, {**attrs, "direction": "out"})
        _cost.add(cost, attrs)
        _latency.record(latency_ms, {**attrs, "ok": ok})
        total = usage.input_tokens + usage.output_tokens + usage.cache_read_tokens + usage.cache_write_tokens
        if total:
            p = self.config.purposes[req.purpose]
            await self.quota.debit(
                step.provider.name, req.purpose, total, p.tokens_per_minute, p.requests_per_minute
            )
        if self.usage_sink is not None:
            record = UsageRecord(
                operator_id=req.operator_id,
                operation_id=req.operation_id,
                purpose=req.purpose,
                provider=step.provider.name,
                model=step.model.name,
                served_model=served,
                ok=ok,
                error_code=code,
                usage=usage,
                cost_usd=cost,
                latency_ms=latency_ms,
            )
            try:
                await self.usage_sink(record)
            except Exception:  # noqa: BLE001 — ghi số liệu lỗi không được làm hỏng lời gọi đã trả tiền
                log.exception("ghi llm_usage lỗi")
        return cost
