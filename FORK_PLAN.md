<!-- SPDX-License-Identifier: GPL-3.0-or-later -->

# FORK_PLAN（M0-04：Xboard-Node 代码审计与改造方案）

- 审计对象：本仓库 `dev` 分支 `80a15a7`（上游 cedar2025/Xboard-Node `0a29338` 加本组织文档提交；截至 2026-09-24 上游 `dev` 没有更新的提交）。
- 内核源码：`go.mod` 的 `replace` 目前指向 `cedar2025/sing-box@2e665cb7`（1.14.0-alpha.2 + 上游作者补丁 `3ac22acd` “UpdateUsers interface”）与 `cedar2025/Xray-core@f479355`（v26.3.27 + 补丁 “per-user bandwidth manager”）。本组织的 `sing-box`、`xray-core` fork 在同一提交上只多了文档提交（`de00961c`、`9798511`），下文内核源码的“文件:行”对两边都成立。
- 依据：spec/20、spec/21（21.1、21.3、AGT-12）、spec/22，契约 panel-spec `v0.1.0`。
- 证据分两类：代码位置（`文件:行`，未注明仓库的路径都在本仓库内），以及本次新增的探针测试 `internal/auditprobe/probe_test.go`。探针带构建标签 `auditprobe`，不进入 `make test`，运行方式：

  ```bash
  go test -tags auditprobe -count=1 -v ./internal/auditprobe/
  ```

  探针直接驱动现有的两个内核实现（未做任何修改），使用真实的 VLESS/TCP 与 VLESS/UDP 连接和已知大小的载荷。本文引用的结果为 2026-09-24 在 linux/amd64、go1.27.1 上的运行结果。

## 0. 结论摘要

1. **三项能力（AGT-12）在两个内核上都不成立。** 增删凭据互不影响只在“基于连接的 TCP 协议”上成立。移除凭据后关闭连接在两个内核上都没有实现，都是空操作。按凭据计量误差为 0 只在 VLESS/TCP 的稳态下成立；sing-box 的 UDP、两个内核的重启与重载、Xray 的“移除后”与 Vision splice 叠加限速的场景都会丢数或记错方向。详见 §4。
2. **sing-box 以数组下标识别用户**，这是最严重的缺陷。上游作者的 `UpdateUsers` 补丁（`3ac22acd`）按下标注册用户，已认证的 QUIC 会话（Hysteria2、TUIC）在每个新流上按旧下标重新取用户名。增删用户后，已有会话的流量会记到另一个用户名下；列表缩短时会越界 panic，并且发生在没有 recover 的 goroutine 中，会使整个进程崩溃。必须在 sing-box fork 中修补（M3-06，属于“增删用户”类补丁）。
3. **通信层需要整体替换**：`internal/panel`、`internal/controlplane/panel.go`、`machine.go` 与 `model/panel.go`。现有的 `controlplane.Source/Sink` 抽象按轮询与推送设计，以 `int` 用户 ID 为键，没有版本、确认与序号，不适合承载 spec/20，也不建议复用。
4. **流量上报不可靠**：计数只在内存中；发送失败后放回内存重发，但没有序号与幂等键。响应丢失时会重复计费，进程崩溃或内核重启时会丢数。与 spec/22 的 WAL 与 `report_seq` 要求差距完整，需要整体新写（M3-04）。
5. 现有模型是“一个节点一个入站”（`model.NodeSpec`），内核实例在启动时按本地配置选定，之后不能切换。spec/21 要求“一个节点多个入站 + 控制面选内核 + 失败回退”，这一重构的工作量不在 M3 现有任务的描述中，见 §7 与未决问题。
6. 协议草案需要修改的地方有 4 项（§6）。其中 mKCP schema 中的 `header_type` 与 `seed` 已被当前 Xray 删除，会导致入站无法加载，属于阻断性问题。

## 1. 包结构地图

