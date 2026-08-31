<!-- SPDX-License-Identifier: Apache-2.0 -->

# 统一认证模型 — §0 源码基线

只读研究产出。每条结论标 `[FACT]`（带 `file:line`）/ `[INFERENCE]` / `[ASSUMPTION]`；找不到的组件写 `NOT FOUND`。
基线快照：`main` @ `2ecb28f`，工作区干净，日期 2026-08-31。

**产品定位（owner，2026-08-31，本会话口述）**：*UI is optional. API is the product.*
本节的路由/契约事实按这个定位记录——契约是产品面，Web 只是它的一个消费者。见 §0-8 与 §0-16。

---

## 鉴权链

### 1. 页面 Token 的签发位置、载荷字段、签名/校验方式、过期处理

- `[FACT]` 签发入口：`internal/httpapi/auth_handlers.go:26` `Login` → `internal/auth/auth.go:77` `Service.Login`。
- `[FACT]` **Token 无载荷**：32 字节 `crypto/rand`，`base64.RawURLEncoding` 编码（`internal/auth/auth.go:166-173` `newToken`）。不是 JWT，没有签名，没有任何可解析字段。
- `[FACT]` 存储形态：只存 SHA-256 摘要（`internal/auth/auth.go:175-178` `hashToken`；写入 `sessions.token_hash`，`internal/store/sql/sessions.sql:4-6` `CreateSession`）。
- `[FACT]` 会话行结构：`sessions(id, user_id, token_hash, user_agent, ip, expires_at)`，建表见 `internal/store/migrations/00001_foundation.sql`（`users` 同文件 `:15-16`）。
- `[FACT]` 校验：`internal/auth/auth.go:130-142` `Authenticate` → `internal/store/sql/sessions.sql:8-13` `GetSessionByTokenHash`，SQL 内联过期条件 `sessions.expires_at > now()`，并 `sqlc.embed(users)` 一次取回身份。
- `[FACT]` 过期处理：查不到行 → `ErrSessionExpired`（`auth.go:133-135`）；中间件清 cookie 并回 `401 SESSION_EXPIRED`（`internal/httpapi/middleware.go:70-73`）。后台清理 `PurgeExpiredSessions`（`auth.go:151-153` → `sessions.sql:23-24`）。
- `[FACT]` 传输载体：HttpOnly + SameSite=Lax cookie，名字可配（`internal/httpapi/auth_handlers.go:102-112` `setSessionCookie`；`internal/config/config.go:248-249` `AICC_SESSION_TTL` 默认 12h、`AICC_SESSION_COOKIE` 默认 `aicc_session`）。
- `[FACT]` 身份载荷（进程内，不在 token 里）：`auth.Identity{UserID, Username, DisplayName, Role, Locale}`（`internal/auth/auth.go:47-53`）；**没有 AgentID 字段**。
- `[INFERENCE]` §1 说的"页面登录签发 Token 时执行 role → scopes 映射"落不到 token 里——token 是不透明随机串。映射只能在 `Authenticate` 返回身份时、或在中间件构造 AuthContext 时执行，用 `users.role` 现算。这是实现约束，不是决策冲突。

### 2. 鉴权中间件的组装位置与顺序；AuthContext 当前的类型定义与所有读取点

- `[FACT]` 组装位置：`internal/httpapi/server.go:158-433` `router()`。chi 层次：
  1. `r.Use(middleware.RequestID)` `:160`
  2. `r.Use(middleware.Recoverer)` `:163`（`middleware.RealIP` 刻意不用，`:161-162`）
  3. `r.Route("/api/v1", …)` `:170`
  4. `v1.Group(short)` + `middleware.Timeout(30s)` `:172-173`
  5. `short.Group(private)`：`private.Use(s.requireSession)` `:178` → `private.Use(s.auditTrail)` `:179`
  6. 组内再按角色分子组（见 §0-3）
  7. 两个 machine 组在 `private` **之外**：`s.requireSessionOrAPIKey` + `s.auditTrail`（`:395-399`、`:408-412`）
  8. `/events` 独立挂载在 `short` 之外（无超时）：`v1.With(s.requireSession).Get("/events", …)` `:423-425`
- `[FACT]` **AuthContext 当前不存在**。等价物是 `context.Context` 里的 `auth.Identity`，键为未导出 `identityKey`（`internal/httpapi/middleware.go:17-19`），读写 `identityFrom` / `contextWithIdentity`（`middleware.go:27-35`）。
- `[FACT]` `identityFrom` 全部读取点（生产代码，共 **17** 处）：

  | file:line | 用途 |
  |---|---|
  | `internal/httpapi/middleware.go:90` | `requireRole` 判角色 |
  | `internal/httpapi/agent_handlers.go:59` | `agentIDFor` 解析坐席 |
  | `internal/httpapi/auth_handlers.go:69` | `GetMe` |
  | `internal/httpapi/audit.go:65` | 审计 actor |
  | `internal/httpapi/call_handlers.go:189` | `ListWaitingCalls` 分角色 |
  | `internal/httpapi/call_handlers.go:297` | `HangupCall` |
  | `internal/httpapi/call_handlers.go:366` | `MonitorCall` |
  | `internal/httpapi/events_handler.go:35` | SSE 订阅范围 |
  | `internal/httpapi/ledger_handlers.go:172` | `ClaimCallback` |
  | `internal/httpapi/ledger_handlers.go:191` | `ReleaseCallback` |
  | `internal/httpapi/ledger_handlers.go:226` | `CompleteCallback` |
  | `internal/httpapi/outbound_handlers.go:112` | `createAgentCall` |
  | `internal/httpapi/outbound_handlers.go:215` | `hasRole` |
  | `internal/httpapi/recording_handlers.go:28` | `mayHearCall` |
  | `internal/httpapi/recording_handlers.go:135` | `CreateRecordingReview` reviewer |
  | `internal/httpapi/transcript_handlers.go:90` | `mayReadTranscript` |
  | `internal/httpapi/user_handlers.go:364` | `RevealExtensionPassword` 单独写审计 |
  | `internal/httpapi/contact_handlers.go:52,63` | 联系人 `updated_by`（Create 与 Update 两处写同一列） |

  （表内 18 行，`contact_handlers.go` 占 2 处 → 共 19 个调用点，17 个不同函数。）

### 3. 所有角色守卫的定义与每处调用

- `[FACT]` 定义只有一个：`requireRole(want auth.Role)`（`internal/httpapi/middleware.go:87-100`），用 `Role.AtLeast` 做等级比较（`internal/auth/auth.go:32-38`，`AGENT=1 < SUPERVISOR=2 < ADMIN=3`）。
- `[FACT]` 两个别名：`requireAgentRole = requireRole(auth.RoleAgent)`（`internal/httpapi/agent_handlers.go:370`）、`requireSupervisorRole = requireRole(auth.RoleSupervisor)`（`internal/httpapi/call_handlers.go:449`）。
- `[FACT]` 路由层调用点共 **14** 处，覆盖 **67** 条路由：

  | file:line | 守卫 | 覆盖的路由 | 条数 |
  |---|---|---|---|
  | `server.go:186` | AGENT | `GET /agent/presence`, `POST /agent/login`, `POST /agent/logout`, `POST /agent/ready`, `POST /agent/not-ready`, `GET /agent/wrap-up`, `POST /agent/wrap-up` | 7 |
  | `server.go:197` | SUPERVISOR | `GET /agents`, `POST /agents/{agentId}/force-logout` | 2 |
  | `server.go:204` | ADMIN | `POST /agents`, `PUT /agents/{agentId}`, `DELETE /agents/{agentId}`, `GET /users` | 4 |
  | `server.go:217` | AGENT | `GET /calls/mine`, `GET /calls/waiting`, `POST /calls/{callId}/{answer,hold,retrieve,mute,unmute,transfer,dtmf}`, `PATCH /calls/{callId}/user-data` | 10 |
  | `server.go:236` | SUPERVISOR | `GET /calls` | 1 |
  | `server.go:239` | SUPERVISOR | `POST /calls/{callId}/monitor` | 1 |
  | `server.go:254` | ADMIN | extensions ×5（含 `/password`）、queues ×5（create/update/delete/staff/unstaff）、`GET /audit-logs`、users ×3、dids ×4 | 18 |
  | `server.go:290` | SUPERVISOR | `GET /queues`, `GET /queues/{queueId}/agents` | 2 |
  | `server.go:301` | ADMIN | flows ×5 | 5 |
  | `server.go:315` | AGENT | `GET /cdrs/mine` | 1 |
  | `server.go:321` | AGENT | `GET /reports/me` | 1 |
  | `server.go:333` | SUPERVISOR | `GET /cdrs`, `GET /cdrs/{callId}`, `GET /calls/{callId}/reviews`, `GET /calls/{callId}/queue-events`, `POST /recordings/{recordingId}/reviews`, `GET /reports/{overview,queues,daily}` | 8 |
  | `server.go:371` | ADMIN | webhook-subscriptions ×6 | 6 |
  | `server.go:381` | ADMIN | `GET /system/health` | 1 |

  合计 ADMIN 34 / AGENT 19 / SUPERVISOR 14 = 67。
