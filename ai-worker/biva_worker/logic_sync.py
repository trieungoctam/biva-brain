"""Job ``index_code`` (M2, S2.5.2): đồng bộ repo biva-integrations → tri thức logic trong Brain.

Đọc từ git (nguồn sự thật của code, docs/logic-knowledge.md §3.2-3.3):

1. ``modules/**/module.yaml`` và ``operators/*/custom/**/module.yaml`` → ``logic_modules``
   (validate schema ``logic.module``; bản (id, version) mới không đè bản cũ).
2. ``operators/*/profile.yaml`` → ``logic_profiles`` + ``logic_param_sources``
   (mỗi tham số có ``source`` là item tri thức — đây là chỗ Brain gắn stale khi tri thức đổi).
3. ``tests/cases.yaml`` của module và của nhà xe → ``logic_tests``.
4. Code ``*.py`` → ``code_chunks``: cắt theo hàm/lớp (AST), kèm tóm tắt (docstring đầu),
   ``search_text`` (textnorm) + embedding (TEI; TEI lỗi → NULL, keyword vẫn tìm được), gắn commit.
   Commit mới → chunk của commit cũ bị xoá khỏi repo đó (cơ chế "quên" của code).

Idempotent theo HEAD: commit đã đồng bộ (bảng ``logic_syncs``) → no-op, trả "không đổi" —
scheduler gọi mỗi phút nên "merge PR → Brain cập nhật" xảy ra trong ≤ 1 phút.

Nguồn repo: payload ``repo`` (URL git hoặc path checkout) hoặc env ``BIVA_INTEGRATIONS_REPO``.
URL thì clone --depth 1 vào thư mục tạm mỗi lần chạy (repo nhỏ); path thì đọc trực tiếp
(commit từ ``git rev-parse HEAD`` — checkout bẩn vẫn đọc được, xem như nội dung HEAD).
"""

from __future__ import annotations

import ast
import asyncio
import os
import shutil
import subprocess
import tempfile
from pathlib import Path
from typing import Any

import asyncpg

from biva_worker import textnorm
from biva_worker.contracts import validate
from biva_worker.embed import Embedder
from biva_worker.index import to_pgvector
from biva_worker.runner import Job, PermanentError, RetryableError


def _git(args: list[str], cwd: Path | None = None) -> str:
    proc = subprocess.run(["git", *args], cwd=cwd, capture_output=True, text=True, check=False)
    if proc.returncode != 0:
        raise PermanentError(f"git {args[0]} lỗi: {proc.stderr.strip()[:200]}", code="GIT_ERROR")
    return proc.stdout.strip()


def resolve_repo(source: str) -> tuple[Path, str, str | None]:
    """→ (workdir, tên repo, tmpdir cần dọn | None). URL thì clone; path thì dùng trực tiếp."""
    if source.startswith(("http://", "https://", "git@", "ssh://")):
        tmp = tempfile.mkdtemp(prefix="biva-integrations-")
        try:
            _git(["clone", "--depth", "1", source, tmp])
        except Exception:
            shutil.rmtree(tmp, ignore_errors=True)  # không để lại pack clone nửa chừng
            raise
        name = source.rstrip("/").removesuffix(".git").rsplit("/", 1)[-1] or "biva-integrations"
        return Path(tmp), name, tmp
    path = Path(source).resolve()
    if not (path / "modules").is_dir():
        raise PermanentError(f"repo logic không hợp lệ (thiếu modules/): {source}", code="INVALID_REPO")
    return path, path.name, None


def load_yaml(path: Path) -> dict[str, Any]:
    import yaml

    try:
        doc = yaml.safe_load(path.read_text(encoding="utf-8"))
    except yaml.YAMLError as exc:
        raise PermanentError(f"{path.name}: YAML lỗi — {exc}") from exc
    if not isinstance(doc, dict):
        raise PermanentError(f"{path.name}: phải là mapping") from None
    return doc


def validate_or_raise(schema: str, doc: dict[str, Any], where: str) -> None:
    errors = validate(schema, doc)
    if errors:
        raise PermanentError(f"{where}: " + "; ".join(errors), code="INVALID_LOGIC_YAML")


# ───────────────────────────── Code chunk theo hàm/lớp ─────────────────────────────


