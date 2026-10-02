"""Job ``logic.propose`` (M2, S2.5.5): tạo PR đề xuất hồ sơ logic vào repo biva-integrations.

- Validate ``profile_yaml`` theo schema ``logic.profile``; mode=custom bắt buộc ADR đã ghi trong Brain
  (``record_decision``) — AC "custom không có ADR → bị từ chối".
- Có ``BIVA_INTEGRATIONS_REPO`` + ``BIVA_INTEGRATIONS_TOKEN``: clone, tạo nhánh ``propose/<op>-<ts>``,
  ghi ``operators/<op>/profile.yaml`` (+ file hook/custom kèm theo), push, mở PR qua GitHub API.
- Chưa có token: trả về nội dung các file + nhánh đề xuất để builder tạo PR tay (status ``manual``).
"""

from __future__ import annotations

import json
import os
import urllib.error
import urllib.request
from datetime import datetime
from pathlib import Path
from typing import Any

import asyncpg
import yaml

from biva_worker.contracts import validate
from biva_worker.logic_sync import _git, resolve_repo
from biva_worker.runner import Job, PermanentError

ACTOR_FALLBACK = "ai:logic_propose"


def _validate_paths(operator: str, files: list[dict[str, str]]) -> list[tuple[str, str]]:
    """Chỉ chấp nhận file .py nằm trong operators/<op>/ và không thoát khỏi thư mục đó."""
    out: list[tuple[str, str]] = []
    for f in files:
        rel = (f.get("path") or "").strip()
        content = f.get("content") or ""
        if not rel or not content:
            raise PermanentError("mỗi file cần path và content", code="INVALID_PAYLOAD")
        p = Path(rel)
        if (
            p.is_absolute()
            or ".." in p.parts
            or p.parts[:1] != ("operators",)
            or len(p.parts) < 2
            or p.parts[1] != operator
            or p.suffix != ".py"
        ):
            raise PermanentError(
                f"path {rel!r} phải nằm trong operators/{operator}/... và là file .py", code="INVALID_PATH"
            )
        out.append((rel, content))
    return out


