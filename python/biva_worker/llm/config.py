"""Đọc và kiểm tra contracts/llm/llm.yaml (schema: contracts/schemas/llm_config.schema.json)."""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import yaml

from biva_worker.contracts import contracts_root, validate


class ConfigError(ValueError):
    pass


@dataclass(frozen=True)
class Price:
    input: float
    output: float
    cache_read: float | None = None

    def cost(self, input_tokens: int, output_tokens: int, cache_read_tokens: int = 0) -> float:
        cache_price = self.input if self.cache_read is None else self.cache_read
        return (
            input_tokens * self.input + output_tokens * self.output + cache_read_tokens * cache_price
        ) / 1e6


@dataclass(frozen=True)
class Provider:
    name: str
    kind: str
    api_key_env: str
    base_url_env: str | None = None
    timeout_s: float = 120.0
    max_retries: int = 1

    @property
    def api_key(self) -> str | None:
        return os.environ.get(self.api_key_env) or None

    @property
    def base_url(self) -> str | None:
        return os.environ.get(self.base_url_env) if self.base_url_env else None


@dataclass(frozen=True)
class Model:
    name: str
    provider: str
    price: Price
    effort: str | None = None
    server_fallback: bool = False


@dataclass(frozen=True)
class Purpose:
    name: str
    tier: str
    tokens_per_minute: int
    requests_per_minute: int
    effort: str | None = None


@dataclass(frozen=True)
class Step:
    """Một lượt thử trong kế hoạch gọi: model + provider + effort đã áp dụng override của purpose."""

    model: Model
    provider: Provider
    effort: str | None


@dataclass(frozen=True)
class LLMConfig:
    providers: dict[str, Provider]
    models: dict[str, Model]
    tiers: dict[str, list[str]]
    purposes: dict[str, Purpose] = field(default_factory=dict)

    def plan(self, purpose: str) -> list[Step]:
        """Thứ tự model sẽ thử cho một purpose (test hợp đồng: Go phải cho cùng kết quả)."""
        if purpose not in self.purposes:
            raise ConfigError(f"purpose không có trong llm.yaml: {purpose}")
        p = self.purposes[purpose]
        steps = []
        for name in self.tiers[p.tier]:
            m = self.models[name]
            # Override effort của purpose chỉ áp dụng cho model có hỗ trợ effort (đã khai báo effort).
            effort = (p.effort or m.effort) if m.effort else None
            steps.append(Step(model=m, provider=self.providers[m.provider], effort=effort))
        return steps


def default_path() -> Path:
    return Path(os.environ.get("BIVA_LLM_CONFIG", contracts_root() / "llm" / "llm.yaml"))


def parse(raw: dict[str, Any]) -> LLMConfig:
    errors = validate("llm_config", raw)
    if errors:
        raise ConfigError("llm.yaml không hợp lệ: " + "; ".join(errors))
    providers = {n: Provider(name=n, **v) for n, v in raw["providers"].items()}
    models = {}
    for n, v in raw["models"].items():
        if v["provider"] not in providers:
            raise ConfigError(f"model {n}: provider {v['provider']!r} chưa khai báo")
        models[n] = Model(
            name=n,
            provider=v["provider"],
            price=Price(**v["price"]),
            effort=v.get("effort"),
            server_fallback=v.get("server_fallback", False),
        )
    for tier, names in raw["tiers"].items():
        for n in names:
            if n not in models:
                raise ConfigError(f"tier {tier}: model {n!r} chưa khai báo")
    purposes = {}
    for n, v in raw["purposes"].items():
        if v["tier"] not in raw["tiers"]:
            raise ConfigError(f"purpose {n}: tier {v['tier']!r} chưa khai báo")
        purposes[n] = Purpose(name=n, **v)
    return LLMConfig(providers=providers, models=models, tiers=raw["tiers"], purposes=purposes)


def load(path: Path | None = None) -> LLMConfig:
    with open(path or default_path(), encoding="utf-8") as f:
        return parse(yaml.safe_load(f))
