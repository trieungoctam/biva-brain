"""Job ``logic.examples`` (M3, S3.4.2): chạy ví dụ của nhà xe này trên code của nhà xe tương tự.

Luồng (docs/logic-knowledge.md §5.3 — tầng hành vi, đáng tin nhất):
1. Ví dụ = logic_tests của nhà xe (cases.yaml đã index) theo capability.
2. Ứng viên = các nhà xe khác có hồ sơ logic cùng capability; code lấy từ repo biva-integrations
   (BIVA_INTEGRATIONS_DIR/BIVA_INTEGRATIONS_REPO): module chuẩn + hook của nhà xe đó, tham số
   lấy từ profile (chỉ ``value``; ``source`` chỉ để truy vết). Mode api/handoff không phải hàm
   thuần → bỏ qua (adapter thật nằm ngoài sandbox).
3. Mỗi case chạy trong sandbox (S3.4.1); so ``out`` chính xác hoặc ``error`` chứa thông điệp.
Kết quả: {operator, mode, pass_rate, passed, failed:[{input, expected, got, note}]} theo ứng viên.
"""

from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any

import asyncpg

from biva_worker import logic_sync
from biva_worker.runner import Job, PermanentError
from biva_worker.sandbox import run_code

MAX_CANDIDATES = 5
MAX_CASES = 30

GLUE = """
{module}

{hooks}

def __biva_entry(**payload):
    for pname, fn_name in {hooks_map}:
        payload[pname] = globals()[fn_name]
    return {entry}(**payload)
"""


def repo_dir() -> tuple[Path, str | None]:
    """(workdir, tmpdir cần dọn | None). URL thì clone tạm; path thì đọc trực tiếp."""
    src = os.environ.get("BIVA_INTEGRATIONS_DIR") or os.environ.get("BIVA_INTEGRATIONS_REPO", "")
    if not src:
        raise PermanentError("thiếu BIVA_INTEGRATIONS_DIR/REPO (repo biva-integrations)", code="NO_REPO")
    workdir, _, tmp = logic_sync.resolve_repo(src)
    return workdir, tmp


def parse_entrypoint(s: str) -> tuple[str, str]:
    file, func = s.split(":")
    return file, func


def build_code(
    workdir: Path, module_path: str, entrypoint: str, hook_files: dict[str, str], hooks_base: Path
) -> str:
    """Ghép module + hook + glue; ref hook tính từ thư mục nhà xe (quy ước profile.yaml)."""
    module_code = (workdir / module_path / parse_entrypoint(entrypoint)[0]).read_text(encoding="utf-8")
    hooks_code = []
    hooks_map: list[list[str]] = []
    for param, ref in hook_files.items():
        f, fn = parse_entrypoint(ref)
        hooks_code.append((hooks_base / f).read_text(encoding="utf-8"))
        hooks_map.append([param, fn])
    return GLUE.format(
        module=module_code,
        hooks="\n\n".join(hooks_code),
        hooks_map=repr(hooks_map),
        entry=parse_entrypoint(entrypoint)[1],
    )


def compare(expected: dict | None, got: dict) -> bool:
    if not expected:
        return False
    if "out" in expected:
        return got.get("ok") and got.get("value") == expected["out"]
    if "error" in expected:
        want = str(expected["error"])[:40].lower()
        return (not got.get("ok")) and want in str(got.get("error", "")).lower()
    return False


