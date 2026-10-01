"""Job ``index.items``: ghi ``search_text`` (textnorm) + ``embedding`` (TEI) cho item — M1, S1.1.4.

- ``search_text`` = textnorm.search_text(topic, key, text) → cột ``tsv`` (generated) cho nhánh keyword;
  query phía Go đi qua cùng textnorm nên gõ có dấu hay không dấu đều khớp.
- ``embedding`` = vector của ``text`` gốc (giữ dấu — bge-m3 hiểu tiếng Việt) cho nhánh semantic.
Chạy theo lô; mỗi lô một transaction. Item đổi nội dung sau đó được index lại khi ``reindex`` hoặc khi
ingest xoá search_text/embedding của nó.
"""

from __future__ import annotations

from typing import Any

import asyncpg

from biva_worker import textnorm
from biva_worker.contracts import validate
from biva_worker.embed import Embedder
from biva_worker.runner import Job, PermanentError

BATCH = 64


def item_search_text(topic: str, key: str | None, text: str) -> str:
    # Từng phần riêng rẽ: không tạo bigram nối giữa topic và text.
    parts = [textnorm.search_text(p) for p in (topic.replace("_", " "), key or "", text)]
    return " ".join(p for p in parts if p)


def to_pgvector(v: list[float]) -> str:
    return "[" + ",".join(f"{x:.7g}" for x in v) + "]"


async def index_items(pool: asyncpg.Pool, embedder: Embedder, payload: dict[str, Any]) -> dict[str, Any]:
    errors = validate("jobs.index_items", payload)
    if errors:
        raise PermanentError("; ".join(errors), code="INVALID_PAYLOAD")

    conds = ["status IN ('pending', 'active')"]
    args: list[Any] = []
    if payload.get("operator_id"):
        args.append(payload["operator_id"])
        conds.append(f"operator_id = ${len(args)}")
    if payload.get("item_ids"):
        args.append(payload["item_ids"])
        conds.append(f"id = ANY(${len(args)}::uuid[])")
    if not payload.get("reindex"):
        conds.append("(embedding IS NULL OR search_text = '')")

    done = 0
    last_id = "00000000-0000-0000-0000-000000000000"
    while True:
        # Duyệt theo id tăng dần: không lặp vô hạn kể cả khi reindex (điều kiện "còn thiếu" không đổi).
        rows = await pool.fetch(
            f"""SELECT id::text, topic, key, text FROM items
                WHERE {" AND ".join(conds)} AND id > ${len(args) + 1}::uuid
                ORDER BY id LIMIT {BATCH}""",
            *args,
            last_id,
        )
        if not rows:
            break
        vectors = await embedder.embed([r["text"] for r in rows])
        await pool.executemany(
            """UPDATE items SET search_text = $2, embedding = $3::vector, updated_at = now()
               WHERE id = $1::uuid""",
            [
                (r["id"], item_search_text(r["topic"], r["key"], r["text"]), to_pgvector(v))
                for r, v in zip(rows, vectors, strict=True)
            ],
        )
        done += len(rows)
        last_id = rows[-1]["id"]

    return {"status": "done", "summary": f"đã index {done} item", "counts": {"indexed": done}}


def handler(pool: asyncpg.Pool, embedder: Embedder):
    async def run(job: Job) -> dict[str, Any]:
        payload = dict(job.payload)
        if job.operator_id and "operator_id" not in payload:
            payload["operator_id"] = job.operator_id
        return await index_items(pool, embedder, payload)

    return run
