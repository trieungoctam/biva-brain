"""Ghi llm_usage vào Postgres (migration 000004)."""

from __future__ import annotations

import asyncpg

from biva_worker.llm.client import UsageRecord


class PgUsageSink:
    def __init__(self, pool: asyncpg.Pool) -> None:
        self.pool = pool

    async def __call__(self, r: UsageRecord) -> None:
        await self.pool.execute(
            """
            INSERT INTO llm_usage (
                operator_id, operation_id, purpose, provider, model, served_model, ok, error_code,
                input_tokens, output_tokens, cache_read_tokens, cost_usd, latency_ms
            )
            VALUES ($1, $2::uuid, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
            """,
            r.operator_id,
            r.operation_id,
            r.purpose,
            r.provider,
            r.model,
            r.served_model,
            r.ok,
            r.error_code,
            r.usage.input_tokens,
            r.usage.output_tokens,
            r.usage.cache_read_tokens,
            round(r.cost_usd, 6),
            r.latency_ms,
        )
