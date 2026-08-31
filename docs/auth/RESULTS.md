<!-- SPDX-License-Identifier: Apache-2.0 -->

# 统一认证模型 + API Key 管理 — 证据账本

追加写入，不改历史条目。每条结论标 `[FACT]`（带 `file:line`）/ `[INFERENCE]` / `[ASSUMPTION]`。
空集相等断言不算通过——所有断言附基数。

---

## 2026-08-31 — §0 源码研究完成

基线：`main` @ `2ecb28f`，工作区干净。完整答案见 `docs/auth/baseline.md`（16 项逐条）。

### 口述定位（本会话，owner）

> UI is optional. API is the product.

`[FACT]` 与代码现状一致：Web 引用的路径 100% 在契约内（见下方 A3），无「UI 私有路由」。

### 结构性发现（决定后续所有阶段的形状）

**A1 — 现有 API Key 不是一个可迁移的东西，是一个环境变量。**
- `[FACT]` `internal/config/config.go:251` `APIKey: env("AICC_API_KEY", "")`；`.env.example:223` 注册。
- `[FACT]` `internal/store/migrations/`（28 文件）+ `internal/store/sql/`（9 文件）grep `api_key|apikey|scope` **命中 2 条，均为无关**（`00026_*.sql:36` 注释、`telephony.sql:136` 的 "Transaction-scoped"）。**API Key 表：NOT FOUND。**
- `[FACT]` 唯一校验点 `internal/httpapi/middleware.go:155-177`；header `X-AICC-Api-Key`（`:114`）；通过后注入硬编码的 `machineIdentity()`（`:129-135`，`Role=SUPERVISOR`，`UserID=uuid.Nil`）。
- `[FACT]` 挂载路由 **2** 条（`server.go:394-400`、`:407-413`）；契约中声明 `apiKeyHeader` 的 operation 也是 **2** 个（`docs/openapi.json:1008`、`:1386`）。两侧一致。
- `[FACT]` webhook **不引用**它——`internal/webhook/webhook.go:177` 用的是客户自己的 `auth_token`，方向相反。
- `[INFERENCE]` §1 的「明文 → 一律重新签发，无迁移路径」分支成立，且没有行可迁。其当前实际能力的最小集合 = `calls:create` + `calls:hangup`。

**A2 — 路由与契约已经严格对齐，基数 85。**
- `[FACT]` `internal/httpapi/server.go` 抽取的挂载路由 **85** 条；`jq` 从 `docs/openapi.json` 抽取的 method×path **85** 个。
- `[FACT]` `comm -23`（挂载未声明）= **0** 条；`comm -13`（声明未挂载）= **0** 条。
- `[FACT]` 已有守门：`internal/httpapi/routes_test.go:36-73` `TestEveryContractOperationIsRouted`（`go/parser` 解析 `server.go` + 反射 `api.ServerInterface`，白名单只有 `StreamEvents`）。

**A3 — Web 无契约外路由。**
- `[FACT]` `web/src/lib/*.ts` 的 `request<>()` 路径字面量归一后 **56** 条不同路径；契约不同路径 **66** 条；差集（Web 有、契约无）= **0** 条。
- `[FACT]` 另外 2 处非 `request()` 的 URL 也在契约内：`web/src/lib/events.ts:32`、`web/src/lib/ledger.ts:131`。

**A4 — 契约中 scope 的基数今天是 0，但逐 operation 声明是可行的。**
- `[FACT]` 三个 securityScheme 全为 `type: apiKey`（`docs/openapi.json:4515-4519`）；全局 `security = [{"cookieSession":[]}]`；**所有 security 值都是空数组**。
- `[FACT]` 36 个 operation 靠全局继承无显式 `security`，49 个显式声明（1 个匿名、2 个含 `apiKeyHeader`、46 个 cookie+csrf）。
- `[FACT]` **实测**：把 `security: [{"cookieSession":["cdr:read"]}]` 加到 `GET /dispositions`，`npx @redocly/cli lint` 通过，warning 数 **9 = 基线 9**（无新增）。OpenAPI 3.1 允许非 oauth2 型 scheme 携带名称数组。
- `[FACT]` 但「词表 + 每个 scope 的说明」在 `apiKey` 型 scheme 上**没有原生字段**（`scopes` 是 oauth2 流对象独有）。→ 待决问题 1。
- `[FACT]` **实测**：`go tool oasdiff breaking --fail-on ERR docs/openapi.json <加了 scope 的副本>` → `No breaking changes to report`，exit 0。补 scope **不触发 §2.5 的 breaking 门禁**。

**A2b — 「85 条」的边界：同一二进制还有 4 个契约外 HTTP 入口。**
- `[FACT]` `GET /metrics` / `/healthz` / `/readyz`——`internal/httpapi/server.go:437-453` `MetricsHandler`，独立监听（`AICC_METRICS_ADDR` 默认 `127.0.0.1:9090`），**无鉴权**；契约自己声明了这个例外（`docs/openapi.json:7` description 末段）。
- `[FACT]` SPA fallback `r.NotFound(s.spa.ServeHTTP)`（`server.go:428-430`），静态资源。
- `[INFERENCE]` §4.6 的 scope 覆盖检查须显式排除这 4 个，排除理由照 `routes_test.go:17-25` 的白名单写法登记。

**A5 — 两个代码生成器都不产出 scope 常量。**
- `[FACT]` `grep -c -i securit internal/api/api.gen.go` = **0**；`web/src/generated/api.ts` 中 `security`/`scopes` 命中 **0**。
- `[INFERENCE]` §1「Go / TS 常量由代码生成」需要在 `scripts/api-generate.sh` 新增一段自写生成。产物路径要加进 `Makefile:56-58` 的 `git diff --exit-code` 列表，否则 `make api-check` 不覆盖它。→ 待决问题 1b。

**A6 — 要删的不只是路由级守卫。**
- `[FACT]` 路由级角色守卫 **14** 处调用，覆盖 **67 / 85** 条路由（ADMIN 34 / AGENT 19 / SUPERVISOR 14）；剩余 **18** 条无路由级角色守卫。
- `[FACT]` **另有 5 处 handler 内的角色判定**：`recording_handlers.go:33-51`、`transcript_handlers.go:89-113`、`outbound_handlers.go:214-222`、`call_handlers.go:200`、`call_handlers.go:297-312`。不一起处理就仍是角色模型。
- `[FACT]` `isMachine`（`middleware.go:140`）正是 §1 禁止的「按凭证类型分支」，3 个下游：`audit.go:66`、`outbound_handlers.go:177`、`outbound_handlers.go:227`。

**A7 — AgentID 今天不在 token 里，也不在身份里。**
- `[FACT]` 页面 token 是 32 字节随机串，无载荷（`internal/auth/auth.go:166-173`）；`auth.Identity` 无 AgentID 字段（`:47-53`）。
- `[FACT]` 每次现查：cookie → `sessions.token_hash` → `users.id` → `GetAgentByUserID`（`internal/store/sql/agents.sql:11-12`）→ `agents.id`。绑定列 `agents.user_id`，唯一 + 级联（`internal/store/migrations/00002_telephony.sql:38,40`）。
- `[FACT]` `agentIDFor` **13** 个调用点 + 2 个直调 `AgentIDForUser`（`events_handler.go:57`、`outbound_handlers.go:230`）。
- `[FACT]` **6 个写入点按 `user_id` 而非 `agent_id` 归属**，落在 **3 张表的 3 个列**上：`callbacks.handled_by`（`ledger_handlers.go:172,191,226`）、`quality_reviews.reviewer_id`（`recording_handlers.go:135`）、`contacts.updated_by`（`contact_handlers.go:52,63`，Create 与 Update 写同一列；`contacts` 无 `created_by`）。
- `[FACT]` 列形态：`callbacks.handled_by uuid` 可空（`00005:106`）、`quality_reviews.reviewer_id uuid **NOT NULL**`（`00005:86`）、`contacts.updated_by uuid` 可空（`00011:29`）。**三列均无指向 `users` 的外键**——全库 `REFERENCES users` 仅 2 处（`sessions.user_id`、`agents.user_id`）。
- `[INFERENCE]` 把 Key id 写进去数据库**不会报错**，只会静默污染 user id 空间；`reviewer_id` NOT NULL 连留空都不行。→ 待决问题 2 必须显式裁定。

**A8 — 审计只有 `actor_id`，没有主体种类、没有名称快照。**
- `[FACT]` `audit_logs(id, occurred_at, actor_id uuid /* 可空、无 FK */, action, target_kind, target_id, detail jsonb, ip)`（`internal/store/migrations/00005_call_ledger.sql:127-136`）。
- `[FACT]` 名称是**读时** LEFT JOIN 解析，非快照（`internal/store/sql/ledger.sql:341-344`）。
- `[FACT]` machine 主体今天藏在 `detail->>'actor' = "api-key"`，`actor_id` 留 NULL（`internal/httpapi/audit.go:66-74`）。
- `[FACT]` 脱敏词表已含 `apikey`（`audit.go:158`），子串匹配、任意深度 → §4「不在审计中输出 Key 明文」已有机制覆盖。
- `[INFERENCE]` 倾向新增 4 列而非改 `actor_id`；迁移须处理既有行（NULL actor → `subject_kind='API_KEY'`，`subject_name` 取自 `detail->>'actor'`）。→ 待决问题 3。

**A9 — 既有缺口（非本任务引入，记录待裁）。**
- `[FACT]` 错误码三处基数一致（契约 22 / `internal/httpapi/errors.go` 22 / `web/src/generated/api.ts:1424` 22），**但 i18n 只有 20 个**，缺 `EXTENSION_POOL_EXHAUSTED`、`LAST_ADMIN`、`OPERATION_NOT_ALLOWED_FOR_CALL_TYPE`、`USER_DATA_TOO_LARGE`。
- `[FACT]` `internal/httpapi/errors.go` 的常量是**手写**的，不由契约生成，无 CI 兜底对齐。→ 待决问题 4。

### 惯例（实现时照抄，不发明新模式）