- `[FACT]` **剩余 18 条路由没有路由级角色守卫**（85 − 67）：`POST /auth/login`（无鉴权）、`POST /auth/logout`、`GET /auth/me`、`GET /calls/{callId}/transcript`、`GET /dispositions`、`GET /calls/{callId}/recordings`、`GET /recordings/{recordingId}/audio`、callbacks ×4、contacts ×4、`POST /calls`、`POST /calls/{callId}/hangup`、`GET /events`。
- `[FACT]` handler 内的角色判定（守卫删除后必须一并处理，共 **5** 处）：
  - `internal/httpapi/recording_handlers.go:33-51` `mayHearCall`（SUPERVISOR 直通，否则查 CDR 的 agent 名单）
  - `internal/httpapi/transcript_handlers.go:89-113` `mayReadTranscript`（SUPERVISOR 直通，否则必须在**在线**通话上）
  - `internal/httpapi/outbound_handlers.go:214-222` `hasRole`
  - `internal/httpapi/call_handlers.go:200` `ListWaitingCalls`（`id.Role.AtLeast(RoleSupervisor)` 走全量视图）
  - `internal/httpapi/call_handlers.go:297-312` `HangupCall`（有腿走坐席路径，否则要 SUPERVISOR）

### 4. SSE 端点的鉴权方式及前端建立连接的代码

- `[FACT]` 服务端：**cookie**。`v1.With(s.requireSession).Get("/events", …)`（`internal/httpapi/server.go:423`），身份从 context 取（`internal/httpapi/events_handler.go:35`）。不接受 API key，`requireSessionOrAPIKey` 未挂在这条路由上。
- `[FACT]` 无 query-param 传 secret 的现存路径；`?types=` 只能**收窄**已有范围（`events_handler.go:25-26,49`），`Last-Event-ID` 从 header 或 query 读且解析失败时降级为新流（`server.go:418-422`）。
- `[FACT]` 前端用**浏览器原生 `EventSource`**：`web/src/lib/events.ts:32` `new EventSource('/api/v1/events', { withCredentials: true })`。不是 fetch 流。
- `[INFERENCE]` `EventSource` 无法设置自定义 header——这正是 §1「页面路径保持不变，仅为 header 增加并行支持」的原因：页面继续用 cookie，API Key 客户端用 `Authorization: Bearer`（不是 `EventSource`，是自己的 HTTP 客户端）。
- `[FACT]` 全应用只有一个 EventSource（`web/src/routes/_app.tsx:48`；`web/src/lib/use-event-stream.ts:109` 说明面板复用同一条流）。

---

## 现有 API Key

### 5. 表结构、迁移文件、存储形态、是否有 scope

- **NOT FOUND — 不存在任何 API Key 表。** `[FACT]` 在 `internal/store/migrations/`（28 个文件）与 `internal/store/sql/`（9 个文件）中 grep `api_key|apikey|scope`，**命中 2 条，均不是**：`internal/store/migrations/00026_a_finished_call_can_be_told_to_somebody_else.sql:36`（注释提到 `AICC_API_KEY`）、`internal/store/sql/telephony.sql:136`（"Transaction-scoped" 一词）。
- `[FACT]` 现有 Key 的全部实现：**一个环境变量**。`internal/config/config.go:122-131` 声明字段，`:251` `APIKey: env("AICC_API_KEY", "")`。`.env.example:223` 注册，默认空。
- `[FACT]` 存储形态：**配置明文**，无 hash、无前缀、无状态、无 `last_used_at`、无过期。
- `[FACT]` scope 或等价概念：**不存在**。Key 被硬编码为 `RoleSupervisor`（`internal/httpapi/middleware.go:129-135` `machineIdentity`），能力边界完全由「挂了哪两条路由」决定。
- `[FACT]` 契约侧同样无 scope：`docs/openapi.json` 全局 `security` 为 `[{"cookieSession":[]}]`，三个 scheme（`cookieSession`/`csrfHeader`/`apiKeyHeader`，`docs/openapi.json:4515-4519`）**全部是 `type: apiKey`**，所有 `security` 数组元素的值都是空数组 `[]`。OAuth2/OIDC scheme 不存在。`[FACT]` 实测（见 §0-16 第 7 行）：在 `apiKey` 型 scheme 上逐 operation 声明 scope 名 Redocly 校验通过；**没有原生位置的只是「词表 + 每个 scope 的说明」**，那是 oauth2 流对象独有的 `scopes` 字段。

### 6. 当前所有校验点与调用方（含 webhook 是否引用）

- `[FACT]` 唯一校验点：`internal/httpapi/middleware.go:155-177` `requireSessionOrAPIKey`。
  - header 名：`X-AICC-Api-Key`（`middleware.go:114` `apiKeyHeader`）。
  - 空配置短路 + `subtle.ConstantTimeCompare`（`middleware.go:167-168`）。
  - 呈递了 key 就绝不回落到 session 路径（`middleware.go:158-174`）；失败回 `401 INVALID_CREDENTIALS`。
  - 通过后注入 `machineIdentity()`：`Username="api-key"`、`DisplayName="API key"`、`Role=SUPERVISOR`、`UserID=uuid.Nil`（`middleware.go:117-135`）。
- `[FACT]` 反向提示点：`middleware.go:59-63`，session-only 路由上带了 key 时回 `401 INVALID_CREDENTIALS` 而非 `SESSION_EXPIRED`。
- `[FACT]` 挂载点共 **2** 条路由：`POST /calls`（`server.go:394-400`）、`POST /calls/{callId}/hangup`（`server.go:407-413`）。
- `[FACT]` 契约中声明 `apiKeyHeader` 的 operation 也是 **2** 个：`docs/openapi.json:1008`（createCall）、`:1386`（hangupCall）。与路由一致。
- `[FACT]` `isMachine(id) = id.UserID == uuid.Nil`（`middleware.go:140`），下游分支 **3** 处：
  - `internal/httpapi/audit.go:66-74`：actor_id 留 NULL，改在 `detail["actor"]` 写 `"api-key"`
  - `internal/httpapi/outbound_handlers.go:177`：拒绝 machine 携带 `callbackId`
  - `internal/httpapi/outbound_handlers.go:227` `signedInAgent`：machine 永远没有坐席
- `[FACT]` **webhook 不引用它。** `internal/webhook/webhook.go:177` 明确写 outbound 投递用的是**客户自己的**凭证（`webhook_subscriptions.auth_token`，`Authorization: Bearer <token>`），方向相反；`docs/design/09-webhooks.md:291,296,370,454` 反复强调 `AICC_API_KEY` 必须不可达 webhook 配置端点，路由上也确实只在 session-only 组里（`server.go:369-379`）。
- `[FACT]` 其它调用方：`deploy/README.md:92,156`（部署清单）、`docs/design/04-api-sse.md:18`（设计说明）。代码中无其它消费点。
- `[INFERENCE]` §1「现有 Key 迁移」适用**明文分支**：`AICC_API_KEY` 是配置明文，无行可迁，**一律重新签发，无迁移路径**。
- `[INFERENCE]` 若要为它推导"当前实际可用能力的最小集合"（用于文档而非迁移）：依据 `server.go:394-413` 两条挂载点 + `call_handlers.go:290-312` 的注释（key 以 SUPERVISOR 身份可挂断**任何**通话），最小集合为 `calls:create` + `calls:hangup`。

