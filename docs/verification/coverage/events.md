# SSE 事件覆盖清单(events.md)

来源:`docs/openapi.json` `components.schemas.SseEventType`(第 5734 行起)的全部 33 个枚举值,与 Go 侧常量 `internal/events/event.go:26-71` 逐一对应(字节一致,已核对)。

- producer 位置 = 实际调用 `Publish`/`writeEvent` 产生该事件类型的代码行。
- consumer 位置 = 前端消费点。所有 33 个类型都在 `web/src/lib/events.ts:54-66` 注册了 EventSource listener(通用分发),`web/src/lib/use-event-stream.ts:15-44` 做缓存失效;下表 consumer 列只列**超出通用分发的具体消费点**,没有则写 `仅通用分发(events.ts:54)`。
- 场景 ID 见任务场景表 S1–S12。

| 项目 | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| PARTY_DIALING | internal/telephony/coordinator.go(addParty,2026-08-23 W7) | 仅通用分发(events.ts:54;use-event-stream.ts:25 使 CALLS 缓存失效) | COVERED(生产者) | [FACT] | **2026-08-23 W7 补上生产者**:发起腿(`Role == ORIGINATOR`,以 DIALING 创建)在 addParty 时宣告。作用域同其它 leg 事件——有坐席则只发给他本人,无坐席(主叫自己的腿)则不进任何坐席的流,只有主管看得到。此前订阅方看到的第一条永远是 PARTY_ESTABLISHED,party 一出现就已在通话,FSM 的起点在流上隐形。载荷取**本腿自己的号 + 目的号**(不是 PARTY_RINGING 那对被叫视角的号),且目的号仅在其为数字时下发 —— 浏览器话机被拨的地址是注册令牌(实测 `doskp0mj`)。三通现场分机互拨验证 |
| PARTY_RINGING | internal/telephony/coordinator.go:404-417(addParty,坐席腿) | web/src/lib/use-event-stream.ts:25(CALLS 失效→软电话/主管视图刷新) | S4, S5 | [FACT] | 仅在"腿投递给已签入坐席"时发布并附 screen-pop 上下文;非坐席腿的 RINGING 不发布 |
| PARTY_ESTABLISHED | internal/telephony/registry.go:336(transition on CHANNEL_ANSWER) | use-event-stream.ts:25 | S1, S4, S7 | [FACT] | payload=role/state(registry.go:405) |
| PARTY_HELD | internal/telephony/registry.go:338(CHANNEL_HOLD) | use-event-stream.ts:25 | S7 | [FACT] | |
| PARTY_RETRIEVED | internal/telephony/registry.go:340(CHANNEL_UNHOLD) | use-event-stream.ts:25 | S7 | [FACT] | |
| PARTY_RELEASED | internal/telephony/registry.go:349(CHANNEL_HANGUP) | use-event-stream.ts:25 | S2, S4, S5, S6 | [FACT] | payload 增 cause / isTransferredAway(registry.go:406-409) |
| PARTY_CHANGED | internal/telephony/coordinator.go:555-562(merge,reason=CALL_MERGED);coordinator.go:694-700(mute,payload isMuted) | use-event-stream.ts:25 | S3, S4, S7 | [FACT] | merge 场景 2026-08-18 现场缺陷的修复(coordinator.go:536-544 注释) |
| PARTY_DTMF | internal/telephony/registry.go:388(ESL DTMF) | 仅通用分发(events.ts:56) | S7(VC-S7-04 已挂,TODO) | [FACT] | 2026-08-20 勘误:已挂 VC-S7-04(双向按键) |
| CALL_USER_DATA | NOT FOUND | use-event-stream.ts:25(有专门分支) | UNCOVERED | [FACT] | 常量 event.go:38;无 publish 点。userData 实际作为字段搭其它事件下发(registry.go:425),独立事件从未发出 |
| CALL_RECORDING_STARTED | internal/telephony/registry.go(actor,2026-08-23 W7①) | 仅通用分发(events.ts:57) | COVERED(生产者) | [FACT] | RECORD_START 早已归一化,只是从来没人消费。现由 call actor 以**通话作用域**发布(`publish(t, nil, nil)`)——录不录音是通话的事,不是某条腿的事。**不带交换机的文件路径**:那是交换机自己磁盘上的路径,浏览器拿着没用,录音按 call_id 走 recordings API 取 |
| CALL_RECORDING_STOPPED | internal/telephony/registry.go(actor,2026-08-23 W7①) | 仅通用分发(events.ts:57) | COVERED(生产者) | [FACT] | 同上 |
| CALL_CDR | internal/telephony/registry.go:351-354(Finish 时) | web/src/lib/use-event-stream.ts:39-43(CDRS+REPORTS 失效) | S2, S4, S6 | [FACT] | 纯 bot 呼叫的主叫腿同样在 registry 中,挂断也发 CALL_CDR(即 S1/S2 覆盖);CDR 行归属另判(cdr.go:79) |
| CALL_TRANSCRIPT | internal/transcript/actor.go:213(partial)、actor.go:261(final) | web/src/lib/transcript.ts:162-171(useEventListener);web/src/components/live-transcript.tsx | S1, S9 | [FACT] | partial 只上流不入库(actor.go:212-214) |
| CALL_TRANSCRIPTION_STATE | internal/transcript/actor.go:190(State());状态源:internal/streamin/streamin.go:391(CONNECTING)、:429(ERROR);internal/streamin/session.go:57(ERROR)、:60(LIVE)、:217(DEGRADED)、:232(STOPPED) | web/src/lib/transcript.ts:173-179;live-transcript.tsx:30-32 | S9 | [FACT] | IDLE 仅作快照默认值(actor.go:117-124),从不发布;ENDED 定义(actor.go:110)从不发布 |
| QUEUE_JOINED | internal/telephony/waiting.go:238-249 | use-event-stream.ts:30-35(WAITING 失效);_app.agent.index.tsx(等待列表) | S3, S6 | [FACT] | scope=QueueID(waiting.go:249) |
| QUEUE_LEFT | internal/telephony/waiting.go:276-282 | use-event-stream.ts:30-35 | S3, S5, S6 | [FACT] | payload 带 cause/cancelReason/waitSec(waiting.go:262-274) |
| QUEUE_COUNT | internal/telephony/waiting.go:293-297 | use-event-stream.ts:30-35 | S3, S6 | [FACT] | |
| QUEUE_AGENT_OFFERED | internal/telephony/coordinator.go:590-600 | use-event-stream.ts:30-35 | S4, S5 | [FACT] | 由 mod_callcenter agent-offering 触发(switchevent.go:358-359) |
| AGENT_LOGGED_IN | internal/agents/service.go:228 | use-event-stream.ts:16-24(ROSTER/PRESENCE/REPORTS/WRAP_UP 失效) | S11 | [FACT] | |
| AGENT_LOGGED_OUT | internal/agents/service.go:235 | use-event-stream.ts:16-24 | S11 | [FACT] | |
| AGENT_READY | internal/agents/service.go:248 | use-event-stream.ts:16-24 | S4, S11 | [FACT] | |
| AGENT_NOT_READY | internal/agents/service.go:255(NotReady)、:274(StartWrapUp)、:343(RingNoAnswer,无调用方) | use-event-stream.ts:16-24 | S4, S11 | [FACT] | :343 路径死代码(见 fsm-edges.md) |
| AGENT_AVAILABILITY | internal/agents/service.go:362(SetOnCall 变化时) | use-event-stream.ts:16-24 | S4, S11 | [FACT] | SetOnCall 由 coordinator.go:419(振铃)/:258(挂断)驱动 |
| DEVICE_REGISTERED | NOT FOUND | 仅通用分发(events.ts:61;ROSTER 失效) | UNCOVERED | [FACT] | 交换机事件 KindDeviceRegistered 存在(switchevent.go:288-300)且驱动 ObserveDevice(cmd/aicc/main.go:399-400);2026-08-22 C28 后 ObserveDevice 按方向发布,但**恢复那一侧仍发 DEVICE_IN_SERVICE**(service.go:409),此类型依旧从未发出 |
| DEVICE_UNREGISTERED | internal/agents/service.go:409-411(ObserveDevice,话机不可达时) | 仅通用分发(events.ts:61) | S13(VC-S13-05 待重跑) | [FACT] | 2026-08-22 C28 修正:此前上线掉线一律发 DEVICE_IN_SERVICE,掉线那条顶着相反的类型名 |
| DEVICE_IN_SERVICE | internal/agents/service.go:409(ObserveDevice,话机可达时) | use-event-stream.ts:16-24;_app.tsx:41 / _app.supervisor.agents.tsx:27(DEVICE_UNREACHABLE 展示) | S11, S12 | [FACT] | 注册与 keepalive 两路信号都汇到它(main.go:399-405) |
| BOT_SESSION_STARTED | NOT FOUND | 仅通用分发(events.ts:62) | UNCOVERED | [FACT] | aicall 路径完全不向 events.Hub 发布(internal/aicall 全包无 Publish 调用);bot 会话开始只有日志 orchestrator.go:310 |
| BOT_INTERRUPTED | NOT FOUND | 仅通用分发(events.ts:63) | UNCOVERED | [FACT] | barge-in 只是 aicall 内部事件(session.go:514) |
| BOT_SESSION_ENDED | NOT FOUND | 仅通用分发(events.ts:63) | UNCOVERED | [FACT] | |
| CALLBACK_CREATED | cmd/aicc/wiring.go:309-317(announceCallback,IsBroadcast;由 internal/aicall/actions.go:118 触发) | use-event-stream.ts:36-38;_app.agent.callbacks.tsx | S3(VC-S3-04 已挂,TODO) | [FACT] | 2026-08-20 勘误:已挂 VC-S3-04 |
| CALLBACK_UPDATED | internal/httpapi/ledger_handlers.go:185(Claim)、:217(Complete),经 publishCallback :222-… | use-event-stream.ts:36-38;_app.agent.callbacks.tsx:130-148 | S3(VC-S3-04 已挂,TODO) | [FACT] | 2026-08-20 勘误:已挂 VC-S3-04 |
| SYSTEM_LINK | cmd/aicc/wiring.go(announceLink,2026-08-23 W7③) | 仅通用分发 | COVERED(生产者) | [FACT] | 挂在 `esl.Link` 的 `OnConnect`/`OnLost` 上,两个方向都发(`{isUp:true|false}`),**广播作用域** —— 交换机没了是坐席既看不见也绕不过去的那种故障,而在此之前唯一的迹象就是"什么都不再发生了"。`OnLost` 这个钩子此前**写好从没被调用**,与 `ListQueueMembers` / `ShowChannels` / `LoadPresence` 同型 |
| SYSTEM_RESET | internal/httpapi/events_handler.go:79-88(resume 点超出 ring 时) | web/src/lib/events.ts:49(onReset);use-event-stream.ts:86(全量 invalidateQueries) | S10 | [FACT] | 判定条件 hub.go:129-133(oldest==0 或 lastEventID+1 < oldest) |

