"""Job index.code (S2.5.2) với repo git thật tạo trong tmp + Postgres thật.

AC: module.yaml/profile.yaml/cases.yaml → các bảng logic_*; code chunk theo hàm/lớp gắn commit;
commit mới → chunk cũ không còn; HEAD không đổi → no-op.
"""

from __future__ import annotations

import asyncio
import os
import subprocess
import uuid

import asyncpg
import pytest

from biva_worker import logic_sync
from biva_worker.runner import init_connection

DB_URL = os.environ.get("BIVA_TEST_DATABASE_URL")
needs_db = pytest.mark.skipif(not DB_URL, reason="đặt BIVA_TEST_DATABASE_URL để chạy test logic_sync")


class FakeEmbedder:
    def __init__(self) -> None:
        self.calls = 0

    async def embed(self, texts: list[str]) -> list[list[float]]:
        self.calls += 1
        return [[0.1, 0.2, 0.3] for _ in texts]


MODULE_YAML = """\
id: fare.mini
version: 1
capability: fare
layer: L1
summary: Giá mini để test
entrypoint: calc.py:calculate
features: [fare.by_route_vehicle]
required_tests: [tests/cases.yaml]
deprecated_by: null
"""

CALC_PY = '''\
"""Module mini."""


def calculate(base: int, seat: str) -> int:
    """Tính giá mini."""
    return base if seat else 0


class Helper:
    """Hỗ trợ."""

    def double(self, x: int) -> int:
        return 2 * x
'''

CASES_YAML = """\
capability: fare
cases:
  - {in: {base: 300000, seat: sleeper}, out: 300000, note: ngày thường}
  - {in: {base: 300000, seat: ""}, error: "ghế rỗng"}
"""

PROFILE_YAML = """\
operator: {op}
capabilities:
  fare:
    mode: config
    module: fare.mini@1
    params:
      base_fare:
        value: 300000
        source: "{item}"
"""


def git(args: list[str], cwd) -> None:
    subprocess.run(["git", *args], cwd=cwd, capture_output=True, text=True, check=True)


def make_repo(path, item: str, calc: str = CALC_PY) -> None:
    (path / "modules" / "fare" / "mini").mkdir(parents=True)
    (path / "modules" / "fare" / "mini" / "module.yaml").write_text(MODULE_YAML, encoding="utf-8")
    (path / "modules" / "fare" / "mini" / "calc.py").write_text(calc, encoding="utf-8")
    (path / "modules" / "fare" / "mini" / "tests").mkdir()
    (path / "modules" / "fare" / "mini" / "tests" / "cases.yaml").write_text(CASES_YAML, encoding="utf-8")
    op_dir = path / "operators" / "alicex"
    (op_dir / "tests").mkdir(parents=True)
    (op_dir / "profile.yaml").write_text(PROFILE_YAML.format(op="alicex", item=item), encoding="utf-8")
    (op_dir / "tests" / "cases.yaml").write_text(CASES_YAML, encoding="utf-8")
    git(["init", "-q"], path)
    git(["config", "user.email", "t@biva.vn"], path)
    git(["config", "user.name", "t"], path)
    git(["add", "-A"], path)
    git(["commit", "-qm", "init"], path)