| 包 / 目录 | 职责 | 处理 |
|---|---|---|
| `cmd/xboard-node` | 入口：读配置、按实例启动 `service`（节点模式或独立模式）或 `machine`（机器模式），处理信号，15 秒强制退出（`main.go:120-128`） | 改造：新增 `cmd/node-agent` 入口，接入新的 Agent 编排；保留旧入口用于独立模式，或以接线级改动切换（登记 UPSTREAM） |
| `cmd/xbctl` | 运维 CLI：状态、升级、卸载、绑定面板（1561 行，单文件） | 替换：新写 `cmd/agentctl`（`enroll`、`status`、`logs`、`doctor`），不在原文件上改 |
| `internal/panel` | Xboard 面板客户端：REST（`/api/v1/server/UniProxy/*`、`/api/v2/server/*`）、Workerman WS、面板 JSON 类型 | 删除（不再编译）；不跟随上游此部分（AGT-03） |
| `internal/controlplane` | 控制面抽象 `Source`/`Sink`（`types.go:62-87`）；实现：`panel.go`（Xboard）、`machine.go`（机器模式）、`local.go`（独立模式）；`mailbox.go`（机器模式事件合并） | `local.go`、`types.go` 保留，供独立模式使用；`panel.go`、`machine.go` 在节点模式下不再使用；`mailbox.go` 随机器模式留到 M5-03 |
| `internal/service` | 单个节点的主循环：拉取与推送、用户增删、内核启停与重载、周期计量与上报（`service.go`，1272 行） | 改造：独立模式继续使用；节点模式由新的编排包接管，保留其中可复用的函数（校验、证书覆盖） |
| `internal/machine` | 机器模式编排：一个进程按面板下发的节点列表运行多个 `service` | 保留，不在 1.0 前启用（M5-03） |
| `internal/kernel` | 内核抽象 `Kernel`（`kernel.go:44-104`）、用户差异计算、自定义配置合并、geo 数据 | 保留；新接口由新包适配（§3） |
| `internal/kernel/singbox` | 嵌入 sing-box：生成配置、启动与热重载、`UpdatableInbound` 热换用户、`ConnTracker`（按用户计量、在线 IP、限速、设备数） | 保留并修正（计量、关闭会话、身份键），改动登记 UPSTREAM |
| `internal/kernel/xray` | 嵌入 Xray：生成配置、`UserManager` 热换用户、内置 stats 计量、`LimitDispatcher`（以 `go:linkname` 替换默认 dispatcher，`dispatcher.go:27-40`）、带宽限速 | 保留并修正（计量、关闭会话、重载），改动登记 UPSTREAM |
| `internal/kernel/geodata` | 下载与缓存 geoip/geosite | 保留 |
| `internal/cert`、`internal/cert/dnsproviders` | certmagic（HTTP-01、DNS-01、21 个 libdns 服务商）、自签、文件、内容，统一为内存 PEM | 保留；配置改由 `RoutesApply` / 快照下发，DNS 凭据按 NODE-25 解密 |
| `internal/config` | YAML 配置、多实例、独立模式配置、文件监视热重载 | 改造：节点模式只保留控制面地址与状态目录（AGT-05）；独立模式保持不变 |
| `internal/model` | 内核无关的节点规格 `NodeSpec`、`UserSpec`；由面板或独立配置转换而来；自定义路由与出站的校验 | 保留；新增由 proto `Inbound`、`Credential` 转换的函数（新文件）；`panel.go` 转换函数随通信层弃用 |
| `internal/limiter` | 设备数与限速令牌桶（按 UUID 查找） | 保留；键由 UUID 改为凭据 ID |
| `internal/tracker` | 由内核累计值求差得到增量、在线 IP、速度 | 被新的计量包取代（ACC-01 要求读出并清零，不做差分） |
| `internal/monitor` | CPU、内存、磁盘、网卡速率 | 保留，供 `ReportStatus` 使用 |
| `internal/nlog` | 带节点上下文的日志（彩色文本） | 保留；敏感字段脱敏（CONV-24）在调用处修正 |
| `install.sh`、`Dockerfile`、`.github/workflows/ci.yml` | 安装脚本、镜像、CI | 改造（M3-07、M3-08），参数改为 `--server`、`--enroll-token` |

新增的包（AGT-01，全部为 GPL-3.0-or-later 新文件；名称即本计划确定的名称）：

| 新包 | 职责 | 对应任务 |
|---|---|---|
| `internal/agent` | 节点模式的编排：状态机（启动 → 按快照服务 → 连接 → 同步 → 运行 → 退出）、按 NODE-23 应用带版本的指令、内核切换与回退、上报调度 | M3-01、M3-03 |
| `internal/proto` | panel-spec `v0.1.0` 生成代码的依赖入口（`buf` 生成物，或依赖 `panel-spec/gen/go` 模块） | M3-01 |
| `internal/transport/session` | 握手（Hello 与 MAC、HKDF、X25519）、帧加解密（XChaCha20-Poly1305，AD = 方向 ‖ seq）、测试向量 | M3-01 |
| `internal/transport/reliable` | seq 与 ack、待确认窗口（1,000 条或 8 MiB）、重连后以新 seq 重发、`idem_key` 去重记录（24 小时，持久化） | M3-01 |
| `internal/transport/wss`、`internal/transport/longpoll` | 两种通道只负责搬运 `Frame`；信封、加密与确认逻辑都在 `session` 与 `reliable` 中共用（长轮询在 M5-02 实现，M3 先定义接口） | M3-01、M5-02 |
| `internal/enroll` | `POST /v1/enrollments`（protobuf 请求体与响应体、problem+json 错误）、重试（10 分钟同指纹幂等） | M3-02 |
| `internal/state` | 本地状态目录（0700，文件 0600）：控制面地址、PSK、加密快照（HKDF “akari-agent-state-v1”）、WAL、`report_seq`、租约、去重记录 | M3-02、M3-04 |
| `internal/kernelx` | spec/21 §21.3 的 `Kernel` 接口、两个内核的适配器、内核管理器（按 `KernelType` 切换并回退）、能力清单（`KernelSupport`） | M3-03、M3-06 |
| `internal/accounting` | 按凭据读出并清零计数、WAL 追加与截断、`ReportTraffic` 组装、6 天保留 | M3-04 |
| `internal/lease` | 配额租约：首连 1 MiB 临时额度、5 秒未获批关闭、20% 续租、耗尽断开、释放、离线临时租约 | M3-05 |
| `internal/auditprobe` | 本次审计的探针测试（仅带标签编译） | M0-04 |

## 2. 通信层边界

### 2.1 现有的通信入口、接口与数据结构

