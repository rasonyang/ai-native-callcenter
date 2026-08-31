<!-- SPDX-License-Identifier: Apache-2.0 -->

# API-first 复核 — 违反准则的，与过度设计的

准则见 `CLAUDE.md` 的 owner directive（2026-08-31）：**UI is optional. API is the product.**

`[FACT]` 带 `file:line` / `[INFERENCE]` / `[ASSUMPTION]`。基线：`main` @ `2ecb28f`。
本文覆盖**整个产品**；只与统一认证任务有关的条目见 `docs/auth/baseline.md` §17（P1–P9），此处不重复展开，只登记编号。

---

## 一、违反准则的

### V1 — 未知的 `/api/v1/*` 路径返回 `200 text/html` ★最严重 — **已修 `9c01e2f`**

`[FACT]` 实测（`New(cfg, Deps{SPA: …})` + `router()`，`httptest`）：

```
GET /api/v1/nope           -> 200 "text/html; charset=utf-8"  <!doctype html>…
GET /api/v1/calls/mine/xx  -> 200 "text/html; charset=utf-8"  <!doctype html>…
GET /api/v1/auth/login/x   -> 200 "text/html; charset=utf-8"  <!doctype html>…
```

`[FACT]` 成因：`internal/httpapi/server.go:428-430` 的 `r.NotFound(s.spa.ServeHTTP)` 挂在**根 router** 上，而 chi 的 `NotFound` 会向下传播进子路由——`mux.go:214-218` `updateSubRoutes(func(subMux *Mux) { if subMux.notFoundHandler == nil { subMux.NotFound(hFn) } })`。`/api/v1` 子树因此继承了 SPA 兜底。

**为什么这是准则问题而不只是 bug**：一个集成方拼错路径、或调用一个这个版本还没有的端点，拿到的是 **200 + HTML**。绝大多数客户端库会在 JSON 解析处炸掉（错误信息与真正的原因毫无关系），少数按状态码判断成败的会**把失败当成功**。契约承诺的是 `404 NOT_FOUND` + `{"error":{code,message,params}}`。

`[FACT]` 这个仓库**已经被它咬过一次**：`internal/httpapi/routes_test.go:30-35` 记着 `GetCallTranscript` 有 handler、有契约条目、没有路由，"in the running product the transcript snapshot returned the SPA's index.html with a 200, the panel's fetch failed to parse it, and the whole backfill half of the feature was dead. Nothing failed."——当时补的是"每个 operation 都必须被路由"的测试，**没有补兜底本身**。

**已修**（`9c01e2f`）：在 `/api/v1` 子路由上认领 `NotFound`——子树自己有了 handler，chi 的传播就跳过它，而 SPA 继续拥有子树之外的每一个路径。`internal/httpapi/fallback_test.go` 钉住三件事：未知 API 路径回 404 信封、子树之外的深链接仍归 SPA、方法不匹配回 405 信封。

### V2 — 方法不匹配返回 `405` 空 body，不带 `Content-Type`，不是错误信封 — **已修 `9c01e2f`**

`[FACT]` 实测：`DELETE /api/v1/auth/login -> 405 ""`，空 Content-Type、空 body。走的是 chi 默认的 `methodNotAllowedHandler`（`mux.go:411-418`）。

`[FACT]` 契约的承诺（`docs/openapi.json:7`）：*"Errors always use the single envelope `{"error": {code, message, params}}`"*。**"always" 现在是假的。**

**已修**（`9c01e2f`）：同 V1 挂 `MethodNotAllowed`。`METHOD_NOT_ALLOWED` 已进契约 `ErrorCode` 枚举（22 → 23，`make api-breaking` exit 0）与两份 `translation.json`；`docs/openapi.json` 第一段那句 "always" 现在把兜底行为也写了进去，让它名副其实。

### V3 — 跑起来的部署拿不到契约

`[FACT]` `grep -rn "openapi\|redoc\|swagger" internal/httpapi/ cmd/` 只命中一条注释（`api_server.go:13`）。**没有任何端点 serve `docs/openapi.json`，也没有文档页。**

契约只存在于 git 仓库里。一个拿到部署实例的集成方无法从产品本身发现 API——这相当于产品出厂不带说明书。SPA 是嵌进二进制的（`go:embed all:dist`），契约却不是。

**处置：并进 §2 的契约提交**（owner，2026-08-31），作为 `2.2f`。按 spec-first 它需要一个新 operation，不能绕过契约先写代码。

`[FACT]` 落地手法已确认：加一个 `docs/embed.go`（`package docs` + `//go:embed openapi.json`），照抄 `web/embed.go:16` 嵌 SPA 的做法。**不能直接 `//go:embed docs/openapi.json`**——`go:embed` 的模式不允许 `..` 跨目录，而 `docs/` 目前不是 Go 包；这个仓库已经用 `web/` 这个包解决过同一个问题。零构建步骤。