---

## 路由与契约

### 7. 全部 HTTP 路由清单；是否在契约中声明；当前守卫

- `[FACT]` **路由 85 条，契约 operation 85 个，双向差集均为 0。**
  - 机械核对：从 `internal/httpapi/server.go` 抽 `.Get|.Post|.Put|.Patch|.Delete("/…")` 得 85 条（去重后）；`jq` 从 `docs/openapi.json` 抽 method×path 得 85 条；`comm -23` = 0 条，`comm -13` = 0 条。
  - `[FACT]` 每条的守卫见 §0-3 的表（67 条有路由级角色守卫）+ §0-3 末尾的 18 条无守卫清单。
- `[FACT]` 契约 operation 的 `security` 现状：**36 个继承全局** `[{"cookieSession":[]}]`（全为 GET），49 个显式声明。显式声明中：
  - `POST /auth/login`：`security: []`（匿名）——1 个
  - `POST /calls`、`POST /calls/{callId}/hangup`：`[{cookieSession,csrfHeader},{apiKeyHeader}]`——2 个
  - 其余 46 个：`[{cookieSession,csrfHeader}]`
  - `[FACT]` **所有 scheme 引用的值都是 `[]`——契约中 scope 的基数为 0。**
- `[FACT]` 挂载纪律：唯一不走生成 wrapper 的是 `StreamEvents`，由 `internal/httpapi/routes_test.go:25` `mountedElsewhere` 白名单登记，`TestEveryContractOperationIsRouted`（`routes_test.go:36-73`）用 `go/parser` 解析 `server.go` + 反射 `api.ServerInterface` 保证「契约里的每个 operation 都被路由」。
- `[FACT]` `internal/httpapi/api_server.go:57` 行内有编译期接口断言（`s.apiWrapper()` 在 `server.go:168`）。
- `[FACT]` **「85 条」指的是 `/api/v1` 这棵 chi 路由树。同一个二进制还serve 另外 4 个 HTTP 入口，都刻意在契约之外**，报「全部 HTTP 路由」时须一并说明：
  - `GET /metrics`、`GET /healthz`、`GET /readyz` —— `internal/httpapi/server.go:437-453` `MetricsHandler`，挂在**独立监听地址**（`AICC_METRICS_ADDR`，默认 `127.0.0.1:9090`），**无鉴权**。契约自己声明了这个例外：`docs/openapi.json:7` 的 description 末段。
  - SPA fallback：`r.NotFound(s.spa.ServeHTTP)`（`server.go:428-430`），静态资源，非 API。
  - `[INFERENCE]` scope 模型不覆盖这 4 个：前 3 个是运维监听面，本就不该有应用身份；第 4 个不是端点。§1「路由表中任一路由在契约中无 scope 声明即失败」的检查须把它们排除，且**排除方式要显式登记**（照 `routes_test.go:17-25` `mountedElsewhere` 的白名单+理由写法）。
- `[FACT]` **oasdiff 实测**：`go tool oasdiff breaking --fail-on ERR docs/openapi.json <加了 scope 的副本>` → `No breaking changes to report`，exit 0。给 operation 补显式 `security` + scopes **不触发 §2.5 的 breaking 门禁**。

### 8. Web 前端引用但契约未声明的路由

- `[FACT]` **空集，且基数非零：Web 引用 56 条不同路径，契约声明 66 条不同路径，`comm -23` 差集 0 条。**
  - 抽取方式：`web/src/lib/*.ts` 中 `request<…>('…')` 的路径字面量，`${…}` 归一为 `{p}`，去 query。
  - 另有 2 处非 `request()` 的 URL 字面量，也都在契约里：`web/src/lib/events.ts:32`（`/api/v1/events`）、`web/src/lib/ledger.ts:131`（`/api/v1/recordings/${recordingId}/audio`）。
  - 统一 base：`web/src/lib/api.ts:32` `const BASE = '/api/v1'`。
- `[INFERENCE]` §2.5「若现有 Web 引用了未声明路由，先补声明再实现」**无事可做**，与「UI is optional. API is the product.」的定位一致——Web 已经严格是契约的消费者。

### 9. 错误码枚举在契约与 Go 中的位置；新增错误码的既有流程

- `[FACT]` 契约：`docs/openapi.json` `components.schemas.ErrorCode.enum`，**22 个值**。
- `[FACT]` Go：`internal/httpapi/errors.go:20-55`，`type ErrorCode string` + 22 个 `Code*` 常量。**手写，不是生成的**——`errors.go` 与生成的 `internal/api/api.gen.go` 是两套。
- `[FACT]` TS：`web/src/generated/api.ts:1424` 联合类型，22 个，由 `openapi-typescript` 生成。
- `[FACT]` 三处基数一致：22 / 22 / 22。
- `[FACT]` 信封：`{"error": {code, message, params}}`，Go 侧 `internal/httpapi/errors.go:57-66` + `writeError`（`:81-83`）；前端 `web/src/lib/api.ts:20-30` `ApiError`。
- `[FACT]` i18n：`web/src/locales/{en,zh}/translation.json` 的 `errors` 键，**只有 20 个**。缺 4 个：`EXTENSION_POOL_EXHAUSTED`、`LAST_ADMIN`、`OPERATION_NOT_ALLOWED_FOR_CALL_TYPE`、`USER_DATA_TOO_LARGE`。**没有任何检查强制这三/四处对齐**（既有缺口，非本任务引入）。
- `[INFERENCE]` 新增错误码的既有流程（由上述四处推导，无成文规范）：改 `docs/openapi.json` 的 enum → `make api-generate`（TS 自动更新）→ 手工在 `internal/httpapi/errors.go` 加常量 → 手工在两个 `translation.json` 加 `errors.<CODE>`。第 3、4 步无 CI 兜底。

### 10. CI 中的生成/检查位置；新增"路由必须声明 scope"检查应放哪

- `[FACT]` `.github/workflows/api.yml`：`make api-check`（`:34`）+ PR 上 `make api-breaking BASE=origin/$BASE_REF`（`:36-41`）。
- `[FACT]` `Makefile:46-47` `api-lint`（Redocly）、`:50-51` `api-generate`（`scripts/api-generate.sh`，唯一入口）、`:54-58` `api-check`（lint + 重新生成 + `git diff --exit-code -- internal/api web/src/generated` + untracked 检查）、`:65-71` `api-breaking`（`go tool oasdiff breaking --fail-on ERR`）。
- `[FACT]` `.github/workflows/ci.yml`：`go` job 带 `postgres:18` service 和 `AICC_TEST_DATABASE_URL`（`:32-50`），跑 `go build` / `go vet` / `gofmt -l cmd internal web` / `go test -race ./...`（`:79`）/ 零分配基准（`:83-89`）；`web` job 跑 `oxlint` / `npm run test` / `npm run build`；`image` job 构镜像。
- `[FACT]` `scripts/` 下只有 `api-generate.sh` 一个脚本——**仓库没有"自定义 CI 检查脚本"的既有目录习惯**。
- `[INFERENCE]` **「路由必须声明 scope」的检查应写成 `internal/httpapi` 的 Go 测试**，紧邻 `routes_test.go`，理由：
  1. `routes_test.go:36-73` 已经建立了「解析 `server.go` + 反射 `api.ServerInterface`」这一模式，同类断言复用同一手法；
  2. 它随 `go test -race ./...` 在 `ci.yml` 自动执行，不需要动 workflow；
  3. Redocly 自定义规则（`redocly.yaml` / `.redocly.lint-ignore.yaml`）只能看契约，看不到 Go 路由表，无法满足「路由表中任一路由在契约中无 scope 声明即失败」的双侧断言。