def _github_api(url: str, token: str, payload: dict[str, Any]) -> dict[str, Any]:
    req = urllib.request.Request(
        url,
        method="POST",
        data=json.dumps(payload).encode(),
        headers={
            "Authorization": f"Bearer {token}",
            "Accept": "application/vnd.github+json",
            "Content-Type": "application/json",
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return json.loads(resp.read().decode())
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode()[:300]
        raise PermanentError(f"GitHub API {exc.code}: {detail}", code="GITHUB_API") from exc


def prepare_branch(
    workdir: Path, operator: str, profile_yaml: str, files: list[tuple[str, str]], actor: str
) -> tuple[str, list[str]]:
    """Tạo nhánh + ghi file trong workdir (không push). Trả (branch, các đường dẫn đã ghi)."""
    branch = f"propose/{operator}-{datetime.now():%Y%m%d%H%M%S}"
    _git(["checkout", "-q", "-b", branch], cwd=workdir)
    profile_path = workdir / "operators" / operator / "profile.yaml"
    profile_path.parent.mkdir(parents=True, exist_ok=True)
    profile_path.write_text(profile_yaml, encoding="utf-8")
    written = [profile_path.relative_to(workdir).as_posix()]
    for rel, content in files:
        target = workdir / rel
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(content, encoding="utf-8")
        written.append(rel)
    _git(["add", "-A"], cwd=workdir)
    _git(["commit", "-qm", f"propose logic profile cho {operator} (bởi {actor})"], cwd=workdir)
    return branch, written


async def propose(pool: asyncpg.Pool, job: Job) -> dict[str, Any]:
    payload = job.payload
    operator = payload.get("operator_id") or job.operator_id
    profile_yaml = (payload.get("profile_yaml") or "").strip()
    if not operator or not profile_yaml:
        raise PermanentError("thiếu operator_id hoặc profile_yaml", code="INVALID_PAYLOAD")
    actor = payload.get("requested_by") or ACTOR_FALLBACK

    try:
        doc = yaml.safe_load(profile_yaml)
    except yaml.YAMLError as exc:
        raise PermanentError(f"profile_yaml không đọc được: {exc}") from exc
    errors = validate("logic.profile", doc)
    if errors:
        raise PermanentError("profile_yaml: " + "; ".join(errors), code="INVALID_LOGIC_YAML")
    if doc["operator"] != operator:
        raise PermanentError(
            f"profile khai báo operator {doc['operator']!r} nhưng đề xuất cho {operator!r}",
            code="INVALID_LOGIC_YAML",
        )

    # AC: custom không có ADR → từ chối.
    for cap, block in doc["capabilities"].items():
        if block["mode"] == "custom":
            dec = block.get("decision") or ""
            known = dec and await pool.fetchval(
                "SELECT EXISTS (SELECT 1 FROM logic_decisions WHERE operator_id = $1 AND id = $2)",
                operator,
                dec,
            )
            if not known:
                raise PermanentError(
                    f"capability {cap}: mode=custom phải có ADR — "
                    f"record_decision trước, rồi ghi id vào decision",
                    code="ADR_REQUIRED",
                )

    files = _validate_paths(operator, payload.get("files") or [])
    repo_url = os.environ.get("BIVA_INTEGRATIONS_REPO", "")
    token = os.environ.get("BIVA_INTEGRATIONS_TOKEN", "")

    if not repo_url or not token:
        return {
            "status": "done",
            "mode": "manual",
            "summary": "chưa cấu hình BIVA_INTEGRATIONS_TOKEN — trả nội dung để builder tạo PR tay",
            "branch": f"propose/{operator}-{datetime.now():%Y%m%d%H%M%S}",
            "files": [{"path": f"operators/{operator}/profile.yaml", "content": profile_yaml}]
            + [{"path": p, "content": c} for p, c in files],
            "repo": repo_url or None,
        }

    workdir, repo_name, tmp = resolve_repo(repo_url)
    original = _git(["rev-parse", "--abbrev-ref", "HEAD"], cwd=workdir) or "main"
    try:
        branch, written = prepare_branch(workdir, operator, profile_yaml, files, actor)
        # Credential qua header thay vì nhúng URL (git in URL ra stderr khi lỗi → lộ token).
        push = [
            "-c",
            "http.https://github.com/.extraheader=AUTHORIZATION: bearer " + token,
            "push",
            "-q",
            repo_url,
            "HEAD:refs/heads/" + branch,
        ]
        try:
            _git(push, cwd=workdir)
        except PermanentError as exc:
            raise PermanentError(str(exc).replace(token, "***"), code=exc.code) from exc
        # Trả workdir về nhánh gốc: index_code (chế độ path) không đọc nhánh chưa duyệt.
        _git(["checkout", "-q", original], cwd=workdir)
        owner_repo = repo_url.rstrip("/").removesuffix(".git").replace("https://github.com/", "")
        pr = _github_api(
            f"https://api.github.com/repos/{owner_repo}/pulls",
            token,
            {
                "title": f"logic profile: {operator}",
                "head": branch,
                "base": "main",
                "body": f"Đề xuất bởi {actor} qua propose_logic_profile.\n\nFiles: " + ", ".join(written),
            },
        )
        return {
            "status": "done",
            "mode": "pull_request",
            "summary": f"PR #{pr.get('number')}",
            "pr_url": pr.get("html_url"),
            "branch": branch,
            "files": written,
        }
    finally:
        try:
            _git(["checkout", "-q", original], cwd=workdir)
        except Exception:
            pass
        import shutil

        if tmp:
            shutil.rmtree(tmp, ignore_errors=True)


def handler(pool: asyncpg.Pool):
    async def run(job: Job) -> dict[str, Any]:
        return await propose(pool, job)

    return run