- `[FACT]` 迁移：`internal/store/migrations/NNNNN_一句人话.sql`，goose 格式，下一个是 **`00029_`**；SPDX 首行 + 「为什么」注释块。验收门 `internal/store/migrate_test.go`（含**对已有数据的库**迁移），由 `ci.yml:32-50` 的 `postgres:18` service 真正执行。
- `[FACT]` DB 枚举：`varchar(n) NOT NULL CHECK (col IN (…))`。**`CREATE TYPE` 在 28 个迁移文件中命中 0 条。**
- `[FACT]` 查询：`internal/store/sql/<domain>.sql` + `-- name: Xxx :one|:many|:exec|:execrows`。`[INFERENCE]` 新建 `api_keys.sql`（与 `sessions.sql` 平级）。
- `[FACT]` Admin 页模板：`web/src/routes/_app.admin.extensions.tsx`——含「新铸密钥只存 `useRef`、state 只记有没有」的既有实现（`:33-36`），正是 §1「Secret 只返回一次」要复制的。
- `[FACT]` 数据层：`web/src/lib/catalog.ts` 的 `X`/`XWrite`/`XDraft` 三件套 + 常量 query key + `catalogApi` 纯函数 + `useXxx` `useQuery` + `useXxxMutations` 统一 `invalidateQueries`。枚举列成常量数组的先例：`:34-38` `STRATEGIES: Strategy[]`。
- `[FACT]` 导航必须加 `web/src/lib/nav.ts` 条目（`nav.test.ts` 断言每页可解析）；建议放 `nav.system` 组、`roles: ['ADMIN']`。
- `[FACT]` CI：`.github/workflows/api.yml` = `make api-check` + `make api-breaking`；`.github/workflows/ci.yml:79` = `go test -race ./...`。`scripts/` 下只有 `api-generate.sh`，**无自定义检查脚本的既有目录习惯**。
- `[INFERENCE]` 「路由必须声明 scope」的检查写成 `internal/httpapi` 的 Go 测试（紧邻 `routes_test.go`），随 `go test -race ./...` 自动执行；Redocly 自定义规则看不到 Go 路由表，做不了双侧断言。

### 与 §1 决策的冲突清单（只记录，不改决策）

| §1 决策 | 判定 | 一句话 |
|---|---|---|
| 禁止按凭证类型分支 | **冲突（需删除）** | `isMachine` + 3 个下游已存在（A6） |
| scope 词表在 `securitySchemes` 中 | **部分冲突** | 逐 operation 声明可行；词表本身在 `apiKey` scheme 上无原生位置（A4） |
| 每 operation 声明 security + scopes | **冲突（现状不满足）** | 36 个靠继承、scope 基数 0（A4） |
| scopes 常量由代码生成 | **冲突（生成器不产出）** | 两个生成器对 security 均输出 0（A5） |
| 审计记 subject_kind/id/name/agent_id | **冲突（表要改）** | 只有 `actor_id`，无快照（A8） |
| Key 的允许代理 Agents | **无依据（全新）** | 无表、无 scope、无代理概念（A1） |
| `last_used_at` 内存节流 | **无依据（全新）** | 仓库内无同类节流器 |

其余 25 条判定为 **支持**，逐条依据见 `docs/auth/baseline.md` §16。

### 待人工裁定（阻塞 §2）

1. scope 词表在 OpenAPI 中的承载形状：`x-scopes` / 只承载词表的 `oauth2` scheme / description。
1b. scope 常量的生成步骤放哪、产物路径是否纳入 `make api-check` 的 diff 列表。
2. A7 的 6 个「按 user_id 归属」端点在 scope 模型下写什么主体。
3. 审计新增 4 列 vs 改 `actor_id`。
4. A9 的 4 个错误码漏译是否顺手补上。

**状态：§0 完成，停在人工门。**

---

## 2026-08-31 — §0 待决问题裁定 + 产品定位复核

### 裁定（owner）

| # | 裁定 | 落地影响 |
|---|---|---|
| 1 | scope 词表用 **`x-scopes` 扩展字段**，不引入假的 oauth2 scheme | `[ASSUMPTION]` 放哪一层待 §2 用 `make api-lint` 实测；已验证的只有 operation 级 `security` 带 scope 名合法 |
| 1b | 生成产物落 `internal/api/scopes.gen.go` + `web/src/generated/scopes.ts` | `Makefile:56-58` 的 diff 路径**无需改动**，CI 门禁自动继承 |
| 2 | `callbacks.handled_by` / `contacts.updated_by` **跟随 `AgentID`**（写坐席绑定的 `users.id`，后者可空时可写 NULL）；`quality_reviews.reviewer_id` **对应 scope 不授予 Key** | 三列语义不变，永远是真人；"是 Key 干的"由审计行独立记录 |
| 3 | 审计**新增 4 列**，`actor_id` 不动 | ⑤ 是纯增量提交；避免 oasdiff 抓不到的语义破坏（`actorId`/`actorUsername` 已在契约与 Admin 页上） |
| 4 | 补齐 4 个漏译 + 在 ⑦ 加对齐断言（契约 enum ≡ `errors.go` 常量 ≡ 两份 i18n 键） | `errors.go` 手写且无 CI 兜底，不加断言新码照样会漏 |

### 产品定位复核结论（详见 `baseline.md` §17）

`[FACT]` **路由层面已经符合定位**：85 = 85 双向差集 0；Web 引用 56 条路径、契约外 0 条；前端类型 100% 来自生成契约。无 UI 私有路由。

`[INFERENCE]` **偏移在能力模型层面，集中于两处**：

- **P1 实时面对机器完全关闭。** `[FACT]` `/events` 只挂 `requireSession`（`server.go:423`），API Key 订阅不到事件流。产品的实时性全在这一条流上，机器客户端今天只能轮询 REST 去猜。建议从 §1 的附注升格为 §2/§4 一等条目 + §3 第 10 条用例。
- **P2 角色泄漏出了 httpapi 包。** `[FACT]` `internal/events/hub.go:28` `IsSupervisor bool`，投递判定 `:225`，由 `events_handler.go:49` 的 `Role.AtLeast(RoleSupervisor)` 喂入。这是 §0-3 五处之外的**第 6 处** handler 外角色判定，按包名搜守卫会漏。订阅范围模型就是 UI 三人格模型本身——**全库定位偏移最深的一处**。
- **P3 scope 命名是最大的陷阱。** scope 若命名成 `agent:*`/`supervisor:*`/`admin:*`，这次重构等于把三人格改个名保留。建议写死 `资源:动作[:范围]` 命名法并进 §4 禁止事项（不得出现角色名）。

`[FACT]` **P5 需要一次单独裁定**：`server.go:363-368` + `docs/design/09-webhooks.md:370,454` 明文把 webhook 配置挡在 API Key 之外，理由是"一把万能钥匙泄漏就外泄全部通话"。per-key scope 让这个前提消失，但它是成文的既有安全决策——要么显式解除（可授予 `webhooks:write`），要么显式保留并改写 design 09，**不能被"所有守卫换成 scope"顺手覆盖**。

执行细则（§2 逐条补 security 时）：P4 Bearer 那一支不带 `csrfHeader`；P6 改写 `docs/openapi.json:7` 里"cookie 是常态、机器是 instead"的浏览器优先措辞；P7 `transcript_handlers.go:110-113` 那条"面板不需要历史"的按屏幕规则要重述为能力；P8 质检打分的能力不对称须在 operation description 里写明理由（限制是"人的判断归属到人"，页面 token ≠ UI）。

`[INFERENCE]` **P9 验收判据**：§4 完成后不存在页面 token 能到达、而持有恰当 scope 的 Key 到不了的 operation（P8 一处白名单例外）。可机械执行——遍历契约每个 operation 的 `security` 是否都有一支 Bearer，例外照 `routes_test.go:17-25` 的白名单+理由写法。落在 ⑦，与裁定 4 的对齐断言同一文件。

**状态：§0 裁定完成，仍停在人工门（P5 待裁；P1/P2/P3 待确认是否纳入 §2/§4 正式条目）。**

---

## 2026-08-31 — P5 解除，P1/P2/P3 转正式条目，§0 人工门通过

**P5 解除（owner）**：webhook-subscriptions 对 API Key 的封锁取消，持 `webhooks:write` scope 的 Key 可配置投递目标。

`[FACT]` 原禁令写在**三处**，§2 必须同步改写——留一处不改，下一个读到的人会把代码当成 bug：
- `internal/httpapi/server.go:363-368`（路由旁注释）
- `docs/design/09-webhooks.md:370`、`:454`
- `docs/openapi.json:4247`（operation description）

`[INFERENCE]` 改写时保留原推理并说明前提变了：**风险没有消失，只是从"钥匙能不能到达"移到了"这把钥匙有没有被授予这个 scope"**——这正是 per-key scope 存在的意义。原禁令成立的前提是"只有一把万能钥匙"，那个前提被本次重构消掉了。

**P1 / P2 / P3 转为正式条目**：
- P1 → `2.2b` `/events` 补 Bearer 支持并在契约声明；`3.1` 第 10 条用例
- P2 → `③` 的守卫删除范围由"5 处"改为"**6 处**"，第 6 处是 `internal/events/hub.go:28` `IsSupervisor`（不在 `internal/httpapi` 包内）
- P3 → `2.0b` 定命名法 + `4.1` 追加禁止事项"scope 名里不得出现角色名"

**§0 至此无遗留待决项，人工门通过。** 下一步 §2 契约变更（单独一次提交，完成后再过一次人工门）。

---

## 2026-08-31 — 产品定位写入 CLAUDE.md + 全产品 API-first 复核

`[FACT]` "UI is optional. API is the product." 已作为 owner directive 写入 `CLAUDE.md`「What this is」节。全产品复核产出 `docs/api-first-audit.md`（V1–V8 违反项、O1–O5 过度设计项）。

**新发现的两个真实缺陷（不在认证任务面上，实测确认）**：
- `[FACT]` **V1**：`GET /api/v1/<未知路径>` → **200 text/html**（SPA index）。成因是 `server.go:428-430` 的 `r.NotFound(s.spa.ServeHTTP)` 经 chi `mux.go:214-218` `updateSubRoutes` 传播进了 `/api/v1` 子树。集成方拼错路径拿到 200 + HTML。仓库已被咬过一次（`routes_test.go:30-35` 记录）。
- `[FACT]` **V2**：`DELETE /api/v1/auth/login` → **405，空 body、空 Content-Type**，不是错误信封。契约 `docs/openapi.json:7` 写的是 "Errors **always** use the single envelope"——这个 always 现在是假的。
- `[FACT]` **V3**：无任何端点 serve 契约（`grep openapi|redoc|swagger` 只命中一条注释）。SPA 嵌进了二进制，契约没有。

**对已批准 §1 的三条减法建议（需 owner 确认）**：
- `[FACT]` **O1**：§1 的"按前缀取行 + 常量时间比较"比仓库现成做法复杂。`internal/store/sql/sessions.sql:8-13` 已经是 `WHERE token_hash = $1` 一次索引命中；按 hash 直查时没有东西需要常量时间比较。建议短前缀列只用于展示，查找照抄 sessions。
- `[INFERENCE]` **O2**：`last_used_at` 内存节流是无测量支撑的优化，且与 `CLAUDE.md` 那条"没跑基准就不许有性能主张"的 directive 同源。建议去掉，每次直接写。
- `[INFERENCE]` **O3**：每把 Key 的"允许代理 Agents"列表是架在 scope 之上的第二条授权轴，单集成客户的答案永远是 `*`。建议 v1 砍掉，`AGENT_NOT_ALLOWED` 不进枚举（新码 4 → 3）。以后补是纯加法。
- `[INFERENCE]` **O5**：scope 词表建议 10–14 个资源级，只在行为真的不同处分 `:own`/`:all`。粒度过细会让集成方一次勾满，模型退化成仪式。

---

