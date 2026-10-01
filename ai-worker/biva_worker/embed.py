"""Embedding qua TEI (text-embeddings-inference, CPU).

Model bge-m3, 1024 chiều — khớp cột items.embedding VECTOR(1024).
"""

from __future__ import annotations

from typing import Protocol

import httpx

from biva_worker.runner import PermanentError, RetryableError

DIM = 1024


class Embedder(Protocol):
    async def embed(self, texts: list[str]) -> list[list[float]]: ...


class TEIEmbedder:
    def __init__(self, base_url: str, *, batch_size: int = 32, timeout_s: float = 60.0) -> None:
        # batch_size ≤ --max-client-batch-size của TEI (mặc định 32).
        self.client = httpx.AsyncClient(base_url=base_url, timeout=timeout_s)
        self.batch_size = batch_size

    async def embed(self, texts: list[str]) -> list[list[float]]:
        out: list[list[float]] = []
        for i in range(0, len(texts), self.batch_size):
            batch = texts[i : i + self.batch_size]
            try:
                resp = await self.client.post(
                    "/embed", json={"inputs": batch, "normalize": True, "truncate": True}
                )
            except httpx.HTTPError as exc:
                raise RetryableError(f"TEI không trả lời: {exc}") from exc
            if resp.status_code == 429 or resp.status_code >= 500:
                raise RetryableError(f"TEI bận/lỗi {resp.status_code}: {resp.text[:200]}")
            if resp.status_code != 200:
                raise PermanentError(
                    f"TEI từ chối {resp.status_code}: {resp.text[:200]}", code="EMBED_REJECTED"
                )
            vectors = resp.json()
            if len(vectors) != len(batch) or any(len(v) != DIM for v in vectors):
                raise PermanentError(f"TEI trả sai kích thước (cần {len(batch)}×{DIM})", code="EMBED_DIM")
            out.extend(vectors)
        return out

    async def aclose(self) -> None:
        await self.client.aclose()
