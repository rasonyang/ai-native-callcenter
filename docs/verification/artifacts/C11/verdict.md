# C11 — 转接呼叫的 CDR 丢失 bot 份额与队列等待账(已修,2026-08-21)

立案时判定为"漂移在读回/快照侧"。实测根因是**两条独立的缺陷**,读回侧本身无罪。

## ① bot 份额被先挂断的那条腿挡住

`freeswitch/scripts/aicc_inbound.lua:64` 导出三个变量到后续腿:

```lua
session:setVariable("export_vars", "aicc_call_id,aicc_did,aicc_language")
```

于是**拨向 bot 网关的那条腿也带着 `aicc_did`**。转接发生时 `a.session.Close()` 让 bot 腿立即挂断
(actions.go:94),它的 `CHANNEL_HANGUP_COMPLETE` 带来一份 `{DID: "95001"}` —— 非零。
`registry.go` 当时的规则是"任一腿都可能携带,先到者胜":

```go
if a.call.Bot.IsZero() && !ev.Bot.IsZero() { a.call.Bot = ev.Bot }
```

一两分钟后主叫挂断,带着 `stampBotShare` 盖上的 `aicc_bot_sec` / `aicc_flow_id` /
`aicc_bot_summary` / `aicc_bot_reason` —— 但 `a.call.Bot` 已非零,**整份被丢弃**。

这解释了立案时观察到的全部现象,包括那个一直没被解释的分界:
`did`、`language` 有值(Lua 导出的),`bot_sec`、`flow_id`、`user_data` 全空(app 盖的)。

**修法**:`BotShare.Merge` 按字段填补,谁先到都只补自己知道的那部分。
`registry.go` 与 `coordinator.go merge()` 两处改为 `Merge`。

**回归测试**:`TestTheBotShareSurvivesTheLegThatHangsUpFirst`。摘掉修复后它报的正是线上那三个值
(`Bot.Sec = 0`、`FlowID = <nil>`、`Summary = ""`),证明它钉的就是本缺陷。

## ② queue_wait_sec 把通话时长算进等待

mod_callcenter 的 `bridge-agent-start` 是在**它拨出的坐席腿**上抛的,而
`normalizeCallcenter` 只在 `ChannelID == ""` 时才回落到 member 通道:

```go
if out.ChannelID == "" { out.ChannelID = out.MemberChannelID }
```

于是 `Registry.Dispatch` 把它投递给坐席腿所在的 call。修 C20 之前那是个幽灵 call,
`Queue.BridgedAt` 落在幽灵上;merge 时 `if call.Queue.JoinedAt.IsZero()` 为 false(主叫已入队),
幽灵的 Queue 整份被丢 —— `BridgedAt` 归零,`assemble` 回落到 `LeftAt`,于是等待时长包含了整段通话。

**修法**:member 相关的六个 kind(MemberJoined/MemberLeft/AgentOffered/BridgeStart/BridgeEnd/
BridgeFailed)一律按 member 通道路由,不再依赖交换机在哪条腿上抛事件。
**回归测试**:`TestQueueEventsRouteToTheWaitingCaller`(并钉住 agent-state-change 保持坐席通道)。

> C20 的修复已顺手治好②的一半(VC-S7-03 那通 `queue_wait_sec=55` 已与 `BRIDGED wait_ms=55819` 吻合);
> 本改动让它不再依赖派单腿的归属时序。

## 现场复验(2026-08-21 11:5x,两通 95002→ben)

```
call_id        01a02275-8884-7617-95ca-e9dbd63ae5e0
did 95002  language zh  has_flow t
bot_sec        17          ← 修复前恒 0
queue_wait_sec 4           ← queue_events BRIDGED wait_ms=4513;LEFT 为 14000
ring_sec 4  talk_sec 10  total_sec 39  status ANSWERED
agent_ids {ben}  primary_agent ben  has_recording t
user_data      {"botSummary": "来电者表示账单出了问题…", "botReason": "来电者账单出现问题…"}
legs           [BOT 17s] [QUEUE support-zh 4s] [AGENT 1007 10s]   ← BOT leg 修复前完全不出现
```

| 计数器 | 基线 | 两通之后 | |
|---|---|---|---|
| cdrs | 6184 | 6186 | 两通两行,无重复 ✓ |
| recordings | 25 | 27 | ✓ |
| 幽灵 OUTBOUND CDR | 47 | 47 | C20 仍生效 ✓ |

## 未决:CDR 归属的竞态(不在本次修复范围)

`CallFinished` 用 `!snap.Bot.IsZero()` 判断"bot 已交接、本路径拥有这一行"。但 `IsZero()`
把 `DID` 也算在内,而 bot 腿总是带着导出的 DID —— 意味着**纯 bot 呼叫(contained)时人工路径
也会尝试写行**,与 aicall recorder 抢同一个 `call_id`,靠 `ON CONFLICT (call_id) DO NOTHING`
(ledger.sql:19)静默决出胜负。近三日两通 contained 呼叫都是 recorder 赢(`bot_sec>0`),但这是
运气而非保证。真正的交接标记应是 `Bot.Sec > 0`(只有 `stampBotShare` 会设)。
本次未改,以免在验收途中变更 CDR 归属语义 —— 另立 **C21**。