## 2026-08-31 — O1–O4 减法采纳（覆盖已批准的 §1）

owner 裁定，依据 `docs/api-first-audit.md` §二。**这四条修改的是已批准的 §1，以此为准。**

| # | 原 §1 | 改为 | 反悔成本 |
|---|---|---|---|
| O1 | 按前缀取行 + 常量时间比较 | `WHERE key_hash = $1` 一次索引命中，照抄 `internal/store/sql/sessions.sql:8-13`。短前缀列保留但只用于展示，不建索引 | 不需要反悔——按 hash 直查时没有东西需要常量时间比较 |
| O2 | `last_used_at` 内存节流，每 Key 每分钟一次 DB 写 | 取消节流，每次直接写 | 真成问题时有真实数字，再加是纯加法 |
| O3 | 每把 Key 的允许代理 Agents（列表或 `*`） | v1 取消。持 `agent:act` 即可代理任意坐席。**`AGENT_NOT_ALLOWED` 不进枚举，新码 4 → 3** | 加法：一个可空列（NULL = 全部）+ 一次检查 + 一个新码，不破坏任何客户端 |
| O4 | `ENABLED / DISABLED / REVOKED` | `ENABLED / REVOKED` 两态 | 加法：一次 CHECK 放宽 |

`[INFERENCE]` O1 的理由值得单独记：**同一个代码库里 `sessions` 已经解决过同一个问题**，而且解法更简单（`GetSessionByTokenHash`）。§1 的设计不是错，是没看现成的。按 hash 查时要么这行存在、要么不存在，SHA-256 不可逆，索引查找不泄漏可用于时序攻击的信息——"常量时间比较"没有对象。

O5（scope 词表 10–14 个资源级）落在 `2.0b`，此处不定死具体词表。

---

## 2026-08-31 — V3 并进 §2

owner 裁定：`GET /openapi.json`（跑起来的部署 serve 自己的契约）不单独提交，**并进 §2 的契约提交**，列为 `2.2f`。按 spec-first 它需要一个新 operation，本就不能绕过契约先写代码。

`[FACT]` 落地手法已确认，避免 §2 开工时踩坑：加一个 `docs/embed.go`（`package docs` + `//go:embed openapi.json`），照抄 `web/embed.go:16` 嵌 SPA 的做法。**不能写 `//go:embed docs/openapi.json`**——`go:embed` 的模式不允许 `..` 跨目录，而 `docs/` 目前不是 Go 包。这个仓库已经用 `web/` 这个包解决过同一个问题，零构建步骤。

`[INFERENCE]` 端点免鉴权：契约是公开文档，而一个还没拿到凭证的集成方正是最需要读它的人。Redoc 文档页是可选的第二步，不进 v1。

---

## 2026-08-31 — scope 词表定稿、`users:write` 单列、act-as header 更名

**裁定 8 — scope 词表 16 个**（owner）。契约可见、改动是破坏性变更，故在 §2 开工前定死。全表见 `baseline.md` 裁定 8。

`[INFERENCE]` 我先前说的"10–14 个"是目标不是计数：逐条映射 89 个 operation，忠实的分法落在 **22** 个左右（每个资源各自一对 `:own`/`:all`，配置类按资源拆开）。把 CDR / 录音 / 转写 / `/reports/me` 合并成一对 `history:read:*`、配置类合并成 `config:*`，才收到 16。取粗的理由：scope 的价值在于**默认最小**，前提是数量少到人愿意逐个想；22 个勾选框的结果是集成方全勾，模型退化成仪式。

`[INFERENCE]` **`users:write` 单列**，不并进 `config:write`：创建账号是**唯一的提权路径**。持 `config:write` 的 Key 泄漏是"改了配置"；持 `users:write` 的 Key 泄漏是"建一个 ADMIN，拿到一切"。爆炸半径差一个数量级，不共用一个开关。

**裁定 9 — act-as header 改 `X-AICC-Agent-ID`**，覆盖 §1 写的 `AICC-Agent-ID`。

`[FACT]` 仓库现有的自定义 header 全带 `X-` 前缀：`X-AICC-Csrf`（`internal/httpapi/middleware.go:24`）、`X-AICC-Api-Key`（`:114`）、SIP 的 `X-AICC-Channel-ID`。`[INFERENCE]` 一致性在这里比 RFC 6648 "不建议新 header 用 X- 前缀"的建议更值钱——两种拼法并存会让集成方每次都要想一下哪个带哪个不带。

`[FACT]` **这是 §0 的漏记，不是 §1 的冲突**：`baseline.md` §16 第 13/15 行照抄了 §1 的 `AICC-Agent-ID`，没有把它和仓库惯例比对。§0 的决策校验表应当抓到这一条而没有抓到。

**§3 用例表随之重算**：原 9 条删 1（`AGENT_NOT_ALLOWED`，随 O3 取消）、加 1（Key 订阅 `/events`，P1）= **9 条**；header 一律写 `X-AICC-Agent-ID`。

**§0 至此无任何遗留待裁项。** §2 的第一个动作是 `2.0`：实测 `x-scopes` 的放置层级。

---

## 2026-08-31 — §2 开工：裁定 8 修正为 18 个 scope，并记下一处刻意的行为拓宽

`[FACT]` **2.0 完成**：`x-scopes` 放在根级 / `components` 级 / scheme 内，三种放法 `npx @redocly/cli lint` **都通过，warning 数均为 9 = 基线 9**。位置是设计选择，不是约束。**选根级**：§1 要求两种 scheme 共用同一词表，放进其中一个 scheme 会让另一个变二等；`components` 按规范是"可被 `$ref` 的具名对象"的容器，而词表不可 `$ref`。根级紧挨全局 `security`，是读者会去看的地方。

**裁定 8 修正 1 — 词表 16 → 18**（owner）。逐条映射 85 个 operation 时发现**联系人无处可归**。它不是平台配置，是坐席边接电话边改的客户数据（`internal/httpapi/server.go:354-361`，今天三个角色都能改）。并进 `config:*` 会强迫 `AGENT` 的 grant 含 `config:write` → 坐席顺带能建队列、改流程、改号码。`[INFERENCE]` 与 owner 否决"`users:write` 并进 `config:write`"是同一个形状，故加 `contacts:read` / `contacts:write`。

**裁定 8 修正 2 — 一处刻意的行为拓宽**（owner）。粗粒度 `config:read` 合并了队列读（今天 `SUPERVISOR`）与分机/号码/流程/webhook/账号名单读（今天 `ADMIN`）。班长墙板要读队列 → `SUPERVISOR` 的 grant 必须含 `config:read` → 顺带能读另一半。接受：那里没有凭证（分机密码是单独的 `config:write`），且拆出 `queues:read` 会开"按资源拆 `config`"的口子。

`[FACT]` **这是行为变更，不是重构。** §5 要求重构与行为变更不混一提交，所以 ③ 那一提交不得把它裹进"无行为变化"里——拓宽要么单独提交，要么在提交信息里显式点名。

### 一条 §0 应当抓到而没抓到的事实：角色守卫是**下限**，不是**匹配**

`[FACT]` `auth.Role.AtLeast` 用 `roleRank` 比大小（`internal/auth/auth.go:32-38`，`AGENT=1 < SUPERVISOR=2 < ADMIN=3`），`internal/httpapi/httpapi_test.go:20-26` 的用例直接写着 "supervisor meets agent"、"admin meets everything"。因此 `requireAgentRole` **不拒绝任何持有有效会话的人**——`/agent/*` 上真正的闸门是 `agentIDFor`（`agent_handlers.go:57-70`），不是角色。

`[INFERENCE]` 这决定了 `role → scopes` 的推导输入：**不是守卫的标签，而是按 rank 的可达性**。R 的 grant = 所有"rank ≤ R 即可达"的 operation 的 scope 之并。这样构造出来的 ③ **按定义是行为保持的**，而每一处偏离都变成可枚举的——目前只有两处：P7（转写历史，已裁定）与上面的 `config:read` 拓宽。

`[FACT]` `baseline.md` §0-3 的表如实记录了守卫标签，但**推导输入是可达性而非标签**，此处补正。

---

## 2026-08-31 — §2 契约完成（`ecf368f` 契约 / `3436dd9` 生成代码），停在 2.6 人工门

### 数字

`[FACT]` operation **91** 个（85 既有 + `getOpenAPI` + 5 个 API Key），**91 个全部显式声明 `security`**；全局 `security` 已移除——漏声明就是漏了，由 ⑦ 的断言抓，而不是悄悄继承兜底。
`[FACT]` 词表 **19** 个（根级 `x-scopes`）。错误码 22 → **26**。描述里 48 处 `Requires ROLE` 清零。
`[FACT]` `make api-lint` **0 error 0 warning**（10 条钉住的例外）；`make api-breaking BASE=main` **exit 0**——移除 `apiKeyHeader` scheme、移除全局 `security`、重写 85 个 operation 的 `security`，oasdiff 全部未判为 ERR。

### 自检抓到的一处未经裁定的提权 → 词表 18 → 19

`[FACT]` `docs/auth/scopemap.py` 的自检枚举"某角色 rank 低于该 operation 今天的下限，却持有它全部 scope"，抓到 **11** 条，其中 2 条不是已裁定的 `config:read`：
- `forceLogoutAgent`（下限 SUPERVISOR）—— `AGENT` 的 grant 必然含 `agent:act`（`/agent/ready` 就要它），于是**坐席顺带能把同事踢下线**。
- `listAgents`（下限 SUPERVISOR）—— `agent:read` 同理让坐席读到整张花名册。

`[INFERENCE]` 粗粒度的 `agent:*` 把"管自己"和"管别人"合成了一个开关——与 owner 否决 `users:write` 并进 `config:write` 是同一个形状。拆出 **`agent:manage`**（读花名册 + 强制登出别人）；`agent:read` / `agent:act` 从此只触及主体自己的坐席身份。修正后自检剩 **9 条拓宽，全部是已裁定的 `config:read`；收窄 0 条**。

`[FACT]` **这是脚本抓到的，不是人眼看出来的。** 把推导写成可重跑的自检，是它值钱的地方——`python3 docs/auth/scopemap.py` 随时可复查。

### role → scopes（机械推导，非拍脑袋）

```
AGENT      (8)  agent:act agent:read calls:control calls:create calls:read:own
                contacts:read contacts:write history:read:own
SUPERVISOR (15) 上列 + agent:manage calls:monitor calls:read:all config:read
                history:read:all quality:review reports:read
ADMIN      (19) 全部（+ audit:read config:write keys:manage users:write）
```

### P9 白名单：只有 2 个 operation 没有 Bearer 支

| operation | 理由 |
|---|---|
| `POST /auth/logout` | 会话机制，Key 没有会话可结束。不是能力不对称 |
| `POST /recordings/{recordingId}/reviews` | P8：人对人的判断，`reviewer_id` 必须指向能被问责的人。**不是 UI 特权**——班长的 session token 用 curl 一样能打；读评分（`GET /calls/{callId}/reviews`）照样有 Bearer 支 |

