# Grok2API & CPA 出口质量守护

这是一个非官方增强分发仓库：为 [chenyme/grok2api](https://github.com/chenyme/grok2api) 提供固定代理快速恢复和出口质量守护补丁，同时提供零 Grok2API 运行时依赖的纯 CPA 原生插件。仓库不复制上游完整源码，只发布可审计的 Git patch、功能说明、AI 指南和 CPA 插件源码。

当前补丁基于：

- 上游版本：`v3.1.2`（质量守护与固定代理快速恢复已合入官方）
- 上游提交：`6e9eef7619b83899c82e24353177c8a819f15914`
- 今日增量：探针方案 + 双探针恢复 + thinking 守卫（输出 ≥ 32 且 reasoning=0 即隔离）
- 补丁文件：`patches/0005-fix-missing-thinking-32-token-floor.patch`（叠在 0004 上）
- 上一增量：`patches/0004-fix-dual-probe-recovery-and-thinking-guard.patch`
- 上游 PR：[chenyme/grok2api#930](https://github.com/chenyme/grok2api/pull/930)
- 可运行 Fork：[lij768423-svg/grok2api](https://github.com/lij768423-svg/grok2api) `main`

仍停在 `v3.0.11` 时，继续使用遗留补丁 `patches/0001-feat-add-egress-recovery-and-quality-guard.patch`（对应已关闭的 [#837](https://github.com/chenyme/grok2api/pull/837)）。

## 包含功能

### 固定代理快速恢复

- 请求提交前发生连接拒绝、reset、timeout 或 EOF 时，固定节点先进入冷却，再立即异步复测。
- 同一节点的并发故障只启动一个探针。
- 后续绑定请求最多等待 5 秒；复测健康后重新读取持久化状态并继续，不健康则保留冷却。
- 请求取消立即停止等待，不会取消共享探针。
- 不重放已经提交的生成请求，也不把认证、额度或限流错误当作代理故障。
- 官方已有的代理池模式继续按新隧道处理，单个旋转出口失败不会冷却整个池。

### 出口质量守护

- 被动审计按 grok2api 面板同口径计算 `输出 Token / (总耗时 - 首字耗时)`，其中输出 Token 包含推理 Token。
- **被动硬阈值立即隔离节点**；软阈值触发固定 Prompt 主动复测，连续命中后才隔离。
- 主动软/硬阈值、连续探测错误、最低健康节点、隔离与自动恢复保护。
- 严格模式下先摘流再确认；短窗口流式缓冲突增会先在原 IP 复测，确认异常后才换 IP。
- 支持受信任的节点级换 IP Webhook，以及 1024Proxy `sid-...-t-...` 粘性会话轮换器。
- 新 IP 只执行一次真实模型质量检测；正常立即恢复，异常或不确定则保持隔离。
- 账号调度失败与代理故障分开处理：暂无可调度账号时延后复测，不累计代理错误、不浪费流量换 IP。
- 目标节点绑定账号不可调度时，管理员质量探针会借用任意健康 Build 账号，但实际请求仍强制走被测节点；普通流量不受影响。
- 整个账号池不可用时按独立长退避延后检测并抑制重复日志，节点仍保持隔离。
- 管理端质量守护页面、手动诊断、策略热加载和累计统计。
- 手动检测与节点操作使用单条可更新提示；隔离或轮换中的节点禁止并发手动检测。
- 在节点质量表中直接添加、编辑、删除、启用、停用和刷新 Build 代理节点。
- 支持单选、全选、批量启用、批量停用和批量删除，并为删除操作提供确认。
- `QUALITY_GUARD_NODE_IDS` 留空时自动发现所有已启用的代理 Build 节点；状态文件同时发布已解析节点，兼容旧版管理页面。
- 独立 Python sidecar、Docker Compose、systemd、安全说明和中英文文档。

### 探针方案（v3.1.2+ 增量）

- 质量守护页增加「探针方案」页签：内置 **预期标记**（最后一行 `QUALITY_OK`）和 **吞吐基线**，也可自建 Prompt / 包含 / 末行 / 正则。
- 标记缺失记为硬异常；短回复命中标记时不因虚高 TPS 或 Token 过少误杀。
- 方案存在 `profiles.json`（与 runtime-config 同目录）；状态 API 只回名称和是否有标记，不回 Prompt / 标记正文。
- 接口：`GET/POST /api/admin/v1/egress-quality-guard/profiles`，`PUT/DELETE .../profiles/{id}`；质量检测可带 `profileId`。

### 降智账号面板（v3.1.2 增量）

- 质量守护页增加「降智账号」页签：按请求审计把用户流式请求（不含 quality-test 探针）归类为 `buffered_burst` / `soft_tps` / `hard_tps`。
- 口径与面板一致：`outputTokens * 1000 / (durationMs - firstTokenMs)`，默认 soft 500、hard 1000；生成窗口短于 1s 且达到 soft 记为 `buffered_burst`。
- 支持 1h / 6h / 24h / 7d 窗口，按邮箱/ID、调度状态、类型、命中次数筛选。
- 时序条从底部堆叠；任意行可勾选，批量「禁掉所选」或「解除禁用」，走现有账号 batch API（`ids` 为字符串）。
- 接口：`GET /api/admin/v1/request-audits/degrade-accounts`。

### CPA 原生出口守护插件

`cpa-plugin/` 现为 **v1.0.10 纯 CPA 原生插件**，不依赖、不连接 Grok2API 运行时。它通过 CPA Host API 读取认证文件和 Usage 事件，把账号的 `proxy_url` 粘性绑定到出口节点，并提供节点 CRUD、逐行批量导入、批量操作、连通性/真实质量检测、可配置探针方案（吞吐基线 / 预期标记 / 自定义 Prompt）、隔离迁号、策略热加载、统计事件和深浅色管理 UI。v1.0.10 起管理页不再对上万账号做 N+1 `host.auth.get`，连通探测支持纯 IPv6 SOCKS（`api64`/`api6`），并正确识别 `socks5h://` 与未加括号的 IPv6 代理 URL；v1.0.9 起主动探测可按方案校验最后一行或正则标记；v1.0.8 起商店安装后注册不再同步扫认证文件，避免多账号时一直「未生效」；v1.0.7 起 CPA 调度跳过隔离/冷却出口，账号或额度错误只记为 ignored，迁移会写后读回校验，并支持节点白名单化的内部换 IP Webhook。构建与部署方法见 [cpa-plugin/README.md](./cpa-plugin/README.md)，代理规划、账号容量、隔离恢复和强制住宅 IP 轮换见 [AI 部署与运维指南](./cpa-plugin/AI_USAGE_GUIDE.md)。

推荐的完整链路部署方式（家宽/Resin → Mihomo 分片与监听器 → Grok2API/CPA 出口节点 → Quality Guard 检测、摘流、轮换与复测）见[推荐出口部署方式](./docs/RECOMMENDED_DEPLOYMENT.md)。

CPA 本身不会让模型“降智”；这个插件只是在多账号、多出口运行场景中，根据可观测质量信号做可选的出口熔断与迁移。单账号或稳定静态代理场景可以不安装。

质量守护是启发式熔断器，不是模型能力鉴定器。中间层缓冲、已有文件、长常量或缓存内容可能造成异常高瞬时 Token/s。硬阈值策略偏激进，可按链路调高 `hard_tps`；软阈值仍以固定 Prompt 复测确认。

## 直接应用

在干净的 grok2api 仓库中执行：

```sh
git fetch --tags origin
git checkout -b egress-enhancements v3.1.2
git am --3way /path/to/grok2api-egress-enhancements/patches/0002-feat-add-degraded-account-monitor.patch
git am --3way /path/to/grok2api-egress-enhancements/patches/0003-feat-add-quality-guard-probe-profiles.patch
git am --3way /path/to/grok2api-egress-enhancements/patches/0004-fix-dual-probe-recovery-and-thinking-guard.patch
git am --3way /path/to/grok2api-egress-enhancements/patches/0005-fix-missing-thinking-32-token-floor.patch
```

仍基于 `v3.0.11` 时改用 `patches/0001-feat-add-egress-recovery-and-quality-guard.patch`。目标版本高于补丁基线时，使用 [AI 合并指南](./docs/AI_MERGE_GUIDE.md)，按功能不变量解决冲突，不要整文件覆盖新版实现。

## 验证

```sh
go test ./...
python3 -m unittest -v \
  tools/egress-quality-guard/quality_guard_test.py \
  tools/egress-quality-guard/session_rotator_test.py  # 26 tests
cd frontend
pnpm lint
pnpm build
```

生产部署前还应验证：

- 固定代理失败后只启动一个立即探针；
- 健康探针只清理 `transport error` 冷却；
- 不健康探针不会提前恢复节点；
- 动态代理池不会因单次连接失败进入全局冷却；
- 管理 API 不返回管理员密码、Client Key 密钥、代理 URL、探测 Prompt 或模型响应正文。
- 账号调度失败不会触发代理 IP 轮换，未经模型质量验证的隔离节点也不会被恢复。

## 安全与隐私

补丁不包含部署配置。不要向 AI 工具提供真实 `.env`、`config.yaml`、数据库、状态卷、代理 URL、账号凭据或生产日志。合并时只需要上游源码、本仓库补丁和测试输出。

## 相关项目

- [Grok Register + Live Panel](https://github.com/lij768423-svg/grok-register-panel)：基于 Camoufox 的 Grok 注册流程与 Web 管理面板，支持多邮箱后端、外部代理池、出口预检、ASN 黑名单、运行统计和账号补录。它是独立项目，不包含在本补丁中。

## 友情链接

- [LINUX DO](https://linux.do) — 新的理想型社区

## 许可与归属

补丁在上游 MIT 许可框架下发布。保留上游项目的 LICENSE、版权信息和提交历史。本仓库不是 grok2api 官方发行版，也不代表上游维护者认可这些改动。

English documentation: [README.en.md](./README.en.md)
