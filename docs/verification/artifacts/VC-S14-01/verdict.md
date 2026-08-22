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
