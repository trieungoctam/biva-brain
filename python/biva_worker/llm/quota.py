"""Quota token bucket trong Redis theo (provider, purpose) — docs/system-architecture.md §3.4.

Hai bucket mỗi khoá: token/phút và request/phút, nạp lại đều theo thời gian của Redis (TIME).
- ``acquire``: trước khi gọi — cần còn token (> 0) và ≥ 1 request; trừ 1 request.
- ``debit``: sau khi gọi — trừ số token thực dùng (có thể âm → chặn cho tới khi nạp lại).
Không biết trước số token output nên không giữ chỗ trước; vượt nhẹ một lần rồi tự hãm là chấp nhận được.

Redis lỗi → **fail-open** (cho gọi, ghi cảnh báo): quota là để bảo vệ ngân sách, không được làm dừng ingest.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass
from typing import Protocol

import redis.asyncio as aioredis
from redis.exceptions import RedisError

log = logging.getLogger(__name__)

# KEYS[1] = hash; ARGV = cap_tokens, cap_requests, op ("acquire" | "debit"), amount
_SCRIPT = """
local cap_t = tonumber(ARGV[1])
local cap_r = tonumber(ARGV[2])
local now = redis.call('TIME')
local now_ms = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)
local s = redis.call('HMGET', KEYS[1], 't', 'r', 'ts')
local t = tonumber(s[1]) or cap_t
local r = tonumber(s[2]) or cap_r
local ts = tonumber(s[3]) or now_ms
local dt = math.max(0, now_ms - ts)
t = math.min(cap_t, t + dt * cap_t / 60000)
r = math.min(cap_r, r + dt * cap_r / 60000)
local ok = 1
local wait_ms = 0
if ARGV[3] == 'acquire' then
  if t <= 0 or r < 1 then
    ok = 0
    local need_t = (t <= 0) and ((1 - t) * 60000 / cap_t) or 0
    local need_r = (r < 1) and ((1 - r) * 60000 / cap_r) or 0
    wait_ms = math.ceil(math.max(need_t, need_r))
  else
    r = r - 1
  end
else
  t = t - tonumber(ARGV[4])
end
redis.call('HSET', KEYS[1], 't', t, 'r', r, 'ts', now_ms)
redis.call('PEXPIRE', KEYS[1], 120000)
return {ok, wait_ms}
"""


@dataclass(frozen=True)
class Decision:
    allowed: bool
    retry_after_s: float = 0.0


class Quota(Protocol):
    async def acquire(
        self, provider: str, purpose: str, tokens_per_minute: int, requests_per_minute: int
    ) -> Decision: ...

    async def debit(
        self, provider: str, purpose: str, tokens: int, tokens_per_minute: int, requests_per_minute: int
    ) -> None: ...


class NoQuota:
    """Không giới hạn (test, hoặc chưa cấu hình Redis)."""

    async def acquire(
        self, provider: str, purpose: str, tokens_per_minute: int, requests_per_minute: int
    ) -> Decision:
        return Decision(True)

    async def debit(
        self, provider: str, purpose: str, tokens: int, tokens_per_minute: int, requests_per_minute: int
    ) -> None:
        return None


class RedisQuota:
    def __init__(self, client: aioredis.Redis, prefix: str = "biva:llm:quota") -> None:
        self.client = client
        self.prefix = prefix
        self._script = client.register_script(_SCRIPT)

    def _key(self, provider: str, purpose: str) -> str:
        return f"{self.prefix}:{provider}:{purpose}"

    async def acquire(
        self, provider: str, purpose: str, tokens_per_minute: int, requests_per_minute: int
    ) -> Decision:
        try:
            ok, wait_ms = await self._script(
                keys=[self._key(provider, purpose)],
                args=[tokens_per_minute, requests_per_minute, "acquire", 0],
            )
        except (RedisError, OSError) as exc:
            log.warning("quota Redis lỗi, cho phép gọi (fail-open): %s", exc)
            return Decision(True)
        return Decision(bool(ok), wait_ms / 1000)

    async def debit(
        self, provider: str, purpose: str, tokens: int, tokens_per_minute: int, requests_per_minute: int
    ) -> None:
        try:
            await self._script(
                keys=[self._key(provider, purpose)],
                args=[tokens_per_minute, requests_per_minute, "debit", tokens],
            )
        except (RedisError, OSError) as exc:
            log.warning("quota Redis lỗi khi trừ token: %s", exc)
