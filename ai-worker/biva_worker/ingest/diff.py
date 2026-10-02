"""Diff item ứng viên với trạng thái hiện tại của nhà xe theo key — S1.2.1. Thuần, không I/O."""

from __future__ import annotations

from dataclasses import dataclass
from datetime import date

from biva_worker import textnorm
from biva_worker.ingest.extract import Candidate, ExistingItem

# Đổi những topic này ảnh hưởng trực tiếp tiền/giờ của khách → luôn cần người duyệt.
HIGH_RISK_TOPICS = frozenset({"fare", "schedule", "cancellation", "payment"})


@dataclass(frozen=True)
class Decision:
    change_kind: str  # NEW | CHANGE | REMOVE | DUPLICATE | CONFLICT
    key: str
    topic: str
    candidate: Candidate | None
    target: ExistingItem | None
    risk: str
    reason: str = ""


def _norm_facts(facts: dict[str, str]) -> dict[str, str]:
    return {textnorm.fold(k): textnorm.fold(v) for k, v in facts.items()}


def same_content(c: Candidate, e: ExistingItem, today: date | None = None) -> bool:
    """Cùng nội dung: hiệu lực không đổi và facts khớp (nếu cả hai có facts) hoặc cùng text đã chuẩn hoá.

    Tin không nêu ngày hiệu lực = không thay đổi hiệu lực (item đang active đã được gán valid_from lúc apply).
    Hai ngày bắt đầu khác nhau nhưng cùng đã qua (≤ ngày nhận tin) cũng không phải thay đổi.
    """
    already_valid = (
        today is not None
        and c.valid_from is not None
        and c.valid_from <= today
        and (e.valid_from is None or e.valid_from <= today)
    )
    if c.valid_from is not None and c.valid_from != e.valid_from and not already_valid:
        return False
    if c.valid_to is not None and c.valid_to != e.valid_to:
        return False
    if c.facts and e.facts:
        # Tin nhắc lại thường nêu ít chi tiết hơn: mọi fact của ứng viên có trong item hiện có, cùng giá trị.
        cf, ef = _norm_facts(c.facts), _norm_facts(e.facts)
        return all(ef.get(k) == v for k, v in cf.items())
    return textnorm.fold(c.text) == textnorm.fold(e.text)


def _same_candidate(a: Candidate, b: Candidate) -> bool:
    return (a.valid_from, a.valid_to) == (b.valid_from, b.valid_to) and (
        _norm_facts(a.facts) == _norm_facts(b.facts)
        if a.facts and b.facts
        else textnorm.fold(a.text) == textnorm.fold(b.text)
    )


def risk_of(change_kind: str, topic: str) -> str:
    """Tự áp dụng chỉ khi chắc chắn vô hại: nhắc lại điều đã đúng (DUPLICATE), hoặc thêm mới (NEW) ở topic
    không đụng tới tiền/giờ. Sửa hay bỏ điều đang đúng (CHANGE, REMOVE), mâu thuẫn → luôn cần người duyệt."""
    if change_kind == "DUPLICATE":
        return "low"
    if change_kind == "NEW" and topic not in HIGH_RISK_TOPICS:
        return "low"
    return "high"


def diff(
    candidates: list[Candidate], existing: dict[str, ExistingItem], today: date | None = None
) -> list[Decision]:
    """``today``: ngày nhận tin (múi giờ VN) — để biết một valid_from đã qua hay còn ở tương lai."""
    by_key: dict[str, list[Candidate]] = {}
    for c in candidates:
        by_key.setdefault(c.key, []).append(c)

    def pick(versions: list[ExistingItem], moment: date) -> ExistingItem:
        """Bản chứa thời điểm hiệu lực của ứng viên; khe hở/key mới thì lấy bản mới nhất."""
        for e in versions:
            if (e.valid_from is None or e.valid_from <= moment) and (
                e.valid_to is None or e.valid_to > moment
            ):
                return e
        return versions[-1]

    # Chấp nhận dict[key → item] (dạng cũ) lẫn dict[key → list[item]] (load_existing mới).
    normalized = {k: (v if isinstance(v, list) else [v]) for k, v in existing.items()}

    decisions: list[Decision] = []
    for key, group in by_key.items():
        versions = normalized.get(key, [])
        target = versions[-1] if versions else None
        upserts: list[Candidate] = []
        for c in group:  # bỏ bản lặp y hệt trong cùng một tin
            if c.action == "upsert" and not any(_same_candidate(c, u) for u in upserts):
                upserts.append(c)
        removes = [c for c in group if c.action == "remove"]

        if len(upserts) > 1:
            for c in upserts:
                decisions.append(
                    Decision(
                        "CONFLICT",
                        key,
                        c.topic,
                        c,
                        target,
                        "high",
                        "cùng một tin có nhiều nội dung cho cùng key",
                    )
                )
            continue
        if upserts:  # upsert thắng remove trong cùng tin ("bỏ giá cũ, giá mới là …")
            c = upserts[0]
            # Target = bản chứa thời điểm đề xuất có hiệu lực (mặc định hôm nay), không phải
            # bản mới nhất: khi có bản lên lịch tương lai, sửa giá hôm nay phải nhắm bản hiện tại.
            moment = c.valid_from or (today or date.today())
            if versions:
                target = pick(versions, moment)
            if target is None:
                kind = "NEW"
            elif same_content(c, target, today):
                kind = "DUPLICATE"
            else:
                kind = "CHANGE"
            decisions.append(Decision(kind, key, c.topic, c, target, risk_of(kind, c.topic)))
            continue
        if removes and target is not None:
            c = removes[0]
            decisions.append(Decision("REMOVE", key, target.topic, c, target, "high", c.text))
        # remove cho key chưa có → bỏ qua
    return decisions
