# VC-S14-01 — 分机生命周期:建 → 注册鉴权 → 删除守卫 · 判定:**FAIL**

执行 2026-08-22 11:37–11:58(UTC 03:37–03:58)。app `logs/aicc-20260822-084508.log`。
建/目录/注册三段成立;**删除守卫不存在**,另在建的那一步查出第二个缺陷。
分机数 20 → 21 → **20**(回到基线),amy 恢复原状(她本就未绑定)。

## note ① 已解:创建请求体的字段名,草案**猜对了**

契约 `ExtensionWrite`:必填 `number`,`password` 建时必填,另有 `displayName` / `isEnabled` / `kind`
(枚举 `AGENT|BOT|PLAIN`)。草案的 `{number,password,displayName}` 可用;
**`kind` 省略时取 `AGENT`**(实测响应即 `"kind":"AGENT"`),不必显式给。

## 逐条对照

| expect | 实测 | |
|---|---|---|
| 建:201 | `http=201` | ✓ |
| `luacc.directory` 立刻可查到 1099(视图,无需 reload) | 见下 **①**,须先启用才可查 | ⚠ |
| 话机注册成功 | `1099@192.168.31.55` `Telephone 1.6` `Registered(UDP)` `Reachable` | ✓ |
| 删**被坐席绑定**的分机:**拒绝**,4xx 且错误码可辨 | **`http=204`,删成功** | ✗ |
| 删 1099:204;删后目录查不到 | `204`;`luacc.directory` 计数 **0** | ✓ |
| 坐席会话执行同样的建:403 | `wei 建分机 http=403` | ✓ |

## 缺陷 ①(建的那一步,与注册无关):不写 `isEnabled` 建出来的分机是停用的,且无任何提示

```
POST {number, password, displayName}                  → isEnabled:false,luacc.directory 计数 0
POST {number, password, displayName, isEnabled:true}   → isEnabled:true, luacc.directory 计数 1
```

同一环境同一时刻的 A/B,只差这一个字段。

**根因**:契约里 `isEnabled` 是**可选**的,而 `CreateExtension`(`catalog_handlers.go:48-55`)
`decode` 进的是普通结构体 `catalog.Extension`,**省略即 Go 零值 `false`**,直接写库 ——
`extensions.is_enabled` 的列默认值 `true` **永远轮不到生效**。
佐证:库里原有 20 个分机 `is_enabled` 全是 `t`,只有这样建出来的是 `f`。

**后果**:`luacc.directory` 带 `WHERE e.is_enabled`(实查视图定义),
所以这部分机 **Lua 查不到、永远注册不上**,而 API 只回 201,管理员这一侧看不出任何异常。
一个"建好了却不能用"的分机,排查会指向话机或网络,而不是这里。
**立案 C29。**

## 缺陷 ②(本用例的主判定):删除守卫根本不存在,坐席被静默解绑

`DeleteExtension`(`catalog_handlers.go:67-69`)直接调 `s.catalog.DeleteExtension`,
后者(`catalog/service.go:128-130`)直接调 store,**中间没有任何检查**。
唯一可能的保护是外键,而它是:

```
fk_agents_extensions  FOREIGN KEY (default_extension_id) REFERENCES extensions(id) ON DELETE SET NULL
```

`SET NULL` —— 不是 `RESTRICT`。实测(amy 绑到 1099、话机正在注册时删):

```
DELETE /extensions/{id}   → http=204
extensions 里 1099 行数    → 0
amy 的绑定                 → (unbound)      ← 静默
luacc.directory 1099       → 0
```

**204,照单全收,坐席的分机绑定就这么没了,没有任何错误、没有任何提示。**
这正是 `failure_looks_like` 写的第一种:那名坐席的话机下次重注册就失败,
而应用里他仍是 READY,队列继续给他派单,直到有人发现"这个人接不到电话"。
契约的 DELETE 也只声明 `204,400,401,403,404,503` —— **没有 409**,即"拒绝"这条路
在契约层面就没有位置。**立案 C30。**

**执行时的一处有意偏离(已回写用例)**:用例原文是"删 1008"。1008 是 wei 正在用的话机,
且 S14-02 / S14-04 / S12-04 都还要用他;既然外键是 `SET NULL`,照原文执行会**当场把 wei 解绑**。
改用未绑定的 **amy 临时绑到 1099** 再删 —— 守卫要挡的是"存在绑定",与绑的是谁无关,断言等价,
且事后 amy 回到原状(未绑定),零残留。

## `failure_looks_like` 的第二种未发生