async def run_examples(
    pool: asyncpg.Pool, operator: str, capability: str, candidates: list[str] | None
) -> dict[str, Any]:
    async with pool.acquire() as con:
        cases = await con.fetch(
            """SELECT input::text AS input, expected::text AS expected, note FROM logic_tests
               WHERE operator_id = $1 AND capability = $2 ORDER BY note LIMIT $3""",
            operator,
            capability,
            MAX_CASES,
        )
        if not cases:
            raise PermanentError(
                f"nhà xe {operator} chưa có ví dụ (logic_tests) cho {capability}", code="NO_EXAMPLES"
            )
        profiles = await con.fetch(
            """SELECT p.operator_id, p.mode, p.module_id, p.module_version, p.entrypoint,
                      p.params::text AS params, p.hooks::text AS hooks
               FROM logic_profiles p
               WHERE p.capability = $1 AND p.status = 'active' AND p.operator_id <> $2""",
            capability,
            operator,
        )
        modules = {
            m["id"]: m
            for m in await con.fetch(
                """SELECT DISTINCT ON (id) id, path, entrypoint FROM logic_modules
               WHERE capability = $1 AND status = 'active' ORDER BY id, version DESC""",
                capability,
            )
        }

    wanted = set(candidates or []) or None
    workdir, tmp = repo_dir()
    try:
        results = await _run_candidates(workdir, profiles, modules, wanted, cases)
    finally:
        if tmp:
            import shutil

            shutil.rmtree(tmp, ignore_errors=True)
    best = results[0] if results else None
    return {
        "status": "done",
        "summary": (
            f"tốt nhất: {best['operator']} {best.get('pass_rate', 0):.0%}"
            if best and "pass_rate" in best
            else "chưa có ứng viên chạy được"
        ),
        "capability": capability,
        "cases": len(cases),
        "results": results,
        "counts": {"candidates": len(results), "cases": len(cases)},
    }


async def _run_candidates(workdir, profiles, modules, wanted, cases) -> list[dict]:
    results = []
    for p in profiles:
        if wanted and p["operator_id"] not in wanted:
            continue
        if len(results) >= MAX_CANDIDATES:
            break
        entry: dict[str, Any] = {"operator": p["operator_id"], "mode": p["mode"], "passed": 0, "failed": []}
        if p["mode"] in ("api", "handoff"):
            entry["skipped"] = "mode không phải hàm thuần"
            results.append(entry)
            continue
        mod = modules.get(p["module_id"]) if p["mode"] in ("config", "hook") else None
        if p["mode"] == "custom":
            # entrypoint của custom tính từ thư mục nhà xe (vd custom/x.py:h) — file không lặp "custom/".
            entrypoint = p["entrypoint"]
            module_path = f"operators/{p['operator_id']}"
        elif mod:
            entrypoint = mod["entrypoint"]
            module_path = mod["path"]
        else:
            entry["skipped"] = f"không tìm thấy module {p['module_id']}"
            results.append(entry)
            continue
        try:
            code = build_code(
                workdir,
                module_path,
                entrypoint,
                json.loads(p["hooks"] or "{}"),
                workdir / "operators" / p["operator_id"],
            )
        except FileNotFoundError as exc:
            entry["skipped"] = f"thiếu file code: {exc.filename or exc}"
            results.append(entry)
            continue
        params = {k: v.get("value") for k, v in (json.loads(p["params"] or "{}")).items()}
        for c in cases:
            got = run_code(code, "__biva_entry", {**json.loads(c["input"]), **params})
            if compare(json.loads(c["expected"]), got):
                entry["passed"] += 1
            else:
                entry["failed"].append(
                    {
                        "input": json.loads(c["input"]),
                        "expected": json.loads(c["expected"]),
                        "got": got.get("value") if got.get("ok") else got.get("error"),
                        "note": c["note"] or "",
                    }
                )
        entry["pass_rate"] = round(entry["passed"] / len(cases), 3)
        results.append(entry)
    results.sort(key=lambda r: (-r.get("pass_rate", -1), r["operator"]))
    return results


def handler(pool: asyncpg.Pool):
    async def run(job: Job) -> dict[str, Any]:
        operator = job.payload.get("operator_id") or job.operator_id
        capability = job.payload.get("capability")
        if not operator or not capability:
            raise PermanentError("thiếu operator_id hoặc capability", code="INVALID_PAYLOAD")
        return await run_examples(pool, operator, capability, job.payload.get("candidates"))

    return run
