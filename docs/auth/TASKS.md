<!-- SPDX-License-Identifier: Apache-2.0 -->

# 统一认证模型 + API Key 管理 — 任务游标

第一个未勾选项即当前位置。证据写入 `docs/auth/RESULTS.md`（追加，不改历史条目）。
本任务独立于 `TASKS.md` / `RESULTS.md`（仓库根的既有账本）。

**人工门**：§0 结论、§2 契约、REVOKED 终态的首次真实执行。

---

## §0 源码研究（只读）

- [x] 0.1 鉴权链（1–4）：token 签发/校验、中间件组装、角色守卫清单、SSE 鉴权
- [x] 0.2 现有 API Key（5–6）：表结构、存储形态、校验点与调用方
- [x] 0.3 路由与契约（7–10）：路由×契约双向差集、Web 引用差集、错误码、CI
- [x] 0.4 agent 作用域（11–12）：归属判定点、AgentID 传递路径
- [x] 0.5 审计（13）：表结构、写入点、actor 形态、名称快照
- [x] 0.6 UI 与数据层惯例（14–15）
- [x] 0.7 决策校验表（16）
- [x] 0.8 产出 `docs/auth/baseline.md`
- [x] 0.9 4 项待决问题已裁定（owner 2026-08-31，见 `baseline.md` §0 待决问题 — 已裁定）
- [x] 0.10 产品定位复核 "UI is optional. API is the product."（`baseline.md` §17，P1–P9）
- [x] 0.11 P5 裁定：**解除** webhook 对 API Key 的封锁；P1/P2/P3 纳入 §2/§4 正式条目（owner 2026-08-31）
- [x] 0.12 O1–O4 减法采纳（owner 2026-08-31）：hash 直查、去节流、砍允许代理列表、两态。**覆盖已批准的 §1 对应条目**
- [x] 0.13 scope 词表定稿 16 个、`users:write` 单列、act-as header 改 `X-AICC-Agent-ID`（`baseline.md` 裁定 8–9）。**§0 无遗留待裁项**
- [x] **§0 完成，人工门已通过**（`baseline.md` §0 裁定 1–6 + §17）

## §1 决策

- [x] 1.0 决策已定（见任务书 §1），本阶段无工作项；与现状的 `支持`/`冲突`/`无依据` 校验见 `baseline.md` §16

## §2 契约变更（先于代码，单独一次提交）

- [ ] 2.0 实测 `x-scopes` 的放置层级（根 / `components` / scheme 内），`make api-lint` 零新增 warning
- [x] 2.0b scope 词表已定稿：**16 个**，`资源:动作[:范围]`，不得出现角色名，`users:write` 单列（`baseline.md` 裁定 8）
- [ ] 2.1 `securitySchemes`：页面 Token 与 API Key 两种 scheme，共用同一 scope 词表
- [ ] 2.2 85 个 operation 逐个补 `security` + scopes；Bearer 那一支**不带** `csrfHeader`（P4）
- [ ] 2.2b `/events` 补 Bearer 支持并在契约声明（P1，正式条目——事件流是产品实时面的全部，不是附注）
- [ ] 2.2c 改写 `docs/openapi.json:7` 的浏览器优先措辞为两种对等凭证（P6）
- [ ] 2.2d `quality_reviews` 打分 operation 的 description 写明能力不对称的理由（P8）
- [ ] 2.2e webhook-subscriptions 六条改为可由 `webhooks:write` scope 到达；同步改写三处成文依据：`server.go:363-368` 注释、`docs/design/09-webhooks.md:370,454`、`docs/openapi.json:4247`（P5 解除）
- [ ] 2.2f 新增 `GET /openapi.json`：契约自己 serve 自己，免鉴权（V3）。`[FACT]` 落地手法照抄 `web/embed.go:16`——加一个 `docs/embed.go`（`package docs` + `//go:embed openapi.json`），因为 `go:embed` 不能用 `..` 跨目录，而 `docs/` 不是 Go 包。零构建步骤，与 SPA 的做法同源
- [ ] 2.3 新增端点：`GET/POST /api-keys`、`GET /api-keys/{id}`、`PATCH /api-keys/{id}`、`POST /api-keys/{id}/revoke`
- [ ] 2.4 错误码 enum 补 **3** 个：`AGENT_REQUIRED` / `AGENT_IMPERSONATION_NOT_ALLOWED` / `INSUFFICIENT_SCOPE`（`AGENT_NOT_ALLOWED` 随 O3 取消）
- [ ] 2.5 `make api-lint` 零 error/warning 增量；`make api-breaking` 通过
- [ ] **2.6 人工批准契约**

