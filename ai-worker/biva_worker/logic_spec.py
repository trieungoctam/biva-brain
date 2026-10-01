"""Job ``logic.spec`` (M2, S2.5.3 extract_logic_spec): dựng logic spec của nhà xe từ tri thức đã duyệt.

Spec là "dấu vân tay" logic theo capability (docs/logic-knowledge.md §4) — **có trước code**: với khách
mới, spec được dựng từ chính sách + ví dụ nhà xe đã duyệt; ``implementation`` chỉ điền khi có hồ sơ
(logic_profiles). LLM chỉ được ánh xạ vào feature **có sẵn** trong danh mục L1 (logic_features, seed từ
kb/L1/<ngành>/features.yaml); feature chưa khớp → ``proposed_features`` chờ review, không tự thêm.

Idempotent theo nội dung: chạy lại cùng tri thức → ghi đè spec (UNIQUE operator+capability), id giữ nguyên.
"""

from __future__ import annotations

import json
from typing import Any

import asyncpg

from biva_worker.embed import Embedder
from biva_worker.index import to_pgvector
from biva_worker.llm import LLMClient
from biva_worker.llm.client import Request
from biva_worker.runner import Job, PermanentError

ACTOR = "ai:logic_spec"
MAX_ITEMS = 200

SYSTEM = """\
Bạn là chuyên gia logic nghiệp vụ của nhà xe khách. Nhiệm vụ: từ tri thức đã duyệt (chính sách, quy tắc,
bài học, data) của MỘT nhà xe, dựng "logic spec" theo capability (fare, schedule, booking, pickup,
transfer, parcel…):

1. Chọn các feature trong DANH MỤC cho sẵn mà nhà xe này CÓ (không đoán — chỉ khi tri thức nói rõ).
   Với feature có tham số, điền tham số đúng type từ tri thức (vd holiday_surcharge {tet: 0.20}).
2. rules_text: diễn đạt lại mỗi quy tắc logic bằng MỘT câu ngắn đã chuẩn hoá (để so sánh giữa các nhà xe).
3. source_item_ids: id của item làm bằng chứng cho từng feature (trong danh sách item cho sẵn).
4. Quy tắc không khớp feature nào → đưa vào proposed_features (id mới theo mẫu <capability>.<tên>,
   kèm reason) — tuyệt đối không tự bịa feature trong danh sách feature của spec.
Chỉ dùng capability và id feature/item cho trước. Trả JSON theo schema."""


def output_schema() -> dict[str, Any]:
    return {
        "type": "object",
        "additionalProperties": False,
        "required": ["specs", "proposed_features"],
        "properties": {
            "specs": {
                "type": "array",
                "items": {
                    "type": "object",
                    "additionalProperties": False,
                    "required": ["capability", "features", "rules_text", "source_item_ids"],
                    "properties": {
                        "capability": {"type": "string", "pattern": "^[a-z][a-z0-9_]*$"},
                        "features": {
                            "type": "array",
                            "items": {
                                "type": "object",
                                "additionalProperties": False,
                                "required": ["id"],
                                "properties": {
                                    "id": {"type": "string"},
                                    "params": {"type": "object"},
                                },
                            },
                        },
                        "rules_text": {"type": "array", "items": {"type": "string", "minLength": 4}},
                        "source_item_ids": {"type": "array", "items": {"type": "string"}},
                    },
                },
            },
            "proposed_features": {
                "type": "array",
                "items": {
                    "type": "object",
                    "additionalProperties": False,
                    "required": ["id", "capability", "description", "reason"],
                    "properties": {
                        "id": {"type": "string"},
                        "capability": {"type": "string"},
                        "description": {"type": "string"},
                        "reason": {"type": "string"},
                    },
                },
            },
        },
    }


def build_prompt(operator: str, items: list[dict], features: list[dict], tests: list[dict]) -> str:
    parts = [
        f"Nhà xe: {operator}",
        "\nDANH MỤC FEATURE (chỉ được dùng những feature này):",
        *[f"- {f['id']} [{f['capability']}]: {f['description']}" for f in features],
        "\nTRI THỨC ĐÃ DUYỆT (id | topic | kind | nội dung):",
        *[f"- {i['id']} | {i['topic']} | {i['kind']} | {i['text']}" for i in items],
    ]
    if tests:
        parts.append("\nVÍ DỤ NHÀ XE (input → expected):")
        parts += [
            f"- [{t['capability']}] {json.dumps(t['input'], ensure_ascii=False)} → "
            f"{json.dumps(t['expected'], ensure_ascii=False)} ({t['note'] or ''})"
            for t in tests
        ]
    return "\n".join(parts)


def validate_specs(
    data: dict[str, Any], catalog: dict[str, dict], item_ids: set[str], operator: str
) -> dict[str, Any]:
    """Chỉ giữ feature có trong catalog và item thuộc nhà xe; raise khi LLM bịa capability."""
    if not data.get("specs"):
        raise PermanentError("spec rỗng — tri thức chưa đủ để dựng logic spec", code="EMPTY_SPEC")
    seen_caps: set[str] = set()
    catalog_caps = {f["capability"] for f in catalog.values()}
    for spec in data["specs"]:
        cap = spec["capability"]
        if cap not in catalog_caps:
            raise PermanentError(
                f"capability {cap} không có trong danh mục feature", code="UNKNOWN_CAPABILITY"
            )
        if cap in seen_caps:
            raise PermanentError(f"capability {cap} xuất hiện 2 lần", code="INVALID_SPEC")
        seen_caps.add(cap)
        kept, bad = [], []
        for feat in spec["features"]:
            if feat["id"] in catalog:
                kept.append({"id": feat["id"], "params": feat.get("params") or {}})
            else:
                bad.append(feat["id"])
        if bad:
            raise PermanentError(
                f"feature {', '.join(bad)} không có trong danh mục — đưa vào proposed_features thay vì spec",
                code="UNKNOWN_FEATURE",
            )
        spec["features"] = kept
        spec["source_item_ids"] = [i for i in spec["source_item_ids"] if i in item_ids]
        if not spec["features"] and not spec["rules_text"]:
            raise PermanentError(
                f"spec {operator}/{cap} không có feature lẫn rules_text", code="INVALID_SPEC"
            )
    data["proposed_features"] = [
        p
        for p in data.get("proposed_features", [])
        if p["id"] not in catalog and p.get("capability") in {f["capability"] for f in catalog.values()}
    ]
    return data