另有 2 个匿名 operation：`POST /auth/login`、`GET /openapi.json`。
`[INFERENCE]` ⑦ 的断言 (a) 必须检查 **`security` 键存在**，而不是 scope 数组非空——`getMe` / `logout` 是"已认证但不需要特定能力"，声明为空数组是正确的。

### 两个执行决定（记录，非裁定）

- `[INFERENCE]` **`X-AICC-Agent-ID` 不声明为 operation 的 header 参数**，只写在 `apiKeyBearer` 的 scheme 描述里。理由：仓库的先例是 `X-AICC-Csrf` —— 凭证呈递方式属于 securityScheme 而非 parameter；且声明为 parameter 会让 oapi-codegen 给几十个 handler 加 params 结构体，在 ③ 里制造大量与授权无关的签名改动。
- `[FACT]` **`GET /openapi.json` 没有 4XX**，`operation-4xx-response` 在 `.redocly.lint-ignore.yaml` 里钉了一条带理由的例外。它免鉴权、无参数，内容在编译期就嵌进二进制（`go:embed` 缺文件是编译错误而非运行时 404）——为了让 linter 闭嘴而声明一个不会发生的 404，是在唯一真相源里写假话。

### 待人工确认的流程问题

`[FACT]` `ecf368f` 与 `3436dd9` 两个提交**构建是红的**：`internal/api/api.gen.go` 的 `ServerInterface` 多了 6 个方法，`internal/httpapi/api_server.go:19` 的编译期断言失败，直到 ④ 给 `Server` 装上 handler。

`[FACT]` 这是 CLAUDE.md 明说的设计意图（"a new spec operation breaks the build until the server grows its method — that is the point"），且 §5 规定的提交切分（①契约 ②生成代码 … ④端点）**必然**产生这个窗口。分支上的中间提交红、PR 头绿，是常规做法。若不可接受，替代方案是把 6 个新 operation 从 ① 拆走、随 ④ 一起进契约——代价是契约变更不再是"单独一次提交"。

---

## 2026-08-31 — §3 测试先行：九条用例已写，在基线上按预期失败

`[FACT]` 文件 `internal/httpapi/scopeauth_test.go`。跑在**真实的 PostgreSQL 与真实的路由树**上：`store.Open` + `Migrate` 建一次性库、`auth.NewService` 真会话、`POST /auth/login` 真登录拿 cookie、`s.router()` 真路由。**鉴权链上没有 mock。** 电话服务用既有的 `stubAgents`，因为真的那个要一台 FreeSWITCH——这条界线是刻意的，被测的是授权不是交换机。

`[FACT]` 基线执行方式：`git worktree` 检出 `main`（构建绿的那个基线），把测试文件拷进去跑。当前分支的构建是红的（编译期断言等 ④），在红树上跑测试证明不了任何事。

```
git worktree add <tmp>/baseline main
cp internal/httpapi/scopeauth_test.go <tmp>/baseline/internal/httpapi/
AICC_TEST_DATABASE_URL=… go test -C <tmp>/baseline ./internal/httpapi/ -run …
```

### 失败形态（逐条）

| # | 用例 | 基线结果 | 形态 |
|---|---|---|---|
| 1 | `TestAKeyActingForAnAgentDrivesThatAgentsPresence` | **FAIL** | `POST /api-keys` → 404，端点不存在 |
| 2 | `TestAKeyWithNoAgentHeaderCannotActForOne` | **FAIL** | 同上 |
| 3 | `TestAKeyWithNoAgentStillReadsWhatItsScopeAllows` | **FAIL** | 同上 |
| 4 | `TestASessionMayNeverActForAnotherAgent` | **FAIL** | `GET /auth/me` 带 `X-AICC-Agent-ID` → **200**。基线**静默忽略**这个头——今天无害（没人读它），但正是这条断言存在的理由 |
| 5 | `TestARevokedKeyStopsAuthenticatingAndStopsBeingTouched` | **FAIL** | `POST /api-keys` → 404 |
| 6 | `TestAnAgentCannotHearSomebodyElsesCall` | **PASS** | 见下 |
| 7 | `TestAKeyMissingTheScopeIsToldWhichWayItFailed` | **FAIL** | `POST /api-keys` → 404 |
| 8 | `TestTheSecretIsReturnedOnceAndStoredNever` | **FAIL** | `POST /api-keys` → 404 |
| 9 | `TestAKeyCanSubscribeToTheEventStream` | **FAIL** | `POST /api-keys` → 404 |

**8 条按预期失败，1 条今天就通过。**

`[FACT]` 第 6 条 `TestAnAgentCannotHearSomebodyElsesCall` 在基线上 **PASS**——`internal/httpapi/recording_handlers.go:33-51` `mayHearCall` 已经拦住了。§3 的表把它列成"预期 403 / 失败形态 200"，事实是它今天就 403。**如实记录：这一条不是失败先行的用例，是回归护栏**——③ 删掉 5 处 handler 内角色判定时，`mayHearCall` 是其中之一，这条断言的作用是保证重写之后它还拦得住。

### §3.3 表格修正（以 §0-7 清单与契约为准）

| 原表 | 修正 | 依据 |
|---|---|---|
| "header 指定不在允许列表的坐席 → `AGENT_NOT_ALLOWED`" | **删除** | O3 取消了允许代理列表 |
| — | **新增**"Key 带 Bearer 订阅 `/events`" | P1 转正式条目 |
| `/agent/ready` 预期 **202** | 改为 **200** | 契约 `.paths["/agent/ready"].post.responses` 只有 200/401/403/409/500/503 |
| header 写作 `AICC-Agent-ID` | 改为 `X-AICC-Agent-ID` | 裁定 9 |
| "任意端点"（第 5 行） | 定为 `GET /auth/me` | 这条规则与端点无关；挑一个永远挂载的，免得 404 掩盖真正的断言 |

九条路径全部在 §0-7 的契约清单里，无其它偏差。

### 两处刻意的取舍

- `[INFERENCE]` **播种直写 SQL，不走写端点**（`insertCDR` / `insertRecordingFor`）。被测的是读端点的授权，让它依赖写端点的授权就把两件事绑在一起了；写端点自己的授权由第 8 条和 ⑦ 的断言管。
- `[FACT]` **第 3 条附带了基数断言**：`items` 至少 1 行，并在失败信息里写明"空集会因为错误的理由让这条断言通过"。第 8 条同理先断言 `api_keys` 有列，再逐列查明文。

---

## 2026-08-31 — 核对游标：`TASKS.md` 漂了四处，其中一处是假的对勾

`[FACT]` 逐条核对 `docs/auth/TASKS.md` 与仓库实际状态，发现四处不一致。已全部更正，并在文件顶部加了进度概览表。

| # | 漂移 | 事实 |
|---|---|---|
| 1 | **②a 被整条勾掉，声称 scope 常量生成步骤已做** | `[FACT]` `internal/api/scopes.gen.go` 与 `web/src/generated/scopes.ts` **都不存在**；`grep -c scope scripts/api-generate.sh` = **0**。实际只做了 `make api-generate`。已拆成 ②a（已做）与 **②b（未做）** |
| 2 | `0.13` / `2.0b` 写"词表 16 个" | 实际 **19** 个：初稿 16 → 18（加 `contacts:*`，owner 裁定）→ 19（加 `agent:manage`，自检抓到未裁定的提权） |
| 3 | §5 的 ①② 未勾，但已提交 | ① = `ecf368f`，② = `3436dd9` |
| 4 | 顶部没有"一眼看完"的进度视图 | 加了进度概览表，每阶段附落地提交哈希 |

`[INFERENCE]` 第 1 条是最要紧的：**一个假的对勾比没有对勾更坏**——它会让 ⑥ 在写创建表单时以为常量已经生成好，直到 import 失败才发现。⑥ 的"scopes 从生成的常量枚举取，不手写"直接依赖 ②b。

`[INFERENCE]` 教训记在这里：勾选的粒度不能大于验证的粒度。②a 原本是一条复合项（"`make api-generate` + scope 常量生成步骤"），跑完前半就整条勾了。此后凡复合项一律拆开。

---

## 2026-08-31 — §4.1 禁止事项 N1 落文字（无代码变更）

`[FACT]` 规则全文写在 `docs/auth/TASKS.md` §4 「N1 — scope 不得是角色的别名」；同一裁定在 `docs/design/07-naming.md:75` 落了一行 `API scopes` ruling（07 是 CLAUDE.md 指定的强制命名规范，scope 名是它此前未覆盖的一类名字）。

### 字面照抄 P3 会当场自相矛盾

`[FACT]` P3 的原话是「不得出现角色名」（`RESULTS.md:136`、`baseline.md:373`、`baseline.md:400`），并且拿 `agent:*` 当反例。
`[FACT]` 但 owner 定稿的 19 个词表里有 **3 个**以 `agent` 开头：`agent:read` / `agent:act` / `agent:manage`（`docs/openapi.json` 根级 `x-scopes`），而 `AGENT` 正是三个角色之一。
`[INFERENCE]` 照字面写下这条禁止事项，正式条目第一天就否掉词表的 3/19，将来任何机械检查都会在这 3 条上失败。**按 P3 真正要防的东西措辞**：资源段必须指一个领域资源，且任何 scope 都不得等价于「某角色的全部能力」改个名。

`[FACT]` 按这个判据，那 3 个是合规的：`agent` 指的是**坐席资源**（在线状态与坐席身份）而不是 `AGENT` 角色——`agent:read`/`agent:act` 只触及主体自己的坐席身份（裁定 8 修正 3 拆出 `agent:manage` 就是为了这个），三者相加也不是 `AGENT` 角色的能力集（该角色另持 `calls:control`、`calls:create`、`calls:read:own`、`contacts:read`、`contacts:write`、`history:read:own` 共 8 个）。被禁掉的是 `supervisor:*`、`admin:*`、`role:agent` 这类：没有资源，只有人格。

`[FACT]` 本步**不引入机械检查**——⑦ 的三条断言不含这一条，N1 是文字规则，给 ③–⑥ 逐条对照用。

---

## 2026-08-31 — ②b scope 常量生成（`c03080d`）

`[FACT]` 两个既有生成器都不产出 scope，A5 已记：`grep -c -i securit internal/api/api.gen.go` = 0，`web/src/generated/api.ts` 中 `security`/`scopes` 命中 0。契约里 `scopes` 只作为请求体字段出现（`api.gen.go:815/837/857/865` 全是 `[]string`，`api.ts:2759/2777/2782` 全是 `string[]`）——**类型是有的，词表是没有的**。

`[FACT]` 新增 `scripts/gen-scopes.mjs`，由 `scripts/api-generate.sh` 末尾调用（唯一生成入口不变），读 `docs/openapi.json` 根级 `x-scopes`，产出：

