"""Chuẩn hoá tiếng Việt cho tìm kiếm — spec: contracts/textnorm/README.md.

Phải cho kết quả giống hệt bản Go (go/internal/textnorm); kiểm bằng contracts/textnorm/cases.jsonl.
"""

from __future__ import annotations

import unicodedata

_D_CHARS = str.maketrans({"đ": "d", "Đ": "d", "ð": "d", "Ð": "d"})


def _is_digit(c: str) -> bool:
    return "0" <= c <= "9"


def _is_token_char(c: str) -> bool:
    return "a" <= c <= "z" or "0" <= c <= "9"


def _fold_chars(s: str) -> str:
    s = unicodedata.normalize("NFKD", s)
    s = "".join(c for c in s if unicodedata.category(c) != "Mn")
    s = s.translate(_D_CHARS)
    return "".join(chr(ord(c) + 32) if "A" <= c <= "Z" else c for c in s)


def _drop_thousand_separators(s: str) -> str:
    out = []
    n = len(s)
    for i, c in enumerate(s):
        if (
            c in ".,"
            and i > 0
            and _is_digit(s[i - 1])
            and all(i + k < n and _is_digit(s[i + k]) for k in (1, 2, 3))
            and (i + 4 == n or not _is_digit(s[i + 4]))
        ):
            continue
        out.append(c)
    return "".join(out)


def tokens(s: str) -> list[str]:
    s = _drop_thousand_separators(_fold_chars(s))
    result: list[str] = []
    start = -1
    for i, c in enumerate(s):
        if _is_token_char(c):
            if start < 0:
                start = i
        elif start >= 0:
            result.append(s[start:i])
            start = -1
    if start >= 0:
        result.append(s[start:])
    return result


def fold(s: str) -> str:
    return " ".join(tokens(s))


def search_text(s: str) -> str:
    toks = tokens(s)
    if len(toks) < 2:
        return " ".join(toks)
    bigrams = [f"{a}_{b}" for a, b in zip(toks, toks[1:], strict=False)]
    return " ".join(toks + bigrams)