| 位置 | 内容 | spec 中的替代 |
|---|---|---|
| `internal/panel/client.go:85-103` | `POST /api/v2/server/handshake`，取得 WS 地址与推送、拉取间隔 | 接入：`POST /v1/enrollments`（NODE-02、NODE-18）；之后直接连接 `EnrollResponse.stream_url` |
| `internal/panel/client.go:360-391`、`416-458` | 全局 API Key（`token`）与 `node_id`/`machine_id` 放在请求体或 URL 查询参数中，每个请求都携带 | 每个节点一把 PSK，只用于握手 MAC（NODE-10）；不在任何 URL 中出现 |
| `internal/panel/client.go:218-281` | `GET config` / `GET user`，带 ETag 轮询 | `SyncFull`（带校验和的快照）与 `SyncDelta` |
| `internal/panel/client.go:105-170` | `POST /api/v2/server/report`：流量（`map[userID][up,down]`）、在线 IP、状态、指标合在一个 JSON 中 | `ReportTraffic`（按凭据、带 `report_seq`）与 `ReportStatus` 分开 |
| `internal/panel/ws.go:199-326` | Workerman WS：JSON 文本帧，事件 `sync.config`、`sync.users`、`sync.user.delta`、`sync.devices`、`sync.nodes`、`node.status`、`report.devices`；首帧为 `auth.success` 或 `error`；没有 seq、ack 与加密，写通道满时丢弃消息（`ws.go:311-314`、`474-478`） | `Frame` + `Envelope`（NODE-11、NODE-12）；Ping 25 秒，3 次无响应重连（NODE-07） |
| `internal/panel/types.go`、`internal/model/panel.go` | 面板 JSON 到 `NodeSpec`/`UserSpec` 的转换（弱类型解码） | proto `Snapshot`、`Inbound`（`settings_json` 按 schema）、`Credential` 到内核输入的转换 |
| `internal/controlplane/panel.go:14-184` | `Source`/`Sink` 的 Xboard 实现；WS 事件通道满时丢事件并改为 REST 对账（`panel.go:141-149`） | 由 `internal/agent` 取代 |
| `internal/controlplane/machine.go`、`internal/machine` | 机器模式（一台机器、一条 WS、多个节点） | M5-03；1.0 前每个节点各建一条连接 |
| `internal/service/service.go:122-130` | 按 `cfg.IsStandalone()` 选择 `LocalControlPlane` 或 `PanelControlPlane` | 节点模式改为构造 `internal/agent`；独立模式不变 |
| `internal/service/service.go:142-151` | 内核在构造时按本地 `cfg.Kernel.Type` 选定，运行期不可更换 | 内核由 `Snapshot.kernel` / `InboundApply` 决定（AGT-07），由 `kernelx.Manager` 切换 |
| `internal/model/validate.go:15-18` | 本地 `kernel.type` 优先于面板下发的 `kernel_type` | 与 AGT-07 相反：节点模式下忽略本地值 |
| `internal/config`（`Panel`、`WS`、`Machine` 字段） | 面板地址、全局 token、节点 ID、WS 参数 | 只保留控制面地址与状态目录；PSK 与节点 ID 在 `internal/state` 中 |

### 2.2 替换后的接口

`internal/agent` 不实现 `controlplane.Source/Sink`。原因：该接口把“拉取快照”和“推送事件”分成两条路径，事件以 `int` 用户 ID 为键，没有 `config_version`、确认与回退语义，事件通道满时会丢事件（`panel.go:141-149`）。在它上面实现 NODE-12、NODE-13、NODE-23 需要改动原有文件的语义，违反 AGT-01。

新接口的边界（签名为计划，M3-01 可调整名称，但不改职责）：

```go
// internal/transport/reliable
type Conn interface { // WSS 与长轮询各实现一份，只搬运已编码的 Frame
    Send(ctx context.Context, frame []byte) error
    Recv(ctx context.Context) ([]byte, error)
    Close(code int, reason string) error
}

type Session interface { // 握手之后的可靠会话，与通道无关
    Send(ctx context.Context, msg *nodev1.Envelope) error // 分配 seq、idem_key、写入待确认窗口
    Inbox() <-chan *nodev1.Envelope                       // 已按 idem_key 去重
    Acked() <-chan uint64                                 // 对端确认到的 seq
}

// internal/agent
type Applier interface { // 由内核管理器实现，编排器按 NODE-23 调用
    ApplySnapshot(ctx context.Context, s *nodev1.Snapshot, raw []byte) error // 失败即回退（AGT-07）
    UpsertCredentials(ctx context.Context, version uint64, c []*nodev1.Credential) error
    RemoveCredentials(ctx context.Context, version uint64, ids []string) error // 1 秒内关闭连接
    ApplyInbounds(ctx context.Context, version uint64, kernel nodev1.KernelType, in []*nodev1.Inbound) error
    ApplyRoutes(ctx context.Context, version uint64, routesJSON, dnsSecret []byte) error
    AppliedVersion() uint64
}
```

调用点的接线（原有文件只做接线级改动，登记 UPSTREAM）：`cmd/xboard-node/main.go:189` 附近按部署模式选择 `agent.New` 或 `service.New`；其余调用在新包内。

### 2.3 需要一并修正的日志泄露（CONV-24）

以下代码虽然随通信层替换或内核修正而改动，仍在此登记，避免新代码沿用：

- `internal/panel/ws.go:213`：Debug 日志输出带 `token` 查询参数的完整 WS URL。
- `internal/kernel/singbox/conntracker.go:224-225`、`282-283`：Info 日志输出 `metadata.User`（即用户 UUID，也就是代理凭据）与完整来源 IP。
- `internal/kernel/xray/dispatcher.go:150`：Debug 日志输出完整来源 IP。

## 3. 内核抽象对照