| 产物 | 内容 |
|---|---|
| `internal/api/scopes.gen.go` | 19 个 untyped string 常量（`ScopeCallsReadOwn = "calls:read:own"`）+ `AllScopes []string` + `ScopeDescriptions map[string]string` + `IsScope(name string) bool` |
| `web/src/generated/scopes.ts` | `type Scope` 19 支联合 + `SCOPES: readonly Scope[]` + `SCOPE_DESCRIPTIONS: Record<Scope, string>` |

`[INFERENCE]` **常量用 untyped string 而不是具名 `Scope` 类型**：契约生成的请求体字段是 `[]string`，具名类型会在每个调用点上加一次转换，换不到任何东西。TS 那边相反——`Scope` 联合类型是白拿的编译期检查，且 `SCOPE_DESCRIPTIONS: Record<Scope, string>` 靠它保证不漏。

`[INFERENCE]` **说明一起生成，不只是名字**。⑥ 的创建表单要给每个 scope 配一句人话；那句话手写就是第二份词表，迟早跟契约漂开。这正是 ②b 存在的理由，只生成名字等于把问题挪到 ⑥。

### 验证形态

`[FACT]` **确定性**：连跑 `scripts/api-generate.sh` 两次，两个产物 `diff` 均无输出（名字 `sort()` 后再发射）。api-check 会重新生成再 diff，不稳定的输出会让 CI 随机红。
`[FACT]` **编译**：`go build ./internal/api/` 通过，`go vet ./internal/api/` 通过，`gofmt -l internal/api/` 空。（**全树仍是红的**——`internal/httpapi/api_server.go:19` 等 ④，与本步无关。）
`[FACT]` **前端**：`web/` 下 `tsc --noEmit` exit 0，`oxlint src/generated/scopes.ts` exit 0。
`[FACT]` **四个守卫逐个验过会挡**（在临时目录用改过的契约副本跑）：

| 输入 | 报错 |
|---|---|
| 无 `x-scopes` | `root x-scopes is missing or empty — there is no vocabulary to generate from` |
| `x-scopes: {}` | 同上 |
| `"Supervisor:All"` | `is not 资源:动作[:范围] (lowercase, 2–3 colon-separated segments)` |
| 说明为空串 | `has no description` |

`[FACT]` **api-check 确实覆盖了这两个新产物**（裁定 1b 成立，`Makefile` 未动）：往契约 `x-scopes` 加一个 `tests:probe` 后 `make api-check` **exit 2**，diff 同时点出 `internal/api/scopes.gen.go` 与 `web/src/generated/scopes.ts`；提交后再跑 **exit 0**。
`[INFERENCE]` 注意 api-check 的语义是"先重新生成再 diff"——手改产物文件不会被它抓到（会被覆盖），被抓的是**契约改了而生成代码没跟着提交**。这与 `api.gen.go` 的情形一模一样，不是新的弱点。

`[FACT]` 名字形状检查（`^[a-z]+(:[a-z]+){1,2}$`）是**生成器的自保**，不是 N1 的机械化：一个带大写或空格的名字会推出坏掉的 Go 标识符。N1 的语义判据（不得是角色的别名）按 §4.1 的裁定**不做机械检查**。

---

## 2026-08-31 — ③ AuthContext + scope 中间件（`a1fa378`）

### 形状：授权改成读契约，而不是照着契约再写一遍

`[FACT]` oapi-codegen 对 `security` **一个字都不生成**：`grep -c -i securit internal/api/api.gen.go` = **0**，在补完 91 个 operation 的 security 之后重新生成，仍然是 0。
`[INFERENCE]` 于是只有两条路：在 `server.go` 里手写 91 处 `requireScope(...)`（把刚删掉的维护问题原样重建），或者把契约生成成一张表让服务器查。选后者。

`[FACT]` `scripts/gen-opsecurity.mjs` → `internal/api/opsecurity.gen.go`：**91** 条，键为 `"METHOD /契约路径"`，值含 `SessionScopes` / `KeyScopes` / `NeedsCSRF` / `IsAnonymous`。
`[INFERENCE]` **不能是一张扁平的 scope 表**：operation 之间的差别不只是要哪些 scope，还有**收不收这种凭证**。`nil` 与空切片是两个答案——`nil` 表示这类凭证在这个 operation 上根本没有分支（P9 白名单的 2 条），空表示"认证过就够了"。

`[FACT]` **落点是生成 wrapper 的 `HandlerMiddlewares`**。先做的 5 分钟 spike 证明：在 wrapper 内部 `chi.RouteContext(r).RoutePattern()` 已经解析完毕，嵌套 `Route`/`Group` 下返回完整模式（`POST /api/v1/calls/2f1c/answer` → `/api/v1/calls/{callId}/answer`）。这是整个方案的承重假设，先验证再动手。
`[FACT]` 查不到的路由**失败关闭**（500 INTERNAL），并由 `TestEveryRouteIsInTheContract` 走 `chi.Walk` 保证它不可达。

### 删掉了什么

| 项 | 数 | 去向 |
|---|---|---|
| 路由级角色守卫 | 14 处 | 契约的 `security` |
| handler 外角色判定 | 6 处 | 能力判定（第 6 处是 `internal/events/hub.go` `IsSupervisor` → `SeesEveryCall`） |
| `isMachine` / `machineIdentity` + 3 个下游 | — | `AuthContext.Kind` |
| `requireSessionOrAPIKey` / `requireSession` | 2 | 一个 `authenticate` |
| `AICC_API_KEY` / `X-AICC-Api-Key` | — | 无迁移路径（§1 明文即重发） |

`[FACT]` `SeesEveryCall` 是**已解析的能力**，不是 scope 字符串：`internal/events` 至此不知道这个产品有角色，也不知道有 scope。
`[FACT]` `AgentID` 在认证时解析一次，替掉 13 处各查各的；无坐席身份时回 **`AGENT_REQUIRED`**（新码），语义是"这个操作走坐席身份，而这个凭证没有"，不是"你缺个权限"。

### ⚠ 一处未裁定的提权，被拦下了而不是被放过

`[FACT]` `createAICall` 今天要 **SUPERVISOR**，而这个检查写在 handler 里（`outbound_handlers.go`），**不在路由表上**——`scopemap.py` 读的是路由表的守卫，因此把 `createCall` 的下限记成了 `N`（任何已认证）。
`[INFERENCE]` 契约给 `POST /calls` 的 scope 是 `calls:create`，而 `AGENT` 持有它（自己的点击外呼要）。**照契约直接放行，等于把"发起外呼机器人"的能力顺手发给每个坐席**——形状与 `agent:manage` 那次一模一样，只是这次自检脚本看不见。
`[FACT]` 处置：**保住原行为**，改判 `config:read`——那恰好就是昨天能做这件事的那批人（SUPERVISOR + ADMIN，从不含 AGENT），且不算胡诌：AI 外呼要挑一个 DID、跑它背后已发布的 flow，`config:read` 正是看见这两样东西的能力。
`[INFERENCE]` **但形状仍不对**——用一个读能力守一个写操作。真正的答案是二选一：契约给它一个自己的 scope（意味着重开 §2），或者裁定 `calls:create` 就该覆盖两种 kind。**⑦ 不得在此之上收工。**

`[FACT]` 另一处同源但已裁定的：`createAgentCall` 里"坐席只能报自己的分机"，原判据是 `!Role.AtLeast(SUPERVISOR)`，改判 `!ac.Has(calls:read:all)`——集合完全相同。

### 行为差异（不是零，如实列出）

`[FACT]` 拓宽 **9** 处已裁定的 `config:read`（`scopemap.py` 自检的输出即回执）+ webhook 配置对 Key 解封（P5，已裁定）。
`[FACT]` `X-AICC-Agent-ID` 从**静默忽略**改为 403 拒绝（§3 第 4 条断言）。
`[FACT]` 收窄 **0** 处。
`[INFERENCE]` 因此 §5 里"③ 是重构，无行为变化"这句话不准确，提交信息里如实写了差异，没有沿用那句话。

### 验证形态

`[FACT]` 分支树仍然是红的（`internal/httpapi/api_server.go:19` 缺 6 个方法，等 ④）。为了让 ③ 能被真正跑一遍，临时加了一个 `//go:build tempstubs` 的桩文件跑测试，**提交前已删除**（`git show --stat` 中没有它）。
`[FACT]` `go build -tags tempstubs ./...` 通过；`go vet -tags tempstubs ./...` **0 条**；`gofmt -l internal/ cmd/` **空**。
`[FACT]` `go test -tags tempstubs -race -count=1 ./...` 全树只剩 **8 条**失败，全部是 ④ 的工作面：

| 失败 | 归属 |
|---|---|
| §3 的 7 条 Key 用例（`POST /api-keys` → 404） | ④ |
| `TestEveryHTTPDependencyIsPlumbed`（`httpapi.Deps.Keys` 未接线） | ④ |

`[FACT]` **§3 第 4 条 `TestASessionMayNeverActForAnotherAgent` 由 FAIL 转 PASS**（基线上是 200，现在 `AGENT_IMPERSONATION_NOT_ALLOWED`）；第 6 条回归护栏 `TestAnAgentCannotHearSomebodyElsesCall` 仍 PASS。
`[FACT]` `make api-check` **exit 0**；`web` 下 `tsc --noEmit` exit 0、`oxlint .` 无新增（3 条既有 warning）。

### 顺带修正的两处文档漂移

`[FACT]` `docs/design/04-api-sse.md:18` 那段"Machine access (2026-08-26)"描述的正是被删掉的模型，已改写；同段"Roles gate route groups (middleware)"也已改写——角色现在只决定登录时授予哪些 scope。

### 一个既有缺口（不是本次引入，留给 ⑦b）

`[FACT]` 契约的 `ErrorCode` 26 个，两份 `translation.json` 的 `errors` 各 24 个：本次新增的 3 个键已补齐，但仍**缺 4 个**（`EXTENSION_POOL_EXHAUSTED` / `LAST_ADMIN` / `OPERATION_NOT_ALLOWED_FOR_CALL_TYPE` / `USER_DATA_TOO_LARGE`），另**多 2 个**（`UNKNOWN` / `rules`）。裁定 4 的三方对齐断言（⑦b）会抓到它。

---

## 2026-08-31 — ④ API Key 存储与端点（`0793e32`）：**构建转绿**

`[FACT]` `go build ./...` 通过。`internal/httpapi/api_server.go:19` 的编译期断言自 `ecf368f` 起红了 5 个提交，到这里补齐 6 个方法（`getOpenAPI` + 5 个 API Key）后转绿。分支上中间提交红、PR 头绿，与 2.6 记录的预期一致。

### 表与查找（O1 / O2 / O4 落地）

