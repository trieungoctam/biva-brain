"""Job logic.propose (S2.5.5): validate profile, chặn custom thiếu ADR, tạo nhánh/ghi file, manual fallback."""

from __future__ import annotations

import asyncio
import os
import subprocess
import uuid
from pathlib import Path

import asyncpg
import pytest

from biva_worker import logic_propose
from biva_worker.logic_propose import _validate_paths, prepare_branch
from biva_worker.runner import Job, PermanentError, init_connection

DB_URL = os.environ.get("BIVA_TEST_DATABASE_URL")
needs_db = pytest.mark.skipif(not DB_URL, reason="đặt BIVA_TEST_DATABASE_URL để chạy test logic_propose")

PROFILE = """\
operator: {op}
capabilities:
  fare:
    mode: config
    module: fare.standard@1
    params:
      holiday_surcharge:
        value: {{tet: 0.20}}
        source: "1c725bb6-cf77-4e81-bc66-dd29f32536d9"
"""


def git(args: list[str], cwd: Path) -> None:
    subprocess.run(["git", *args], cwd=cwd, capture_output=True, text=True, check=True)


def make_repo(path: Path) -> None:
    (path / "operators").mkdir(parents=True)
    (path / "README.md").write_text("x", encoding="utf-8")
    git(["init", "-q", "-b", "main"], path)
    git(["config", "user.email", "t@biva.vn"], path)
    git(["config", "user.name", "t"], path)
    git(["add", "-A"], path)
    git(["commit", "-qm", "init"], path)


@needs_db
def test_propose_manual_mode_and_adr_gate(monkeypatch):
    async def t() -> None:
        pool = await asyncpg.create_pool(DB_URL, init=init_connection)
        op = f"prx{uuid.uuid4().hex[:8]}"
        await pool.execute("INSERT INTO operators (id, name) VALUES ($1, 'P')", op)
        try:
            run = logic_propose.handler(pool)
            job = Job(
                str(uuid.uuid4()),
                "logic.propose",
                op,
                {
                    "operator_id": op,
                    "profile_yaml": PROFILE.format(op=op),
                    "files": [{"path": f"operators/{op}/hooks/a.py", "content": "def h():\n    return 1\n"}],
                },
                1,
                5,
                {},
            )

            # Custom chưa có ADR → từ chối (AC).
            custom_tpl = (
                "operator: {op}\ncapabilities:\n  fare:\n    mode: custom\n"
                "    entrypoint: custom/x.py:h\n    decision: {adr}\n"
            )
            j2 = Job(
                str(uuid.uuid4()),
                "logic.propose",
                op,
                {"operator_id": op, "profile_yaml": custom_tpl.format(op=op, adr="adr_khong_co")},
                1,
                5,
                {},
            )
            with pytest.raises(PermanentError, match="ADR"):
                await run(j2)

            # Có ADR trong Brain → custom được nhận (mode manual vì chưa có token).
            await pool.execute(
                """INSERT INTO logic_decisions (id, operator_id, capability, title, decision, author)
                   VALUES ('adr_test01', $1, 'fare', 't', 'd', 'ai:t')""",
                op,
            )
            j3 = Job(
                str(uuid.uuid4()),
                "logic.propose",
                op,
                {"operator_id": op, "profile_yaml": custom_tpl.format(op=op, adr="adr_test01")},
                1,
                5,
                {},
            )
            res = await run(j3)
            assert res["mode"] == "manual" and res["files"][0]["path"] == f"operators/{op}/profile.yaml"

            # Config + token thiếu → manual; path lạ bị chặn.
            res = await run(job)
            assert res["mode"] == "manual" and len(res["files"]) == 2
            j4 = Job(
                str(uuid.uuid4()),
                "logic.propose",
                op,
                {
                    "operator_id": op,
                    "profile_yaml": PROFILE.format(op=op),
                    "files": [{"path": "../evil.py", "content": "x"}],
                },
                1,
                5,
                {},
            )
            with pytest.raises(PermanentError, match="operators/"):
                await run(j4)
        finally:
            await pool.execute("DELETE FROM operators WHERE id = $1", op)

    asyncio.run(t())


def test_prepare_branch_ghi_file_va_commit(tmp_path):
    make_repo(tmp_path)
    op = "alice"
    branch, written = prepare_branch(
        tmp_path,
        op,
        PROFILE.format(op=op),
        [(f"operators/{op}/hooks/a.py", "def h():\n    return 1\n")],
        "ai:t",
    )
    assert branch.startswith("propose/alice-")
    assert written == [f"operators/{op}/profile.yaml", f"operators/{op}/hooks/a.py"]
    assert (tmp_path / "operators" / op / "profile.yaml").exists()
    log = subprocess.run(["git", "log", "--oneline", "-1"], cwd=tmp_path, capture_output=True, text=True)
    assert "propose logic profile" in log.stdout


def test_validate_paths_chặn_ngoai_thư_mục():
    ok = _validate_paths("alice", [{"path": "operators/alice/hooks/a.py", "content": "x"}])
    assert ok == [("operators/alice/hooks/a.py", "x")]
    for bad in ["evil.py", "operators/bob/a.py", "operators/alice/../x.py", "operators/alice/a.txt"]:
        with pytest.raises(PermanentError):
            _validate_paths("alice", [{"path": bad, "content": "x"}])