- `[FACT]` 工具版本锁定处：`go.mod` `tool` 指令（oapi-codegen、oasdiff）、`web/package.json` 精确 devDependencies（openapi-typescript、@redocly/cli）、`oapi-codegen.yaml`（`name-normalizer` + `additional-initialisms`）。

---

## agent 作用域

### 11. 所有按 `agent_id` 判定归属的端点及判定代码

- `[FACT]` 归属解析只有一个入口：`internal/httpapi/agent_handlers.go:57-70` `agentIDFor`——从 `identityFrom` 取 `UserID`，再 `s.agentDir.AgentIDForUser`。失败一律 `403 FORBIDDEN "this account is not an agent"`。
- `[FACT]` `agentIDFor` 调用点共 **13** 处：

  | file:line | 端点 | 判定方式 |
  |---|---|---|
  | `agent_handlers.go:73,94,103,112,138,178,227` | `/agent/*` 全部 7 条 | 直接以自己的 agentID 驱动 presence |
  | `call_handlers.go:66` | `/calls/mine` | `CallsForAgent(agentID)` |
  | `call_handlers.go:168` | `/calls/{callId}/*` 控制族 | 自己的腿 |
  | `call_handlers.go:208` | `/calls/waiting` | `QueuesForAgent(agentID)` 过滤 |
  | `call_handlers.go:412` | `/calls/{callId}/monitor` | 班长自己的分机 |
  | `ledger_handlers.go:52` | `GET /cdrs/mine` | `store.CDRFilter{AgentID:&agentID}` |
  | `ledger_handlers.go:95` | `GET /reports/me` | 自己的当日聚合 |
  | `transcript_handlers.go:98` | `GET /calls/{callId}/transcript` | `mayReadTranscript`，须在**在线**通话上 |
  | `recording_handlers.go:36` | `GET /recordings/{id}/audio`、`GET /calls/{callId}/recordings` | `mayHearCall` → `agentWasOnCall(cdr, agentID)` |

- `[FACT]` 归属判定的核心谓词：`internal/httpapi/recording_handlers.go:55-60` `agentWasOnCall`——`cdr.PrimaryAgentID == agentID` 或 `slices.Contains(cdr.AgentIDs, agentID)`。
- `[FACT]` 另外 **2** 处不经 `agentIDFor` 直接调 `AgentIDForUser`：`internal/httpapi/events_handler.go:57`（SSE 订阅范围，失败静默降级）、`internal/httpapi/outbound_handlers.go:230` `signedInAgent`（machine 直接返回 false）。
- `[FACT]` **按 `user_id` 而非 `agent_id` 归属的端点（§1 的 Subject/AgentID 分离在这里最要紧）共 6 处**：
  - callbacks 三条：`ledger_handlers.go:172,191,226` 用 `identity.UserID` 做 claim/release/complete 的归属键
  - `recording_handlers.go:135` `CreateRecordingReview` 的 `ReviewerID = identity.UserID`
  - `contact_handlers.go:52,63` 联系人 `updated_by` = `identity.UserID`（Create 与 Update 写同一列；`contacts` **没有** `created_by`）
- `[FACT]` 这 4 个归属列的形态（决定待决问题 2 的可选项）：
  - `callbacks.handled_by uuid`（可空），`internal/store/migrations/00005_call_ledger.sql:106`
  - `quality_reviews.reviewer_id uuid **NOT NULL**`，`00005_call_ledger.sql:86`
  - `contacts.updated_by uuid`（可空），`00011_contacts.sql:29`
  - **四列都没有指向 `users` 的外键**——全库 `REFERENCES users` 只有 2 处：`sessions.user_id`（`00001:32`）与 `agents.user_id`（`00002:40`）。
  - `[INFERENCE]` 因此把 Key 的 id 写进这些列**不会被数据库拒绝**，只会静默地让「user id 空间」混入 key id。风险是沉默的，不是报错的——这正是待决问题 2 必须显式裁定而不能靠默认行为的原因。`quality_reviews.reviewer_id` 是 NOT NULL，连"留空"这条退路都没有。

### 12. AgentID 在页面 Token 与 AuthContext 之间的传递路径

- `[FACT]` **不传递。** 页面 Token 不透明（§0-1），`auth.Identity` 无 AgentID 字段（`internal/auth/auth.go:47-53`），每次需要时**现查数据库**。
- `[FACT]` 完整链路：cookie → `sessions.token_hash` → `users.id`（`internal/store/sql/sessions.sql:8-13`）→ `agentDirectory.AgentIDForUser`（`cmd/aicc/main.go:471-479`）→ `GetAgentByUserID`（`internal/store/sql/agents.sql:11-12`，`SELECT * FROM agents WHERE user_id = $1`）→ `agents.id`。
- `[FACT]` 绑定关系存在 `agents.user_id`，唯一 + 级联：`internal/store/migrations/00002_telephony.sql:38` `CONSTRAINT uq_agents_user_id UNIQUE (user_id)`，`:40` `fk_agents_users … ON DELETE CASCADE`。
- `[FACT]` 生产实现只有一个：`cmd/aicc/main.go:471-490` `agentDirectory`（另有 6 个测试假实现）。接口定义 `internal/httpapi/agent_handlers.go:22-28` `AgentDirectory`（`AgentIDForUser` + `QueuesForAgent`）。
- `[INFERENCE]` 因此 §1「页面登录的坐席：`Subject=用户`，`AgentID=其绑定坐席`」是**每请求一次查询**，不是 token 载荷。要么在 `requireSession` 里查一次填进 AuthContext（每请求 +1 次 DB 往返，但省掉当前 13 个调用点各自的重复查询），要么保持惰性。前者更符合「业务代码只读 AuthContext」。

---

## 审计

### 13. 审计表结构、写入点、actor 字段形态；是否有名称快照

- `[FACT]` 表：`audit_logs(id bigserial, occurred_at timestamptz default now(), actor_id uuid, action varchar(64), target_kind text, target_id text, detail jsonb, ip inet)`——`internal/store/migrations/00005_call_ledger.sql:127-138`。
  - `actor_id` **可空、且没有外键**（`:130`，对比同文件其它表）。
  - **没有** `subject_kind` / `subject_name` / `agent_id` 列。
- `[FACT]` 写入：`internal/store/sql/ledger.sql:149-151` `InsertAuditLog`，参数即上述 6 列。
- `[FACT]` 写入点 **2** 处：
  - `internal/httpapi/audit.go:34-91` `auditTrail` 中间件——只审计**成功的**变更请求（`isMutating` + `recorder.status < 300`，`:36,60`），`action = METHOD + chi 路由模板`（`:81`），`target` 取路径中最后一个 `{xxxId}`（`:187-200`）。
  - `internal/httpapi/user_handlers.go:362-371`——`RevealExtensionPassword` 是 GET，中间件不覆盖，故手写一条。
- `[FACT]` **没有名称快照，只存 id。** 名字在**读时** LEFT JOIN 解析：`internal/store/sql/ledger.sql:341-344` `COALESCE(u.username, '')::text AS actor_username … LEFT JOIN users u ON u.id = a.actor_id`。
- `[FACT]` machine 的当前形态：`audit.go:66-74`——`actor_id` 留 NULL，改在 `detail["actor"] = "api-key"`（字符串）。这是唯一一个「主体不是 user」的既有表示法，且它**不在列里**。
- `[FACT]` 脱敏：`redactSecrets` 按字段名子串匹配 `password|secret|token|apikey|credential`（`audit.go:156-164`），任意深度替换为 `"[redacted]"`；`/api/v1/auth/` 前缀的请求体整体不入库（`audit.go:46`）。**`apikey` 已在词表内**——§4「不在审计中输出 Key 明文」已被现有机制覆盖，前提是字段名含 `apiKey`。
- `[INFERENCE]` §1 的落地方式：**新增列**（`subject_kind` / `subject_id` / `subject_name` / `agent_id`），不改 `actor_id`。理由：`actor_id` 无 FK 且已有历史行（含 NULL 的 machine 行），把它改名/改语义会让历史行含义漂移；而 `subject_name` 快照与现有的读时 JOIN 是两种事实，要并存一段时间。迁移须处理既有行（CLAUDE.md 的「有行的库」规则）：历史行 `subject_kind='USER'`, `subject_id=actor_id`；`actor_id IS NULL` 的行 `subject_kind='API_KEY'`，`subject_name` 从 `detail->>'actor'` 取。

