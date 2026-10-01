"""Job ``promote`` (M3, S3.2.2): tìm observation giống nhau ở ≥ 3 nhà xe → đề xuất rule L1 (PROMOTE).

- So observation L2 (kind='observation') giữa các nhà xe theo từng topic: cặp nào cosine ≥ 0.7
  (có embedding) hoặc Jaccard bigram ≥ 0.6 (fallback keyword) coi là cùng thông lệ.
- Gom cụm (union-find); cụm đủ ≥ 3 nhà xe và chưa có review PROMOTE (open/applied) cùng nội dung
  → tạo item L1 pending + review_items PROMOTE (operator_id = nhà xe đại diện, danh sách đầy đủ
  trong after.operators). Builder duyệt qua apply_review như mọi đề xuất — L1 chỉ vào sau khi duyệt.
"""

from __future__ import annotations

from typing import Any

import asyncpg

from biva_worker import textnorm
from biva_worker.runner import Job

MIN_OPERATORS = 3
COSINE_SAME = 0.7
JACCARD_SAME = 0.45  # bigram keyword thận trọng; cosine (embedding) vẫn 0.7


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


def _jaccard(a: str, b: str) -> float:
    sa = set(textnorm.search_text(a).split())
    sb = set(textnorm.search_text(b).split())
    if not sa or not sb:
        return 0.0
    return len(sa & sb) / len(sa | sb)


def sentences(text: str) -> list[str]:
    return [line[2:] for line in text.splitlines() if line.startswith("- ")] or [text]


def same_practice(a: dict, b: dict) -> float:
    """Mức giống nhau của 2 observation = similarity CAÂU GIỐNG NHẤT giữa chúng
    (một observation gom nhiều câu; so cả khối sẽ bị câu riêng của mỗi nhà xe pha loãng)."""
    if a["embedding"] and b["embedding"]:
        return _cosine(a["embedding"], b["embedding"])
    best = 0.0
    for sa in sentences(a["text"]):
        for sb in sentences(b["text"]):
            best = max(best, _jaccard(sa, sb))
    return best * 0.9  # keyword thận trọng hơn embedding


def clusters(observations: list[dict]) -> list[list[dict]]:
    """Union-find theo tương tự; trả các cụm có >= MIN_OPERATORS nhà xe khác nhau."""
    parent = list(range(len(observations)))

    def find(i: int) -> int:
        while parent[i] != i:
            parent[i] = parent[parent[i]]
            i = parent[i]
        return i

    for i in range(len(observations)):
        for j in range(i + 1, len(observations)):
            if observations[i]["operator"] == observations[j]["operator"]:
                continue
            both_emb = bool(observations[i]["embedding"]) and bool(observations[j]["embedding"])
            thr = COSINE_SAME if both_emb else JACCARD_SAME
            if same_practice(observations[i], observations[j]) >= thr:
                parent[find(i)] = find(j)
    groups: dict[int, list[dict]] = {}
    for i, ob in enumerate(observations):
        groups.setdefault(find(i), []).append(ob)
    out = []
    for g in groups.values():
        if len({o["operator"] for o in g}) >= MIN_OPERATORS:
            out.append(sorted(g, key=lambda o: o["operator"]))
    return out


async def promote(pool: asyncpg.Pool) -> dict[str, Any]:
    async with pool.acquire() as con:
        rows = await con.fetch(
            """SELECT o.id::text, o.operator_id, o.topic, o.text, o.embedding::text
               FROM items o
               WHERE o.status = 'active' AND o.kind = 'observation' AND o.layer = 2"""
        )
        observations = [
            {
                "id": r["id"],
                "operator": r["operator_id"],
                "topic": r["topic"],
                "text": r["text"],
                "embedding": [float(x) for x in (r["embedding"] or "[]").strip("[]").split(",") if x],
            }
            for r in rows
        ]
        created = 0
        for cluster in clusters(observations):
            topic = cluster[0]["topic"]
            ops = [o["operator"] for o in cluster]
            rep = cluster[0]
            key = f"l1.promoted.{topic}.{rep['id'][:8]}"
            already = await con.fetchval(
                """SELECT EXISTS (SELECT 1 FROM review_items
                   WHERE change_kind = 'PROMOTE' AND key = $1 AND status IN ('open', 'applied'))""",
                key,
            )
            if already:
                continue
            async with con.transaction():
                item_id = await con.fetchval(
                    """INSERT INTO items (layer, operator_id, kind, topic, key, text, status, metadata)
                       VALUES (1, NULL, 'policy', $1, $2, $3, 'pending',
                               $4::jsonb) RETURNING id::text""",
                    topic,
                    key,
                    rep["text"],
                    {"source": "promote", "operators": ops, "from_observations": [o["id"] for o in cluster]},
                )
                await con.execute(
                    """INSERT INTO review_items (operator_id, key, topic, change_kind, risk, item_id,
                           after, reason, proposed_by)
                       VALUES ($1, $2, $3, 'PROMOTE', 'high', $4::uuid, $5::jsonb, $6, 'system:promote')""",
                    ops[0],
                    key,
                    topic,
                    item_id,
                    {"operators": ops, "text": rep["text"]},
                    f"{len(ops)} nhà xe có cùng thông lệ: {', '.join(ops)}",
                )
                created += 1
        return {
            "status": "done",
            "summary": f"{created} đề xuất promote lên L1" if created else "chưa có cụm đủ 3 nhà xe",
            "counts": {"promoted_proposals": created, "observations": len(observations)},
        }


def handler(pool: asyncpg.Pool):
    async def run(job: Job) -> dict[str, Any]:
        return await promote(pool)

    return run
