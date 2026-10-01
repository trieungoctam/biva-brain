"""Job ``consolidate`` (M3, S3.2.1): gom item active của một scope thành observation.

Quy tắc (theo Hindsight, docs/architecture.md §3.2):
- 1 facet (topic) / observation trong scope; observation là item ``kind='observation'`` — không vào
  knowledge pack / recall (đã lọc), chỉ dùng cho consolidate/promote.
- Ưu tiên update observation sẵn có; nguồn ghi vào ``observation_sources`` kèm trích đoạn;
  item nguồn đánh dấu ``consolidated_at`` để lần sau chỉ gom phần mới.
- Near-duplicate: câu trùng (cosine ≥ 0.97 khi cả hai có embedding, ngược lại cùng textnorm.fold)
  không lặp lại trong observation. Không tự tính toán, không bỏ hạn chế.
"""

from __future__ import annotations

from typing import Any

import asyncpg

from biva_worker import textnorm
from biva_worker.runner import Job

BATCH = 200
NEAR_DUP = 0.97


def _cosine(a: list[float], b: list[float]) -> float:
    n = min(len(a), len(b))
    dot = na = nb = 0.0
    for i in range(n):
        dot += a[i] * b[i]
        na += a[i] * a[i]
        nb += b[i] * b[i]
    if na == 0 or nb == 0:
        return 0.0
    return dot / ((na**0.5) * (nb**0.5))


def parse_vector(s: str | None) -> list[float]:
    if not s:
        return []
    return [float(x) for x in s.strip("[]").split(",") if x]


def merge_sentences(existing: list[dict], incoming: list[dict]) -> tuple[list[dict], list[dict]]:
    """Thêm câu mới không trùng (near-dup) vào observation; trả (câu giữ lại, câu vừa thêm)."""
    kept = list(existing)
    added: list[dict] = []
    for inc in incoming:
        dup = False
        for ex in kept:
            if inc["embedding"] and ex["embedding"]:
                if _cosine(inc["embedding"], ex["embedding"]) >= NEAR_DUP:
                    dup = True
                    break
            elif inc["fold"] == ex["fold"]:
                dup = True
                break
        if not dup:
            kept.append(inc)
            added.append(inc)
    return kept, added


async def consolidate(pool: asyncpg.Pool, operator_id: str | None) -> dict[str, Any]:
    """operator_id None = scope L0/L1 (thông lệ ngành). Trả số observation cập nhật/tạo."""
    async with pool.acquire() as con:
        async with con.transaction():
            where_scope = (
                "operator_id IS NULL AND layer <= 1"
                if operator_id is None
                else "operator_id = $1 AND layer = 2"
            )
            args: list[Any] = [operator_id] if operator_id else []
            rows = await con.fetch(
                f"""SELECT id::text, topic, text, embedding::text FROM items
                    WHERE status = 'active' AND kind <> 'observation' AND consolidated_at IS NULL
                      AND {where_scope}
                    ORDER BY topic, id LIMIT {BATCH}""",
                *args,
            )
            if not rows:
                return {
                    "status": "done",
                    "summary": "không có item mới để gom",
                    "counts": {"observations": 0, "sources": 0},
                }

            by_topic: dict[str, list[dict]] = {}
            for r in rows:
                by_topic.setdefault(r["topic"], []).append(
                    {
                        "id": r["id"],
                        "text": r["text"],
                        "fold": textnorm.fold(r["text"]),
                        "embedding": parse_vector(r["embedding"]),
                    }
                )

            n_obs = n_src = 0
            for topic, items in by_topic.items():
                key = f"obs.{topic}"
                obs_row = await con.fetchrow(
                    f"""SELECT id::text, text FROM items
                        WHERE status = 'active' AND kind = 'observation' AND key = $1
                          AND {
                        "operator_id IS NULL AND layer = 1"
                        if operator_id is None
                        else "operator_id = $2 AND layer = 2"
                    }""",
                    key,
                    *(args or []),
                )
                existing = []
                if obs_row:
                    existing = [
                        {"text": line, "fold": textnorm.fold(line), "embedding": []}
                        for line in obs_row["text"].splitlines()
                        if line.startswith("- ")
                    ]
                    for e in existing:
                        e["text"] = e["text"][2:]
                merged, added = merge_sentences(existing, items)
                new_text = "\n".join("- " + m["text"] for m in merged)
                if obs_row:
                    await con.execute(
                        "UPDATE items SET text = $2, updated_at = now(), consolidated_at = now()"
                        " WHERE id = $1::uuid",
                        obs_row["id"],
                        new_text,
                    )
                    obs_id = obs_row["id"]
                else:
                    obs_id = await con.fetchval(
                        """INSERT INTO items (layer, operator_id, kind, topic, key, text, status, metadata)
                           VALUES ($1, $2, 'observation', $3, $4, $5, 'active',
                                   $6::jsonb) RETURNING id::text""",
                        1 if operator_id is None else 2,
                        operator_id,
                        topic,
                        key,
                        new_text,
                        {"internal": "consolidate"},
                    )
                    n_obs += 1
                src_rows = [(obs_id, it["id"], it["text"]) for it in added]
                if src_rows:
                    await con.executemany(
                        """INSERT INTO observation_sources (observation_id, source_item_id, quote)
                           VALUES ($1::uuid, $2::uuid, $3) ON CONFLICT DO NOTHING""",
                        src_rows,
                    )
                    n_src += len(src_rows)
                await con.execute(
                    "UPDATE items SET consolidated_at = now() WHERE id = ANY($1::uuid[])",
                    [it["id"] for it in items],
                )
            return {
                "status": "done",
                "summary": f"đã gom {n_obs} observation mới, {n_src} nguồn",
                "counts": {"observations": n_obs, "sources": n_src},
            }


def handler(pool: asyncpg.Pool):
    async def run(job: Job) -> dict[str, Any]:
        op = job.payload.get("operator_id") or job.operator_id
        return await consolidate(pool, op)

    return run
