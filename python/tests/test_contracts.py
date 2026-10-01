"""Kiểm tra JSON Schema với fixture dùng chung — cùng bộ fixture mà test Go dùng."""

import json

import pytest

from biva_worker.contracts import contracts_root, schema_path, validate

FIXTURES = contracts_root() / "fixtures" / "schemas"


def _cases():
    for d in sorted(p for p in FIXTURES.iterdir() if p.is_dir()):
        for f in sorted(d.glob("*.json")):
            yield pytest.param(d.name, f, id=f"{d.name}/{f.name}")


@pytest.mark.parametrize(("schema", "fixture"), list(_cases()))
def test_fixture(schema, fixture):
    errors = validate(schema, json.loads(fixture.read_text(encoding="utf-8")))
    if fixture.name.startswith("valid_"):
        assert errors == [], f"{fixture.name} phải hợp lệ: {errors}"
    elif fixture.name.startswith("invalid_"):
        assert errors, f"{fixture.name} phải bị từ chối"
    else:
        pytest.fail(f"tên fixture phải bắt đầu bằng valid_ hoặc invalid_: {fixture.name}")


def test_every_fixture_dir_has_a_schema():
    for d in FIXTURES.iterdir():
        if d.is_dir():
            assert schema_path(d.name).is_file(), f"thiếu schema cho {d.name}"