---

## UI 与数据层惯例

### 14. Admin 区现有管理页的路由文件、表单/列表组件、数据获取模式

- `[FACT]` 路由文件（TanStack Router 文件约定，`web/src/routes/`）：`_app.admin.extensions.tsx`（分机）、`_app.admin.users.tsx`（坐席/账号）、`_app.admin.numbers.tsx`（号码/DID）、`_app.admin.routing.tsx`（队列）、`_app.admin.bots.index.tsx` + `.$flowId.tsx`、`_app.admin.audit.tsx`、`_app.admin.webhooks.tsx`、`_app.admin.cdr.index.tsx` + `.$callId.tsx`、`_app.admin.reports.tsx`、`_app.admin.index.tsx`。
- `[FACT]` **最贴近 API Keys 页的模板是 `_app.admin.extensions.tsx`**——它有列表 + 创建/编辑对话框 + 删除 + 「只显示一次的密钥」语义（`:33-36`：新铸密码只存 `useRef`，state 只记「有没有」，不进表单值、不进 devtools 可见的 draft）。这正是 §1「Secret 仅在创建响应返回一次」要复制的模式。
- `[FACT]` 页面骨架（`_app.admin.extensions.tsx:1-30`）：
  - `createFileRoute('/_app/admin/extensions')({ beforeLoad: ({context}) => requireRole(context.user, 'ADMIN'), component })`（`:21-24`）——守卫在 `web/src/lib/guards.ts`
  - `<PageHeader title description actions>`（`@/components/page-header`）
  - `<DataTable><THead><Th/></THead><TBody><Tr><Td/></Tr></TBody></DataTable>`（`@/components/table`）
  - 表单：`RecordDialog` + `Field` / `Input` / `Select` + `useRecordForm`（`@/components/record-dialog`）
  - 按钮 `@/components/ui/button`；`web/src/components/ui/` 只有 `badge/button/input/label/tabs`
  - 错误渲染：`describeError(error, t)` / `fieldErrorText`（`@/lib/errors`）
  - 图标 `lucide-react`；对话框/浮层 `radix-ui`
  - 文案全部 `t('…')`（react-i18next），`en` + `zh` 两份
- `[FACT]` 数据获取（`web/src/lib/catalog.ts`）：
  - 类型只从 `components['schemas'][…]` 取（`:3-25`），三件套 `X` / `XWrite` / `XDraft = Partial<X & XWrite>`
  - 常量 query key：`export const EXTENSIONS_KEY = ['catalog','extensions'] as const`（`:30-32`）
  - 纯函数 API 对象 `catalogApi`（`:41-56`），只调 `request()`
  - `useXxx()` = `useQuery({queryKey, queryFn})`（`:81-89`）
  - `useCatalogMutations()` 统一返回一组 `useMutation`，成功后 `queryClient.invalidateQueries({queryKey})`（`:105-148`）
  - 枚举取生成类型再列成常量数组：`export const STRATEGIES: Strategy[] = [...]`（`:34-38`）——§1「scopes 从生成的常量枚举取，不手写」在这里有现成先例
- `[FACT]` 侧边栏与面包屑由 `web/src/lib/nav.ts` 的配置驱动（`:70-97`，每项 `{to, labelKey, icon, roles, isReady}`），有 `nav.test.ts` 断言「每个页面都能在导航里解析」。新增 API Keys 页必须加一项，`roles: ['ADMIN']`，放 `nav.system` 组（与 audit / webhooks 同组更贴切）。
- `[FACT]` 设计系统是硬约束：`web/CLAUDE.md`（13px 基准、单一强调色 #4F46E5、用边框不用阴影、6px 圆角、所有数字 `tabular-nums`）。

### 15. sqlc 查询/迁移的命名与放置惯例；DB 层枚举表示方式

- `[FACT]` 迁移：`internal/store/migrations/NNNNN_snake_case_sentence.sql`，goose 格式，5 位序号，当前最大 `00028_contact_numbers_normalized.sql`——**下一个是 `00029_`**。文件名是一句人话（`00024_the_directory_knows_who_the_agent_is.sql`）。每个文件首行 `-- SPDX-License-Identifier: Apache-2.0`，随后是解释「为什么」的注释块，再 `-- +goose Up` / `-- +goose Down`（`00028` 是完整范例）。
- `[FACT]` 查询：`internal/store/sql/<domain>.sql`，9 个域文件（`agents/contacts/flows/ledger/seq/sessions/telephony/users/webhooks`）。每条查询 `-- name: XxxYyy :one|:many|:exec|:execrows`。`[INFERENCE]` API Key 属于新域，应新建 `internal/store/sql/api_keys.sql`（与 `sessions.sql` 平级——同样是凭证）。
- `[FACT]` sqlc 配置 `sqlc.yaml`：`schema: internal/store/migrations`（schema 就是迁移，无单独 schema.sql），`queries: internal/store/sql`，输出 `internal/store/queries` 包名 `queries`，`sql_package: pgx/v5`，`emit_json_tags` + `json_tags_case_style: camel` + `emit_pointers_for_null_types` + `emit_empty_slices`，`rename: {ip: IP, url: URL, sip_url: SIPURL}`，`uuid` 覆盖为 `github.com/google/uuid.UUID`（可空则 `*uuid.UUID`）。
- `[FACT]` **DB 层枚举一律是 `varchar(n) NOT NULL CHECK (col IN ('A','B',…))`，`CREATE TYPE` 的命中数为 0**（全 28 个迁移文件中 grep `CREATE TYPE` = 0 条）。样例：`00001_foundation.sql:15` `role varchar(16) NOT NULL CHECK (role IN ('AGENT','SUPERVISOR','ADMIN'))`、`:16` `status … CHECK (status IN ('ACTIVE','SUSPENDED'))`、`00006_callback_claim.sql:8`（收窄 CHECK 的范例）。
  - `[INFERENCE]` `api_keys.status` 应写成 `varchar(16) NOT NULL DEFAULT 'ENABLED' CHECK (status IN ('ENABLED','DISABLED','REVOKED'))`。
- `[FACT]` 值一律 SCREAMING_SNAKE，跨 JSON/TS/DB 逐字节一致（`docs/design/07-naming.md`；`00005_call_ledger.sql:14,33,36` 等）。
- `[FACT]` 迁移的验收门：`internal/store/migrate_test.go` 的 `TestMigrations`——从零全量应用、全量回滚再应用、以及**对已有数据的库**迁移；`ci.yml:32-50` 提供 `postgres:18` service 使其真正执行。收窄 CHECK 的迁移必须先改写既有行。

---

## 16. 决策校验表

对 §1 每条决策给 `支持` / `冲突` / `无依据`，附事实编号。**冲突与无依据只记录，不改决策。**