`[INFERENCE]` 端点免鉴权：契约本来就是公开文档，而且一个还没拿到凭证的集成方正是最需要读它的人。要文档页的话再加一个 Redoc 单页，但那是可选的第二步。

### V4 — `/events` 只认 cookie，机器订阅不到事件流

`[FACT]` `internal/httpapi/server.go:423`。详见 `docs/auth/baseline.md` §17 P1。产品的实时性全在这一条流上，集成方今天只能轮询 REST 去猜。**已列为 §2 正式条目 `2.2b`。**

### V5 — 角色泄漏出了 `httpapi` 包

`[FACT]` `internal/events/hub.go:28` `IsSupervisor bool`，投递判定 `:225`。详见 §17 P2。事件分发范围模型就是 UI 三人格模型本身。**已列入 ③ 的守卫删除范围。**

### V6 — 契约第一段把 cookie 写成常态、把机器写成例外

`[FACT]` `docs/openapi.json:7`：*"Authentication is an HttpOnly session cookie … A system integrating without a browser authenticates **instead** with the X-AICC-Api-Key header."* 详见 §17 P6。**已列为 `2.2c`。**

### V7 — 按屏幕写的授权规则

`[FACT]` `internal/httpapi/transcript_handlers.go:110-113`：坐席读不到已结束通话的转写，注释理由是"转写面板是给眼前这通电话用的，历史属于监督"。规则的依据是**某个 UI 面板的用途**，不是能力。详见 §17 P7。**已列入 ③。**

### V8 — 46 个 operation 把 `csrfHeader` 写成无条件要求

`[FACT]` 契约里 46 个 operation 是 `security: [{cookieSession, csrfHeader}]`；CSRF 存在的唯一理由是 cookie 会被浏览器自动携带（`internal/httpapi/middleware.go:21-24`）。契约在告诉集成方"你得假装自己是浏览器"。详见 §17 P4。**已列为 `2.2` 的执行细则。**

### 不算违反的（复核后排除）

- **错误 `message` 是英文散文、只有 `code` 可机读**（`internal/httpapi/errors.go:15-17`）：这是**正确**的 API 设计——`code` 是契约，`message` 是给人看的。前端本地化不是特权，是它自己的事。
- **`POST /auth/login` 只回 `Set-Cookie` 不回 token**：机器用 Key，且 Key 可以 act-as 坐席，能力上不缺。刻意设计，不是偏袒。
- **`/metrics` `/healthz` `/readyz` 在契约外**：运维监听面，契约自己声明了这个例外（`docs/openapi.json:7`）。
- **`/reports/{overview,queues,daily,me}` 是屏幕形状的聚合**：`[INFERENCE]` 是 UI 派生的，但它们在契约里、可被任何客户端调用、语义清楚。**形状可议，不违反准则。**

---

## 二、过度设计的（单机版小客户视角）

判据：**这个功能，一个装在自己机房里、有 1–2 个集成、几十个坐席的客户，会用到吗？** 用不到、且以后补上是**加法**（非破坏性）的，就该现在砍掉。

### O1 — 按前缀取行 + 常量时间比较 ★仓库里已经有更简单的做法

**§1 的设计**：Key 格式 = 固定前缀 + 展示用短前缀 + 高熵随机段；短前缀单独列并建索引；**查找按前缀取行，再对 SHA-256 做常量时间比较**。

`[FACT]` **同一个代码库里，会话已经用更简单的办法解决了同一个问题**：`internal/store/sql/sessions.sql:8-13` `GetSessionByTokenHash` —— `WHERE sessions.token_hash = $1`。直接按 hash 查，一次索引命中。`internal/auth/auth.go:130-142` 就是全部实现。

`[INFERENCE]` 按 hash 查的时候，**没有东西需要"常量时间比较"**：要么这一行存在，要么不存在。SHA-256 不可逆，索引查找不泄漏任何可用于时序攻击的信息。"按前缀取行再比较" 是把一次索引查找拆成两步，多引入一个索引、一条查询路径和一次手写的常量时间比较——**比现成的做法复杂，安全性不更高**。

**建议**：短前缀列**保留**（列表页要显示 `aicc_live_a1b2…`，这是它存在的理由），但**查找走 `WHERE key_hash = $1`**，照抄 `sessions` 的做法。短前缀列不必建索引。

### O2 — `last_used_at` 的内存节流

**§1 的设计**：同一 Key 每分钟最多一次 DB 写；进程重启丢失最近一次未落盘更新"是可接受的"。

`[INFERENCE]` 这是一个**没有测量支撑的优化**，代价是一个 mutex、一张 map、一次时间比较，外加一个写进设计里的数据丢失窗口。单机部署、几十把 Key 的表，无节流地 `UPDATE … SET last_used_at = now() WHERE id = $1` 每秒几次是完全无感的负载。

`[FACT]` 而且这个仓库的 `CLAUDE.md` 有一条 owner directive：**没跑过基准就不许有性能主张**。为一个没测过的性能问题预先加复杂度，和那条规矩是同一个毛病的两面。