@needs_db
def test_sync_modules_profiles_tests_chunks_and_forget():
    async def t() -> None:
        pool = await asyncpg.create_pool(DB_URL, init=init_connection)
        op = f"lgx{uuid.uuid4().hex[:8]}"
        try:
            await pool.execute("INSERT INTO operators (id, name) VALUES ($1, 'Logic test')", op)
            item_id = await pool.fetchval(
                """INSERT INTO items (layer, operator_id, kind, topic, text, status)
                   VALUES (2, $1, 'policy', 'fare', 'Giá cơ bản 300k', 'active') RETURNING id::text""",
                op,
            )
            import tempfile
            from pathlib import Path

            with tempfile.TemporaryDirectory() as tmp:
                repo = Path(tmp) / "biva-integrations"
                repo.mkdir()
                make_repo(repo, item_id)
                # profile của nhà xe chưa onboard → skipped, nhưng module/test/chunk vẫn vào.
                first = await logic_sync.sync(pool, FakeEmbedder(), str(repo))
                assert first["changed"] and first["counts"]["modules"] == 1
                assert first["counts"]["skipped_profiles"] == 1
                assert (
                    first["counts"]["tests"] == 2
                )  # của module; 2 case của nhà xe chưa onboard → skipped_tests
                assert first["counts"]["skipped_tests"] == 2

                mod = await pool.fetchrow("SELECT * FROM logic_modules WHERE id = 'fare.mini'")
                assert mod["version"] == 1 and mod["capability"] == "fare" and mod["layer"] == "L1"
                assert mod["commit"] == first["commit"] and mod["features"] == ["fare.by_route_vehicle"]

                chunks = await pool.fetch(
                    "SELECT symbol, summary, module_id FROM code_chunks WHERE repo = $1 ORDER BY symbol",
                    "biva-integrations",
                )
                assert {(c["symbol"], c["module_id"]) for c in chunks} == {
                    ("Helper", "fare.mini"),
                    ("Helper.double", "fare.mini"),
                    ("calculate", "fare.mini"),
                }
                by_sym = {c["symbol"]: c for c in chunks}
                assert by_sym["calculate"]["summary"] == "Tính giá mini."
                emb = await pool.fetchval(
                    "SELECT embedding IS NOT NULL FROM code_chunks WHERE repo=$1 AND symbol='calculate'",
                    "biva-integrations",
                )
                assert emb is True
                # Còn skipped → mốc HEAD không được ghi, sync lại khi HEAD vẫn thế (tự lành).
                again = await logic_sync.sync(pool, FakeEmbedder(), str(repo))
                assert again["changed"] is True and again["counts"]["skipped_profiles"] == 1

                # Onboard nhà xe + đổi code → profile vào, chunk cũ bị "quên", commit mới.
                await pool.execute("INSERT INTO operators (id, name) VALUES ('alicex', 'Alice X')")
                (repo / "modules" / "fare" / "mini" / "calc.py").write_text(
                    CALC_PY.replace("def calculate", "def calculate_v2"), encoding="utf-8"
                )
                git(["add", "-A"], repo)
                git(["commit", "-qm", "rename calculate"], repo)
                third = await logic_sync.sync(pool, FakeEmbedder(), str(repo))
                assert third["changed"] and third["counts"]["profiles"] == 1
                assert third["counts"]["tests"] == 4  # đã onboard: cả 2 của module + 2 của nhà xe
                assert third["commit"] != first["commit"]

                prof = await pool.fetchrow(
                    "SELECT * FROM logic_profiles WHERE operator_id = 'alicex' AND capability = 'fare'"
                )
                assert prof["mode"] == "config" and prof["module_id"] == "fare.mini"
                src = await pool.fetchrow(
                    "SELECT item_id::text FROM logic_param_sources WHERE profile_id = $1", prof["id"]
                )
                assert src["item_id"] == item_id

                symbols = {
                    r["symbol"]
                    for r in await pool.fetch(
                        "SELECT symbol FROM code_chunks WHERE repo = $1 AND commit = $2",
                        "biva-integrations",
                        third["commit"],
                    )
                }
                assert "calculate_v2" in symbols and "calculate" not in symbols
                old = await pool.fetchval(
                    "SELECT count(*) FROM code_chunks WHERE repo=$1 AND commit=$2",
                    "biva-integrations",
                    first["commit"],
                )
                assert old == 0  # chunk của commit cũ bị bỏ
        finally:
            await pool.execute("DELETE FROM operators WHERE id IN ($1, 'alicex')", op)

    asyncio.run(t())


def test_yaml_sai_schema_bao_loi_ro(tmp_path):
    import yaml

    from biva_worker.runner import PermanentError

    doc = yaml.safe_load(MODULE_YAML)
    doc.pop("entrypoint")
    with pytest.raises(PermanentError, match="entrypoint"):
        logic_sync.validate_or_raise("logic.module", doc, "modules/fare/mini/module.yaml")


