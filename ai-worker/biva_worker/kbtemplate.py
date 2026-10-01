"""Đọc template onboarding L1 (kb/L1/<ngành>/template.yaml) — bộ topic chuẩn cho items.topic."""

from __future__ import annotations

import os
from functools import cache
from pathlib import Path
from typing import Any

import yaml

from biva_worker.contracts import validate

_DEFAULT_KB = Path(__file__).resolve().parents[2] / "kb"


def kb_root() -> Path:
    return Path(os.environ.get("BIVA_KB_DIR", _DEFAULT_KB))


@cache
def template(industry: str = "xe-khach") -> dict[str, Any]:
    with open(kb_root() / "L1" / industry / "template.yaml", encoding="utf-8") as f:
        doc = yaml.safe_load(f)
    errors = validate("kb.template", doc)
    if errors:
        raise ValueError(f"template {industry} không hợp lệ: {'; '.join(errors)}")
    return doc


def topics(industry: str = "xe-khach") -> list[str]:
    return [s["topic"] for s in template(industry)["sections"]]
