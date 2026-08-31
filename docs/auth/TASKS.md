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

- [x] 2.0 `x-scopes` 三种放法 lint 均干净；选**根级**（两种 scheme 共用，放进其一会让另一个变二等）
- [x] 2.0b scope 词表已定稿：**16 个**，`资源:动作[:范围]`，不得出现角色名，`users:write` 单列（`baseline.md` 裁定 8）
- [x] 2.1 `cookieSession` + `csrfHeader` + 新 `apiKeyBearer`（`http`/`bearer`）；`apiKeyHeader` 移除；根级 `x-scopes` 19 个
- [x] 2.2 **91** 个 operation 全部显式 `security`；全局 `security` 移除；Bearer 支不带 `csrfHeader`；描述里 48 处 `Requires ROLE` 清零
- [x] 2.2b `/events` 已有 Bearer 支，`calls:read:own` 为下限、`calls:read:all` 拓宽投递（P1 / P2）
- [x] 2.2c `info.description` 重写为两种对等凭证，并明说 web 应用只是消费者（P6）
- [x] 2.2d 打分 operation 写明理由：页面 token ≠ UI，读评分照样有 Bearer 支（P8）
- [x] 2.2e webhook 配置改为 `config:write` 可达；design 09 两处 + 契约描述已改写。**`server.go:363-368` 的注释留到 ③**——它描述的代码此刻还没变，现在改会让注释说谎
- [x] 2.2f 新增 `GET /openapi.json`：契约自己 serve 自己，免鉴权（V3）。`[FACT]` 落地手法照抄 `web/embed.go:16`——加一个 `docs/embed.go`（`package docs` + `//go:embed openapi.json`），因为 `go:embed` 不能用 `..` 跨目录，而 `docs/` 不是 Go 包。零构建步骤，与 SPA 的做法同源
- [x] 2.3 新增 5 个端点（路径参数为 `keyId`）。`PATCH` 改为改名/调 scopes——两态之下唯一的状态变更是吊销，而吊销是终态、有自己的端点
- [x] 2.4 错误码 22 → 26（含上一提交的 `METHOD_NOT_ALLOWED`）
- [x] 2.5 `make api-lint` 0 error 0 warning（10 条钉住）；`make api-breaking` exit 0
- [x] **2.6 契约已批准**（owner 2026-08-31）；中间提交构建红，owner 确认可接受

## §3 测试先行

- [x] 3.1 九条用例已写：`internal/httpapi/scopeauth_test.go`，真库 + 真路由 + 真鉴权中间件
- [x] 3.2 基线（`main` worktree）执行：**8 条按预期失败，1 条今天就通过**（`mayHearCall` 已拦住，改列为回归护栏）。形态逐条记入 RESULTS.md
- [x] 3.3 五处修正已记：删 `AGENT_NOT_ALLOWED` 行、加 `/events` 行、`/agent/ready` 202→200、header 改 `X-AICC-Agent-ID`、第 5 行「任意端点」定为 `GET /auth/me`

## §4 禁止事项

- [x] 4.0 无工作项，为全程约束；实现时逐条对照任务书 §4
- [ ] 4.1 追加一条禁止事项：**scope 名里不得出现角色名**——scope 按 `资源:动作[:范围]` 命名（P3，正式条目）

## 实现（§5 提交切分的工作面，编号沿用提交序号）

- [x] ②a 生成代码：`make api-generate` + scope 常量生成步骤，产物落 `internal/api/scopes.gen.go` 与 `web/src/generated/scopes.ts`（裁定 1b，Makefile 无需改）
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
