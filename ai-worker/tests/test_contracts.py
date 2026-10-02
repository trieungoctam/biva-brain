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


def test_operation_result_chap_nhan_ket_qua_thuc_te_cua_handler():
    """Regression: handler trả trường riêng (run_id, reply, commit…) phải qua schema
    operation_result — từng bị additionalProperties:false đánh failed cả job thành công."""
    from biva_worker.contracts import validate

    cases = [
        {"status": "done", "summary": "9/9 pass (100%)", "run_id": "r1", "counts": {"total": 9, "passed": 9, "failed": 0}, "failed_cases": []},
        {"status": "done", "reply": "vé 120k", "tools_called": ["query_data"]},
        {"status": "done", "summary": "HEAD không đổi", "commit": "abc", "changed": False},
        {"status": "done", "answer": "phân tích", "warnings": []},
        {"status": "done", "capability": "fare.standard", "cases": [{"name": "c1"}], "results": [{"name": "c1", "ok": True}]},
        {"status": "done", "proposed_features": ["fare.split_round_trip"]},
        {"status": "done", "contradictions": [{"explanation": "giá lệch", "ids": ["a", "b"]}]},
        {"status": "done", "branch": "propose/x", "pr_url": None},
        {"status": "failed", "error": {"code": "X", "message": "lỗi"}, "commit": None},
    ]
    for c in cases:
        errs = validate("operation_result", c)
        assert errs == [], (c, errs)
