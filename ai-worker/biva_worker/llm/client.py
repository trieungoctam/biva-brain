"""Thư viện llm/: gọi model Gemini theo purpose, fallback, quota, đo chi phí.

Spec: docs/system-architecture.md §3.4.

    client = LLMClient(config.load(), quota=RedisQuota(redis), usage_sink=PgUsageSink(pool))
    result = await client.complete(Request(purpose="ingest", messages=[...], schema=ITEM_SCHEMA,
                                           operator_id="phuongnam", operation_id=job.id))
    result.data   # dict đã kiểm theo schema

Kế hoạch gọi = danh sách model của tier (config.plan). Mỗi lượt:
quota → gọi → kiểm output → ghi usage. Lượt lỗi được ghi lại rồi chuyển lượt kế tiếp:

- 429, ≥ 500, timeout, mất kết nối, hết quota, bị chặn (safety), output sai schema → model kế tiếp;
  mọi lượt đều vậy → ``LLMUnavailable`` (retryable: job thử lại theo backoff).
- 401/403, API key sai → bỏ mọi model cùng provider.
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

import httpx
from google import genai
from google.genai import errors as genai_errors
from google.genai import types as genai_types
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

DEFAULT_MAX_TOKENS = 16000  # gồm cả token suy luận (thinking) của Gemini


# ─────────────────────────────── kiểu dữ liệu ───────────────────────────────


@dataclass(frozen=True)
class Request:
    purpose: str
    messages: list[dict[str, Any]]
    system: str | None = None
    schema: dict[str, Any] | None = None  # JSON Schema → structured output, kết quả ở Result.data
    max_tokens: int = DEFAULT_MAX_TOKENS
    operator_id: str | None = None
    operation_id: str | None = None


@dataclass(frozen=True)
class Usage:
    input_tokens: int = 0  # input không tính phần đọc từ cache
    output_tokens: int = 0  # gồm cả token suy luận (tính giá như output)
    cache_read_tokens: int = 0


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


class GeminiProvider:
    """Gemini Developer API qua SDK ``google-genai`` (client.aio)."""

    # finish_reason / block_reason coi như "bị chặn" → thử model kế tiếp.
    BLOCKED = {
        "SAFETY",
        "BLOCKLIST",
        "PROHIBITED_CONTENT",
        "SPII",
        "RECITATION",
        "LANGUAGE",
        "JAILBREAK",
        "MODEL_ARMOR",
    }

    def __init__(self, cfg: Provider, client: genai.Client | None = None) -> None:
        if client is None:
            http = genai_types.HttpOptions(
                timeout=int(cfg.timeout_s * 1000),  # mili giây
                retry_options=genai_types.HttpRetryOptions(attempts=cfg.max_retries + 1),
                base_url=cfg.base_url,
            )
            client = genai.Client(api_key=cfg.api_key, http_options=http)
        self.client = client

    async def complete(self, step: Step, req: Request, schema: dict[str, Any] | None) -> RawResponse:
        thinking = None
        if step.thinking_level:
            thinking = genai_types.ThinkingConfig(thinking_level=step.thinking_level.upper())
        elif step.thinking_budget is not None:
            thinking = genai_types.ThinkingConfig(thinking_budget=step.thinking_budget)
        config = genai_types.GenerateContentConfig(
            system_instruction=req.system,
            max_output_tokens=req.max_tokens,
            thinking_config=thinking,
            response_mime_type="application/json" if schema is not None else None,
            response_json_schema=schema,
            # Brain không truyền tool cho model: tắt automatic function calling của SDK.
            automatic_function_calling=genai_types.AutomaticFunctionCallingConfig(disable=True),
        )
        contents = [
            genai_types.Content(
                role="model" if m["role"] == "assistant" else "user",
                parts=[genai_types.Part(text=m["content"])],
            )
            for m in req.messages
        ]
        try:
            resp = await self.client.aio.models.generate_content(
                model=step.model.name, contents=contents, config=config
            )
        except genai_errors.APIError as exc:
            raise _AttemptFailed(*_classify(exc)) from exc
        except httpx.TimeoutException as exc:
            raise _AttemptFailed("TIMEOUT", True, f"{type(exc).__name__}: {exc}") from exc
        except httpx.TransportError as exc:
            raise _AttemptFailed("CONNECTION", True, f"{type(exc).__name__}: {exc}") from exc

        u = resp.usage_metadata
        cached = (u.cached_content_token_count or 0) if u else 0
        usage = Usage(
            input_tokens=max(0, ((u.prompt_token_count or 0) if u else 0) - cached),
            output_tokens=((u.candidates_token_count or 0) + (u.thoughts_token_count or 0)) if u else 0,
            cache_read_tokens=cached,
        )
        served = resp.model_version or step.model.name
        block = resp.prompt_feedback.block_reason if resp.prompt_feedback else None
        if block is not None:
            return RawResponse(text="", stop_reason="refusal", served_model=served, usage=usage)
        finish = resp.candidates[0].finish_reason if resp.candidates else None
        name = getattr(finish, "value", finish) or "STOP"
        if name in self.BLOCKED:
            stop = "refusal"
        elif name == "MAX_TOKENS":
            stop = "max_tokens"
        elif name in ("STOP", "FINISH_REASON_UNSPECIFIED"):
            stop = "end_turn"
        else:
            stop = "error"
        text = "" if stop == "refusal" else (resp.text or "")
        return RawResponse(text=text, stop_reason=stop, served_model=served, usage=usage)


def _classify(exc: genai_errors.APIError) -> tuple[str, bool, str]:
    """Lỗi HTTP của Gemini → (mã, retryable, mô tả)."""
    detail = f"{exc.code} {exc.status or ''}: {exc.message or ''}".strip()
    if exc.code in (401, 403) or "API_KEY_INVALID" in str(exc) or "API key not valid" in str(exc):
        return "AUTH", False, detail
    if exc.code == 429:
        return "RATE_LIMIT", True, detail
    if exc.code >= 500:
        return "SERVER", True, detail
    return "BAD_REQUEST", False, detail


# ─────────────────────────────── schema ───────────────────────────────

# Gemini (response_json_schema) hỗ trợ một tập con JSON Schema; các từ khoá dưới đây được gỡ khi gửi,
# còn schema đầy đủ vẫn được kiểm lại phía client bằng jsonschema.
_UNSUPPORTED = {
    "pattern", "minLength", "maxLength", "uniqueItems", "minProperties", "maxProperties",
    "multipleOf", "exclusiveMinimum", "exclusiveMaximum", "$schema", "$id",
}  # fmt: skip


def api_schema(schema: dict[str, Any]) -> dict[str, Any]:
    """Bản schema gửi cho API: bỏ ràng buộc API không hỗ trợ (bản gốc không bị sửa)."""

    def walk(node: Any) -> Any:
        if isinstance(node, list):
            return [walk(x) for x in node]
        if not isinstance(node, dict):
            return node
        return {k: walk(v) for k, v in node.items() if k not in _UNSUPPORTED}

    return walk(copy.deepcopy(schema))


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
            name: GeminiProvider(p) for name, p in config.providers.items() if p.kind == "gemini"
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
        # Giá theo model thực sự trả lời (alias như *-latest có thể trỏ sang model khác).
        priced = self.config.models.get(served or "", step.model)
        cost = priced.price.cost(usage.input_tokens, usage.output_tokens, usage.cache_read_tokens)
        attrs = {
            "purpose": req.purpose,
            "model": served or step.model.name,
            "operator_id": req.operator_id or "",
        }
        _tokens.add(
            usage.input_tokens + usage.cache_read_tokens,
            {**attrs, "direction": "in"},
        )
        _tokens.add(usage.output_tokens, {**attrs, "direction": "out"})
        _cost.add(cost, attrs)
        _latency.record(latency_ms, {**attrs, "ok": ok})
        total = usage.input_tokens + usage.output_tokens + usage.cache_read_tokens
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