| # | §1 决策 | 判定 | 依据 |
|---|---|---|---|
| 1 | AuthContext 字段 `Subject` / `AgentID` / `Scopes` | **支持** | §0-2：AuthContext 尚不存在，`auth.Identity` 是全新替换的干净起点，19 个读取点全部集中在 `internal/httpapi` |
| 2 | Go `AgentID` / JSON `agentId` / DB `agent_id` | **支持** | §0-12：`agents.user_id` 已按此命名；`sqlc.yaml` 的 `json_tags_case_style: camel` 自动产出 `agentId` |
| 3 | `Subject` 与 `AgentID` 永不合并 | **支持** | §0-11/12：今天已是两个概念（`identity.UserID` vs `agentIDFor`），且 §0-11 末尾的 6 个 user 归属端点证明合并会立刻出错 |
| 4 | 页面坐席 `Subject=用户, AgentID=其绑定坐席` | **支持**（有实现约束） | §0-12：绑定在 `agents.user_id`，唯一索引存在；但 AgentID 只能每请求查库，不能塞进不透明 token（§0-1） |
| 5 | API Key `Subject=Key, AgentID=header 指定` | **支持** | §0-5/6：现有 Key 完全没有主体表示（只有 `machineIdentity()` 里硬编码的假身份），新建即可，无历史包袱 |
| 6 | 禁止按凭证类型分支（`isAPIKey` 等不得进 AuthContext） | **冲突（需删除）** | §0-6：`isMachine`（`middleware.go:140`）今天就是这种分支，3 个下游：`audit.go:66`、`outbound_handlers.go:177`、`outbound_handlers.go:227`。三处都要改写为「读 `Subject.Kind` 决定审计怎么写」/「读 `AgentID` 是否为空」的语义，而不是问凭证来源 |
| 7 | scope 词表在 `securitySchemes` 中定义，是唯一来源 | **部分冲突（声明可行，词表无原生位置）** | `[FACT]` 实测：把 `security: [{"cookieSession":["cdr:read"]}]` 加到 `GET /dispositions` 上，`npx @redocly/cli lint` 通过，warning 数与基线同为 9（无新增）。OpenAPI 3.1 允许非 oauth2 型 scheme 的 security 值携带名称数组，**逐 operation 声明 scope 完全可行**。但 `apiKey` 型 scheme **没有 `scopes` 字段**可承载「词表 + 每个 scope 的说明」——只有 `oauth2` 流对象有。词表需要一个位置：(a) `x-scopes` 扩展字段；(b) 增设一个只为承载词表的 `oauth2` scheme；(c) 写进 scheme 的 `description`（不可机读，不推荐）。**这是 §2.1 落地前要定的形状问题，但不阻塞 §2.2** |
| 8 | 每个 operation 必须声明 `security` + scopes | **冲突（现状不满足）** | §0-7：36 个 operation 靠全局继承，无显式 `security`；85 个 operation 的 scope 基数为 0。全部要补 |
| 9 | CI 增加「路由无 scope 声明即失败」的检查 | **支持** | §0-10：`routes_test.go` 已建立「解析 server.go + 反射 ServerInterface」的双侧断言模式，`ci.yml:79` 自动跑。第 7 行已证明 operation 侧的 scope 声明合法，故此检查不被词表位置阻塞 |
| 10 | 页面签发时 `role → scopes` 映射，映射表在服务端配置 | **支持** | §0-1/3：角色只有 3 个且已有 `AtLeast` 等级（`auth.go:32-38`），映射表可直接由 `Role` 键入 |
| 11 | §0-3 列出的全部角色守卫删除，替换为 scope 守卫 | **支持（范围比想象大）** | §0-3：路由级 14 处 / 67 条路由，**外加 handler 内 5 处角色判定**（`mayHearCall`、`mayReadTranscript`、`hasRole`、`ListWaitingCalls`、`HangupCall`）。后者不是「守卫」，但同样在读 `Role`，不一起处理就还是角色模型 |
| 12 | 不允许「页面 Token 跳过 scope 检查」的特例 | **支持** | §0-2：没有既有特例可继承 |
| 13 | `Authorization: Bearer <key>` + 可选 `AICC-Agent-ID` | **支持（是替换，非兼容）** | §0-6：现状是 `X-AICC-Api-Key`。`Authorization` 头目前**未被本平台入站使用**（`internal/webhook/webhook.go:177` 是出站方向），无冲突。旧 header 与 `AICC_API_KEY` 一并废弃，符合 §1「明文 → 重新签发，无迁移路径」 |
| 14 | 无 header → `AgentID=nil`；agent 端点回 `AGENT_REQUIRED` | **支持** | §0-11：13 个 `agentIDFor` 调用点已有统一的「解析不到就拒绝」出口（`agent_handlers.go:63-67`），换错误码即可 |
| 15 | 页面 Token 带 `AICC-Agent-ID` → `AGENT_IMPERSONATION_NOT_ALLOWED` | **支持** | §0-4：`EventSource` 不能带自定义 header，页面天然不会误发；无既有代码读这个 header |
| 16 | Key 的允许代理 Agents（列表或 `*`）；越界 → `AGENT_NOT_ALLOWED` | **无依据（全新）** | §0-5：无表、无 scope、无任何代理概念。纯新增，无冲突也无先例 |
| 17 | 缺 scope → `INSUFFICIENT_SCOPE` | **支持** | §0-9：错误码新增流程清晰（契约 enum → `errors.go` 常量 → 两份 translation.json）。注意第 3、4 步无 CI 兜底，且已有 4 个码漏译 |
| 18 | 四个新错误码进入契约 enum | **支持** | §0-9：22 → 26；三处基数今天一致 |
| 19 | SSE：API Key 走 header；页面路径不动 | **支持（走 cookie 分支）** | §0-4：现状是 cookie（`server.go:423`），故适用 §1「页面路径保持不变，仅为 header 增加并行支持」 |
| 20 | Key 格式：固定前缀 + 展示用短前缀 + 高熵随机段 | **支持** | §0-1：`newToken()`（32 字节 rand + base64url）是现成的高熵生成器，加前缀即可 |
| 21 | 只存 SHA-256；按前缀取行 + 常量时间比较；不用 bcrypt/argon2 | **支持** | §0-1：`hashToken` 已是 SHA-256；§0-6：`subtle.ConstantTimeCompare` 已在用（`middleware.go:168`）。argon2 只用于用户口令（`internal/auth/password.go`），语义正确地区隔 |
| 22 | 状态 `ENABLED/DISABLED/REVOKED`，REVOKED 终态，v1 不硬删 | **支持** | §0-15：`varchar + CHECK IN` 是本仓库唯一的枚举写法，且 `00006` 提供了收窄 CHECK 的范例 |
| 23 | `last_used_at` 内存节流，每 Key 每分钟最多一次 DB 写 | **无依据（全新）** | 仓库内无同类节流器。`[INFERENCE]` 与 `TouchUserLogin`（`auth.go:122`，每次登录写一次，无节流）不同量级，需自建 |
| 24 | 现有 Key 迁移：明文 → 一律重新签发，无迁移路径 | **支持** | §0-5：`AICC_API_KEY` 是配置明文，DB 里零行可迁。其「当前实际可用能力的最小集合」= `calls:create` + `calls:hangup`（依据 §0-6 的两个挂载点） |
| 25 | 审计记 `subject_kind + subject_id + subject_name 快照 + agent_id` | **冲突（表要改）** | §0-13：`audit_logs` 只有 `actor_id uuid`（可空、无 FK），名称是读时 JOIN，无快照；machine 主体今天藏在 `detail->>'actor'`。需新增 4 列 + 迁移既有行 |
| 26 | 审计通过 `subject_id` 关联，不依赖名称唯一 | **支持** | §0-13：现状已是 id 关联（`ledger.sql:344` LEFT JOIN），只是缺快照 |
| 27 | 新增 API Keys Admin 页（列表/创建/启用禁用/吊销） | **支持** | §0-14：`_app.admin.extensions.tsx` 是完整模板，含「新铸密钥只显示一次」的既有实现（`:33-36`） |
| 28 | 创建表单 scopes 从生成常量枚举取，不手写 | **冲突（生成器不产出）** | `[FACT]` oapi-codegen 与 openapi-typescript **都不为 `security`/scopes 产出任何东西**：`grep -c -i securit internal/api/api.gen.go` = 0，`web/src/generated/api.ts` 中 `security`/`scopes` 命中 0。§1「Go / TS 常量由代码生成」需要在 `scripts/api-generate.sh` 里**新增一个自写的生成步骤**（读 `docs/openapi.json` 的词表 → 写 Go 常量 + TS 联合类型）。UI 侧的消费模式有先例（`web/src/lib/catalog.ts:34-38` 的 `STRATEGIES: Strategy[]`），缺的是产出端 |
| 29 | Secret 仅创建时返回一次；列表/详情不返回 secret 或 hash | **支持** | §0-14：与 `GET /extensions/{id}/password` 的既有纪律一致（列表不带、单独端点、读取入审计，`user_handlers.go:362-371`） |
| 30 | 页面仅调用契约中声明的端点 | **支持（已成立）** | §0-8：Web 引用 56 条路径，契约外 0 条 |
| 31 | 不新增 `tenant_id`（§4） | **支持** | CLAUDE.md 全局约束；本次不涉及 |
| 32 | 不加限流/IP 白名单/Key 过期（§4） | **支持** | §0-9：`RATE_LIMITED` 错误码虽已在 enum 中，但代码里无限流实现，不构成反例 |

