# SPDX-License-Identifier: GPL-3.0-or-later
#
# 本组织新增的检查目标（spec/42 42.2），由 Makefile 末尾的 include 引入，以免改动上游 Makefile 的正文。
# 本地与 CI 执行同一目标：make ci。REUSE lint 由 CI 的 reuse 任务执行（需要 reuse 工具）。
# 协议一致性套件与内核一致性测试在 M3 加入（M3-06）。

GOVULNCHECK_VERSION := v1.8.0
RELEASE_TAGS := with_quic,with_utls,with_wireguard,with_acme,with_clash_api

.PHONY: ci check-spdx licenses vulncheck reuse

ci: check-spdx licenses test build

# 前两行之内必须有 SPDX 标识（CONV-25）；REUSE.toml 中登记的原有文件除外。
check-spdx:
	python3 scripts/check_spdx.py

# 依赖许可证扫描；例外登记在 LICENSE-EXCEPTIONS。
licenses:
	python3 scripts/check_licenses.py

vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) -tags $(RELEASE_TAGS) ./...

reuse:
	reuse lint
