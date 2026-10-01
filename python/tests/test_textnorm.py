"""textnorm phải khớp từng byte với bản Go — cùng fixture contracts/textnorm/cases.jsonl."""

from __future__ import annotations

import json

import pytest

from biva_worker.contracts import contracts_root
from biva_worker.textnorm import fold, search_text

CASES = [
    json.loads(line)
    for line in (contracts_root() / "textnorm" / "cases.jsonl").read_text(encoding="utf-8").splitlines()
]


def test_enough_cases():
    assert len(CASES) >= 200


@pytest.mark.parametrize("case", CASES, ids=lambda c: c["input"][:30] or "<rỗng>")
def test_fixture(case):
    assert fold(case["input"]) == case["fold"]
    assert search_text(case["input"]) == case["search_text"]