目标接口（spec/21 §21.3，本计划确定的名称见最后一列；放在 `internal/kernelx`）：

| spec/21 方法 | 现有实现（`internal/kernel/kernel.go`） | sing-box 现状 | Xray 现状 | 差距与改造 |
|---|---|---|---|---|
| `Start(ctx, InboundSet)` | `Start(nodeSpec, users, tls)`（`kernel.go:57`） | 生成配置后新建 box 并替换；旧实例后台回收（`singbox.go:92-162`） | 同上，旧实例后台排空最长 5 分钟（`xray.go:117-173`、`674-692`） | 只支持单入站（`model/types.go:5-48`，入站标签固定为 `<protocol>-in`：`singbox/config.go:434`、`xray/config.go:206`）；凭据与入站耦合传入；没有 ctx。改为 `Start(ctx, InboundSet)`，`InboundSet` = 内核类型、入站列表（每个带 tag 与 version）、凭据、路由、证书 |
| `ApplyInbounds(cfg)`：只中断被修改或删除的入站 | `Reload(nodeSpec, users, tls)`（`kernel.go:65`） | 配置哈希一变就删除并重建全部入站，路由变化也会触发（`singbox.go:233-302`）；已有 TCP 连接在探针中存活（路由变更） | 哈希一变就整实例重启（`xray.go:183-196`）；`lastKernelHash` 只在 `Start` 中更新（`xray.go:158`），增删用户之后任何一次 `Reload` 都会整实例重启 | 按 tag + `Inbound.version` 做差异：未变化的入站不动；sing-box 用 `InboundManager.Remove/Create` 逐个替换；Xray 用 `inbound.Manager.RemoveHandler/AddHandler` 逐个替换，路由用 router 的规则替换接口，不再整实例重启 |
| `UpsertCreds(tag, []Credential)`：不影响其他凭据 | `AddUsers`、`UpdateUsers`（`kernel.go:71-79`） | 每次都重新生成完整配置，并对每个入站调用 `UpdateUsers(全量列表)`（`singbox.go:524-614`）；fork 的实现以下标识别用户（§4.1） | `UserManager.AddUser/RemoveUser`（`xray.go:288-361`），以 email（`user@<id>`，`xray/config.go:239-241`）识别 | 身份键改为凭据 ID（sing-box 的 `name`、Xray 的 `email`），不再使用秘密值（UUID）作为用户名；同一凭据秘密变化时先关闭再加入；按 tag 施加 |
| `RemoveCreds(tag, ids)`：关闭这些凭据的全部连接 | `RemoveUsers` + `CloseUserConnections`（`kernel.go:75`、`92`） | `RemoveUsers` 只把用户从认证表中去掉；`CloseByUUID` 是空操作（`conntracker.go:458-463`） | `RemoveUsers` 只调用 `validator.Del`；`CloseUserConnections` 是空操作（`xray.go:253-256`） | 两个内核都要在 fork 中补“按凭据关闭会话”（§4.3）；Agent 侧在 1 秒计时内完成并验证 |
| `Sessions(credID) []Session` | 无；只有 `GetUserTraffic` 返回在线 IP 与总连接数（`kernel.go:88`） | `connMap` 只登记 TCP，UDP 不登记（`conntracker.go:239-242`、`261-264`） | 无逐连接记录，只有 dispatcher 的 IP 引用计数（`dispatcher.go:159-173`） | 新增：按凭据登记连接（ID、来源前缀、开始时间、网络类型、所属父连接），供关闭、设备数与 `SourceSet`（ACC-12）使用 |
| `Capabilities() KernelSupport` | `Protocols()` + `Capabilities()`（布尔能力位）（`kernel.go:49-51`） | 协议列表含 matrix 之外的 naive、socks、http、mieru、hysteria v1（`singbox.go:85-90`） | 协议列表用 `hysteria` 表示 Hysteria2（`xray.go:97-102`） | 返回 proto `KernelSupport`：内核版本（`runtime/debug.ReadBuildInfo` 中的 replace 版本）、稳定与实验协议、传输；以 spec/21 矩阵为上限，不上报矩阵之外的协议 |
| `Close()` | `Stop()`（`kernel.go:59`） | 关闭监听，排空 5 秒后强制关闭（`singbox.go:340-372`） | 排空 5 秒后关闭（`xray.go:200-214`） | 名称对齐即可；退出流程按 21.4 第 4 步：停止接受、刷写 WAL、最多等 30 秒。现有进程级强制退出为 15 秒（`cmd/xboard-node/main.go:126`），需改为 30 秒 |
| 规格未列出，但 spec/22 需要 | `GetUserTraffic` 返回累计值 | 累计值（`conntracker.go:419-441`） | 读出并清零内置计数器，再累加到 `cumTraffic`（`xray.go:709-743`） | 新增 `DrainTraffic() map[credID]Counters`：以原子交换读出并清零（ACC-01），并包含已移除凭据与已回收实例的剩余计数 |
| 规格未列出，已有 | `SetSpeedLimitFunc`、`SetDeviceLimitFunc`、`UpdateGlobalDevices`、`ClearGlobalDevices` | 已实现 | 限速走 fork 补丁的带宽管理器；设备数在 dispatcher 中执行 | 保留；键改为凭据 ID；`UpdateGlobalDevices` 对应 `SourceSet`（M4-06） |

