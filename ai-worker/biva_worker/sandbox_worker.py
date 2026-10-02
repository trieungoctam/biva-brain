"""Bootstrap sandbox (chạy bên trong process con,Mode ``-I``): chặn mạng + import whitelist.

- ``socket``/``ssl`` bị thay bằng stub raise TRƯỚC khi exec code người dùng — kể cả khi lọt qua
  whitelist bằng import động.
- Import chỉ cho phép module thuần: math/datetime/json/re/decimal/fractions/itertools/functools/
  collections/typing/statistics/unicodedata/string/random/hashlib/hmac/base64/textwrap.
- Đọc job JSON một dòng từ stdin, exec code trong dict riêng, gọi ``entry(**payload)``,
  in kết quả JSON một dòng.
"""

from __future__ import annotations

import importlib.abc
import json
import sys


class Blocked(Exception):
    pass


def _blocked(*_a, **_k):
    raise Blocked("bị cấm trong sandbox (không mạng / không hệ thống)")


# ── Chặn mạng ở tầng thấp nhất có thể ──
import socket  # noqa: E402

for name in ("socket", "create_connection", "socketpair", "getaddrinfo", "gethostbyname"):
    setattr(socket, name, _blocked)
del socket

ALLOWED = {
    "math",
    "datetime",
    "json",
    "re",
    "decimal",
    "fractions",
    "numbers",
    "itertools",
    "functools",
    "collections",
    "typing",
    "statistics",
    "unicodedata",
    "string",
    "random",
    "hashlib",
    "hmac",
    "base64",
    "textwrap",
    "heapq",
    "bisect",
    "operator",
    "dataclasses",
    "enum",
    "abc",
}


class WhitelistImporter(importlib.abc.MetaPathFinder):
    def find_spec(self, fullname, path=None, target=None):  # noqa: ANN001
        root = fullname.split(".")[0]
        if root in sys.builtin_module_names and root not in ("sys", "builtins", "_imp"):
            if root in (
                "posix",
                "nt",
                "posixpath",
                "ntpath",
                "pwd",
                "grp",
                "resource",
                "signal",
                "fcntl",
                "termios",
                "msvcrt",
                "_winapi",
                "subprocess",
                "threading",
            ):
                raise Blocked(f"import {fullname} bị cấm trong sandbox")
            return None  # builtin vô hại: để máy mặc định xử lý
        if root in ALLOWED:
            return None
        # Tiện ích C nội bộ của stdlib (_decimal, _datetime, _sha256...) — thuần tính toán.
        if root.startswith("_") and root not in UNDER_DENY:
            return None
        raise Blocked(f"import {fullname} bị cấm trong sandbox (chỉ cho phép: {', '.join(sorted(ALLOWED))})")


# Extension C nội bộ bị từ chối (mạng / hệ thống / luồng).
UNDER_DENY = {
    "_socket",
    "_ssl",
    "_thread",
    "_winapi",
    "_multiprocessing",
    "_posixsubprocess",
    "_posixshmem",
    "_subprocess",
    "_signal",
    "_imp",
}

sys.meta_path.insert(0, WhitelistImporter())

# Module nguy hiểm đã có sẵn trong sys.modules từ lúc khởi động interpreter → bỏ khỏi cache
# để lần import sau phải đi qua whitelist.
for m in [
    m
    for m in sys.modules
    if m.split(".")[0]
    in (
        "os",
        "posix",
        "nt",
        "subprocess",
        "threading",
        "signal",
        "resource",
        "socket",
        "ssl",
        "select",
        "urllib",
        "http",
        "ftplib",
        "smtplib",
        "pathlib",
    )
]:
    del sys.modules[m]


def main() -> None:
    import builtins

    job = json.loads(sys.stdin.readline())
    safe_builtins = dict(builtins.__dict__)
    for name in ("open", "input", "breakpoint", "exit", "quit"):
        safe_builtins[name] = _blocked  # hàm thuần không đọc file / console
    # io đã bị purge khỏi sys.modules + deny ở finder — chặn nốt tham chiếu còn sót.
    import sys as _sys

    if "io" in _sys.modules:
        _sys.modules["io"].open = _blocked
    del _sys
    env: dict = {"__name__": "sandboxed", "__builtins__": safe_builtins}
    exec(compile(job["code"], "<sandbox>", "exec"), env)  # noqa: S102 - đây là mục đích của sandbox
    fn = env.get(job["entry"])
    if not callable(fn):
        raise Blocked(f"không có hàm {job['entry']} trong code")
    value = fn(**job["payload"])
    sys.stdout.write(json.dumps({"value": value}, ensure_ascii=False, default=str))
    sys.stdout.flush()


if __name__ == "__main__":
    try:
        main()
    except Blocked as exc:
        print(f"SANDBOX_BLOCKED: {exc}", file=sys.stderr)
        sys.exit(3)
    except RecursionError:
        print("SANDBOX_BLOCKED: đệ quy quá sâu", file=sys.stderr)
        sys.exit(3)
