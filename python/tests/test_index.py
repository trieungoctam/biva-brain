"""Job index.items (S1.1.4) với Postgres thật + embedder giả; TEIEmbedder với server giả lập."""

from __future__ import annotations

import asyncio
import hashlib
import json
import math
import os
import threading
import uuid
from http.server import BaseHTTPRequestHandler, HTTPServer

import asyncpg
import pytest

from biva_worker import textnorm
from biva_worker.embed import DIM, TEIEmbedder
from biva_worker.index import index_items, item_search_text, to_pgvector
from biva_worker.llm.client import Usage, UsageRecord
from biva_worker.llm.usage import PgUsageSink
from biva_worker.runner import PermanentError, RetryableError, init_connection

DB_URL = os.environ.get("BIVA_TEST_DATABASE_URL")
needs_db = pytest.mark.skipif(not DB_URL, reason="đặt BIVA_TEST_DATABASE_URL để chạy test index")


def fake_vector(text: str) -> list[float]:
    """Vector tất định theo nội dung (đã chuẩn hoá) — cùng text → cùng vector."""
    seed = hashlib.sha256(textnorm.fold(text).encode()).digest()
    raw = [((seed[i % 32] * (i + 1)) % 97) - 48.0 for i in range(DIM)]
    n = math.sqrt(sum(x * x for x in raw))
    return [x / n for x in raw]


class FakeEmbedder:
    def __init__(self) -> None:
        self.calls = 0

    async def embed(self, texts: list[str]) -> list[list[float]]:
        self.calls += 1
        return [fake_vector(t) for t in texts]


def test_item_search_text_no_cross_field_bigram():
    s = item_search_text("gia_ve", "sgn-dl", "Giường nằm 300.000đ")
    assert s == "gia ve gia_ve sgn dl sgn_dl giuong nam 300000d giuong_nam nam_300000d"
    assert "ve_sgn" not in s and "dl_giuong" not in s


def test_to_pgvector():
    assert to_pgvector([0.5, -1.0, 1e-9]) == "[0.5,-1,1e-09]"


@needs_db
def test_index_and_search_with_and_without_diacritics():
    """AC S1.1.4: item mới tìm được bằng cả có dấu và không dấu."""

    async def t() -> None:
        pool = await asyncpg.create_pool(DB_URL, init=init_connection)
        op = f"idx{uuid.uuid4().hex[:8]}"
        try:
            await pool.execute("INSERT INTO operators (id, name) VALUES ($1, 'Index test')", op)
            texts = [
                ("chuyen_xe", "Xe giường nằm Sài Gòn – Đà Lạt chạy 22h30 mỗi tối"),
                ("hanh_ly", "Mỗi khách được mang 20kg hành lý miễn phí"),
                ("thu_cung", "Thú cưng phải để trong lồng, phụ thu 50.000đ"),
            ]
            ids = [
                await pool.fetchval(
                    "INSERT INTO items (layer, operator_id, kind, topic, text, status) "
                    "VALUES (2, $1, 'policy', $2, $3, 'active') RETURNING id::text",
                    op,
                    topic,
                    text,
                )
                for topic, text in texts
            ]
            emb = FakeEmbedder()
            result = await index_items(pool, emb, {"operator_id": op})
            assert result["counts"]["indexed"] == 3

            # Chạy lại: không còn gì thiếu → không gọi TEI.
            again = await index_items(pool, emb, {"operator_id": op})
            assert again["counts"]["indexed"] == 0 and emb.calls == 1

            for query in ["giường nằm", "giuong nam", "GIƯỜNG NẰM", "Đà Lạt", "da lat", "phu thu 50.000d"]:
                hit = await pool.fetch(
                    "SELECT id::text FROM items "
                    "WHERE operator_id = $1 AND tsv @@ plainto_tsquery('simple', $2)",
                    op,
                    textnorm.fold(query),
                )
                expected = ids[2] if "phu thu" in query else ids[0]
                assert [r["id"] for r in hit] == [expected], query

            # Nhánh semantic: vector gần nhất của chính câu đó là item đó.
            nearest = await pool.fetchval(
                "SELECT id::text FROM items WHERE operator_id = $1 ORDER BY embedding <=> $2::vector LIMIT 1",
                op,
                to_pgvector(fake_vector(texts[1][1])),
            )
            assert nearest == ids[1]

            # reindex theo item_ids: chỉ đúng item được chọn.
            res = await index_items(pool, emb, {"item_ids": [ids[2]], "reindex": True})
            assert res["counts"]["indexed"] == 1
        finally:
            await pool.execute("DELETE FROM operators WHERE id = $1", op)
            await pool.close()

    asyncio.run(t())


@needs_db
def test_index_rejects_bad_payload():
    async def t() -> None:
        pool = await asyncpg.create_pool(DB_URL, init=init_connection)
        try:
            with pytest.raises(PermanentError):
                await index_items(pool, FakeEmbedder(), {"item_ids": ["khong-phai-uuid"]})
        finally:
            await pool.close()

    asyncio.run(t())


@needs_db
def test_pg_usage_sink():
    async def t() -> None:
        pool = await asyncpg.create_pool(DB_URL, init=init_connection)
        op = f"usage{uuid.uuid4().hex[:8]}"
        try:
            model = "gemini-3.5-flash"
            record = UsageRecord(
                op,
                None,
                "ingest",
                "gemini",
                model,
                model,
                True,
                None,
                Usage(1000, 200, 300),
                0.0042,
                850,
            )
            await PgUsageSink(pool)(record)
            row = await pool.fetchrow("SELECT * FROM llm_usage WHERE operator_id = $1", op)
            assert (row["purpose"], row["ok"], row["input_tokens"], row["cache_read_tokens"]) == (
                "ingest",
                True,
                1000,
                300,
            )
            assert float(row["cost_usd"]) == pytest.approx(0.0042)
        finally:
            await pool.execute("DELETE FROM llm_usage WHERE operator_id = $1", op)
            await pool.close()

    asyncio.run(t())


# ─────────────────────────────── TEIEmbedder với server giả lập ───────────────────────────────


class FakeTEI:
    def __init__(self, status: int = 200, dim: int = DIM) -> None:
        self.batches: list[list[str]] = []
        outer = self

        class H(BaseHTTPRequestHandler):
            def do_POST(self):  # noqa: N802
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                assert body["normalize"] is True and body["truncate"] is True
                outer.batches.append(body["inputs"])
                data = json.dumps(
                    [[0.0] * dim for _ in body["inputs"]] if status == 200 else {"error": "x"}
                ).encode()
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


@pytest.mark.parametrize(
    "status,dim,exc",
    [(200, DIM, None), (503, DIM, RetryableError), (413, DIM, PermanentError), (200, 768, PermanentError)],
)
def test_tei_embedder(status, dim, exc):
    tei = FakeTEI(status, dim)

    async def t():
        e = TEIEmbedder(tei.url, batch_size=32)
        try:
            return await e.embed([f"câu {i}" for i in range(70)])
        finally:
            await e.aclose()

    try:
        if exc is None:
            vectors = asyncio.run(t())
            assert len(vectors) == 70 and [len(b) for b in tei.batches] == [32, 32, 6]
        else:
            with pytest.raises(exc):
                asyncio.run(t())
    finally:
        tei.server.shutdown()


def test_tei_down_is_retryable():
    async def t():
        e = TEIEmbedder("http://127.0.0.1:1", timeout_s=1)
        try:
            await e.embed(["x"])
        finally:
            await e.aclose()

    with pytest.raises(RetryableError):
        asyncio.run(t())