---

## §0 待决问题 — 已裁定（owner，2026-08-31）

1. **scope 词表承载形状 → `x-scopes` 扩展字段（选项 a）。** 不引入假的 oauth2 scheme。词表与 `securitySchemes` 同层，两种 scheme 共用。`[ASSUMPTION]` 具体放哪一层（根 / `components` / scheme 内）待 §2 开工时用 `make api-lint` 实测确定——已验证的只有 operation 级 `security` 带 scope 名合法。
1b. **生成产物落进现有 generated 目录**：`internal/api/scopes.gen.go` + `web/src/generated/scopes.ts`。`Makefile:56-58` 的 diff 路径列表**无需改动**，`DO NOT EDIT` 纪律与 CI 门禁自动继承。生成器由 `scripts/api-generate.sh` 调用（唯一入口）。
2. **三个归属列分别裁定**：
   - `callbacks.handled_by` → **跟随 `AgentID`**：Key 代理坐席 X 时写 X 绑定的 `users.id`。列的含义不变（永远是真人），"是 Key 干的"由审计行独立记录。
   - `contacts.updated_by` → **同上**；Key 未指定坐席时写 `NULL`（列可空）。它只是溯源，不是权限依据。
   - `quality_reviews.reviewer_id` → **对应 scope 不授予 Key**。打分是一个人的判断，不是可代理的动作；该列 NOT NULL，也没有留空的退路。见 §17 P8：这是本次唯一的能力不对称，须在契约里写明理由。
3. **审计新增 4 列（选项 A）**，`actor_id` 保持不动。不改造 `actor_id` 的语义——`GET /audit-logs` 已在响应里暴露 `actorId` / `actorUsername`，改含义而不改字段名/类型是 **oasdiff 抓不到的破坏**。⑤ 那一提交因此是纯增量。
4. **补齐 4 个漏译**（单独小提交），并在 ⑦ 加一条对齐断言：契约 `ErrorCode` enum ≡ `internal/httpapi/errors.go` 常量 ≡ 两份 `translation.json` 的 `errors` 键。`errors.go` 是手写且无 CI 兜底，不加这条断言，新增的 4 个码明天照样会漏。
5. **P5 解除**（owner，2026-08-31）：webhook-subscriptions 对 API Key 的封锁**取消**。持有 `webhooks:write` scope 的 Key 可以配置投递目标。原禁令的前提是"只有一把万能钥匙"，per-key scope 让它消失——只持 `calls:create` 的 Key 泄漏本就碰不到 webhook。**须同步改写三处成文依据**，不能只改代码：`internal/httpapi/server.go:363-368` 的注释、`docs/design/09-webhooks.md:370,454`、`docs/openapi.json:4247` 的 operation description。
6. **P1 / P2 / P3 纳入正式条目**（owner，2026-08-31）：不再是复核建议，是 §2 / §4 的必做项。见 `docs/auth/TASKS.md`。
7. **O1–O4 减法全部采纳**（owner，2026-08-31，依据 `docs/api-first-audit.md` §二）。**这四条修改的是已批准的 §1，以此裁定为准**：
   - **O1**：Key 查找改为 `WHERE key_hash = $1` 一次索引命中，照抄 `internal/store/sql/sessions.sql:8-13`。短前缀列**保留但只用于展示**，不建索引。**取消"按前缀取行 + 常量时间比较"**——按 hash 直查时没有东西需要常量时间比较。
   - **O2**：**取消 `last_used_at` 的内存节流**，每次直接写。无测量支撑的优化，且与 `CLAUDE.md` 那条"没跑基准就不许有性能主张"同源。
   - **O3**：**v1 取消每把 Key 的"允许代理 Agents"列表**。持 `agent:act` scope 即可代理任意坐席。`AGENT_NOT_ALLOWED` **不进错误码枚举**——新增码由 4 个减为 **3** 个（`AGENT_REQUIRED` / `AGENT_IMPERSONATION_NOT_ALLOWED` / `INSUFFICIENT_SCOPE`）。以后要加是纯加法（一个可空列，NULL = 全部）。
   - **O4**：状态由三态减为两态 **`ENABLED / REVOKED`**。`DISABLED` 是听起来有用、然后没人用的状态；以后要加是一次 CHECK 放宽。
   - **O5**：scope 词表**取粗粒度 16 个**，见下方裁定 8。
8. **scope 词表定稿（owner，2026-08-31）**——契约可见、改动是破坏性变更，故在 §2 开工前定死：

   ```
   calls:read:own      calls:read:all
   calls:control       calls:create
   calls:monitor                          # 监听/耳语/强插，强能力单列
   agent:read          agent:act
   contacts:read       contacts:write     # 客户数据，不是平台配置（修正 1）
   history:read:own    history:read:all   # CDR + 录音 + 转写 + /reports/me 合一
   reports:read                           # 队列/总览/日报
   quality:review                         # 写不授予 Key（§17 P8）
   config:read         config:write       # 分机/队列/号码/流程/webhook
   users:write                            # 账号、角色、重置密码
   keys:manage         audit:read
   ```

   共 **18** 个。

   **修正 1（owner，2026-08-31，映射 85 个 operation 时发现）**：初稿 16 个**没有联系人的位置**。联系人不是平台配置，是坐席边接电话边改的客户数据（今天三个角色都能改）。把它并进 `config:*` 会强迫 `AGENT` 的 grant 含 `config:write`，于是坐席顺带能建队列、改流程、改号码——**与 owner 否决"`users:write` 并进 `config:write`"是同一个形状**。故加 `contacts:read` / `contacts:write`，16 → 18。

   **修正 2（owner，2026-08-31）——一处刻意的行为拓宽**：粗粒度的 `config:read` 把队列（今天 `SUPERVISOR` 可读）与分机/号码/流程/webhook/账号名单（今天只有 `ADMIN`）合到了一起。班长的墙板要读队列，所以 `SUPERVISOR` 的 grant 必须含 `config:read`，他因此**顺带获得读另一半的能力**。接受，理由：那里没有凭证（分机密码是单独的 `config:write`），班长是受信任的内部员工，而拆出第 19 个名字会开"按资源拆 `config`"的口子。**这是行为变更，不是重构**——`docs/auth/TASKS.md` 的 ③ 不得把它混进"无行为变化"的那一提交。`[INFERENCE]` 我先前说的"10–14 个"是目标不是计数——逐条映射 89 个 operation，忠实的分法落在 22 个左右；把 CDR / 录音 / 转写 / `/reports/me` 合并成一对 `history:read:*`、把配置类合并成 `config:*`，才收到 16。取粗的理由：scope 的价值在于**默认最小**，前提是数量少到人愿意逐个想；22 个勾选框的结果是集成方全勾，模型退化成仪式。

   **`users:write` 单列**，不并进 `config:write`：创建账号是**唯一的提权路径**。持 `config:write` 的 Key 泄漏是"改了配置"；持 `users:write` 的 Key 泄漏是"建一个 ADMIN，拿到一切"。两者爆炸半径差一个数量级，不共用一个开关。

   **命名法**（§4 禁止事项）：`资源:动作[:范围]`，**不得出现角色名**。`role → scopes` 只是给页面登录用的便利映射，不是模型本身。
9. **act-as 的 header 名改为 `X-AICC-Agent-ID`**（owner，2026-08-31），**覆盖 §1 写的 `AICC-Agent-ID`**。理由：仓库现有的自定义 header 全带 `X-` 前缀（`X-AICC-Csrf`、`X-AICC-Api-Key`、SIP 的 `X-AICC-Channel-ID`），一致性在这里比 RFC 6648 那条"不建议新 header 用 X- 前缀"的建议更值钱——两种拼法并存会让集成方每次都要想一下哪个带哪个不带。**此条是 §0 的漏记，不是 §1 的冲突**：baseline §16 第 13/15 行照抄了 §1 的 `AICC-Agent-ID`，没有比对仓库惯例。

