---
name: kernel-conformance
description: 修改 Kernel 接口、任一内核实现或 fork 补丁后使用，确保两个内核行为一致。
---
# 内核一致性

1. 运行 `make conformance`；可用 `PROTO=`、`KERNEL=`、`CASE=` 缩小范围。
2. 必须通过的场景：增删凭据不影响他人连接；计量误差为 0；移除凭据后 1 秒内关闭全部会话；限速生效；入站重建后恢复服务。
3. 两个内核结果不同时，先判断哪一个符合 spec/21；不可调和的差异记录到 `internal/kernel/CLAUDE.md` 的“已知差异”，并在控制面按能力位处理。
4. 大范围验证（全部协议 × 两个内核）使用 workspace 的 `workflows/core-sync.md`。
