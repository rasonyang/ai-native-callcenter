<!-- SPDX-License-Identifier: Apache-2.0 -->

# 统一认证模型 + API Key 管理 — 任务游标

第一个未勾选项即当前位置。证据写入 `docs/auth/RESULTS.md`（追加，不改历史条目）。
本任务独立于 `TASKS.md` / `RESULTS.md`（仓库根的既有账本）。

## 怎么接着做

新会话在仓库根目录说一句 **「继续 docs/auth/TASKS.md」** 即可。要点：

- **分支 `docs/auth-baseline`**。第一个未勾选项就是当前位置。
- **构建是绿的**（④ 起）。`go test -race ./...` 全过，`internal/httpapi/scopeauth_test.go` 的九条用例全部通过。`ecf368f`–`a1fa378` 那五个中间提交是红的，那是 spec-first 的设计意图（`CLAUDE.md`：*a new spec operation breaks the build until the server grows its method — that is the point*），owner 已确认。
- **数据库要起着**：`docker compose -f deploy/dev/docker-compose.yml up -d`，然后
  `AICC_TEST_DATABASE_URL='postgres://aicc:aicc@127.0.0.1:5432/aicc?sslmode=disable'`。
- **先读这三份再动手**：`docs/auth/baseline.md`（§0 事实 + 全部裁定 1–9 及其修正）、`docs/auth/RESULTS.md`（证据账本，含每一步的失败形态）、`docs/api-first-audit.md`（V1–V8 违反项、O1–O5 减法及处置）。`CLAUDE.md` 里那条 owner directive **"UI is optional. API is the product."** 是本任务全部决策的依据。
- **`python3 docs/auth/scopemap.py`** 随时可重跑：打印 19 个 scope、role→scopes，并自检拓宽与收窄。拓宽必须**只**剩 9 条 `config:read`（裁定 8 修正 2），收窄必须为 0。
- **勾选的粒度不能大于验证的粒度。** 复合项一律拆开——②a 曾因为跑完前半就整条勾掉，谎报了一个没写的生成器（见 ②b）。

**人工门**：§0 结论 ✅、§2 契约 ✅、REVOKED 终态的首次真实执行 ⬜、**③b AI 外呼的能力待裁定** ⬜。

## 进度概览（2026-08-31）

| 阶段 | 状态 | 落地提交 |
|---|---|---|
| §0 源码研究 | ✅ 完成，人工门已过 | `2d33ca1` `293db19` `657cd13` `f676276` `365d763` `b219244` `c10648a` |
| §1 决策 | ✅ 无工作项（O1–O4、裁定 8–9 覆盖了原 §1 若干条） | — |
| §2 契约变更 | ✅ 完成，人工门已过 | `ecf368f` 契约 / `3436dd9` 生成代码 |
| §3 测试先行 | ✅ 8 条按预期失败 + 1 条回归护栏 | `40aed73` |
| §4 禁止事项 | ✅ N1 已写入（§4） | — |
| ②b scope 常量生成 | ✅ 生成器 + 两个产物,api-check 已覆盖 | `c03080d` |
| ③ AuthContext + 删守卫 | ✅ 完成（⚠ 留一处待裁定，见 ④ 上方） | `a1fa378` |
| ④ Key 存储与端点 | ✅ 完成，**构建已转绿** | `0793e32` |
| ⑤ 审计 4 列 | ✅ 完成，§3 九条全过 | `e605235` |
| ⑥ Admin UI | ⬜ 未开始 | — |
| ⑦ CI 三条断言 | ⬜ 未开始 | — |

**本任务之外、同分支上的两个提交**：`9c01e2f` 修 V1/V2（API 自答 404/405，构建绿、测试全过）、`f676276`/`b219244` 等文档裁定。全部记在 `docs/api-first-audit.md` 的处置表。

**当前构建：绿**（④ `0793e32` 起）。`go build` / `go vet` / `gofmt` / `go test -race ./...` / `make api-check` 全通过，§3 九条全过。

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
- [x] 0.13 scope 词表定稿、`users:write` 单列、act-as header 改 `X-AICC-Agent-ID`（`baseline.md` 裁定 8–9）。**§0 无遗留待裁项**
- [x] 0.14 词表两次修正：初稿 16 → **18**（加 `contacts:*`，owner 裁定）→ **19**（加 `agent:manage`，`scopemap.py` 自检抓到未裁定的提权）
- [x] **§0 完成，人工门已通过**（`baseline.md` §0 裁定 1–6 + §17）

## §1 决策

- [x] 1.0 决策已定（见任务书 §1），本阶段无工作项；与现状的 `支持`/`冲突`/`无依据` 校验见 `baseline.md` §16

## §2 契约变更（先于代码，单独一次提交）

- [x] 2.0 `x-scopes` 三种放法 lint 均干净；选**根级**（两种 scheme 共用，放进其一会让另一个变二等）
- [x] 2.0b scope 词表已定稿：**19 个**，`资源:动作[:范围]`，不得是角色的别名（规则全文见 §4 N1），`users:write` 与 `agent:manage` 单列（`baseline.md` 裁定 8 及其三处修正）
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
- [x] 4.1 追加禁止事项 **N1**（P3，正式条目）；同一条裁定在 `docs/design/07-naming.md` §7 落一行 ruling

### N1 — scope 不得是角色的别名

**规则**：scope 按 `资源:动作[:范围]` 命名，资源段必须指一个**领域资源**；任何一个 scope 都不得是某个角色的别名，即不得等价于「某角色的全部能力」改个名字。

