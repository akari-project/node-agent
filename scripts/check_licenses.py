#!/usr/bin/env python3
# SPDX-License-Identifier: GPL-3.0-or-later
"""依赖许可证扫描（spec/42 42.2，GPL-3.0 仓库的允许清单）。

用 go-licenses 列出构建产物实际链接的依赖（带发布构建标签），
许可证不在允许清单内、也未在 LICENSE-EXCEPTIONS 登记的依赖使扫描失败。
本模块自身不参与判定。
"""

import csv
import io
import os
import subprocess
import sys

GO_LICENSES = "github.com/google/go-licenses/v2@v2.0.1"
SELF = "github.com/cedar2025/xboard-node"
# 与 Makefile 的 build-linux 目标一致，保证扫描到全部内核协议的依赖。
BUILD_TAGS = "with_quic,with_utls,with_wireguard,with_acme,with_clash_api"

# spec/42 42.2：MIT、BSD-2-Clause、BSD-3-Clause、Apache-2.0、ISC，加 MPL-2.0、LGPL-2.1+、LGPL-3.0、GPL-3.0。
# 识别器不区分 LGPL-2.1 的 only 与 or-later，按名称接受。
ALLOWED = {
    "MIT", "BSD-2-Clause", "BSD-3-Clause", "Apache-2.0", "ISC",
    "MPL-2.0",
    "LGPL-2.1", "LGPL-2.1-or-later",
    "LGPL-3.0", "LGPL-3.0-or-later",
    "GPL-3.0", "GPL-3.0-or-later",
}


def load_exceptions(path: str = "LICENSE-EXCEPTIONS") -> dict[str, str]:
    exceptions = {}
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            fields = line.split(None, 2)
            if len(fields) < 3:
                sys.exit(f"LICENSE-EXCEPTIONS 格式错误（需要 模块 许可证 理由）：{line}")
            exceptions[fields[0]] = fields[1]
    return exceptions


def module_of(pkg: str, modules: set[str]) -> str | None:
    best = None
    for m in modules:
        if (pkg == m or pkg.startswith(m + "/")) and (best is None or len(m) > len(best)):
            best = m
    return best


def main() -> int:
    exceptions = load_exceptions()
    env = dict(os.environ, GOFLAGS=f"-tags={BUILD_TAGS}")
    proc = subprocess.run(
        ["go", "run", GO_LICENSES, "report", "./...", "--ignore", SELF],
        env=env, capture_output=True, text=True,
    )
    if proc.returncode != 0:
        sys.stderr.write(proc.stderr)
        return proc.returncode

    bad, used = [], set()
    for row in csv.reader(io.StringIO(proc.stdout)):
        if len(row) < 3:
            continue
        pkg, license_name = row[0], row[2]
        if license_name in ALLOWED:
            continue
        mod = module_of(pkg, set(exceptions))
        if mod is not None:
            used.add(mod)
            continue
        bad.append(f"{pkg}: {license_name}")

    for mod in sorted(set(exceptions) - used):
        print(f"提示：LICENSE-EXCEPTIONS 中的 {mod} 已不需要登记，可以删除")
    if bad:
        print("以下依赖的许可证不在允许清单内，且未在 LICENSE-EXCEPTIONS 登记：")
        print("\n".join(sorted(set(bad))))
        return 1
    print("licenses: 通过")
    return 0


if __name__ == "__main__":
    sys.exit(main())
