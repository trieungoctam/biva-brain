"""Hợp đồng dữ liệu dùng chung với Go: JSON Schema trong contracts/schemas."""

from __future__ import annotations

import json
import os
from functools import cache
from pathlib import Path
from typing import Any

from jsonschema import Draft202012Validator, FormatChecker

_DEFAULT_ROOT = Path(__file__).resolve().parents[2] / "contracts"


def contracts_root() -> Path:
    """Thư mục contracts/. Ghi đè bằng BIVA_CONTRACTS_DIR (vd trong container)."""
    return Path(os.environ.get("BIVA_CONTRACTS_DIR", _DEFAULT_ROOT))


def schema_path(name: str) -> Path:
    """'jobs.ingest' → contracts/schemas/jobs/ingest.schema.json (cùng quy ước với Go)."""
    return contracts_root() / "schemas" / (name.replace(".", "/") + ".schema.json")


@cache
def validator(name: str) -> Draft202012Validator:
    schema = json.loads(schema_path(name).read_text(encoding="utf-8"))
    Draft202012Validator.check_schema(schema)
    return Draft202012Validator(schema, format_checker=FormatChecker())


def validate(name: str, document: Any) -> list[str]:
    """Trả về danh sách lỗi (rỗng = hợp lệ)."""
    return [e.message for e in validator(name).iter_errors(document)]