def chunk_file(path: Path, root: Path) -> list[dict[str, str]]:
    """Cắt một file Python theo hàm/lớp (kèm method của class) — không theo số dòng."""
    source = path.read_text(encoding="utf-8")
    tree = ast.parse(source)
    lines = source.splitlines(keepends=True)
    chunks: list[dict[str, str]] = []

    def doc_head(node: ast.AST) -> str:
        doc = ast.get_docstring(node)
        return (doc or "").strip().splitlines()[0] if doc else ""

    def add(symbol: str, node: ast.AST) -> None:
        text = "".join(lines[node.lineno - 1 : node.end_lineno])
        chunks.append({"symbol": symbol, "text": text, "summary": doc_head(node)})

    for node in tree.body:
        if isinstance(node, ast.FunctionDef | ast.AsyncFunctionDef):
            add(node.name, node)
        elif isinstance(node, ast.ClassDef):
            add(node.name, node)
            for sub in node.body:
                if isinstance(sub, ast.FunctionDef | ast.AsyncFunctionDef):
                    add(f"{node.name}.{sub.name}", sub)
    return chunks


def module_context(path: Path, root: Path) -> tuple[str | None, str | None]:
    """Chunk này thuộc module nào (walk lên tìm module.yaml) / nhà xe nào (operators/<id>/…)."""
    rel = path.relative_to(root)
    parts = rel.parts
    operator = parts[1] if len(parts) >= 2 and parts[0] == "operators" else None
    module_id: str | None = None
    for parent in [path, *path.parents][: len(parts) + 1]:
        manifest = parent / "module.yaml"
        if manifest.is_file():
            module_id = load_yaml(manifest).get("id")
            break
        if parent == root:
            break
    return module_id, operator


async def _embed_missing(pool: asyncpg.Pool, embedder: Embedder, repo: str, commit: str) -> int:
    """Nhúng các chunk của (repo, commit) còn thiếu vector; TEI lỗi thì raise RetryableError."""
    rows = await pool.fetch(
        "SELECT id, text FROM code_chunks WHERE repo = $1 AND commit = $2 AND embedding IS NULL",
        repo,
        commit,
    )
    if rows:
        vectors = await embedder.embed([r["text"] for r in rows])
        await pool.executemany(
            "UPDATE code_chunks SET embedding = $2::vector WHERE id = $1",
            [(r["id"], to_pgvector(v)) for r, v in zip(rows, vectors, strict=True)],
        )
    return len(rows)


# ───────────────────────────────────── Đồng bộ ─────────────────────────────────────