内核切换（AGT-07）：现有代码没有这一能力。`kernelx.Manager` 持有当前内核；收到不同的 `KernelType` 时，先用新内核 `Start`，成功后再 `Close` 旧内核；失败时保留旧内核与旧入站，并在 `ReportStatus.kernel_error` 中报告。两个内核同时绑定相同端口时，sing-box 与 Xray 都需要 `SO_REUSEPORT`（Xray 配置已设置 `reusePort`：`xray/config.go:211-213`；sing-box 需要在 listen 选项中设置），否则只能先停后启，回退路径也要按这个顺序处理。

## 4. 三项能力现状（AGT-12）

### 4.1 结论表

| 能力 | sing-box | Xray-core | 证据 |
|---|---|---|---|
| 增删凭据不影响其他连接 | **部分成立**。基于连接的 TCP 协议（VLESS、VMess、Trojan、SS、AnyTLS）成立；**Hysteria2、TUIC 不成立**：用户以数组下标识别，增删用户后，已有 QUIC 会话的新流会记到另一个用户名下，列表缩短时越界 panic 并使进程崩溃 | **成立**（UserManager 以 email 为键）。例外：增删用户后，任何一次 `Reload` 都会整实例重启（哈希未更新），影响全部连接 | 探针 `TestProbeAddRemoveIsolation` 在两个内核上均通过（VLESS/TCP：新增 C、移除 A 之后 B 的已有连接与新连接正常）。sing-box：fork 的 `protocol/hysteria2/user.go:7-19`、`protocol/tuic/user.go:9-30` 按下标注册；`protocol/hysteria2/inbound.go:158-161`、`180-182`、`protocol/tuic/inbound.go:115-117` 每个流都取 `userNameList[userID]`，没有越界检查；sing-quic `v0.6.0` `hysteria2/service.go:196-202` 只在会话开始时认证并保存下标，`261-268`、`277` 在无 recover 的 goroutine 中为每个流复用该下标。另外 VLESS、VMess、Trojan、SS 的 `h.users = users` 与连接处理中的 `h.users[userIndex]` 之间没有锁（`protocol/vless/user.go:9`、`protocol/vless/inbound.go:170-179`），存在数据竞争与短暂错配。Xray：`xray.go:158`、`183-196` |
| 移除凭据后 1 秒内关闭其全部连接 | **不成立**。已有连接不关闭，新连接被拒绝；已认证的多路复用会话与 QUIC 会话还能继续开新流 | **不成立**。同上 | 探针 `TestProbeRemoveClosesSessions`：两个内核上被移除用户 A 的已有连接在 1 秒后仍能收发，已移除用户的新连接被拒绝。sing-box：`conntracker.go:458-463`（`CloseByUUID` 空操作）；多路复用子流经 `common/mux/router.go:70-73` 进入 mux 服务，父连接不经过 tracker（`route/route.go:153-155` 只包装子流），只关闭子流不能阻止父连接开新流。Xray：`xray.go:247-256`（两个关闭方法都是空操作）；`proxy/vless/inbound/inbound.go:244-248`、`proxy/hysteria/server.go:61-63` 的 `RemoveUser` 只删除验证表项 |
| 按凭据计量误差为 0 | **不成立**。VLESS/TCP 稳态准确；**UDP 错误**：上行记为下行，下行丢失；内核重启时丢数 | **不成立**。VLESS/TCP 与 UDP 稳态准确；**移除后的字节丢失**；**重载（整实例重启）后字节丢失**；Vision splice 与限速同时启用时下行少计 | 探针 `TestProbeMeteringExact`：两个内核 4 × 256 KiB 往返都得到 `[1048576 1048576]`。`TestProbeUDPDirection`（上行 1000 B、下行 3000 B）：sing-box 得到 `[0 1000]`，Xray 得到 `[1000 3000]`。`TestProbeRemoveClosesSessions`：移除 A 之后再传 5000 B，sing-box 计入（`[6020 6020]`，含探测字节），Xray 的 A 计数消失（`[0 0]`）。`TestProbeReloadMetering`（只改路由）：sing-box `[30000 30000]` 正确；Xray 连接存活，但计数为 `[0 0]`，重载后的 20000 B 全部丢失 |

### 4.2 计量问题逐项

