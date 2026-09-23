# node-agent

fork 自 [Xboard-Node](https://github.com/cedar2025/Xboard-Node)。规格：spec/21（Agent）、spec/20（协议）、spec/22（计量）。开工前先读 `FORK_PLAN.md`（M0-04 产出）。

## 必须遵守
- AGT-01 新代码放新包；修改原有文件只做接线级改动，并登记到 `UPSTREAM.md`。
- AGT-02 原有文件保留 MPL-2.0 声明；新文件第一行 `// SPDX-License-Identifier: GPL-3.0-or-later`。
- AGT-05 本地只保存控制面地址与节点密钥；运行配置全部来自控制面；控制面不可达时按最后快照服务。
- AGT-06 不开放 HTTP 管理端口；指标只监听 127.0.0.1。
- AGT-07 内核由控制面下发（`SyncFull.kernel`）；本地 `kernel.type` 只在独立模式生效。
- AGT-12 三项内核能力在两个内核上都成立。
- ACC-01、ACC-02、ACC-09 计数不丢、WAL 兼容、租约耗尽立即断开。

## 命令
- `make build`、`make test`（含 -race）
- `make conformance`：协议一致性套件（与模拟 Agent 相同）+ 内核一致性测试（按 spec/21 协议矩阵生成）

## Skills
- `upstream-sync`：挑选合入上游 Xboard-Node 修复
- `kernel-conformance`：修改内核相关代码后