@needs_db
def test_item_nguon_doi_profile_logic_stale():
    """Regression (migration 000024): item là nguồn tham số của profile bị supersede/
    expire/rút → profile chuyển 'stale'; đồng bộ lại (merge PR) → 'active'."""
    import asyncio
    import uuid

    import asyncpg

    from biva_worker.runner import init_connection

    async def t() -> None:
        pool = await asyncpg.create_pool(DB_URL, init=init_connection)
        op = "st" + uuid.uuid4().hex[:8]
        await pool.execute("INSERT INTO operators (id, name) VALUES ($1, 'S')", op)
        try:
            item = await pool.fetchval(
                """INSERT INTO items (layer, operator_id, kind, topic, key, text, value, status)
                   VALUES (2, $1, 'data', 'fare', 'fare.sg_dl.base', 'Giá cơ sở 320k', '{}', 'active')
                   RETURNING id::text""",
                op,
            )
            prof = await pool.fetchval(
                """INSERT INTO logic_profiles (operator_id, capability, mode, module_id, commit)
                   VALUES ($1, 'fare.standard', 'config', 'fare.standard', 'deadbeef')
                   RETURNING id::text""",
                op,
            )
            await pool.execute(
                """INSERT INTO logic_param_sources (profile_id, param_path, item_id)
                   VALUES ($1::uuid, 'base_fare', $2::uuid)""",
                prof,
                item,
            )
            # Item nguồn bị thay: cả hai bản ghi trong MỘT transaction (constraint
            # items_scope_key_validity hoãn; trigger stale cũng hoãn tới commit).
            # Constraint items_scope_key_validity là immediate: rút bản cũ ra trước
            # (id bản mới sinh sẵn ở client để gắn superseded_by), rồi mới thêm bản mới.
            new_item = str(uuid.uuid4())
            async with pool.acquire() as con:
                async with con.transaction():
                    # EXCLUDE items_scope_key_validity là immediate và chỉ áp item active:
                    # 1) thêm bản mới ở pending, 2) supersede bản cũ, 3) active bản mới.
                    # Trigger stale (deferred) thấy trạng thái cuối cùng khi commit.
                    await con.execute(
                        """INSERT INTO items (id, layer, operator_id, kind, topic, key, text, value, status)
                           VALUES ($1::uuid, 2, $2, 'data', 'fare', 'fare.sg_dl.base', 'Giá cơ sở 350k',
                                   '{}', 'pending')""",
                        new_item,
                        op,
                    )
                    await con.execute(
                        "UPDATE items SET status = 'superseded', superseded_by = $2::uuid WHERE id = $1::uuid",
                        item,
                        new_item,
                    )
                    await con.execute("UPDATE items SET status = 'active' WHERE id = $1::uuid", new_item)
            st = await pool.fetchval("SELECT status FROM logic_profiles WHERE id = $1::uuid", prof)
            assert st == "stale", f"item nguồn superseded mà profile vẫn {st}"

            # Profile khác của cùng nhà xe không dính (không lấy tham số từ item này).
            await pool.execute(
                """INSERT INTO logic_profiles (operator_id, capability, mode, module_id, commit)
                   VALUES ($1, 'booking.hold', 'config', 'booking.hold', 'deadbeef')""",
                op,
            )
            n = await pool.fetchval(
                "SELECT count(*) FROM logic_profiles WHERE operator_id = $1 AND status = 'active'", op
            )
            assert n == 1, "chỉ profile không liên quan còn active"

            # Đồng bộ lại (upsert của logic_sync) hồi phục về active.
            await pool.execute(
                """INSERT INTO logic_profiles (operator_id, capability, mode, module_id, commit, synced_at)
                   VALUES ($1, 'fare.standard', 'config', 'fare.standard', 'beefcafe', now())
                   ON CONFLICT (operator_id, capability) DO UPDATE SET
                       commit = EXCLUDED.commit, status = 'active', synced_at = now()""",
                op,
            )
            st = await pool.fetchval("SELECT status FROM logic_profiles WHERE id = $1::uuid", prof)
            assert st == "active"
        finally:
            await pool.execute("DELETE FROM operators WHERE id = $1", op)
            await pool.close()

    asyncio.run(t())
