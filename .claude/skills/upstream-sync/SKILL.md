---
name: upstream-sync
description: 检查并挑选合入上游 Xboard-Node 的修复时使用（每月一次，或上游发布安全修复时）。
---
# 合入上游 Xboard-Node

1. `git fetch upstream`，列出自 `UPSTREAM.md` 记录的上次合入之后的上游提交。
2. 逐个分类：内核集成、协议、证书、路由、安装 → 候选；与 Xboard 面板通信相关 → 跳过；其他 → 人工判断。
3. 候选提交逐个 cherry-pick；冲突集中在登记过的原有文件，按 `UPSTREAM.md` 的记录解决。
4. 若上游更新了内核 fork 的 `replace` 指向，同步更新本组织的内核 fork（使用 workspace 的 `core-upgrade` Skill）。
5. 运行 `make test && make conformance`。
6. 在 `UPSTREAM.md` 登记已合入与跳过的提交。