| 编号 | 内核 | 问题 | 位置 | 修正方 |
|---|---|---|---|---|
| MTR-1 | sing-box | UDP 零拷贝路径的计数方向反了：`UnwrapPacketReader` 返回下载计数器，`UnwrapPacketWriter` 返回上传计数器；`ReadPacket`、`WritePacket` 中的方向是正确的，两条路径不一致。探针观测到上行被记为下行，下行完全缺失 | `internal/kernel/singbox/conntracker.go:790-802`（对照 `701-755`） | node-agent（在 M3-04 中修正，改动登记 UPSTREAM） |
| MTR-2 | sing-box | 内核重启时新建 tracker，旧实例排空期间（5 秒）的字节与上次读取之后的余量无人读取 | `singbox.go:143`、`157`、`167-190` | node-agent：重启前 drain 旧 tracker，并在回收结束时再 drain 一次 |
| MTR-3 | sing-box | 以用户名（等于秘密值 UUID）查找用户 ID，查不到时记到 `uid=0`、`us=nil`，即不计数；只要某一时刻映射与内核不一致，就会漏计 | `conntracker.go:212-217`、`270-275` | node-agent：以凭据 ID 作为 name，直接作为键 |
| MTR-4 | sing-box | Hysteria2、TUIC 的下标错配导致字节记到其他用户名下（见 4.1） | fork `protocol/hysteria2/inbound.go:158-161`、`protocol/tuic/inbound.go:115-117` | sing-box fork（M3-06） |
| MTR-5 | Xray | 只读取当前用户列表的计数器；已移除用户的计数器仍在累加，但不再被读取 | `xray.go:720-741`；fork `app/dispatcher/default.go:165-186`（计数器由 `GetOrRegisterCounter` 注册，移除用户后不注销） | node-agent：遍历 stats 管理器中所有 `user>>>` 计数器，或在移除时最后读取一次，再保留读取到连接结束 |
| MTR-6 | Xray | 整实例重启时 `cumTraffic` 被清空，旧实例在后台排空（最长 5 分钟）期间的计数不再读取 | `xray.go:157`、`163`、`674-692` | node-agent：`ApplyInbounds` 改为逐入站替换，不整实例重启；必须重启时，旧实例关闭前再读一次 |
| MTR-7 | Xray | Vision splice 的用户下行计数依赖 `writer.(*dispatcher.SizeStatWriter)` 类型断言；上游作者的带宽补丁在其外层包了 `RateLimitWriter`，有限速的用户在 splice 路径上少计下行（splice 同时绕过限速） | fork `proxy/proxy.go:752-768`（`754`）、`app/dispatcher/default.go:231`、`290-298` | Xray fork（M3-06，属于“计量钩子”补丁）：splice 前向内解包 writer 查找计数器，或者对有限速的用户禁用 splice。未写探针，需要 Linux + Vision + 限速的环境验证 |
| MTR-8 | 两者 | `tracker.Process` 遇到计数器回退时取当前值作为增量，丢弃回退前未读的部分 | `internal/tracker/tracker.go:95-101` | 被 `internal/accounting` 取代（读出并清零，不做差分） |
| MTR-9 | 两者 | 计量口径：两者都在“解密后的代理载荷”层计数（sing-box 在 router 的 tracker 包装：`route/route.go:153-155`；Xray 在 dispatcher 链路：`app/dispatcher/default.go:165-186`、`201-243`）；多路复用只计子流载荷，不计 mux 帧头。与 AGT-12 口径一致 | 同左 | 无需修改；一致性测试需要覆盖 mux、XUDP、Hysteria2、TUIC |

### 4.3 关闭会话的补丁方向（M3-06）

- **sing-box**（fork 补丁，类别“按凭据关闭会话”）：在各入站的连接入口按凭据登记“顶层连接”，包括 TCP 连接、mux 父连接与 QUIC 会话。提供 `CloseUser(name)`：关闭该用户的全部顶层连接（mux 父连接关闭后，子流随之结束），并断开 QUIC 连接（`quicConn.CloseWithError`）。同时修正用户身份：`UpdateUsers` 改为按名称（凭据 ID）建表，QUIC 会话保存用户名而不是下标，并加锁（类别“增删用户”）。node-agent 侧的 `ConnTracker` 同时拒绝 `metadata.User` 不在当前凭据集中的新子流，作为兜底。
- **Xray**（fork 补丁，类别“按凭据关闭会话”）：`LimitDispatcher` 只能看到逐请求的链路，无法关闭 mux.cool 父连接与 Hysteria 的 QUIC 会话。补丁在入站 worker 为每条已认证的顶层连接登记 `email → cancel/close`，`RemoveUser` 之后由新接口 `CloseUserConnections(email)` 统一关闭，范围包括 TCP、mux.cool、XUDP 与 Hysteria。node-agent 侧在 dispatcher 中拒绝 email 不在当前集合中的新请求，作为兜底。
- 验收：`make conformance` 为每个“协议 × 传输”（按 spec/21 矩阵生成）断言“移除后 1 秒内所有连接收到 EOF 或 RST，且之后不再产生计数”。本次的探针可以作为这一套件的起点。

## 5. 流量上报可靠性

### 5.1 现状

- **数据流**：`trackAndEnforce` 每 `TrackInterval` 秒读取一次内核累计值并求差（`service.go:939-960`，`tracker.go:80-131`），增量累积在内存 `pendingTraffic` 中；`pushReportAsync` 每 `PushInterval` 秒（默认 60 秒）取出并清空（`service.go:978`），以 HTTP POST 发送（`service.go:987`，`client.go:105-170`）。
- **断线与失败**：发送失败时把这一批加回内存（`service.go:989-991`，`tracker.go:144-153`），与之后的增量合并，在下一个周期整体重发。退避期间跳过发送但保留数据（`service.go:972-976`）。
- **去重**：没有。请求中没有序号或幂等键。HTTP 客户端超时为 30 秒（`client.go:55`）；请求已被面板处理、但响应丢失或超时时，这批数据会被加回并再次发送，**造成重复计费**。
- **持久化**：没有。进程崩溃、被强杀或 OOM 时，内存中未发送的增量（最多约一个推送周期）与内核中尚未读取的部分全部丢失。正常退出时只尝试同步发送一次（`service.go:1004-1018`），失败即丢失；进程在 15 秒后被强制退出（`cmd/xboard-node/main.go:126`）。
- **内核重启与重载**：见 MTR-2、MTR-6、MTR-8，会丢数。
- **WS 通道**：流量不走 WS。WS 的写通道满时直接丢弃消息（`ws.go:311-314`）。

结论：现有上报在“断线后恢复”这一场景下大体不丢数，但**既不防重也不防崩溃丢失**，不满足 ACC-01、ACC-02、ACC-03。

### 5.2 与 spec/22 的差距与做法（M3-04）