## 缺口汇总

### 需删除(或降级出契约)
- 无强删除建议——以下"需实现"项若产品决定不做,应从 `SseEventType` 契约与 `internal/events/event.go` 同步移除:~~PARTY_DIALING~~(2026-08-23 W7 起有生产者)、CALL_USER_DATA、~~CALL_RECORDING_STARTED/STOPPED~~(2026-08-23 W7① 起有生产者)、DEVICE_REGISTERED、~~DEVICE_UNREGISTERED~~(2026-08-22 C28 起有生产者)、BOT_SESSION_STARTED、BOT_INTERRUPTED、BOT_SESSION_ENDED、~~SYSTEM_LINK~~(2026-08-23 W7③ 起有生产者)(原 10 个契约内类型零生产者,现 5 个)。删除属于 breaking change,需走 `make api-breaking`。

### 需实现(契约已承诺、代码未生产)——【2026-08-20 决议 D6:以下全部实现 → TASKS W7(四组推进);"需删除"选项作废】
- ~~PARTY_DIALING — producer NOT FOUND(event.go:26 仅定义)。~~ **已实现(2026-08-23,W7)。**
- CALL_USER_DATA — producer NOT FOUND,**且不应补**(2026-08-23 查明,C42):
  `userData` 没有任何写入路径(建呼叫时设不了、无接口可改、`MergeUserData` 只有测试调用),
  **没有变更就没有变更事件**。要做须先建写入能力,那是产品决策。
