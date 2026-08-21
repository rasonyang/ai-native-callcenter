# VC-S12-01 — 通话中重启 app · 判定:**FAIL**

执行 2026-08-21 20:09。呼叫 `01a02439-ad5a-…`(主叫腿)/ `01a02439-ad7b-…`(bot 腿)。
人工步骤:拨 95001,bot 说话中执行 `deploy/dev/restart.sh`。
**owner 听感:"直接断了"** —— 没有保持音,通话直接结束。

## 逐条对照

| expect | 实测 | |
|---|---|---|
| `restart.sh` 输出 `started:` | `started: logs/aicc-20260821-200933.log` | ✓ |
| 新实例日志含 `database ready` 与 `voice leg listening` | 计数 2 | ✓ |
| FS console:**恰好 1 行** `aicc_inbound: bot leg failed for 95001` | **0 行** | ✗ |
| `/calls/waiting`:主叫出现在 support-en 等待名单 | `{"items":[]}` | ✗ |
| (判定辅助)switch 侧 members | 无该主叫;仅有更早一通已 `Abandoned` 的遗留行 | — |

**判定 FAIL。** 且**不适用** expect 里"两侧皆空 = 落在 ESL 断档窗(F4)→ 重跑"的判定辅助:
那条辅助假设主叫**确实进了队列**只是应用没看到。本次有主证据表明主叫**从未进入队列**,
重跑会得到同样结果。

## 根因(主证据,非推断)

`freeswitch/scripts/aicc_inbound.lua:69-70`:

```lua
session:setVariable("hangup_after_bridge", "true")
session:setVariable("continue_on_fail", "true")
```

FreeSWITCH 日志逐行印证,两条腿相隔 **50 毫秒**:

```
20:09:31.106596  01a02439-ad7b  sofia/external/95001            hanging up, cause: NORMAL_CLEARING
20:09:31.156604  01a02439-ad5a  …18688886669  Overriding SIP cause 480 with 200 from the other leg
20:09:31.156604  01a02439-ad5a  …18688886669  hanging up, cause: NORMAL_CLEARING
```

bot 腿一死,`hangup_after_bridge=true` 立刻把主叫腿一起挂掉,
**Lua 第 104 行的 `if session:ready()` 兜底块根本没有机会执行**。

`continue_on_fail=true`(第 70 行)覆盖的是**另一种**故障:桥接**压根没接通**
(拨号那一刻网关就是死的)。它不覆盖"接通了、然后对端消失" —— 而
**app 中途重启、进程崩溃、provider 掉线,全都是后者**,也就是这个兜底最该管的那些情况。

所以这不是"兜底不可靠",是**兜底对最可能的故因不可达**。case 的 `failure_looks_like`
预言的是"进了队列但应用看不见",实际比那更早一步就断了。

## 修法的两难(这是它没被简单修掉的原因)

直接把 `hangup_after_bridge` 改成 `false` 会**破坏正常通话**:bot 说完再见、自己关掉 SIP 腿时,
主叫腿会存活下来落进 `if session:ready()`,于是**每一通 bot 正常收官的电话都会被塞进人工队列**。

所以兜底必须能分辨"**bot 把事办完了**"与"**bot 消失了**"。可用的形态:
bot 在关闭自己那条腿之前,往主叫通道盖一个收官印记(与它转接时盖 `aicc_bot_sec` 同一手法),
Lua 见印记则挂断、无印记则转 fallback 队列。

→ 立案 **C26**。

## 次生实证:被重启打断的呼叫不进账本

该呼叫**一行 CDR 都没有**(`cdrs` 6205 → 6206,那 +1 是更早一通在队列放弃的
`01a02438-136c`)。挂断事件落在新实例不认识的 channel 上被丢弃 ——
这正是 **VC-S12-02** 的 expect 里作为"已知缺口的既成事实"记着的那件事,
在本用例上一并显形。S12-02 执行时可引本次为佐证。