"删了行但 Lua 目录还查得到(视图未过滤或有缓存),分机被删了却还能注册":
删后 `luacc.directory` 计数即为 **0**,目录是视图、跟着表走,没有缓存。
FreeSWITCH 侧那条注册仍在(`EXPSECS` 从 198 往下走),那是**交换机自己的注册缓存**,
不是目录还认它 —— 观察其到期即可证实它续不上(见 output.txt 末尾轮询)。

**实测确认(03:58:37Z)**:`1099` 的注册到期后**没有再上来**,注册条数由 3 → 0。
删掉分机行 → 视图随之为空 → 话机续注册被拒。这一路是对的。

## 更正:上文"零残留"说早了

执行 VC-S14-02 的前置检查时发现,把 amy 临时绑到 1099 这一步在**交换机侧**留下了东西 ——
应用侧 amy 确实回到了未绑定,但 mod_callcenter 里多出了:

```
agent-amy   contact=user/1099@192.168.31.55   status=Logged Out
tier        agent-amy|support-en@192.168.31.55
```

`contact` 指向的 **1099 已经被删掉了**,是个悬空联系地址。因为她 `Logged Out`,不会被派单,
但这是本次测试引入的、测试前不存在的状态(amy 的 `queue_agents` 行是原有的,
她此前没出现在交换机上,是因为她没有分机)。

已清理并**以复查为准**确认(不认 `+OK`,认 `tier list` / `agent list` 的复读,C1 的教训):

```
callcenter_config tier del support-en@192.168.31.55 agent-amy   → +OK
callcenter_config agent del agent-amy                           → +OK
复查:tier list 回到 3 条(agent-ben|support-zh、agent-wei|support-en、agent-wei|support-zh)
      agent list 中 amy 已消失
```

**教训**:临时绑一个坐席到探针分机,副作用不止在应用库里 —— 绑定会把这个坐席
**镜像进交换机**,而随后删掉分机不会把镜像撤回去。下次做同类替身测试,
收尾要连交换机侧一起复查,不能只看应用库。

---

## 重跑 —— 2026-08-23 09:16–09:36(C29 修于 `4b371de`,C30 修于 `42d7fc2`+`10ca069`)

两条失败断言**原样重跑**,没有一条被改软。用例本身只改了一处:创建请求体**去掉**了
`isEnabled: true` —— 那是当初为绕开 C29 加的,并写明"C29 修好后这一条可去掉"。
现在"省略"本身就是被断言的行为。

### C29 那半:不带 `isEnabled` 建出来的分机是启用的

```
POST /extensions {"number":"1099","password":"…","displayName":"VC Probe"}
→ 201  {"id":"01a02c30-…","number":"1099","kind":"AGENT","isEnabled":true,…}

luacc.directory where number='1099'  → 1099|VC Probe        （8-22 首跑:0 行）
show registrations                    → 1099 已注册（WSS，192.168.31.55:50938）
```

`luacc.directory` 带 `WHERE e.is_enabled`,所以**查得到这一行**就是"它是启用的"的证据 ——
不需要另取一条断言。话机随后真的注册上了,这是这半修复的完整闭环:
8-22 那次建出来的分机 Lua 查不到,话机永远注册不上,而 API 一样回 201。

`kind` 仍然省略,仍然取 `AGENT` —— 与 8-22 一致。

### C30 那半:删一个坐席正在用的分机被拒

| 步骤 | 8-22 首跑 | 8-23 重跑 |
|---|---|---|
| amy 绑到 1099 后 DELETE | **204** | **409** `EXTENSION_ASSIGNED_TO_AGENT` |
| 报文 | (无) | `an agent has that extension as their phone; unbind them before deleting it` |
| amy 的绑定 | **被静默置 NULL** | **仍然绑着**(still_bound = t) |
| 分机行 | 已删除 | **还在**,注册也没断 |
| 解绑后 DELETE | —— | 204 |
| 删后 `luacc.directory` | 0 | 0 |
| 坐席会话建分机 | 403 | 403 `FORBIDDEN` / requiredRole ADMIN |

守卫是外键而不是 service 预检,所以**不走 API 也一样挡得住** —— 顺手证了一次:

```
psql> delete from extensions where number='1099';
ERROR:  update or delete on table "extensions" violates RESTRICT setting of
        foreign key constraint "fk_agents_extensions" on table "agents"
DETAIL:  Key (id)=(01a02c30-…) is referenced from table "agents".
```

### 现场抓到的第二个缺陷:守卫成立,但报错报错了

**第一次跑 collect 第 6 条,返回的是 `503 STORAGE_DOWN`,不是 409。**
删除确实被挡住了(分机还在、amy 还绑着),但操作员收到的是"存储故障" ——
正是修复代码自己的注释里写着要避免的那件事:*"a guard that reports itself as storage down
teaches the operator to retry, and retrying will never work."*