**建议**：**去掉节流，每次直接写**。真的成了问题，那时有真实数字，再加节流是纯加法。

### O3 — 每把 Key 的"允许代理 Agents"列表

**§1 的设计**：显式列表或 `*`；越界回 `AGENT_NOT_ALLOWED`。

`[INFERENCE]` 这是**架在 scope 之上的第二条授权轴**，要一个存储形态（数组列或关联表）、一个错误码、一次 UI 多选、一条中间件检查。而一个单机小客户的集成通常只有一个（CRM），它的答案永远是 `*`。这个列表要开始有意义，前提是**同一个部署里有两个互不信任的集成，且它们各自只该代理一部分坐席**——那是多集成商场景，不是这个客户。

**建议**：**v1 砍掉**，`AGENT_NOT_ALLOWED` 不进错误码枚举（4 个新码变 3 个）。Key 持有 `agent:act` scope 即可代理任意坐席。以后要加是**纯加法**：一个可空列（NULL = 全部）+ 一次检查 + 一个新错误码，不破坏任何已有客户端。

反方意见（记录，不采纳）：允许列表能收窄泄漏后的爆炸半径。但同样的收窄由 scope 已经提供了一层，第二层的边际收益对这个客户接近零。

### O4 — `ENABLED / DISABLED / REVOKED` 三态

`[INFERENCE]` `DISABLED` 与 `REVOKED` 的差别只在**可不可逆**。一个小客户想停掉一把 Key 的时候，几乎总是"这把不要了"——那就是 REVOKED。`DISABLED` 是那种听起来有用、然后没人用的状态。

**建议**：**`ENABLED / REVOKED` 两态**。以后要加 `DISABLED` 是一次 CHECK 放宽（加法），本仓库的迁移测试完全能覆盖（`00006_callback_claim.sql` 有收窄的范例，放宽更简单）。

优先级低于 O1–O3：三态的实现成本本来就小，砍它省的是**概念**不是代码。

### O5 — scope 词表的粒度 ★还没定，现在定最省事

`[INFERENCE]` 89 个 operation 如果对应 30+ 个 scope，一个小客户的集成会直接把所有 scope 都勾上——模型就退化成仪式。scope 的价值在于**默认最小**，前提是数量少到人愿意逐个想。

**建议：10–14 个，资源级**，只在**行为真的不同**的地方才分 `:own` / `:all`：

```
calls:read          calls:control       calls:create
agent:read          agent:act
cdr:read:own        cdr:read:all
recordings:listen:own   recordings:listen:all
transcripts:read:own    transcripts:read:all
config:read         config:write        # extensions/queues/dids/users/flows/webhooks
keys:manage         audit:read
```

配套的两条硬规则（已进 `docs/auth/TASKS.md` `2.0b` / `4.1`）：**scope 名里不得出现角色名**；`role → scopes` 只是给页面登录用的便利映射，不是模型本身。

### 不算过度设计的（复核后排除）

- **审计的 `subject_name` 快照**：一列。改名很少发生，但**删号会发生**，删号之后 LEFT JOIN 出来就是空白。留。
- **`AICC-Agent-ID` act-as 模型**：整个需求就是 CRM 替坐席拨号，这是它的核心，不是装饰。
- **每个 operation 都要声明 security + scopes**：89 处机械编辑，但这正是准则要求的可检验形式，且 CI 兜得住。
- **REVOKED 终态、不硬删**：一行 CHECK，换来"这把 Key 到底被谁停的、什么时候停的"永远查得到。
- **事件流的 hi/lo 序号块 + ring buffer + `SYSTEM_RESET`**：已经在跑、已经有测试，不在本次改动面上，不重新翻案。

---

## 三、建议的处置

| # | 处置 | 落在哪 |
|---|---|---|
| V1 V2 | ~~`/api/v1` 子路由挂 JSON 版 `NotFound` / `MethodNotAllowed`~~ | **已修 `9c01e2f`**（独立提交，不在认证任务的 7 个提交里）|
| V3 | ~~`GET /openapi.json`（`docs/embed.go` 照抄 `web/embed.go`）~~ | **已落地**：契约 `ecf368f`，handler + embed `0793e32`。实测免鉴权 200 / 275,468 字节 |
| V4–V8 | ~~已在 `docs/auth/TASKS.md` 排好（`2.2b` `2.2c` `2.2` `③`）~~ | **全部落地**：契约 `ecf368f`，角色守卫删除与 `events` 包去角色化 `a1fa378` |
| O1 O2 O3 O4 | ~~改 §1 的实现细节~~ | **已采纳 `657cd13`**：hash 直查、去节流、砍允许代理列表（新码 4 → 3）、两态 |
| O5 | ~~采纳 10–14 个资源级 scope 词表~~ | **已定稿并落地**：最终 **20** 个（16 → 18 加 `contacts:*` → 19 加 `agent:manage` → 20 加 `calls:create:ai`）。契约 `ecf368f`，生成常量 `c03080d` |
