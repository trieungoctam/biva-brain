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


# Builtin thuần TÍNH TOÁN cho phép (mọi builtin khác bị chặn mặc định — _io/_socket từng
# lọt qua vì deny cũ chỉ áp cho module không-builtin; _io là builtin trên mọi build CPython).
BUILTIN_OK = frozenset(
    {
        "sys",
        "builtins",
        "_imp",
        "_frozen_importlib",
        "_frozen_importlib_external",
        "_codecs",
        "_collections",
        "_functools",
        "_operator",
        "_itertools",
        "_heapq",
        "_bisect",
        "_sre",
        "_string",
        "_struct",
        "_random",
        "_json",
        "_datetime",
        "_decimal",
        "_stat",
        "_sha256",
        "_sha512",
        "_sha3",
        "_sha1",
        "_md5",
        "_blake2",
        "_ast",
        "_weakref",
        "math",
        "cmath",
        "binascii",
        "_tokenize",
        "_locale",
        "posixpath",
        "ntpath",
    }
)


class WhitelistImporter(importlib.abc.MetaPathFinder):
    def find_spec(self, fullname, path=None, target=None):  # noqa: ANN001
        root = fullname.split(".")[0]
        # 1) Deny tuyệt đối trước mọi nhánh (kể cả builtin: _socket/_io từng chạy được).
        if root in UNDER_DENY:
            raise Blocked(f"import {fullname} bị cấm trong sandbox")
        # 2) Builtin: deny-by-default, chỉ cho phép bộ tính toán thuần.
        if root in sys.builtin_module_names:
            if root not in BUILTIN_OK:
                raise Blocked(f"import builtin {fullname} bị cấm trong sandbox")
            return None
        if root in ALLOWED:
            return None
        # Extension C nội bộ còn lại của stdlib cho phép nếu không nằm trong deny —
        # nhưng mọi builtin nguy hiểm đã chặn ở nhánh 2.
        if root.startswith("_"):
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

# NẠP SẴN toàn bộ module được phép TRƯỚC khi purge: import machinery đọc file .py qua
# builtin _io, nên sau khi chặn _io thì chỉ module có sẵn trong cache import được —
# tức whitelist đóng băng, module ngoài không thể nạp thêm (kể cả file trong repo).
for _m in sorted(ALLOWED):
    try:
        __import__(_m)
    except Exception:  # noqa: BLE001 — module nào lỗi vẫn để finder quyết định sau
        pass

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
        # builtin/extension nguy hiểm đã nạp sẵn từ khởi động: import lấy thẳng từ
        # sys.modules KHÔNG qua find_spec — phải bỏ khỏi cache thì deny mới tới được.
        "io",
        "_io",
        "marshal",
        "_socket",
        "_ssl",
        "_thread",
        "_signal",
        "select",
        "faulthandler",
        "zipimport",
        "runpy",
        "code",
        "codeop",
        "linecache",
        "pickle",
        "_pickle",
        "ctypes",
        "multiprocessing",
    )
]:
    del sys.modules[m]


def main() -> None:
    import builtins

    job = json.loads(sys.stdin.readline())
    safe_builtins = dict(builtins.__dict__)
    for name in ("open", "input", "breakpoint", "exit", "quit"):
        safe_builtins[name] = _blocked  # hàm thuần không đọc file / console
    # io/_io đã purge + deny ở finder; sys.stdout/stderr vẫn dùng được qua object đã có.
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
