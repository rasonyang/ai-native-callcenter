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

---

## 重跑 —— 2026-08-23 09:55–10:12(C26 修于 `bd27bce`;途中又修 C35 `1ed4cc9`)

### 基线先跑:一通正常收尾的电话仍旧只是结束

修法靠"让主叫活过 bot 腿"实现,所以它能坏的另一个方向是**每一通谈完的电话都被塞进人工队列** ——
比原缺陷更吵。先证这个方向没坏:

```
09:56:28  flow reached a terminal phase … node=farewell
09:56:29  tool ran  tool=hangup  isOk=true
09:56:37  hanging up after the farewell → sip bye sent
09:56:37.338  aicc_inbound: bot finished the call on 95001 (HANGUP)
09:56:37.388  Channel …18688886669 hanging up, cause: NORMAL_CLEARING
```

见到印记 **50 毫秒后**主叫被正常挂断,队列成员为空,
CDR:`INBOUND | 95001 | ANSWERED | NORMAL_CLEARING | bot_sec=49`。

顺带证实:节点 `tools: []` 不妨碍内建工具 —— 模型确实调了 `hangup`,走的是 `HANGUP` 那条印记而非
`FLOW_END`。两条路都盖印,判定不受影响。

**采集差点说谎**:`fs_cli console last 300` 里 grep 不到那一行,读起来像"兜底块没执行"。
实际是**收官那行是 INFO**,控制台缓冲区按 loglevel 过滤又只有 N 行。账本两条 collect 已改成
直接读 `/usr/local/freeswitch/log/freeswitch.log`(`f899890`)。
与 C28 那轮"SPA 兜底把未知路径也回 200"、C34"删不存在的东西也回 204"同类:**采集手段本身会说谎**。

### C26 那半:主叫活下来了,并且真的进了队列

通话中执行 `restart.sh`(09:59:11–09:59:17):

```
09:59:14.358  bot 腿 (sofia/external/95001) 被重启杀掉
09:59:14.378  aicc_inbound: bot leg vanished for 95001 (SUCCESS)   ← 8-21 首跑此处 0 行
09:59:14.378  CoreSession::setVariable(hangup_after_bridge, true)  ← 交出去前把规则放回
09:59:14.378  Transfer …18688886669 to XML[7001@aicc]
09:59:14.388  Member 18688886669 joining queue support-en
09:59:14.398  Queue has 1 waiting calls
09:59:14.418  Updated Agent agent-wei set state = Receiving
```

`vanished` 而非 `failed`:`bridge_uuid` 存在,说明是**接通之后**对端消失,与"压根没接通"分开了。
**C26 的断言到此成立** —— 8-21 那次主叫在 50 毫秒内被跟随挂断,兜底块无机会执行。

### 但本用例仍判 FAIL,卡在两处新暴露的缺陷

主叫第一次活得够久,于是暴露了下游:

**① 应用侧没有收养这通电话(→ C36)。** 主叫在队列里(`queue list members support-en` 有行、
`state=Trying`、`serving_agent=agent-wei`),而 `/calls/waiting` **是空的**;
`/api/v1/calls` 里只有一通 **OUTBOUND**、party 是 1008 的 `DIALING` 腿 ——
每派单一次就多开一通假外呼,主叫本人从头到尾不在应用里。
这正是本用例 `failure_looks_like` 写的第二种:*"这通电话直到有人接起前对所有屏幕都是隐形的。"*
本用例 expect 要求主叫出现在等待名单,故 **FAIL**。

**② 派单被取消后坐席话机被挂死的 INVITE 卡住(→ C37)。**

```
09:59:14.428  第一次派单
09:59:16.128  Agent agent-wei Origination Canceled : ORIGINATOR_CANCEL
09:59:16.2 起 USER_BUSY … 一簇一簇地重试,簇内约 70 毫秒一次;三分钟里共 42 次拒绝
10:02:25     Member … abandoned waiting in queue support-en
```

第一条振铃腿没被拆干净,此后浏览器话机对每一个新 INVITE 回 486,
而 `reject_delay_time=0` 让 mod_callcenter 立刻重试 —— 拒绝不是无应答,不走 RONA 退避。
owner 手测时的观感就是"只听到保持音,并没有看到 1008 响铃"。

### 途中手测逼出 C35:bot 把主叫转进了错误的 context

owner 另拨两通做转接手测,同样只有保持音。日志一比就清楚了:

```
09:59:14  Transfer … to XML[7001@aicc]     ← 今天新写的 Lua 兜底 → joining queue
10:02:44  Transfer … to XML[7001@default]  ← bot 的 transfer_to_agent → 一行 joining queue 都没有
10:03:40  Transfer … to XML[7001@default]  ← 同上
```

`7001` 只在 aicc context 有匹配;`default` 里没有任何队列分机。
四个调用点都显式传 `"default"`,把 adapter 早已备好的"空 = aicc"默认顶掉了。
**已修**(详见 TASKS.md C35),修完 owner 再拨一通:

```
10:11:49.208  CoreSession::setVariable(hangup_after_bridge, false)   ← 新 Lua
10:12:02.298  Transfer …18688886669 to XML[7001@aicc]
10:12:02.318  Member 18688886669 joining queue support-en
10:12:02.338  Updated Agent agent-wei set state = Receiving
10:12:06.328  Agent agent-wei answered "18688886669" from queue support-en
10:12:06.358  Member … is bridged to agent agent-wei
10:12:12.538  Channel …18688886669 hanging up, cause: NORMAL_CLEARING   ← 坐席挂机,主叫跟随
```

**bot → 队列 → 坐席接起 → 通话 → 坐席挂机 → 主叫跟着结束**,整条链第一次完整走通。
CDR 一行:`INBOUND | 95001 | ANSWERED | NORMAL_CLEARING | bot_sec=13 | talk_sec=6 | bill_sec=23`。

这一通同时证了 C26 修复里最容易被忽略的那一半:`hangup_after_bridge` 是留在**主叫通道**上的,
转接前必须放回 `true`,否则坐席挂机后主叫会活下来。应用日志没有
"could not restore the caller's teardown rule",而最后主叫确实跟着坐席一起结束了 ——
`handOnCaller()` 生效。(注:Go 侧走 ESL `uuid_setvar`,不产生 `CoreSession::setVariable` 日志行,
所以这一条只能从**结果**上证,不能从日志行上证。)

### 判定

**FAIL(C26 那半已修并证实;卡在 C36)。**
不把 expect 降级成"主叫进了队列就算过" —— 队列里有一通所有屏幕都看不见的电话,
正是本用例一开始就写明的失败形态。