| 规则 | 要求 | 现状 | 做法 |
|---|---|---|---|
| ACC-01 | 原子交换读出并清零，不丢数 | 内核累计值加差分（Xray 读后清零，但又累加回 `cumTraffic`） | `kernelx.DrainTraffic` 以原子交换读取；已移除凭据与回收中的实例也要读取 |
| 22.1 第 1 步 | 先追加 WAL 再发送，确认后截断 | 无 WAL | `internal/accounting`：每个报告周期把 `ReportTraffic{report_seq, window, items}` 追加到 WAL 并 fsync，再交给 `reliable.Session`；收到覆盖该信封 seq 的 `ack` 后截断 |
| ACC-02 | WAL 格式兼容旧文件；重启后先重传；最多保留 6 天 | 无 | 记录带格式版本与 CRC；启动时先重放未确认的报告，并保留原 `report_seq` 与 `idem_key`；超过 6 天的丢弃并告警 |
| ACC-03 | `report_seq` 严格递增、永不回退，并持久化；新装时从 `HelloAck.last_report_seq + 1` 开始 | 无 | 计数器与 WAL 放在同一状态目录；每次分配前先持久化；取 `max(本地, last_report_seq) + 1` |
| NODE-14 | 节点 → 控制面的流量报告永不丢弃，窗口满时背压 | 写通道满即丢 | `reliable` 窗口满时暂停从 WAL 取数，报告留在 WAL 中 |
| 21.4 第 3 步 | 常规每 30 秒上报，租约低于 20% 时缩短到 5 秒 | 默认 60 秒，最短 5 秒（`service.go:186`） | 由 `internal/lease` 通知 `accounting` 缩短周期 |
| 按凭据计量 | 键为 `credential_id` | 键为 `int` 用户 ID | 身份统一为凭据 ID（§3） |

## 6. 对协议草案的修改建议

以下各项只列出并说明理由，未修改 spec 与 panel-spec。按 CLAUDE.md，应由 spec-owner 通过 `spec-change` 流程处理；是否纳入 v0.1.x 见“未决问题”。

1. **mKCP schema 与当前 Xray 不兼容（阻断）**。`panel-spec/schemas/inbound/vless-mkcp.schema.json`（与 `vmess-mkcp`）定义了 `mkcp.header_type` 与 `mkcp.seed`。本组织 Xray fork 在配置中出现 `header` 或 `seed` 时直接报错：“mkcp header & seed 已删除，改用 finalmask/udp header-* 与 mkcp-original、mkcp-aes128gcm”（`xray-core/infra/conf/transport_internet.go:110-111`）。按现有 schema 下发的入站无法加载。建议删除这两个字段，改为 finalmask 的对应字段（名称与取值需要由 spec-owner 对照 Xray 的 `finalmask` 配置确定）。这一项同时完成 AGT-14 要求的“mKCP 字段在 M0-04 审计时确认”。
2. **`Credential.secret` 的解释规则需要按协议写明**。当前 proto 注释只写“按协议解释：UUID、密码等”（`messages.proto:16`）。凭据不带入站 tag，按现在的设计会施加到节点的全部入站上，而同一份秘密值要同时满足：
   - VLESS、VMess、TUIC 需要 UUID；
   - Trojan、Hysteria2 需要任意字符串；
   - TUIC 同时需要 UUID 与密码；
   - Shadowsocks 2022 多用户需要长度恰好为 16 或 32 字节的 base64 密钥。

   现有代码的做法彼此不一致：sing-box 把 UUID 文本截断或补零到密钥长度后做 base64（`internal/kernel/singbox/config.go:513-527`）；Xray 直接把 UUID 文本当作密钥（`internal/kernel/xray/xray.go:572-575`）。同一凭据在两个内核上会得到不同的 SS2022 密钥，而且 32 字节密码套件的密钥熵不足。建议在 proto 注释与 spec/21 中规定：`secret` 为 16 字节随机值，各协议所需的形式由它确定性派生（例如 UUID = 按 RFC 9562 设置版本位后的 16 字节；密码 = base64url；SS2022 密钥 = `HKDF-SHA256(secret, info = "akari-ss2022-" ‖ method, L = 密钥长度)`），并提供测试向量。这一规则同时影响控制面导出配置（spec/23）。
3. **`Credential` 与入站的关系需要写明**。spec/21 §21.3 的 `UpsertCreds(tag, …)`、`RemoveCreds(tag, …)` 以入站 tag 为参数，而 `CredUpsert`、`CredRemove`、`SyncDelta` 与 `Snapshot.credentials` 都不带 tag。建议在 spec/20 中写明“凭据施加于该节点的全部入站”；或者，如果要支持“某凭据只用于部分入站”，就需要新增字段。按 AGT-12 与 NODE-24，前者更简单。Agent 按前者实现，接口中的 tag 参数仅供内核适配器内部使用。
4. **Hysteria2 在 Xray 上的实验状态应当保留，并补充说明**。Xray 的 Hysteria 入站在 node-agent 中只接受 v2（`internal/kernel/xray/config.go:404-406`）；Xray 的 QUIC 会话同样存在“移除后仍能开新流”的问题。建议在 spec/21 AGT-12 的验收中写明：实验协议在三项能力达标之前，不得从“实验”改为“稳定”。这一项只增加说明，不改契约。