---

## §17 产品定位复核 — "UI is optional. API is the product."

按 owner 定位重新审视 §1 决策集与上述裁定。**P1–P7 是发现，P8 是已知例外，P9 是验收判据。**

### P1 — `/events` 只认 cookie，机器订阅不到事件流 ★最要紧

`[FACT]` `v1.With(s.requireSession).Get("/events", …)`（`internal/httpapi/server.go:423`），`requireSessionOrAPIKey` 未挂在这条路由上；前端用原生 `EventSource`（`web/src/lib/events.ts:32`）。

事件流是这个产品**实时性的全部**——通话状态、坐席状态、转写、回呼，全在这一条流上。今天一个 API 客户端只能轮询 REST 去猜。按"API is the product"，这不是一条附注，是产品最核心的一面缺了。§1 已经要求"仅为 header 增加并行支持"，**建议把它从附注升格为 §2/§4 的一等条目**，并且它是 §3 测试表该补的第 10 条用例（Key 带 Bearer 订阅 `/events`，收到自己代理坐席的 `PARTY_*`）。

### P2 — 角色泄漏进了 `events` 包，不在 `httpapi` 里

`[FACT]` `internal/events/hub.go:28` `IsSupervisor bool`（注释：grants the unrestricted view），投递判定在 `:225`，由 `internal/httpapi/events_handler.go:49` 的 `id.Role.AtLeast(auth.RoleSupervisor)` 喂入。

这是 §0-3 列出的"5 处 handler 内角色判定"之外的**第 6 处**，而且**不在 `internal/httpapi` 包内**——按包名搜守卫会漏掉它。scope 化后 `IsSupervisor` 必须换成能力字段（例如 `HasFullView`），由 scope 决定而非由角色决定。**订阅范围本身是 UI 三人格模型的直接投影，这是整个代码库里定位偏移最深的一处。**

### P3 — scope 命名是最大的陷阱

§1 没有规定 scope 的命名法。若命名成 `agent:*` / `supervisor:*` / `admin:*`，就是把三个 UI 人格改个名保留下来，一切照旧。

**建议写死一条命名规则**：scope 按 `资源:动作[:范围]` 命名（`calls:control`、`cdr:read:own`、`cdr:read:all`、`flows:publish`、`keys:manage`），**不得出现角色名**。`role → scopes` 映射随之退化为"给页面登录用的便利表"，而不是模型本身——这正是 §1 想要的结构，但需要一条明文规则去保证它。这条规则应当进 §4 禁止事项（"不在 scope 名里出现角色名"）。

### P4 — CSRF 只属于 cookie 方案，不能写进 Bearer 那一支

`[FACT]` 契约里 46 个 operation 现在是 `security: [{cookieSession, csrfHeader}]`；`X-AICC-Csrf` 存在的唯一理由是 cookie 会被浏览器自动携带（`internal/httpapi/middleware.go:21-24`），代码里也已经免除了 key 路径（`:150-154`）。

§2.2 逐条补 security 时必须写成**两个并列的 requirement**，Bearer 那一支**不带** `csrfHeader`：

```json
"security": [
  {"cookieSession": ["cdr:read"], "csrfHeader": []},
  {"apiKeyBearer": ["cdr:read"]}
]
```

写错的后果是机器客户端被迫发一个对它毫无意义的头——契约在告诉集成方"你得假装自己是浏览器"。

### P5 — webhook 对 Key 的封锁需要重新裁定，不能被静默抹掉

`[FACT]` `internal/httpapi/server.go:363-368` 与 `docs/design/09-webhooks.md:370,454` 明确把 webhook-subscriptions 挡在 API Key 之外，理由是"一把万能钥匙泄漏就能把每一通完成的通话外泄"。

那个理由**成立的前提是只有一把万能钥匙**。有了 per-key scope，它就消失了：一把只持 `calls:create` 的 key 泄漏，本来就碰不到 webhook。按"API is the product"，配置投递目标显然是集成方该能做的事。

**裁定：解除**（owner，2026-08-31）。持 `webhooks:write` 的 Key 可配置投递目标。

`[FACT]` 这条禁令写在**三处**，都要改，不能只动代码——留一处不改，下一个读到的人会以为代码是 bug：
- `internal/httpapi/server.go:363-368`（路由旁的注释，讲的就是这条理由）
- `docs/design/09-webhooks.md:370`（"Role: ADMIN, and deliberately not reachable by `AICC_API_KEY`"）与 `:454`（表格行"Configuration role | ADMIN; `AICC_API_KEY` must not reach it"）
- `docs/openapi.json:4247`（operation description：*"Deliberately out of reach of AICC_API_KEY … would let a leaked key exfiltrate every one of them (design 09 §10)"*）

改写时保留原推理并说明前提变了：**风险没有消失，只是从"钥匙能不能到达"移到了"这把钥匙有没有被授予这个 scope"**——这正是 per-key scope 存在的意义。

### P6 — 契约的第一段自我描述是浏览器优先的

`[FACT]` `docs/openapi.json:7` 的 description：*"Authentication is an HttpOnly session cookie issued by POST /auth/login … A system integrating without a browser authenticates **instead** with the X-AICC-Api-Key header."*

cookie 是常态、机器是"instead"的例外。契约的第一段就是产品定位对外的声明，§2.1 应当改写成两种**对等**凭证。纯符号性，但改起来是 0 成本。

### P7 — `mayReadTranscript` 是按屏幕写的规则，不是按能力

`[FACT]` `internal/httpapi/transcript_handlers.go:110-113`：坐席读不到**已结束**通话的转写，注释理由是"转写面板是给眼前这通电话用的，历史属于监督"。

这条规则的依据是**某个 UI 面板的用途**。scope 化时必须重述为能力问题（`transcript:read:own` 是否覆盖历史），由契约回答，而不是照搬"面板不需要"。同类需要复核的还有 `mayHearCall`（`recording_handlers.go:33-51`，这条是按 CDR agent 名单判的，是能力语言，没问题）。

### P8 — 已知的唯一能力不对称：质检打分

裁定 2 让 `quality_reviews` 的 scope 不授予 Key。**这不违反定位**：限制是"人的判断必须归属到一个人"，而**页面 token ≠ UI**——一个班长用 curl 带 session cookie 一样能打分，产品面并没有把这个能力关进浏览器。

但契约里必须把这条理由写出来（operation 的 description），否则后来者只看到一个没解释的不对称，会当成疏漏去"修复"。

### P9 — 把定位变成可检验的验收判据

建议在 ⑦ 那一提交里落一条测试，和 P3、裁定 4 的对齐断言同处一个文件：

> §4 完成后，**不存在**页面 token 能到达、而持有恰当 scope 的 API Key 到不了的 operation——除 P8 一处显式登记的例外。

这条断言可以机械执行：遍历契约的 85+4 个 operation，检查每个的 `security` 数组里是否都有一支 Bearer；例外走 `routes_test.go:17-25` 那样的**白名单 + 理由**写法。有了它，"UI is optional"就不再是一句定位，而是 CI 每次都在验的事实。

### 复核结论

`[FACT]` 现状已经支持定位的部分：路由 85 = 契约 85、双向差集 0；Web 引用 56 条路径、契约外 0 条；`web/src/lib/api.ts` 的类型 100% 来自生成契约。**没有"UI 私有路由"要清理。**

`[INFERENCE]` 偏移集中在**两处**，都不是路由层面而是**能力模型**层面：
1. **实时面**（P1 + P2）——事件流对机器完全关闭，且它的分发模型 (`IsSupervisor`) 就是 UI 人格模型本身。
2. **命名**（P3）——scope 若按角色命名，这次重构会原样保留 UI 的三人格，只是换了个词。

**处置（owner 2026-08-31 全部裁定完毕）**：P1 / P2 / P3 **已纳入** §2 与 §4 的正式条目；P5 **解除**（三处成文依据同步改写）；P4 / P6 / P7 / P8 是 §2 逐条补 security 时的执行细则；P9 是 ⑦ 的验收测试。§0 至此无遗留待决项。