根因在日志里一眼可见:

```
msg="catalog request failed" error="ERROR: update or delete on table \"extensions\"
violates RESTRICT setting of foreign key constraint \"fk_agents_extensions\"
on table \"agents\" (SQLSTATE 23001)"
```

**23001 `restrict_violation`,不是 23503 `foreign_key_violation`。**
PostgreSQL 对**显式 `ON DELETE RESTRICT`** 用前者,`NO ACTION` 与插入侧才用后者。
边界只认 23503,于是落进 default 变成 503。

**为什么单元测试没拦住**:那条测试是我**用自己以为的错误码**造出来的 `PgError` 喂给 handler 的 ——
它和服务端犯的是同一个错,于是两边一致通过。**桩件不会在世界的问题上反驳你。**
现已改成两个 SQLSTATE 都认,测试里的 code 与 message **抄自这次真实失败**;
摘除 23001 那一支,测试报 `SQLSTATE 23001: http = 503, want 409`。修在 `10ca069`。

这一条是"现场重跑"相对"跑测试"的全部价值所在:代码、单测、契约三方一致,
却一致地错着,只有真库能说话。

### 收尾:这次交换机侧零残留

8-22 那次把 amy 绑到 1099 是**走 API** 的,于是她被镜像进 mod_callcenter,
删掉分机后留下一个指向已删分机的悬空 contact,事后专门清理过(见上一节)。
本轮账本明确改成 **psql 直写夹具**,复查证实没有再产生镜像:

```
callcenter_config agent list → agent-ben / uiagent / agent-wei   （无 amy）
callcenter_config tier list  → 3 条,与清理后一致
```

应用库同样干净:1099 已删、amy 未绑定、探针队列已删。

### 顺带取到的证据:C33(同型缺陷的整数版)

修 C29 时读代码发现、本轮实测坐实 —— **不带那三个整数字段建队列,建出来是 0,不是列默认值**:

```
POST /queues {"name":"vc-c33-probe","extNumber":"7099","displayName":"C33 Probe"}
→ 201  discardAbandonedAfterSec:0   ronaDelaySec:0   slaThresholdSec:0
        列默认值分别是            60             10              20
   同一响应:isEnabled:true  isRecordingEnabled:true  ← 布尔那半已经好了
```

机制与 C29 完全相同(`validate()` 不给这三个兜底,INSERT 又把列名一一写出,列默认永不生效),
差别在于**后果的可见性**:布尔那半是"建出来就不通",整数这半是"建出来就在跑,只是参数不是文档说的那个"。

**库里已经有一个真实受害者**:`support-zh` 现在是 `0|0|0`,而 `support-en` 是 `60|10|20` ——
种子对两条队列用的是**同一条 INSERT**(`seed.go:252`,只显式写 `sla_threshold_sec=20`),
所以 support-zh 是后来被某次 API 写操作抹平的。
**连带**:凡在 support-zh 上量过的 SLA 数字,门限都是 **0 秒**,不是 20 秒。

C33 **未修**,只立案,不夹带进 C29 的修复。探针队列已删除。

### 判定

**PASS。** 两条断言各自成立且都不是靠改软过的;C30 那半在重跑当场又暴露并修掉了报错映射的错误,
修完再跑才算数。C33 是本轮的新增留证,不影响本条判定。

### 补跑:坐席会话的"删"那一半(此前两轮都没采到)

expect 最后一行写的是"坐席会话执行同样的**建/删**:403",但 8-22 首跑与 8-23 重跑
**都只采了『建』**。补上:

```
坐席 jar  DELETE /extensions/00000000-…  → 403 FORBIDDEN  requiredRole=ADMIN
```

为确认这个 403 来自角色而不是"这一行不存在",用 ADMIN 会话跑了同一条命令作对照 ——
**回的是 204**。403 确实是角色判定,在查库之前就发生了。本条断言至此完整成立。

### 对照跑出来的第三个缺陷:C34

上面那条对照本身就是发现:**ADMIN 删一个不存在的分机,返回 204。**
契约给 `DELETE /extensions|queues|dids/{id}` 都声明了 404,三条实测**全是 204**:

```
DELETE /extensions/00000000-…  → 204
DELETE /queues/00000000-…      → 204
DELETE /dids/00000000-…        → 204
```

`writeDeleted` 只看 err、不看删了几行,而删零行不是错误 —— 404 那一支永远走不到。
**方法论上的连带更重要**:本用例第 9 条 `delete 1099 http=204`,
在分机根本不存在时长得**一模一样** —— 真正把它钉住的是第 10 条目录计数归零。
与 C28 那轮"SPA 兜底把未知路径也回 200"是同一类陷阱。**已立案 C34,未修。**