`[FACT]` 迁移 `00029_a_key_is_a_credential_with_a_name.sql`：`api_keys(id, name, key_hash bytea UNIQUE, key_prefix, status, scopes text[], created_at, created_by, last_used_at, revoked_at)`，两个 CHECK——状态只能是 `ENABLED`/`REVOKED`，且 `(status = 'REVOKED') = (revoked_at IS NOT NULL)`（一个事实的两半不许互相矛盾）。
`[FACT]` 查找 `WHERE key_hash = $1 AND status = 'ENABLED'`（O1，照抄 `sessions.sql:8-13`）。
`[INFERENCE]` **状态条件写在 SQL 里而不是应用层**，这才让 REVOKED 在唯一要紧的地方成为终态：吊销过的 Key 不是"查出来再被拒"，而是**根本查不出来**——于是被拒的请求也就碰不到它的 `last_used_at`。§3 第 5 条断言的正是这个（`after != before` 即失败）。
`[FACT]` `RevokeAPIKey` 的 `WHERE id = $1 AND status = 'ENABLED'`：重复吊销匹配不到行 → `ErrKeyAlreadyRevoked` → **409**，而不是静默 200 移动 `revoked_at`。
`[FACT]` `last_used_at` 每次直写，无节流（O2）。`key_prefix` 只用于展示、**不建索引**、从不用来找行。
`[INFERENCE]` 按前缀查等于按截图上能读到的东西查——这就是 O1 取消"前缀取行 + 常量时间比较"之后剩下的唯一正确形状。

### 明文

`[FACT]` 32 字节 `crypto/rand` → `base64.RawURLEncoding`，立即取 SHA-256 存库。§3 第 8 条逐列扫描 `api_keys`：无一列装明文；`length(key_hash) = 32`；`GET /api-keys` 与 `GET /api-keys/{id}` 的响应里没有 `secret`、`keyHash`、`key_hash`。

### 未知 scope 拒绝而不是忽略

`[FACT]` `checkedScopes` 用 ②b 生成的 `api.IsScope`，未知名字 → 422 并列出 `unknown` 与 `allowed`。
`[INFERENCE]` 存下来会让运维以为这把 Key 有一个它没有的能力，然后在别处、以一个没人能联系回这张表单的理由失败。词表来自契约的 `x-scopes`，所以这条校验不可能与 operation 实际要的东西漂开。

### 裁定 2 的落地形状

`[FACT]` 三个归属列**仍然装 user id**：`AgentDirectory` 新增 `UserIDForAgent`，`AuthContext` 新增 `ActorUserID`（会话 = 本人；Key 代理坐席 X = X 绑定的 `users.id`；Key 未代理 = `uuid.Nil`）。
`[INFERENCE]` **"谁认证的"与"这是谁干的活"是两回事**，合并它们正是会把 key id 写进 user id 空间的那一步——而 baseline §0-11 已记：四列都没有指向 `users` 的外键，数据库不会拒绝，风险是沉默的。
`[FACT]` callbacks 三条在无人可归属时回 `AGENT_REQUIRED`（列语义不许留空）；`contacts.updated_by` 可空，写 `NULL`。

### 清干净的旧模型

`[FACT]` `AICC_API_KEY` / `X-AICC-Api-Key` 的最后残留一并删除：`.env.example`（改写成"这里没有 API Key 设置了"）、`deploy/README.md` 两处（含上线检查单那条"泄漏只能靠重启轮换、没法单独吊销"）、契约里 webhook `authToken` 说明中的那句反向引用。

---

## 2026-08-31 — ⑤ 审计的四列（`e605235`）

`[FACT]` 迁移 `00030_the_trail_says_who_and_as_whom.sql`：`subject_kind` / `subject_id` / `subject_name` / `agent_id`，两个部分索引，一个 CHECK（`USER` / `API_KEY`）。**`actor_id` 一个字未动**（裁定 3）。
`[FACT]` `SubjectKey` 的值定为 **`API_KEY`** 而不是 `KEY`——它进 `subject_kind` 列、被人读，`KEY` 与 `USER` 并排像是一种人。
`[FACT]` `subject_name` 是**写入时快照**，读取时不 join：Key 会被吊销、账号会被删除，为一个名字去 join 是账本变成空白的原因。`actor_username` 保留原来的 join，因为那是已发布字段，行为不能变。
`[FACT]` 回填：既有行全部来自人（在此之前没有别的东西能动手）；老共享密钥写的行 `actor_id` 为空，回填后 subject 仍为空。
`[INFERENCE]` 给它编一个身份，是在唯一不许伪造的表里伪造。

`[FACT]` **migrate_test 加了 fixture** `TestMigrationsKeepTheAuditTrailsHistory`：先迁到 29，播 2 行有 actor + 1 行无 actor，再迁完，断言 2 / 1。空库测不出这两半中的任何一半——约束若装在回填之前会 "is violated by some row"，而回填若悄悄没生效会让一年的审计说没人干过。

`[FACT]` 契约按 spec-first 先改：`AuditEntry` 加 4 个可选字段，`make api-lint` 0 error 0 warning，`make api-breaking BASE=main` **exit 0**（纯加法）。

### 全绿

`[FACT]` `go build ./...` / `go vet ./...` / `gofmt -l internal/ cmd/` / `go test -race -count=1 ./...` **全部通过**；`make api-check` exit 0；`web` 下 `tsc --noEmit` exit 0。
`[FACT]` **§3 九条全部 PASS**（基线上 8 条按预期失败的那些，现在逐条通过）。

### 一处被重写的旧断言

`[FACT]` `TestTheKeyIsAuditedAsItselfRatherThanAsAUser` 原本断言 "detail 的 jsonb 里出现 api-key 这个词"。那是四列不存在时的权宜之计，⑤ 之后改为断言 `subject_kind` / `subject_name` / `subject_id` 三个列。**要求没变，检查的地方变了**——原来的写法把一个事实存在了没法过滤的地方。

---

## 2026-08-31 — ③b 裁定：第 20 个 scope `calls:create:ai`（owner）

`[FACT]` 裁定：**加 `calls:create:ai`**，词表 19 → **20**。`calls:create` 到达 operation；`kind=AI_OUTBOUND` 这一半另要 `calls:create:ai`。SUPERVISOR / ADMIN 持有，AGENT 不持有——与基线上那个 handler 内 SUPERVISOR 检查的集合完全相同。

`[INFERENCE]` **那半条检查留在 handler 里，不进契约的 `security`**：一个 operation 只能带一个 scope，而这条路由服务两种 kind、两个答案。契约在 `POST /calls` 的 description 里写明了这一点，词表里也写明了"Required in addition to calls:create for kind=AI_OUTBOUND"。把它写进 `security` 会让点击外呼也要这个 scope，那是另一个 bug。

`[FACT]` `make api-lint` 0 error 0 warning；`make api-breaking BASE=main` **exit 0**（加一个 scope 是纯加法）。

### `scopemap.py` 学会了看 handler 内的判定

`[FACT]` 新增 `HANDLER_CHECKS` 表，与 `OPS` 同格式，合并进 `role_scopes()` / `widenings()` / `narrowings()`；打印基数时分开计（`operation 91 个，handler 内判定 1 个`）。
`[INFERENCE]` 这才是这次事故的正解。**漏掉 `createAICall` 不是眼睛不好，是脚本的输入里根本没有它**——它读路由表的守卫，而这条判定从来不在路由表上。列在 `HANDLER_CHECKS` 里，下一个同类判定就会进推导，而不是留在推导之外等着被发现。
`[FACT]` 修正后自检：**拓宽 9 条，全部是已裁定的 `config:read`；收窄 0 条。** AGENT 8 / SUPERVISOR 16 / ADMIN 20。

### 生成器的一处命名修正

`[FACT]` `scripts/gen-scopes.mjs` 原来朴素地 PascalCase，`calls:create:ai` 会生成 `ScopeCallsCreateAi`——违反 07-naming §2（Go 名里首字母缩写全大写）。加了 `INITIALISMS` 集合，与 `oapi-codegen.yaml` 的 `additional-initialisms` 对齐，现在是 `ScopeCallsCreateAI`。**两个生成器把同一个词拼成同一个样子。**

### 一条新断言，当场抓到一次漏改

`[FACT]` 新增 `TestALoginsGrantIsTheDerivedOne`：钉住 AGENT 8 / SUPERVISOR 16 / ADMIN 20、词表 20，并单独断言 AGENT **不**持有 `calls:create:ai` 而 SUPERVISOR / ADMIN 持有。
`[FACT]` **它第一次跑就失败了**——上一批改动里有一个 python 脚本在中途 assert 失败，后面三处编辑（`grants.go`、`outbound_handlers.go`、`outbound_handlers_test.go`）根本没执行，而当时 `go build` / `go vet` / `go test` 全绿，因为旧的 `config:read` 检查还在原地、行为没变。
`[INFERENCE]` **一次不改变行为的漏改，是测试套件抓不到的**——除非有人把"应该变成什么"写下来。这条断言就是那个"写下来"。

---

## 2026-08-31 — 在跑着的 dev stack 上真实执行（③④⑤ + ③b）

`[FACT]` 依据 owner 那条"绿的测试套件不是验证"。`/tmp/aicc` 起在 8080，PostgreSQL 是**带着历史的那个开发库**，不是测试用的一次性库。

### 迁移第一次对着真实历史跑

`[FACT]` `goose_db_version`：29 与 30 均 `is_applied = t`，时间戳 `2026-08-31 06:56:48`。
`[FACT]` 回填结果（762 行既有审计行）：

| subject_kind | 行数 | 有 subject_id | 有 subject_name |
|---|---|---|---|
| `USER` | 739 | 739 | 736 |
| `NULL` | 23 | 0 | 0 |

`[INFERENCE]` **736 而不是 739 是对的**：3 行的账号此后被删了，join 找不到用户名，快照就写空——这正是"名字要快照"要处理的那种行，而不是缺陷。23 行无 subject 的是老共享密钥（或更早）写的，保持空白，没有被编造身份。

### Key 的完整生命周期（curl，真服务器）

| # | 动作 | 结果 |
|---|---|---|
| 1 | 会话签发 Key（`history:read:all` + `agent:act`） | 201，`secret` 只出现这一次，`keyPrefix=fo7xW73a` |
| 2 | 签发时带未知 scope `supervisor:all` | 422 `VALIDATION_FAILED`，`params.allowed` 列出全部 20 个 |
| 3 | Key 读 `GET /cdrs` | **200** |
| 4 | Key 读 `GET /queues`（缺 `config:read`） | **403 `INSUFFICIENT_SCOPE`**，`params.requiredScope=config:read` |
| 5 | Key 带 `X-AICC-Agent-ID` 打 `POST /agent/ready` | **200**，`agent_states` 里 wei 真的变成 `READY` |
| 6 | Key 不带该头打同一端点 | **403 `AGENT_REQUIRED`** |
| 7 | 会话带该头打 `GET /auth/me` | **403 `AGENT_IMPERSONATION_NOT_ALLOWED`** |
| 8 | Key 带 `calls:read:own` 订阅 `/events` | **200 `text/event-stream`**；只带 `history:read:all` 的那把是 403 |
| 9 | `GET /api-keys` | 两把都在（含 REVOKED 的），响应里没有 `secret` / `keyHash` / `key_hash` |
| 10 | 免鉴权 `GET /openapi.json` | **200**，275,468 字节，`application/json` |