## §3 测试先行

- [ ] 3.1 用例表按裁定重算：原 9 条**删 1**（`AGENT_NOT_ALLOWED`，随 O3 取消）、**加 1**（Key 带 Bearer 订阅 `/events`，收到其代理坐席的 `PARTY_*`，P1）= **9 条**；header 一律 `X-AICC-Agent-ID`
- [ ] 3.2 确认全部在当前基线上按预期失败，失败形态记入 RESULTS.md
- [ ] 3.3 表中端点路径与 §0-7 清单的差异修正记录在案

## §4 禁止事项

- [x] 4.0 无工作项，为全程约束；实现时逐条对照任务书 §4
- [ ] 4.1 追加一条禁止事项：**scope 名里不得出现角色名**——scope 按 `资源:动作[:范围]` 命名（P3，正式条目）

## 实现（§5 提交切分的工作面，编号沿用提交序号）

- [ ] ②a 生成代码：`make api-generate` + scope 常量生成步骤，产物落 `internal/api/scopes.gen.go` 与 `web/src/generated/scopes.ts`（裁定 1b，Makefile 无需改）
- [ ] ③ AuthContext（`Subject` / `AgentID` / `Scopes`）+ scope 中间件；删除 14 处路由级角色守卫与 **6** 处 handler 外角色判定（5 处在 `internal/httpapi`，第 6 处是 `internal/events/hub.go:28` `IsSupervisor`，P2）；删除 `isMachine` / `machineIdentity` 及其 3 个下游分支；重述 `mayReadTranscript` 的按屏幕规则为能力（P7）
- [ ] ④ API Key 存储与 4 个端点。按裁定 7：状态 **`ENABLED / REVOKED`** 两态；查找 `WHERE key_hash = $1`（照抄 `sessions.sql:8-13`），短前缀列只用于展示、不建索引；`last_used_at` 每次直接写、**无节流**；**无允许代理 Agents 列表**。`callbacks.handled_by` / `contacts.updated_by` 改为跟随 `AgentID`（裁定 2）
- [ ] ⑤ 审计：**新增** `subject_kind` / `subject_id` / `subject_name` / `agent_id` 4 列，`actor_id` 不动，回填既有行（裁定 3）
- [ ] ⑥ Admin UI：API Keys 页 + `nav.ts` 条目（列表列去掉「允许 Agents」，状态只有两个值）
- [ ] ⑦ CI 检查（同一文件三条断言）：
      a. 路由表中任一路由在契约中无 scope 声明即失败（排除 `/metrics`、`/healthz`、`/readyz`、SPA fallback，理由显式登记）
      b. 契约 `ErrorCode` enum ≡ `internal/httpapi/errors.go` 常量 ≡ 两份 `translation.json` 的 `errors` 键（裁定 4）
      c. 每个 operation 的 `security` 都有一支 Bearer——白名单例外仅 P8（P9 验收判据）
- [ ] **REVOKED 终态的首次真实执行（人工门）**

## §5 提交与记录

不混提交：重构与行为变更分开。

- [ ] ① 契约
- [ ] ② 生成代码
- [ ] ③ AuthContext + 中间件 + 角色守卫删除（重构，无行为变化）
- [ ] ④ API Key 存储与端点
- [ ] ⑤ 审计
- [ ] ⑥ Admin UI
- [ ] ⑦ CI 检查
