<!-- SPDX-License-Identifier: GPL-3.0-or-later -->

# 与上游 Xboard-Node 的差异登记

- 上游：https://github.com/cedar2025/Xboard-Node（远端名 `upstream`）
- 分叉基点：`0a29338e1f102a462363ce3527417029f89bab28`（截至 2026-09-24，上游 `dev` 仍在该提交上）
- 规则：AGT-01（新代码放新包；原有文件只做接线级修改并在此登记）、AGT-02（原有文件保留 MPL-2.0，登记到 `REUSE.toml`）、AGT-03（每月挑选合入上游的内核、协议、证书修复；不跟随通信层）。
- “原有文件”指分叉基点中已存在的文件。本组织新增的文件（`CLAUDE.md`、`FORK_PLAN.md`、`UPSTREAM.md`、`.claude/`、`internal/auditprobe/` 等）不在此登记。

## 登记格式

每次修改原有文件，在“对原有文件的修改”中加一行：

| 字段 | 填写 |
|---|---|
| 文件 | 相对仓库根目录的路径；一行一个文件 |
| 修改原因 | 做了什么、为什么必须改原文件而不能放在新包中，并引用规则编号或任务编号 |
| 冲突风险 | `低`：上游极少修改，或改动局限在几行接线；`中`：上游偶尔修改，改动集中在一两个函数；`高`：上游频繁修改，或改动分散。附一句合入上游时的处理办法 |
| 提交 | 本仓库中引入该修改的提交（短 SHA） |
| 日期 | YYYY-MM-DD |

## 对原有文件的修改

| 文件 | 修改原因 | 冲突风险 | 提交 | 日期 |
|---|---|---|---|---|

（M0-04 没有修改原有文件。）

## 预计修改的原有文件（M3，供评估冲突风险）

以下是 FORK_PLAN.md 中计划要改的原有文件。实际修改时移到上表，并填写提交与日期。

| 文件 | 计划的修改 | 冲突风险 | 任务 |
|---|---|---|---|
| `cmd/xboard-node/main.go` | 节点模式构造 `internal/agent`，独立模式仍构造 `service`；退出等待由 15 秒改为 30 秒（spec/21 21.4） | 中：上游常改入口与信号处理；只改一个分支点与一个常量 | M3-01 |
| `internal/kernel/singbox/conntracker.go` | UDP 计数方向（FORK_PLAN MTR-1）；以凭据 ID 为键（MTR-3）；按凭据登记与关闭连接；拒绝不在凭据集中的新子流；日志脱敏（CONV-24） | 高：上游计量与限速逻辑都在这个文件中；修正尽量写成可以单独 cherry-pick 的小提交 | M3-04、M3-06 |
| `internal/kernel/singbox/singbox.go` | 重启前与回收结束时 drain 旧 tracker（MTR-2）；逐入站 `ApplyInbounds`；实现 `CloseUserConnections` | 高：上游经常改热重载路径 | M3-03、M3-04、M3-06 |
| `internal/kernel/singbox/config.go` | 入站 tag 与多入站；用户 `name` 使用凭据 ID；SS2022 密钥按协议草案的派生规则（FORK_PLAN §6 第 2 项） | 高：上游频繁增加协议与字段；多入站尽量放在新包中生成，本文件只接收参数 | M3-03 |
| `internal/kernel/xray/xray.go` | 读取全部 `user>>>` 计数器（MTR-5）；不再整实例重启（MTR-6）；修正 `lastKernelHash`；实现 `CloseUserConnections` | 高：同上 | M3-03、M3-04、M3-06 |
| `internal/kernel/xray/config.go` | 多入站；email 使用凭据 ID；mKCP 与 finalmask 字段（取决于协议修改） | 高 | M3-03 |
| `internal/kernel/xray/dispatcher.go` | 拒绝 email 不在当前集合中的新请求；日志脱敏 | 中 | M3-06 |
| `internal/kernel/kernel.go` | 不修改；spec/21 §21.3 的新接口放在 `internal/kernelx` | — | — |
| `internal/model/validate.go` | 节点模式下忽略本地 `kernel.type`（AGT-07） | 低：只改优先级判断的几行 | M3-03 |
| `internal/service/service.go` | 原则上不修改；独立模式继续使用。如需导出校验函数，只做导出级改动 | 中 | M3-01 |
| `internal/config/config.go` | 节点模式的配置只保留控制面地址与状态目录 | 中：上游常增字段；新字段放在新结构中 | M3-02 |
| `Makefile` | `test` 加内核构建标签；新增 `conformance` 目标 | 低 | M3-06 |
| `install.sh`、`Dockerfile`、`.github/workflows/ci.yml` | 参数改为 `--server`、`--enroll-token`；镜像名与 volume；REUSE 与 SPDX 检查 | 中：`install.sh` 上游改动频繁，可能整体重写为新文件，旧文件停用 | M3-07、M3-08 |
| `go.mod` | `replace` 改为本组织的内核副本（AGT-04）；模块路径是否改名待定 | 中：每次上游升级内核都会改 `replace` | M3-06 |
| `internal/panel/*`、`internal/controlplane/panel.go`、`internal/controlplane/machine.go`、`internal/model/panel.go` | 不修改，节点模式不再引用；是否删除见 FORK_PLAN 未决问题 | — | M3-01 |

## 已合入的上游提交

| 上游提交 | 内容 | 合入日期 |
|---|---|---|

（分叉基点之后上游暂无新提交。）

## 不再跟随的上游部分

- 与 Xboard 面板的通信层：`internal/panel/`、`internal/controlplane/panel.go`、`internal/controlplane/machine.go`、`internal/model/panel.go`，以及 `cmd/xbctl` 中与面板绑定相关的部分。
- 全局 API Key 鉴权与 `kernel.type` 的本地优先逻辑。
- `cmd/xbctl`（由 `cmd/agentctl` 取代）；`install.sh` 中与 Xboard 面板参数相关的部分。