async def sync(pool: asyncpg.Pool, embedder: Embedder, source: str) -> dict[str, Any]:
    # resolve_repo chạy git clone đồng bộ (không timeout) — đẩy sang thread để
    # không chặn event loop/heartbeat lease của worker.
    workdir, repo, tmp = await asyncio.to_thread(resolve_repo, source)
    try:
        commit = _git(["rev-parse", "HEAD"], cwd=workdir)
        row = await pool.fetchrow("SELECT commit FROM logic_syncs WHERE repo = $1", repo)
        if row and row["commit"] == commit:
            # HEAD không đổi, nhưng lần trước TEI có thể lỗi → chunk còn embedding NULL.
            # Phải hoàn thiện nốt: không thì chunk thiếu vector mãi đến khi repo có commit mới.
            missing = await pool.fetchval(
                "SELECT count(*) FROM code_chunks WHERE repo = $1 AND commit = $2 AND embedding IS NULL",
                repo,
                commit,
            )
            if not missing:
                return {
                    "status": "done",
                    "summary": "HEAD không đổi — bỏ qua",
                    "commit": commit,
                    "changed": False,
                }
            return {
                "status": "done",
                "summary": f"HEAD không đổi — hoàn thiện embedding cho {missing} chunk",
                "commit": commit,
                "changed": False,
                "embedded": await _embed_missing(pool, embedder, repo, commit),
            }

        counts = {"modules": 0, "profiles": 0, "tests": 0, "chunks": 0}
        async with pool.acquire() as con:
            async with con.transaction():
                # 1. module.yaml — module chuẩn L1 và custom L2 của nhà xe.
                for manifest_path in sorted(workdir.glob("modules/**/module.yaml")) + sorted(
                    workdir.glob("operators/*/custom/**/module.yaml")
                ):
                    doc = load_yaml(manifest_path)
                    validate_or_raise("logic.module", doc, str(manifest_path.relative_to(workdir)))
                    rel_dir = manifest_path.parent.relative_to(workdir).as_posix()
                    operator = rel_dir.split("/")[1] if rel_dir.startswith("operators/") else None
                    if doc["layer"] == "L2" and not operator:
                        raise PermanentError(
                            f"{rel_dir}: module L2 phải nằm trong operators/<id>/custom/",
                            code="INVALID_LOGIC_YAML",
                        )
                    await con.execute(
                        """INSERT INTO logic_modules (id, version, layer, operator_id, capability, summary,
                               entrypoint, features, params_schema, hooks, required_tests, deprecated_by,
                               repo, path, commit, synced_at)
                           VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15, now())
                           ON CONFLICT (id, version) DO UPDATE SET
                               layer = EXCLUDED.layer, operator_id = EXCLUDED.operator_id,
                               capability = EXCLUDED.capability, summary = EXCLUDED.summary,
                               entrypoint = EXCLUDED.entrypoint, features = EXCLUDED.features,
                               params_schema = EXCLUDED.params_schema, hooks = EXCLUDED.hooks,
                               required_tests = EXCLUDED.required_tests,
                               deprecated_by = EXCLUDED.deprecated_by,
                               repo = EXCLUDED.repo, path = EXCLUDED.path, commit = EXCLUDED.commit,
                               synced_at = now()""",
                        doc["id"],
                        doc["version"],
                        doc["layer"],
                        operator,
                        doc["capability"],
                        doc["summary"],
                        doc["entrypoint"],
                        doc.get("features", []),
                        doc.get("params_schema", {}),
                        doc.get("hooks", []),
                        doc.get("required_tests", []),
                        doc.get("deprecated_by"),
                        repo,
                        rel_dir,
                        commit,
                    )
                    counts["modules"] += 1

                # 2. profile.yaml — hồ sơ logic nhà xe + nguồn tham số.
                for profile_path in sorted(workdir.glob("operators/*/profile.yaml")):
                    rel = profile_path.relative_to(workdir)
                    operator = rel.parts[1]
                    doc = load_yaml(profile_path)
                    validate_or_raise("logic.profile", doc, str(rel))
                    if doc["operator"] != operator:
                        raise PermanentError(
                            f"{rel}: operator khai báo {doc['operator']!r} "
                            f"nhưng nằm trong thư mục {operator!r}",
                            code="INVALID_LOGIC_YAML",
                        )
                    # Nhà xe chưa onboard trong Brain → bỏ qua profile (FK operators); vì còn skipped nên
                    # logic_syncs không được ghi ở dưới — tick kế thử lại (upsert idempotent).
                    known = await con.fetchval(
                        "SELECT EXISTS (SELECT 1 FROM operators WHERE id = $1)", operator
                    )
                    if not known:
                        counts.setdefault("skipped_profiles", 0)
                        counts["skipped_profiles"] += 1
                        continue
                    for capability, cap in doc["capabilities"].items():
                        params = cap.get("params", {})
                        module_ref = cap.get("module", "")
                        mod_id, _, mod_ver = module_ref.partition("@")
                        prof_id = await con.fetchval(
                            """INSERT INTO logic_profiles (operator_id, capability, mode, module_id,
                                   module_version, params, hooks, entrypoint, decision_id, derived_from,
                                   commit, synced_at)
                               VALUES ($1,$2,$3,$4,$5::int,$6::jsonb,$7::jsonb,$8,$9,$10,$11, now())
                               ON CONFLICT (operator_id, capability) DO UPDATE SET
                                   mode = EXCLUDED.mode, module_id = EXCLUDED.module_id,
                                   module_version = EXCLUDED.module_version,
                                   params = EXCLUDED.params,
                                   hooks = EXCLUDED.hooks, entrypoint = EXCLUDED.entrypoint,
                                   decision_id = EXCLUDED.decision_id, derived_from = EXCLUDED.derived_from,
                                   commit = EXCLUDED.commit, status = 'active', synced_at = now()
                               RETURNING id""",
                            operator,
                            capability,
                            cap["mode"],
                            mod_id,
                            int(mod_ver or 1),
                            params,
                            cap.get("hooks", {}),
                            cap.get("entrypoint"),
                            cap.get("decision"),
                            cap.get("derived_from"),
                            commit,
                        )
                        await con.execute("DELETE FROM logic_param_sources WHERE profile_id = $1", prof_id)
                        rows = [(prof_id, name, p["source"]) for name, p in params.items() if p.get("source")]
                        if rows:
                            await con.executemany(
                                """INSERT INTO logic_param_sources (profile_id, param_path, item_id)
                                   VALUES ($1, $2, $3::uuid)
                                   ON CONFLICT DO NOTHING""",
                                rows,
                            )
                        counts["profiles"] += 1

                # 3. tests/cases.yaml — của module (không nhà xe) và của nhà xe.
                for cases_path in sorted(workdir.glob("**/tests/cases.yaml")):
                    rel = cases_path.relative_to(workdir)
                    doc = load_yaml(cases_path)
                    validate_or_raise("logic.cases", doc, str(rel))
                    parts = rel.parts
                    operator = parts[1] if parts[0] == "operators" else None
                    if operator and not await con.fetchval(
                        "SELECT EXISTS (SELECT 1 FROM operators WHERE id = $1)", operator
                    ):
                        counts.setdefault("skipped_tests", 0)
                        counts["skipped_tests"] += len(doc["cases"])
                        continue
                    module_id = None
                    if parts[0] == "modules":
                        manifest = cases_path.parent.parent / "module.yaml"
                        module_id = load_yaml(manifest)["id"] if manifest.is_file() else None
                    await con.execute(
                        "DELETE FROM logic_tests WHERE coalesce(module_id, '') = coalesce($1, '')"
                        " AND operator_id IS NOT DISTINCT FROM $2",
                        module_id,
                        operator,
                    )
                    await con.executemany(
                        """INSERT INTO logic_tests (operator_id, module_id, capability, input, expected,
                               note, source_item_id, commit, synced_at)
                           VALUES ($1, $2, $3, $4, $5, $6, $7::text, $8, now())""",
                        [
                            (
                                operator,
                                module_id,
                                doc["capability"],
                                c["in"],
                                ({"out": c["out"]} if "out" in c else {"error": c.get("error")}),
                                c.get("note"),
                                c.get("source"),
                                commit,
                            )
                            for c in doc["cases"]
                        ],
                    )
                    counts["tests"] += len(doc["cases"])

                # 4. code_chunks — chunk của commit cũ ở repo này bị bỏ (cơ chế "quên").
                await con.execute("DELETE FROM code_chunks WHERE repo = $1", repo)
                pending: list[tuple[str, str, str, str, str | None, str | None, str]] = []
                for py in sorted(workdir.glob("**/*.py")):
                    if "/.venv/" in py.as_posix() or "/tests/" in py.relative_to(workdir).as_posix():
                        continue
                    module_id, operator = module_context(py, workdir)
                    for chunk in chunk_file(py, workdir):
                        pending.append(
                            (
                                py.relative_to(workdir).as_posix(),
                                chunk["symbol"],
                                chunk["text"],
                                chunk["summary"],
                                module_id,
                                operator,
                                textnorm.search_text(chunk["symbol"])
                                + " "
                                + textnorm.search_text(chunk["text"]),
                            )
                        )
                if pending:
                    await con.executemany(
                        """INSERT INTO code_chunks (repo, commit, path, symbol, text, summary, search_text,
                               module_id, operator_id)
                           VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)""",
                        [(repo, commit, p[0], p[1], p[2], p[3], p[6], p[4], p[5]) for p in pending],
                    )
                counts["chunks"] = len(pending)

                if not counts.get("skipped_profiles"):
                    await con.execute(
                        """INSERT INTO logic_syncs (repo, commit, synced_at) VALUES ($1, $2, now())
                           ON CONFLICT (repo) DO UPDATE SET commit = EXCLUDED.commit, synced_at = now()""",
                        repo,
                        commit,
                    )
    finally:
        if tmp:
            shutil.rmtree(tmp, ignore_errors=True)

    # Embedding sau transaction: TEI lỗi không làm hỏng đồng bộ — chunk vẫn tìm được bằng keyword.
    embedded = 0
    try:
        embedded = await _embed_missing(pool, embedder, repo, commit)
    except RetryableError as exc:
        return {
            "status": "done",
            "summary": f"đồng bộ xong, embedding bỏ qua: {exc}",
            "commit": commit,
            "changed": True,
            "counts": counts,
            "embedded": 0,
        }

    return {
        "status": "done",
        "summary": f"đồng bộ {repo}@{commit[:8]}",
        "commit": commit,
        "changed": True,
        "counts": counts,
        "embedded": embedded,
    }


def handler(pool: asyncpg.Pool, embedder: Embedder):
    async def run(job: Job) -> dict[str, Any]:
        source = job.payload.get("repo") or os.environ.get("BIVA_INTEGRATIONS_REPO")
        if not source:
            raise PermanentError("thiếu payload.repo hoặc env BIVA_INTEGRATIONS_REPO", code="INVALID_PAYLOAD")
        return await sync(pool, embedder, source)

    return run