- CALL_RECORDING_STARTED / CALL_RECORDING_STOPPED — RECORD_START/STOP 已订阅并归一化(switchevent.go:33-34、:259-264)但事件链在 coordinator 处中断。
- DEVICE_REGISTERED / ~~DEVICE_UNREGISTERED~~ — 交换机侧信号已达 ObserveDevice。**2026-08-22 C28 已区分发布**:
  不可达发 DEVICE_UNREGISTERED,可达发 DEVICE_IN_SERVICE。剩 DEVICE_REGISTERED 一个 ——
  它与 DEVICE_IN_SERVICE 的分工要先定语义(注册成功 vs 可服务),不是补一行 publish 的事。
- BOT_SESSION_STARTED / BOT_INTERRUPTED / BOT_SESSION_ENDED — aicall 路径无 Hub 依赖,三类型全空。
- SYSTEM_LINK — 无发布点(ESL 断连/重连当前只有日志 + OnConnect 钩子)。

### 需补场景(有生产者、S1–S12 未覆盖)
- ~~PARTY_DTMF~~ 已挂 VC-S7-04(2026-08-20 勘误)。
- ~~CALLBACK_CREATED / CALLBACK_UPDATED~~ 已挂 VC-S3-04(2026-08-20 勘误:原笔误 VC-SX-CB-01)。
