#!/usr/bin/env python3
# SPDX-License-Identifier: GPL-3.0-or-later
"""SPDX 头检查（spec/02 CONV-25，spec/21 AGT-02）。

本组织新增的每个已跟踪文件，前两行之内必须有 SPDX-License-Identifier。
REUSE.toml 中以 precedence = "override" 登记的路径（Xboard-Node 原有文件、
无法加头的文件）与 LICENSES/ 不检查；这些文件的许可证由 REUSE lint 核对。
"""

import re
import subprocess
import sys
import tomllib

TAG = "SPDX-License-" + "Identifier:"


def glob_to_regex(pattern: str) -> re.Pattern:
    # REUSE.toml 的匹配规则：* 不跨越 /，** 跨越 /，\* 为字面量 *。
    out, i = "", 0
    while i < len(pattern):
        if pattern.startswith("\\*", i):
            out, i = out + re.escape("*"), i + 2
        elif pattern.startswith("**", i):
            out, i = out + ".*", i + 2
        elif pattern[i] == "*":
            out, i = out + "[^/]*", i + 1
        else:
            out, i = out + re.escape(pattern[i]), i + 1
    return re.compile(out + r"\Z")


def registered_patterns() -> list[re.Pattern]:
    with open("REUSE.toml", "rb") as f:
        data = tomllib.load(f)
    patterns = []
    for ann in data.get("annotations", []):
        if ann.get("precedence") != "override":
            continue
        paths = ann["path"]
        for p in [paths] if isinstance(paths, str) else paths:
            patterns.append(glob_to_regex(p))
    return patterns


def main() -> int:
    patterns = registered_patterns()
    files = subprocess.run(
        ["git", "ls-files", "-z"], check=True, capture_output=True
    ).stdout.decode().split("\0")
    missing = []
    for path in files:
        if not path or path.startswith("LICENSES/"):
            continue
        if any(p.match(path) for p in patterns):
            continue
        try:
            with open(path, encoding="utf-8", errors="replace") as f:
                head = [f.readline(), f.readline()]
        except FileNotFoundError:
            continue  # 已删除但未提交的文件
        if not any(TAG in line for line in head):
            missing.append(path)
    if missing:
        print("缺少 SPDX 头（前两行之内），或应在 REUSE.toml 中登记：")
        print("\n".join(missing))
        return 1
    print("check-spdx: 通过")
    return 0


if __name__ == "__main__":
    sys.exit(main())