`[FACT]` 第 5 步写下的审计行，正是 ⑤ 存在的理由：

```
subject_kind | subject_name   | agent_id                             | actor_null | action
API_KEY      | live-check-crm | 807b2164-bd6b-47e9-9286-39a1ca831cea | t          | POST /api/v1/agent/ready
```

`[INFERENCE]` 这一行读出来是「**CRM 把 wei 置成了示闲**」。改之前它只能是一个 actor 为空、"api-key" 埋在 jsonb 里的行。

### REVOKED 终态 —— 人工门的证据

`[FACT]` 吊销前 `last_used_at = 2026-08-31 06:59:33.118958+00`。
`[FACT]` `POST /api-keys/{id}/revoke` → **200**，`status=REVOKED`，`revokedAt=…06:59:33.195436`。
`[FACT]` 用同一把 Key 再打 `GET /cdrs` → **401 `INVALID_CREDENTIALS`**。
`[FACT]` 那次被拒之后再读 `last_used_at`：**`2026-08-31 06:59:33.118958+00`，一个字没动**。
`[INFERENCE]` 这是"状态条件写在 SQL 的 WHERE 里"唯一能被观察到的后果：**被拒的请求不算一次使用**。若是查出来再拒，这个值会往前跳，运维看着一把已经吊销的 Key"还在用"。
`[FACT]` 再吊销一次 → **409 `CONFLICT` "the key is already revoked"**，`revoked_at` 未被改写。

**⬜ 这一段是 REVOKED 人工门的证据，门本身由 owner 勾。**

### SPA（Browser Harness，按 owner 的规矩，不用 devtools MCP）

`[FACT]` `admin / aicc@12345` 登录成功 → `/admin`。管理端逐屏无错、有数据：`/admin/users` 21 行、`/admin/extensions` 9 行、`/admin/audit` 50 行、`/admin/routing` 3 行、`/admin/numbers` 7 行、`/admin/bots` 7 行。（`/admin/queues` 是 Not Found——它本来就不是路由，nav 里的「Queues & Routing」指向 `/admin/routing`。）
`[FACT]` `wei / aicc@12345` 登录 → `/agent` 坐席台。11 个 agent 作用域的 operation 全部成功：`auth/me`、`agent/presence`、`callbacks`、`dispositions`、`cdrs/mine`、`agent/wrap-up`、`calls/mine`、`calls/waiting`、`reports/me`、`contacts`。
`[FACT]` 事件流已连接（页面上的提示语："Live updates from the server are connected"）。
`[FACT]` 页面自己的 fetch 打过去：`POST /agent/not-ready` 200、`POST /agent/ready` 200、**同一请求去掉 `X-AICC-Csrf` → 403 `FORBIDDEN` "missing X-AICC-Csrf header"**。
`[INFERENCE]` 最后这条要紧：CSRF 的要求现在来自契约的 `NeedsCSRF`，不再来自"方法是不是 GET"的硬编码判断——真实浏览器上行为不变。
`[FACT]` 服务端日志里没有与本次改动相关的 error/warn（只有既有的 webhook 投递失败，目标 `127.0.0.1:9111` 没起）。

`[FACT]` 两把试验 Key 都已吊销，未留启用状态的凭证。

---

## 2026-08-31 — ⑥ Admin API Keys 页（`ee72b18`）

`[FACT]` `web/src/routes/_app.admin.keys.tsx` + `web/src/lib/keys.ts`，`nav.ts` 挂在 **System** 组下，`requireRole(ADMIN)`。
`[INFERENCE]` 放 System 而不是 Manage，理由与审计日志同源：Manage 改的是通话怎么被处理，而一把 Key 不改变任何一通电话——它改变的是**谁可以要求平台去处理**。

`[FACT]` 创建表单的 **20** 个能力项（名字 + 那句人话）全部来自 `web/src/generated/scopes.ts`，即 ②b 生成的、源自契约 `x-scopes` 的那份。浏览器上实测：弹窗里 `input[type=checkbox]` 数量 = 20。
`[INFERENCE]` 这就是 ②b 的兑现。手写这 20 行就是第二份词表，加一个 scope 的那天表单会继续提供昨天那份——而 `calls:create:ai` 恰好在 ⑥ 之前一天才加进来，它是自动出现在表单里的。

`[FACT]` 明文存在 `useRef`，不进表单 state、不进 query 缓存；弹窗写明"只出现这一次…丢了就吊销重发"。
`[FACT]` 吊销像删除一样就地确认；**吊销过的 Key 留在列表里**（上个月那条审计行指的就是它）；状态列只有两个值；**没有「允许 Agents」列**（O3）。

### Browser Harness 上的完整流程

| 步骤 | 结果 |
|---|---|
| 列表 | 6 列（名称/前缀/能力/最后使用/状态/操作），既有 Key 正常渲染 |
| 签发（勾 `calls:create:ai`） | 弹窗显示明文一次 + `Authorization: Bearer …` 用法 |
| 用这把 Key 打 `POST /calls` (AI_OUTBOUND) | **403 `INSUFFICIENT_SCOPE`，点名 `calls:create`** |
| 改名 + 改 scopes（PATCH） | 列表当场更新为 `ui-check-renamed` / `contacts:read history:read:all` |
| 吊销 | 行变 REVOKED，操作列少一个按钮；那把 Key 随即 401 |
| 中文界面 | 表头/状态/描述逐项核对，**无字面量漏出** |

`[INFERENCE]` 第三行是个值得记的确认：只持 `calls:create:ai` 到不了 `POST /calls`——`calls:create` 才是到达 operation 的那一个，`:ai` 是那一半额外要的。报错按顺序点名先缺的那个，正确。

`[FACT]` 顺手补了 `common.done`——它在真实页面上以字面量 `common.done` 露出来过，是 Browser Harness 看出来的，`tsc`/`oxlint`/单元测试都不会报。

---

## 2026-08-31 — ⑦ 契约门三条断言（`ee21e58`）

`[FACT]` `internal/httpapi/contract_gate_test.go`，随 `go test -race ./...` 在 `ci.yml` 自动执行，不需要动 workflow（§0-10 的推断成立）。

| 断言 | 内容 |
|---|---|
| (a) `TestEveryMountedRouteDeclaresItsAuthorization` | 走 `chi.Walk`，每条 `/api/v1` 路由都要在 `api.OperationSecurityByRoute` 里，且要有一支凭证够得着。4 个排除项在 `notTheAPI` 里各带理由 |
| (b) `TestOneErrorCodeIsSpelledTheSameEverywhere` | 契约 enum ≡ `errors.go` 常量 ≡ 两份 `translation.json`。另有 `TestTheUntranslatableKeysAreStillThere` |
| (c) `TestASystemCanReachWhatAPersonCan` | 没有 Bearer 支的 operation 必须在 `browserOnly` 白名单里（2 个，各带理由）；且白名单不许比它豁免的东西活得久 |

`[FACT]` 另加 `TestEveryContractOperationHasASecurityRow`：生成表的 operation 数 ≡ `api.ServerInterface` 的方法数（**91 = 91**）。

### 三条都做了反证 —— 通过的断言在证明它抓得住之前不算数

| 注入的缺陷 | 断言的反应 |
|---|---|
| `server.go` 加一条契约外的 `GET /secret-backdoor` | (a) FAIL：`mounted and not declared in the contract: GET /secret-backdoor` |
| 删掉 `zh` 的 `LAST_ADMIN` | (b) FAIL：`the contract has LAST_ADMIN and web/src/locales/zh/translation.json does not` |
| 拿掉 `createRecordingReview` 的豁免 | (c) FAIL：`reachable by a browser session and by nothing else: createRecordingReview (POST /recordings/{recordingId}/reviews)` |

`[FACT]` 全部还原后重跑，五条断言均通过。

### 两处登记的例外

`[FACT]` `notTheAPI` 4 条：`GET /metrics` / `/healthz` / `/readyz`（在 `AICC_METRICS_ADDR` 那个独立监听上，契约的 description 自己声明了这个例外）、`GET /*`（SPA 兜底，它是本 API 的一个消费者而不是它的一部分）。
`[FACT]` `untranslatable` 2 条：`UNKNOWN`（前端 `describeError` 的 `defaultValue` 兜底）、`rules`（`errors.rules.<RULE>` 的嵌套表，由 `fieldErrorText` 按 `rule` 参数查，不是错误码）。
`[INFERENCE]` 登记而不是宽松比较：**宽松的比较正是漏译能藏身的地方**。所以另有一条断言盯着这两个键还在——兜底悄悄消失会让界面直接显示原始 key，而 (b) 还会继续通过。

`[FACT]` 删掉了 `workspace_test.go` 里的 `TestEveryRouteIsInTheContract`——(a) 是它的完整版（多查一条"有凭证够得着"），两条测同一件事只会让人不知道该改哪条。

### ⑦ 之前的那一小步

`[FACT]` 裁定 4 要求先补齐 4 个漏译再装断言，已单独一个提交（`47491ee`）：`USER_DATA_TOO_LARGE` / `OPERATION_NOT_ALLOWED_FOR_CALL_TYPE` / `LAST_ADMIN` / `EXTENSION_POOL_EXHAUSTED`。
`[INFERENCE]` **先把账平了，再装那个不许它再次失衡的秤**——反过来做，第一次 CI 红的会是一个与本次改动无关的历史欠账。

---

## 2026-08-31 — ⑦(b) 的一处真实缺口：手抄的那条腿

`[FACT]` `contract_gate_test.go` 初版里的 `goErrorCodes()` **把 26 个常量手抄了一遍**。三方对齐于是变成了：契约 ↔ 翻译（真的）、契约 ↔ 我抄的那份（假的）。

`[INFERENCE]` 走一遍失败路径：有人往 `errors.go` 加 `CodeQuotaExceeded` 并在 handler 里用上，忘了改契约。契约没有它、我抄的列表没有它、翻译没有它——**三方比较全部通过**，而线上发出的是一个契约没声明的码，界面显示「未知错误」。这正是裁定 4 点名要防的那件事（"`errors.go` 是手写且无 CI 兜底，不加这条断言，新增的 4 个码明天照样会漏"）。

`[FACT]` 当时的反证只删了 `zh` 的 `LAST_ADMIN`——那验的是**翻译那条腿**，不是这条。**一次反证只证明它验到的那条路径。**

`[FACT]` 改法：用 `go/parser` 解析 `errors.go`，取 `ErrorCode` 类型 const 块里的字面量（`routes_test.go:36-73` 解析 `server.go` 的同一手法）。
`[FACT]` 补的反证：往 `errors.go` 加一个 `CodeQuotaExceeded`，断言 FAIL —— `internal/httpapi/errors.go has QUOTA_EXCEEDED and the contract does not`。还原后通过。

`[INFERENCE]` 记在这里而不只是改掉：这与 ②a 的假对勾是同一个形状——**一个看起来在守着的检查，实际守的是它自己的副本**。

