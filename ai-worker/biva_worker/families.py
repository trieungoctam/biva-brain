"""Họ logic (M3, S3.4.3 + S3.2.3): gom cụm logic spec của các nhà xe theo capability.

Mỗi lần job promote chạy: với từng capability, union-find các spec theo độ trùng feature
(Jaccard >= 0.6 — chuẩn hoá theo danh mục, không theo embedding), cụm >= 2 nhà xe thành một họ:
- centroid_features = feature chung của mọi thành viên;
- recommended_implementation = (mode, module) phổ biến nhất trong họ;
- promote_candidate = cùng mode hook/custom ở >= 3 thành viên (docs/logic-knowledge.md §8: đề xuất
  promote thành tham số / hook chuẩn, kèm danh sách nhà xe được lợi — hiện qua list_logic_families;
  phần tương đồng code (embedding chunks) và behavior (run_examples_against) bổ sung vào
  logic_similarity khi có dữ liệu nhiều nhà xe).
"""

from __future__ import annotations

from typing import Any

import asyncpg

MIN_OVERLAP = 0.6
MIN_PROMOTE = 3


def feature_ids(features: list) -> set:
    return {f["id"] for f in features}


def jaccard(a: set[str], b: set[str]) -> float:
    if not a or not b:
        return 0.0
    return len(a & b) / len(a | b)


def clusters(specs: list[dict]) -> list[list[dict]]:
    parent = list(range(len(specs)))

    def find(i: int) -> int:
        while parent[i] != i:
            parent[i] = parent[parent[i]]
            i = parent[i]
        return i

    for i in range(len(specs)):
        for j in range(i + 1, len(specs)):
            if specs[i]["operator"] == specs[j]["operator"]:
                continue
            if jaccard(specs[i]["features"], specs[j]["features"]) >= MIN_OVERLAP:
                parent[find(i)] = find(j)
    groups: dict[int, list[dict]] = {}
    for i, sp in enumerate(specs):
        groups.setdefault(find(i), []).append(sp)
    return [g for g in groups.values() if len({s["operator"] for s in g}) >= 2]


def family_rows(capability: str, group: list[dict]) -> dict[str, Any]:
    common: set[str] | None = None
    for s in group:
        f = s["features"]
        common = f if common is None else (common & f)
    common = common or set()
    modes = {}
    for s in group:
        impl = s.get("implementation") or {}
        if impl.get("mode"):
            key = (impl["mode"], impl.get("module") or "")
            modes[key] = modes.get(key, 0) + 1
    rec = max(modes, key=modes.get) if modes else None
    custom_hooks = sum(1 for s in group if (s.get("implementation") or {}).get("mode") in ("hook", "custom"))
    slug = sorted(common)[0].split(".", 1)[-1] if common else "mixed"
    return {
        "id": f"{capability}.{slug}",
        "capability": capability,
        "name": f"Họ {capability}: {', '.join(sorted(common)[:3]) or 'lai'} ({len(group)} nhà xe)",
        "centroid_features": sorted(common),
        "members": sorted({s["operator"] for s in group}),
        "recommended_implementation": ({"mode": rec[0], "module": rec[1]} if rec else None),
        "promote_candidate": custom_hooks >= MIN_PROMOTE,
    }


async def update_families(pool: asyncpg.Pool) -> dict[str, Any]:
    async with pool.acquire() as con:
        rows = await con.fetch(
            """SELECT s.operator_id, s.capability, s.features::text, s.implementation::text
               FROM logic_specs s WHERE s.status = 'active'"""
        )
        specs = [
            {
                "operator": r["operator_id"],
                "capability": r["capability"],
                "features": feature_ids(_loads(r["features"]) or []),
                "implementation": _loads(r["implementation"]),
                "id": f"{r['operator_id']}/{r['capability']}",
            }
            for r in rows
        ]
        # Cache điểm tương đồng spec mọi cặp cùng capability (dùng cho so sánh/điều tra sau).
        by_cap: dict[str, list[dict]] = {}
        for sp in specs:
            by_cap.setdefault(sp["capability"], []).append(sp)
        pairs = []
        for cap, group in by_cap.items():
            for i, a in enumerate(group):
                for b in group[i + 1 :]:
                    pairs.append(
                        (
                            cap,
                            min(a["operator"], b["operator"]),
                            max(a["operator"], b["operator"]),
                            jaccard(a["features"], b["features"]),
                        )
                    )
        if pairs:
            await con.executemany(
                """INSERT INTO logic_similarity (capability, operator_a, operator_b, spec_score, computed_at)
                   VALUES ($1, $2, $3, $4, now())
                   ON CONFLICT (capability, operator_a, operator_b)
                   DO UPDATE SET spec_score = EXCLUDED.spec_score, computed_at = now()""",
                pairs,
            )

        n = 0
        keep = set()
        for cap, group in by_cap.items():
            for cl in clusters(group):
                fam = family_rows(cap, cl)
                keep.add(fam["id"])
                await con.execute(
                    """INSERT INTO logic_families (id, capability, name, centroid_features, members,
                           recommended_implementation, promote_candidate, updated_at)
                       VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, now())
                       ON CONFLICT (id) DO UPDATE SET
                           name = EXCLUDED.name, centroid_features = EXCLUDED.centroid_features,
                           members = EXCLUDED.members,
                           recommended_implementation = EXCLUDED.recommended_implementation,
                           promote_candidate = EXCLUDED.promote_candidate, updated_at = now()""",
                    fam["id"],
                    fam["capability"],
                    fam["name"],
                    fam["centroid_features"],
                    fam["members"],
                    fam["recommended_implementation"],
                    fam["promote_candidate"],
                )
                n += 1
        # Họ không còn thành viên đủ điều kiện → bỏ (spec đổi).
        await con.execute("DELETE FROM logic_families WHERE id <> ALL($1::text[])", sorted(keep))
        return {
            "status": "done",
            "summary": f"{n} họ logic",
            "counts": {
                "families": n,
                "promote_candidates": await con.fetchval(
                    "SELECT count(*) FROM logic_families WHERE promote_candidate"
                ),
            },
        }


def _loads(s: str | None) -> Any:
    import json

    return json.loads(s) if s and s != "null" else None