async def extract(
    pool: asyncpg.Pool, llm: LLMClient, embedder: Embedder, operator: str, job: Job
) -> dict[str, Any]:
    async with pool.acquire() as con:
        if not await con.fetchval("SELECT EXISTS (SELECT 1 FROM operators WHERE id = $1)", operator):
            raise PermanentError(f"không có nhà xe {operator}", code="UNKNOWN_OPERATOR")
        features = [
            dict(r)
            for r in await con.fetch(
                """SELECT id, capability, description, params_schema::text AS params_schema
               FROM logic_features WHERE status = 'active' ORDER BY capability, id"""
            )
        ]
        items = [
            dict(r)
            for r in await con.fetch(
                """SELECT id::text, topic, kind, text FROM items
               WHERE operator_id = $1 AND status = 'active' AND kind <> 'data'
               ORDER BY topic, kind LIMIT $2""",
                operator,
                MAX_ITEMS,
            )
        ]
        if not items:
            raise PermanentError(f"nhà xe {operator} chưa có tri thức đã duyệt", code="NO_KNOWLEDGE")
        tests = [
            dict(r)
            for r in await con.fetch(
                """SELECT t.id::text, t.capability, t.input::text AS input,
                          t.expected::text AS expected, t.note
               FROM logic_tests t WHERE t.operator_id = $1 LIMIT 50""",
                operator,
            )
        ]
        profiles = {
            r["capability"]: dict(r)
            for r in await con.fetch(
                """SELECT capability, mode, module_id, module_version, hooks::text AS hooks
               FROM logic_profiles WHERE operator_id = $1""",
                operator,
            )
        }

    catalog = {f["id"]: f for f in features}
    result = await llm.complete(
        Request(
            purpose="logic",
            system=SYSTEM,
            messages=[{"role": "user", "content": build_prompt(operator, items, features, tests)}],
            schema=output_schema(),
            max_tokens=8000,
            operator_id=operator,
            operation_id=job.id,
        )
    )
    data = validate_specs(result.data, catalog, {i["id"] for i in items}, operator)

    async with pool.acquire() as con:
        async with con.transaction():
            for spec in data["specs"]:
                cap = spec["capability"]
                impl = None
                if cap in profiles:
                    p = profiles[cap]
                    impl = {
                        "mode": p["mode"],
                        "module": p["module_id"],
                        "module_version": p["module_version"],
                        "hooks": json.loads(p["hooks"] or "{}"),
                    }
                example_ids = [t["id"] for t in tests if t["capability"] == cap]
                await con.execute(
                    """INSERT INTO logic_specs (operator_id, capability, features, rules_text,
                           source_item_ids, example_ids, proposed_features,
                           implementation, created_by, updated_at)
                       VALUES ($1, $2, $3, $4, $5::uuid[], $6::uuid[], $7, $8, $9, now())
                       ON CONFLICT (operator_id, capability) DO UPDATE SET
                           features = EXCLUDED.features, rules_text = EXCLUDED.rules_text,
                           source_item_ids = EXCLUDED.source_item_ids, example_ids = EXCLUDED.example_ids,
                           proposed_features = EXCLUDED.proposed_features,
                           implementation = EXCLUDED.implementation, status = 'active', updated_at = now()""",
                    operator,
                    cap,
                    spec["features"],
                    spec["rules_text"],
                    spec["source_item_ids"],
                    example_ids,
                    data["proposed_features"],
                    impl,
                    ACTOR,
                )

    # Embedding rules_text (so ngữ nghĩa giữa các nhà xe): TEI lỗi → NULL, keyword vẫn dùng được.
    embedded = 0
    try:
        rows = await pool.fetch(
            "SELECT capability, rules_text FROM logic_specs WHERE operator_id = $1", operator
        )
        for r in rows:
            text = "\n".join(r["rules_text"])
            if not text:
                continue
            vec = await embedder.embed([text])
            await pool.execute(
                "UPDATE logic_specs SET embedding = $2::vector WHERE operator_id = $1 AND capability = $3",
                operator,
                to_pgvector(vec[0]),
                r["capability"],
            )
            embedded += 1
    except Exception:
        embedded = 0

    return {
        "status": "done",
        "summary": f"đã dựng logic spec cho {len(data['specs'])} capability của {operator}",
        "counts": {
            "specs": len(data["specs"]),
            "proposed_features": len(data["proposed_features"]),
            "embedded": embedded,
        },
        "proposed_features": data["proposed_features"],
    }


def handler(pool: asyncpg.Pool, llm: LLMClient, embedder: Embedder):
    async def run(job: Job) -> dict[str, Any]:
        operator = job.payload.get("operator_id") or job.operator_id
        if not operator:
            raise PermanentError("thiếu operator_id", code="INVALID_PAYLOAD")
        return await extract(pool, llm, embedder, operator, job)

    return run