以下情况已核对，不需要修改契约：`TrafficItem.raw_up/raw_down` 的口径与两个内核的计数层一致（MTR-9）；`ReportStatus.running_kernel` 与 `kernel_error` 足以报告切换失败；`Capabilities.kernels` 能表达两个内核的协议与传输。

## 7. M3 工作量估算

单位为人日，指一名熟悉 Go 的开发者或等价的单会话工作量，包含测试与评审修改，不含等待。区间的上限考虑了内核 fork 调试的不确定性。

| 任务 | 估算 | 主要工作 | 依赖与风险 |
|---|---|---|---|
| M3-01 替换通信层 | 8–10 | `transport/session`（握手、HKDF、XChaCha、测试向量）、`transport/reliable`（seq/ack、窗口、重发、`idem_key` 持久化去重）、`transport/wss`、`internal/agent` 编排与 NODE-23 版本规则、`hello_reject` 各原因的处理、移除 `internal/panel` 的编译依赖；让 `e2e/conformance` 对真实 Agent 通过 | 依赖 M0-07 的模拟 Agent 与一致性套件；长轮询只定义接口 |
| （新增）多入站与凭据模型重构 | 6–8 | `NodeSpec`（单入站）→ `InboundSet`（多入站、按 tag 与 version）；`settings_json` 按 schema 转换为两个内核的配置；身份键 `int` 用户 ID → 凭据 ID；`ApplyInbounds` 逐入站替换 | 现有 M3 任务没有覆盖；可以并入 M3-03，但会使 M3-03 从 3 天增加到约 10 天（见未决问题） |
| M3-02 接入令牌与本地凭据存储 | 3–4 | `internal/enroll`、`internal/state`（0700/0600、PSK、加密快照、原子替换）、`agentctl enroll`；security-reviewer 评审 | 依赖 M2-01 的接口实现 |
| M3-03 控制面选内核与能力上报 | 3–4（不含多入站重构） | `kernelx.Manager`（切换、回退、`SO_REUSEPORT`）、`KernelSupport` 生成、忽略本地 `kernel.type` | 端口复用在两个内核同时运行时的行为需要实测 |
| M3-04 计量、WAL、`report_seq` | 5–6 | `DrainTraffic`、修正 MTR-1/2/3/5/6/8、WAL（格式、CRC、fsync、截断、6 天）、重启重传、背压；concurrency-reviewer 评审；断线与重启的不丢不重测试 | 依赖 M3-01 的 ack 语义 |
| M3-05 配额租约 | 5–7（node-agent 侧） | `internal/lease`（首连临时额度、5 秒超时、20% 续租、耗尽断开、释放、离线临时租约、全量同步后重新申请）；与 panel 联调；5 节点满速测试 | 断开依赖 M3-06 的关闭会话能力 |
| M3-06 三项内核能力补齐 | sing-box 5–7，Xray 5–7 | sing-box：按名称识别用户、QUIC 会话保存名称、加锁、`CloseUser`（TCP、mux 父连接、QUIC）；Xray：顶层连接登记与 `CloseUserConnections`（TCP、mux.cool、XUDP、Hysteria）、splice 计数（MTR-7）；两个 fork 的 `PATCHES.md` 登记；按矩阵生成的内核一致性测试（`make conformance`） | 最大风险项。审计已确认两个内核都有缺口，不是“若审计发现缺口”。补丁需要能随上游 rebase |
| M3-07 安装脚本、Docker、`agentctl` | 3–4 | 新 `install.sh` 参数、`agentctl status/logs/doctor`（时钟、端口、证书、连通性、权限）、镜像与 volume | 可与 M3-04、M3-05 并行 |
| M3-08 许可证与来源整理 | 1–2 | `REUSE.toml` 登记全部上游文件、`LICENSES/`、`NOTICE`、新文件 SPDX 检查进入 CI | 上游文件没有逐文件的 MPL 声明，见未决问题 |
| M3-09 部署文档与 v0.1 发布 | 3–4 | 单机部署文档（ACC-07 三种情况）、签名与 SBOM、`TestM3_Full` | 依赖以上全部 |
| **合计** | **约 47–63 人日** | | 5 周工期需要 2–3 条并行线：通信层（M3-01 → M3-02）、内核（重构 → M3-06 → M3-03）、计量（M3-04 → M3-05）；M3-07、M3-08 穿插进行 |

## 8. 其他发现

- `go.mod` 的 `replace` 仍指向 `cedar2025/*`，还没有改为本组织的副本（AGT-04），应在 M3-06 开工时先改。
- 模块路径仍为 `github.com/cedar2025/xboard-node`。是否改名见未决问题；改名会触及全部原有文件的 import 行。
- `make test` 不带 `with_quic` 等构建标签（`Makefile` 的 `test` 目标），Hysteria2、TUIC 相关代码在 CI 中没有被编译测试；`make conformance` 目标还不存在。
- CI 使用 Go 1.25.7（`.github/workflows/ci.yml`），`go.mod` 要求 `go 1.26`，依赖工具链自动下载才能通过。
- Xray 集成通过 `go:linkname` 改写 xray-core 内部的 `typeCreatorRegistry` 来替换 dispatcher（`internal/kernel/xray/dispatcher.go:27-40`）。升级 Xray 时可能静默失效，应在一致性测试中断言 `LimitDispatcher` 已安装。
- 在机器模式或多实例模式下，`globalLimitDispatcher` 是进程级全局变量，只由创建锁 `xrayCreationMu` 串行化（`xray.go:132-135`）；一个进程中只运行一个内核实例时没有问题。