**这条规则不是「scope 名里不许出现 agent 字样」。** 词表里的 `agent:read` / `agent:act` / `agent:manage` 合规，因为这里的 `agent` 指的是**坐席这个资源**（在线状态与坐席身份），不是 `AGENT` 这个角色：`agent:read` / `agent:act` 只触及主体自己的坐席身份，`agent:manage` 才触及别人的——三者加起来也不等于 `AGENT` 角色的能力集（角色还持有 `calls:*`、`contacts:*`、`history:read:own`）。真正被这条规则禁掉的是 `supervisor:*`、`admin:*`、`role:agent` 这类：它们没有资源，只有人格。

**为什么**：scope 若照角色切分，这次重构就只是把三人格改个名保留下来，`role → scopes` 也就不再是「给页面登录用的便利映射」而重新变成模型本身（`baseline.md` §17 P3、行 373 与 400）。

**判据**（给 ③–⑥ 逐条对照用）：新增一个 scope 前问两句——① 资源段能不能在领域里指出一个东西？② 把它发给一个新主体，等于授予「一个角色」还是「一件事」？第二问答「一个角色」就是违规。

**不在本步引入机械检查**：⑦ 的三条断言不含这一条，这里只立文字规则。

## 实现（§5 提交切分的工作面，编号沿用提交序号）

- [x] ②a `make api-generate`：`internal/api/api.gen.go` + `web/src/generated/api.ts` 已重新生成（`3436dd9`）
- [x] ②b scope 常量生成：`scripts/gen-scopes.mjs`（由 `scripts/api-generate.sh` 调用）读根级 `x-scopes`，写出
      `internal/api/scopes.gen.go`（常量 + `AllScopes` + `ScopeDescriptions` + `IsScope`）与
      `web/src/generated/scopes.ts`（`Scope` 联合类型 + `SCOPES` + `SCOPE_DESCRIPTIONS`）。
      **说明一并生成**——⑥ 的表单要给每个 scope 配标签，手写标签就是第二份词表。
      产物在 `make api-check` 已 diff 的两个目录内，`Makefile` 未改（裁定 1b）。验证形态见 RESULTS.md。
- [x] ③ AuthContext + scope 中间件。**授权改成读契约**：`scripts/gen-opsecurity.mjs` 把 91 个 operation 的 `security` 生成成
      `api.OperationSecurityByRoute`，`enforceContract` 装在生成 wrapper 的 `HandlerMiddlewares` 上（chi 刚解析完路由的一刻）。
      14 处路由级守卫、6 处 handler 外角色判定（含 `events` 包的 `IsSupervisor` → `SeesEveryCall`）、`isMachine` / `machineIdentity` +
      3 个下游、`requireSessionOrAPIKey`、`AICC_API_KEY` / `X-AICC-Api-Key` 全部删除；`mayReadTranscript` 改判 `history:read:all`（P7）。
      验证形态与行为差异见 RESULTS.md。
- [ ] ③b **待裁定：AI 外呼的能力**。`createAICall` 今天要 SUPERVISOR，检查在 handler 里，`scopemap.py` 看不见，
      所以它把 `createCall` 的下限记成了「任何已认证」。照契约的 `calls:create` 直接放行 = 把外呼机器人发给每个坐席。
      ③ 暂用 `config:read` 保住原行为并在代码里标了 ⚠。**两个选项**：契约给它一个自己的 scope（重开 §2），
      或裁定 `calls:create` 覆盖两种 kind。**⑦ 不得在此之上收工。**
- [x] ④ API Key 存储与端点（迁移 `00029`）。两态、hash 直查、prefix 不建索引、`last_used_at` 无节流、无允许代理列表。
      吊销的终态写在 SQL 的 `WHERE` 里——吊销过的 Key 查不出来，于是被拒的请求也碰不到它的 `last_used_at`。
      未知 scope 拒绝而非忽略（`api.IsScope`）。裁定 2：三列仍装 user id，靠 `UserIDForAgent` + `AuthContext.ActorUserID`。
      `GET /openapi.json` 由 `docs/embed.go` 提供。**构建在此转绿。**
- [x] ⑤ 审计四列（迁移 `00030`）：`subject_kind` / `subject_id` / `subject_name` / `agent_id`，`actor_id` 未动，既有行已回填，
      `migrate_test` 有 fixture。契约 `AuditEntry` 加 4 个可选字段（纯加法，oasdiff exit 0）。**§3 九条全过。**
- [ ] ⑥ Admin UI：API Keys 页 + `nav.ts` 条目（列表列去掉「允许 Agents」，状态只有两个值）
- [ ] ⑦ CI 检查（同一文件三条断言）：
      a. 路由表中任一路由在契约中无 scope 声明即失败（排除 `/metrics`、`/healthz`、`/readyz`、SPA fallback，理由显式登记）
      b. 契约 `ErrorCode` enum ≡ `internal/httpapi/errors.go` 常量 ≡ 两份 `translation.json` 的 `errors` 键（裁定 4）
      c. 每个 operation 的 `security` 都有一支 Bearer——白名单例外仅 P8（P9 验收判据）
- [ ] **REVOKED 终态的首次真实执行（人工门）**

## §5 提交与记录

不混提交：重构与行为变更分开。

- [x] ① 契约 —— `ecf368f`
- [x] ② 生成代码 —— `3436dd9`（scope 常量那半还欠着，见 ②b）
- [x] ③ AuthContext + 中间件 + 角色守卫删除 —— `a1fa378`（**不是零行为变化**：9 处已裁定的 `config:read` 拓宽、act-as 头改为拒绝、webhook 对 Key 解封；收窄 0）
- [x] ④ API Key 存储与端点 —— `0793e32`
- [x] ⑤ 审计 —— `e605235`
- [ ] ⑥ Admin UI
- [ ] ⑦ CI 检查
