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