---

## 2026-08-31 — ⑥ 的能力选择器改成按资源分组（owner 提出）

`[FACT]` 20 个 scope 装在一个滚动框里，一屏看不完。owner 问能不能全选、或者按分类选。

### 裁定：分组 + 组内全选，**不做全局全选**

`[INFERENCE]` **全选在权限表单上是反模式**。勾满全部 scope 的那把 Key 同时持有 `keys:manage`（能再签发别的 Key）与 `users:write`（能重置密码、改角色）——那正是一把万能钥匙，是这次重构删掉的 `AICC_API_KEY` 换个样子回来。让它一键可得，等于把它请回来。**这里的麻烦是特性。**
`[INFERENCE]` 分组减少的是**翻找的成本**，不是**决定的成本**：组标题上的「本组全选」回答的是「这个集成要不要碰通话」，那是个真实的问题；全局全选回答的是「要不要全都给它」，那个问题的正确答案几乎总是「不」。

`[FACT]` **分组是从名字推出来的，不是列出来的**：scope 是 `资源:动作[:范围]`（N1），取第一段即资源。所以这里同样没有需要维护的清单——新加一个资源的 scope 会自动出现一个新组。没写标签的资源用它自己的名字当标题，**新能力绝不会因为没人写文案而悄悄不出现**。

`[FACT]` 组标题的 checkbox 是三态的（none / some / all）。`[INFERENCE]` "some" 必须与 "none" 长得不一样——否则一个收起的组里勾了 2/6，读起来跟没碰过一样，读者只好展开去看，而那正是分组要省掉的翻找。

### 浏览器上实测

`[FACT]` 弹窗里可见的 checkbox 从 **20 个变成 10 个**（10 个收起的组）：`agent 3` / `audit 1` / `calls 6` / `config 2` / `contacts 2` / `history 2` / `keys 1` / `quality 1` / `reports 1` / `users 1`。
`[FACT]` 点 `calls` 组的一个 checkbox → 组标题变 `6/6`。展开后取消 `calls:monitor` → 标题变 `5/6`，组 checkbox 变为 `checked=false, indeterminate=true`。
`[FACT]` 提交后落库的正是那 5 个：`calls:control calls:create calls:create:ai calls:read:all calls:read:own`。
`[FACT]` 中文界面逐组核对，标签正常，无字面量漏出。试验 Key 已吊销。

### 一个被拒绝的选项，记下来

`[FACT]` 另一个候选是「预设模板」（一键勾出「CRM 坐席代理」那 8 个）。**没做。**
`[INFERENCE]` 一个叫「坐席代理」的预设，勾出来的恰好就是 `AGENT` 角色的 grant——**那是把角色模型从前端后门放回来**。技术上无害（存库的仍是一个个 scope，没有任何代码分支在预设上），但它会让人重新按人格思考，而 N1 的整条规则就是为了防这个形状。要做需要 owner 明确裁定。

### 组的顺序：按 Key 是为什么存在的排，不是按字母

`[FACT]` owner 要求按使用频率排。**没有真实的调用统计**，所以这是按「Key 这条路径存在的理由」推的，代码里写明了这一点——将来若有数据，这份列表就是该被它取代的东西。

`[FACT]` 顺序：`calls` → `agent` → `contacts` → `history` → `reports` → `config` → `quality` → `audit` → `users` → `keys`。

`[INFERENCE]` 推导：没有浏览器的系统首先是发起和结束通话、跟着事件流（`calls`，这也是老共享密钥当年唯一够得着的两个 operation），常常是替一个「电话注册着但从没登录过本应用」的坐席做（`agent`），边做边查来电是谁（`contacts`）。然后是读发生过什么的集成（`history`、`reports`），再然后是改平台配置的（`config`）。其余的是偶发。

`[INFERENCE]` **两个能提权的落在最后，这个巧合不值得藏着**：`users:write` 是通往角色的唯一路径，`keys:manage` 能再签发凭证。既少用、又离随手一点最远，对它们俩都是对的位置。

`[FACT]` 不在列表里的资源**不报错也不消失**，排到末尾、按词表原顺序。加一个 scope 永远不需要为了让它可达而改这个文件。

### 吊销后整行的样子（owner 报的缺陷）

`[FACT]` owner 报「test-1 已经 REVOKED，但是 UI 没有变化」。**先复现再改**，实测同一行前后：

| | 启用 | 吊销后 |
|---|---|---|
| 状态列 | 启用 | 已吊销 |
| 吊销按钮 | 有 | 消失 |
| 编辑按钮 | `disabled=false` | `disabled=true`，opacity 0.5 |
| **整行文字颜色** | `rgb(24,24,27)` | **`rgb(24,24,27)`——没变** |
| **整行透明度** | 1 | **1——没变** |

`[INFERENCE]` 缺陷成立，而且它是个**表述层级放错了**的问题：吊销是关于**整行**的事实，却只体现在一个小药丸和一个消失的图标上。这张表是用来扫「哪些还在工作」的，而一把死掉的凭证和一把活的在扫视距离上完全一样。

`[FACT]` 改法：吊销的行整体降为次要文字（`text-muted-foreground`），名字不再 `font-medium`。**不加删除线、不用红色、不加底色**——前两者读起来是「删掉了」和「出错了」，都不对（吊销既不是删除也不是错误，是结束），底色则是 `web/CLAUDE.md` 明令禁止的。灰是这套设计系统里 Offline 的语义，正合适。
`[FACT]` 仓库里此前**没有「这一行不生效」的先例**，所以这是立了一个；照 `web/CLAUDE.md` 的一致性规则，后面同类表格应当复用它。

`[FACT]` 实测：启用行 `rgb(24,24,27)` / `font-weight 500`，吊销行 `rgb(113,113,122)` / `400`。

### 截图暴露的第二个缺陷

`[FACT]` 改完截图，发现中文「已吊销」在药丸里**折成两行**（「已吊 / 销」），把那一行撑到比别的行高。加 `whitespace-nowrap` 修掉，实测三行高度均为 36px（与设计系统的 36px 行高一致）。
`[INFERENCE]` 这一条是**看了图才发现的**——`tsc`、`oxlint`、单元测试、乃至读 DOM 属性都不会报它。与 `common.done` 那次同源：有些缺陷只在渲染出来的像素上存在。

### 服务器违反了自己的契约：已吊销的 Key 还能被编辑

`[FACT]` owner 指出吊销后铅笔图标还在。查契约，`PATCH /api-keys/{keyId}` 的 description 白纸黑字写着 **"A revoked key cannot be edited."**，`409` 也早已在 responses 里声明。**所以这不是设计问题，是服务器没做到它自己承诺的事。**

`[FACT]` 实测复现（真服务器）：对一把已吊销的 Key `PATCH {"name":"EDITED-AFTER-REVOKE","scopes":["users:write","keys:manage"]}` → **200**，能力从 `calls:*` 被改写成了 `users:write keys:manage`。

`[INFERENCE]` **这不是外观问题。** 审计行说这把 Key 做过事；它记录在案的能力可以被事后改写，于是几个月前那些行描述的是一把从未存在过的 Key——而那是唯一不许伪造的一张表。`api_keys` 的行之所以不许硬删，理由与此完全相同（迁移 `00029` 的注释），却漏了「也不许改」这一半。

`[FACT]` 修法：条件写进 SQL 的 `WHERE id = $1 AND status = 'ENABLED'`，与吊销同一手法；store 区分「不存在」与「已吊销」，handler 回 **409 CONFLICT**。
`[INFERENCE]` 写在 `WHERE` 里而不是调用方的 `if` 里，理由和吊销那次一样：**一条在触碰行的地方生效的规则，不会被第二个忘了它的调用方绕过。**

`[FACT]` 新增 `TestARevokedKeyCannotBeEdited`，并做了反证：把 SQL 里的 `AND status = 'ENABLED'` 拿掉 → FAIL，两条断言都点名（`status = 200, want 409` 和 `scopes = [keys:manage] — 这把已吊销的 Key 获得了签发其它 Key 的能力`）。还原后通过。
`[INFERENCE]` 断言里**除了状态码还查了库里的值**：只断言 409，一个「先写库再回错」的实现照样能过。

`[FACT]` UI 同步：吊销的行**不再渲染任何操作按钮**（此前是一个永远 disabled 的铅笔）。一个永远不会变成可用的禁用控件是家具——它招来一次什么也不会发生的点击，而一整列灰图标比一列空白说的更少。实测吊销行 `actions: 0`，启用行 `actions: 2`。

`[FACT]` 开发库里留下了一行 `EDITED-AFTER-REVOKE`（能力显示为 `keys:manage users:write`），那是这次复现留下的痕迹。它已吊销、认证不了，也**再也改不动了**——按设计没有硬删，就留着。

---

## 2026-08-31 — REVOKED 终态人工门：**owner 确认通过**

`[FACT]` owner 于 2026-08-31 确认。四道人工门（§0 结论、§2 契约、③b AI 外呼能力、REVOKED 终态）**全部通过**，`docs/auth/TASKS.md` 无未勾选项。

`[FACT]` 门所验的形态，全部在真服务器、带历史的开发库上执行：

| 断言 | 观察到的结果 |
|---|---|
| 吊销成功 | 200，`status=REVOKED`，`revokedAt` 落值 |
| 吊销即刻生效 | 同一把 Key 下一次请求 **401 `INVALID_CREDENTIALS`** |
| **被拒的请求不算一次使用** | `last_used_at` 停在 `06:59:33.118958`，未被那次 401 推进 |
| 吊销是终态（重复吊销） | **409**，`revoked_at` 未被改写 |
| 吊销是终态（事后编辑） | **409**，名字与 scopes 均未改动（回查库确认） |

`[INFERENCE]` 最后两行是这道门真正的内容。"终态"若只意味着"认证不过"，那它只挡住了一半：**一把吊销后还能被改写能力的 Key，会让几个月前的审计行描述一把从未存在过的 Key。** 那一半是 owner 看着界面上的铅笔图标发现的，不是任何自动检查发现的。

## 任务收尾

`[FACT]` 分支 `docs/auth-baseline`，36 个提交。全绿：`go build` / `go vet` / `gofmt` / `go test -race ./...` / `make api-check` / `make api-breaking` / `tsc --noEmit` / `oxlint` / 155 个前端测试 / `scopemap.py` 自检。

`[FACT]` 最终形状：**20 个 scope**、**91 个 operation 全部显式声明 security**、**0 处路由级角色守卫**、**2 个登记在案的浏览器专属 operation**、**审计行能说出「谁、以谁的身份」**。

`[INFERENCE]` 一句话总结这次改动改了什么：**授权从"路由挂在哪个 group 里"变成了"契约怎么写"。** 角色没有从产品里消失——账号仍然有角色，`/auth/me` 仍然返回它——但角色现在只是"登录时授予哪些 scope"的那张便利映射表，它下游没有任何一行代码再问"你是什么角色"。
