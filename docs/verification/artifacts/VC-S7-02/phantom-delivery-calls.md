# C20 — 队列派单腿被当成坐席自己发起的外呼

发现于 2026-08-21 T3.5 执行途中(owner 截图:"Ringing 变为 Dialing")。

## 现象

排队电话派给 wei 期间,坐席工作台把这条派单腿显示成 **CALLING OUT / Dialling 18688886669**,
同时下方 MY QUEUE 还写着 "1 waiting 18688886669"。同一条腿被显示了两次,一次是"来电振铃",
一次是"我在外呼"。

## 不是 FSM 违规

owner 给出的规则 —— `DIALING→TALKING`、`RINGING→TALKING`,DIALING 与 RINGING 互斥、
彼此之间无转换 —— 代码里**已经是这样**(`internal/telephony/call.go:64` 的注释与
`partyTransitions` 表:两个状态各自只有 ANSWER/RELEASE 两条出边)。

所以界面上的 "Ringing → Dialling" 不是一个 party 改了状态,而是**同一条物理腿存在于两个
不同的 Call 聚合里**,工作台在两者之间翻转:

| | callId | role | state | callType |
|---|---|---|---|---|
| 真实来电 | `01a021f5-0ce9…` | TARGET | RINGING | INBOUND |
| 幽灵 | `01a021f6-3e64…` | ORIGINATOR | DIALING | OUTBOUND |

`Call.AddParty`(call.go:205)规定"第一个 party 即 originator,起始 DIALING;之后的都 RINGING"。
派单腿在幽灵 call 里是第一个 party,于是拿到 ORIGINATOR/DIALING;在真实 call 里是后来的,
于是拿到 TARGET/RINGING。两条记录都合法,只是其中一条不该存在。

## 根因

mod_callcenter originate 派单腿时,coordinator 见到一条陌生的 outbound channel,先铸一个
**provisional agent-only call**;直到 `CHANNEL_BRIDGE` 才由 `join`(coordinator.go:423)
把它并入主叫的 call。**振铃期间两个 call 并存**,`/calls/mine` 两条都返回。

`callTypeOf` 对这个幽灵没有 `aicc_call_type` 印记可用(该变量只由点击外呼
`internal/outbound/outbound.go:196` 打),于是退回方向猜测:腿是 outbound → **OUTBOUND**。

## 二级后果:幽灵 CDR

派单腿若在接通前被 mod_callcenter 取消(agent 没接、重试下一轮),这个 agent-only call
从未 bridge 就死了,却**照样写了一条 CDR**。本次单通排队电话产生 12 条:

```
01a021f6-3a9d… OUTBOUND 1008 → 18688886669 NO_ANSWER 0s
… ×11 …
01a021f5-5300… OUTBOUND 1008 → 18688886669 NO_ANSWER 59s
```

即"呼叫方=坐席分机、被叫=主叫号码"的倒置外呼记录。这正是先前记为"未接 b-leg 的残留
幽灵 CDR"的那件事,实测规模是**每次派单重试一条**,不是偶发。

历史核查:`from_number ~ '^[0-9]{4}$'` 的 OUTBOUND CDR 最早出现在 2026-08-19,
**早于 2026-08-20 的 CallType/CDR 改动**,故为既有缺陷,非本次回归。今天变化的只是
① 重试次数把它放大到 12 条,② 工作台改版后这个幽灵会渲染成 "Calling out"。

## 可用的修法(待批准)

mod_callcenter 在派单腿上设了这些通道变量(`strings mod_callcenter.so` 实证):

```
cc_side                    # "agent" | "member"
cc_member_session_uuid     # 主叫的 channel uuid  ← 合并键
cc_queue  cc_agent  cc_agent_uuid
```

`cc_member_session_uuid` 正是缺失的合并键。建议:

1. `switchevent.go` 归一化时读出 `cc_side` 与 `cc_member_session_uuid`
   (与既有的 `variable_aicc_queue` 同一处,switchevent.go:92)。
2. coordinator 见到 `cc_side=agent` 的 CHANNEL_CREATE 时,**直接把这条腿挂到
   `cc_member_session_uuid` 所属的 call 上**,不铸 provisional call。
   派单腿因此从出生就是 TARGET/RINGING,callType 保持主叫的 INBOUND。
3. 幽灵 call 不再产生 ⇒ 幽灵 CDR 与 "Ringing 变为 Dialling" 一并消失,
   且不必给 `/calls` 加过滤(过滤会误伤硬话机直拨发起的真实外呼)。

风险面:`join` 的 `isAgentOnly` 优先级逻辑(coordinator.go:439-447)在派单腿不再是
agent-only call 后走不到,需确认转接/二次派单路径仍能正确合并。
