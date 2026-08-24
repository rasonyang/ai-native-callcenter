# 状态机迁移边覆盖清单(fsm-edges.md)

**范围说明(先读)**:任务指定的 `internal/actor/` 目录**不存在**(NOT FOUND,已用 `ls` 证实)。本仓库的 actor-per-call 实现位于:

- 呼叫/腿状态机:`internal/telephony/call.go`(转移表)+ `internal/telephony/registry.go`(每呼叫一个 actor goroutine,registry.go:82)
- 坐席在场状态机:`internal/agents/state.go` + `service.go`(互斥锁保护,非 actor,但为任务口径中的"坐席状态机")
- 转写 actor:`internal/transcript/actor.go`(每呼叫一个,seq 独占分配)
- 流程(flow)阶段机:数据驱动(见 §5),边在已发布的 flow JSON 里,不在代码里

## 1. Party 状态机(转移表 internal/telephony/call.go:66-87;应用 apply call.go:132-145;非法转移拒绝并告警 registry.go:396-404)

> **规格是 `docs/design/01-telephony.md` §2 的状态图**(owner 2026-08-24)。本表记录的是**实现**的边;
> 实现与规格之间尚存的差(无 IDLE / 无 QUEUED / DIALING→RINGING 规格允许而实现禁止)登记在 C56。

| 项目(from → event → to) | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| (新originator腿) → AddParty → DIALING | call.go:208-209 | coordinator.adopt→addParty(coordinator.go:379-397) | S1 | [FACT] | 首腿=originator |
| (新target腿) → AddParty → RINGING | call.go:207 | 同上;坐席腿发 PARTY_RINGING(coordinator.go:404-417) | S4, S5 | [FACT] | |
| DIALING → ANSWER → TALKING | call.go:68 | **registry.establish(),由 CHANNEL_BRIDGE 触发**(2026-08-24 C55 起;此前为 CHANNEL_ANSWER) | S1(主叫腿被桥接) | [FACT] | 应答只记 AnsweredAt,不再迁移状态 |
| DIALING → RELEASE → RELEASED | call.go:69 | registry.go:341-349 | S1(VC-S1-03 已挂,TODO) | [FACT] | 2026-08-20 勘误:未知号 95999 在 answer 前即挂断(lua:47-49),VC-S1-03 正验证此边 |
| RINGING → ANSWER → TALKING | call.go:72 | **registry.establish(),由 CHANNEL_BRIDGE 触发**(C55) | S4 | [FACT] | 一次桥接把两条腿一起送进 TALKING |
| RINGING → RELEASE → RELEASED | call.go:73 | registry.go:341-349 | S5, S6 | [FACT] | 振铃未接即挂(RONA 重排队/振铃中放弃) |
| TALKING → HOLD → HELD | call.go:76 | registry.go:337-338(→PARTY_HELD) | S7 | [FACT] | |
| TALKING → RELEASE → RELEASED | call.go:77 | registry.go:341-349 | S2, S4 | [FACT] | |
| TALKING → ANSWER → TALKING(幂等自环) | call.go:78-81 | registry.go:335-336 | S4 | [INFERENCE] | FS 对同一腿可能同时报 CHANNEL_ANSWER 与 bridge(注释 call.go:78-79);S4 会自然触发但无专门断言——备注:验证方法为 S4 期间 grep 日志无 "rejected party transition" |
| HELD → RETRIEVE → TALKING | call.go:83 | registry.go:339-340(→PARTY_RETRIEVED) | S7 | [FACT] | |
| HELD → RELEASE → RELEASED | call.go:84 | registry.go:341-349 | UNCOVERED | [FACT] | 保持中挂断;S7 未含此变体。需补场景(VC-S7-03 已补) |
| RELEASED →(任何)→ 拒绝(终态) | call.go:86(空表) | registry.go:400(warn "rejected party transition") | S4 | [FACT] | 迟到/乱序事件的正常防线 |
| 表外任意组合(如 DIALING→HOLD)→ 拒绝 | call.go:133-135(ErrIllegalTransition) | registry.go:396-404 | UNCOVERED | [FACT] | 需补场景:注入乱序事件(可用 fs_cli uuid_hold 对未接腿)验证仅告警不崩溃 |

## 2. Call 聚合状态机(internal/telephony/call.go:16-25)

| 项目(from → event → to) | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| (新建) → CreateCall → CREATED | call.go:194-202(NewCall);registry.go:95-121 | 快照/API | S1 | [FACT] | |
| CREATED → AddParty → RUNNING | call.go:220-222 | 同上 | S1, S4 | [FACT] | |
| RUNNING → 最后一腿 RELEASE(Finish)→ ENDED | call.go:310-317;触发 registry.go:350-361 | OnCallFinished→CDR(cdr.go:72)+转写收官(wiring.go:115);OnCallRetired→tap 收官(wiring.go:121) | S2, S4, S6 | [FACT] | ENDED 后 actor 自停(registry.go:360) |
| ~~* → ENDING → *~~ | — | — | 已删除 | [FACT] | 2026-08-24 C4:状态与契约枚举一并删除 |
| RUNNING → Retire(被合并吸收)→ actor 退出(无 ENDED) | registry.go:224-231;coordinator.go:525(merge) | OnCallRetired(registry.go:267-274) | S3, S4 | [FACT] | 被吸收呼叫不产生 CDR/CALL_CDR(coordinator.go:487 注释) |
| * → Shutdown → actor 退出 | registry.go:241-253 | 同上 | S12 | [FACT] | 优雅关闭停所有 actor |
| 身份改写:provisional → minted(reidentify/merge) | coordinator.go:297-326;merge :488-528 | announceMerge→PARTY_CHANGED(coordinator.go:545-563) | S1, S3, S4 | [FACT] | 2026-08-18 现场缺陷补的通告 |

## 3. 坐席在场状态机(internal/agents/state.go;所有变更经 service.change 持久化+镜像+发布 service.go:553-580,持久化失败回滚 :569-575)

| 项目(from → event → to) | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| LOGGED_OUT → Login → NOT_READY(LOGIN) | state.go:102-112;service.go:187-230 | 镜像 mirrorRegistration(service.go:595-626,含 tier 收敛 :624);SSE AGENT_LOGGED_IN | S11 | [FACT] | 守卫:非 LOGGED_OUT 拒绝(ErrAlreadyLoggedIn state.go:104);分机被占拒绝(service.go:204-207) |
| 已签入 → Logout → LOGGED_OUT | state.go:115-125;service.go:234-244 | mirrorStatus("Logged Out" state.go:226);SSE AGENT_LOGGED_OUT | S11 | [FACT] | 忘记 lastWrapUpCall(service.go:240) |
| 已签入 → Ready → READY | state.go:128-137;service.go:247-251 | mirrorStatus("Available");SSE AGENT_READY | S4, S11 | [FACT] | 任何签入态可达,提前终止 ACW(clearWrapUp :135) |
| 已签入 → NotReady(reason) → NOT_READY(reason) | state.go:140-152;service.go:254-258 | mirrorStatus("On Break");SSE AGENT_NOT_READY | S11 | [FACT] | 守卫:未知 reason 拒绝(state.go:144-146) |
| 已签入 → StartWrapUp(callID) → NOT_READY(AFTER_CALL_WORK)+wrap_up_call_id | state.go:161-174;入口 service.go:267-294(REST)与 :303-308(交换机路径 ← coordinator.go:262-264) | OpenWrapUp 开单(service.go:289);SSE AGENT_NOT_READY | S4 | [FACT] | 无时限(state.go:156-160 注释);只有已应答的腿触发(coordinator.go:259-264) |
| NOT_READY(AFTER_CALL_WORK) → EndWrapUp → READY | service.go:331-339 | 经 Ready 全链 | S4 | [FACT] | 非 ACW 态则原地不动(:335-337) |
| 已签入 → RingNoAnswer → NOT_READY(SYSTEM) | state.go:189-194;service.go:342-346 | — | UNCOVERED | [FACT] | **无调用方(死边)**;2026-08-20 决议 D7②:实现全链(含 missed_reason 互斥修复)→ TASKS W2;落地后 VC-S5-01 由 T6.9 改写 |
| 守卫:未签入做任何变更 → 拒绝 | state.go:116/:129/:141(ErrNotLoggedIn) | agent_handlers 映射 AGENT_NOT_LOGGED_IN | S11 | [FACT] | |
| 观测位(非 FSM 态):IsOnCall 翻转 | service.go:350-364(SetOnCall ← coordinator.go:419/:258) | Availability 派生 state.go:201;SSE AGENT_AVAILABILITY | S4 | [FACT] | 先摘 on-call 再开 ACW 的顺序是刻意的(coordinator.go:254-258 注释) |
| 观测位:IsRegistered/IsDeviceInService | service.go:371-396(ObserveDevice ← main.go:399-405;登录时回填 :217;重连对账 wiring.go:138-146) | Availability(DEVICE_UNREACHABLE state.go:207-210);SSE DEVICE_IN_SERVICE | S11, S12 | [FACT] | |
| 恢复:重启 → Restore 回灌持久化状态 | service.go:505-523(main.go:142) | 签入不丢(S12) | S12 | [FACT] | wrap_up_call_id 一并恢复(:516-519) |

## 4. 转写 actor 状态(internal/transcript/actor.go:103-111;字符串状态,无守卫表,后写覆盖先写)

| 项目(from → event → to) | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| (默认)IDLE(仅快照) | actor.go:117-124 | transcript_handlers.go:29(快照);web transcript.ts:108-134 | S9 | [FACT] | 从不作为事件发布 |
| * → tap 已受理 → CONNECTING | streamin/streamin.go:385-403(Expect) | SSE CALL_TRANSCRIPTION_STATE;live-transcript.tsx:30 | S9 | [FACT] | |
| CONNECTING → 双 ASR 启动成功 → LIVE | streamin/session.go:44-62 | 同上 | S9 | [FACT] | |
| CONNECTING → 12s 未回拨 → ERROR(STREAM_NEVER_CONNECTED) | streamin/streamin.go:415-431(connectGrace :381) | 同上 | S9 | [FACT] | +OK≠已连接(tap.go:177-181 注释) |
| 启动失败 → ERROR(ASR_START_FAILED/ASR_NEVER_STARTED) | session.go:47-58 | 同上 | S9 | [FACT] | |
| LIVE → 单侧 ASR 故障 → DEGRADED(degradedSpeakers) | session.go:212-218 | 同上 | S9 | [FACT] | |
| * → 流结束/关闭 → STOPPED | session.go:222-233(closeLocked) | 同上 | S9 | [FACT] | |
| * → ENDED | NOT FOUND | — | UNCOVERED | [FACT] | 死值(enums.md §18) |
| actor 生命期:呼叫结束 → Close(排空邮箱) | registry.Close(actor.go:370-378)← wiring.go:115(retireTranscriptWithCall) | — | S2, S4 | [FACT] | 不退休即每呼叫泄漏一个 goroutine(wiring.go:160-165 注释) |

## 5. 流程(flow)阶段机 —— 数据驱动,边不在代码内

引擎只有两个通用迁移原语:工具结果触发 `fire`(internal/flow/engine.go:155-…,终态短路 :156)与无输入触发 `OnNoInput`(engine.go:128-136),进入新阶段 `enter`(engine.go:202);终态判定 `IsTerminal`(engine.go:59-61,spec.go:182-183);跑飞环守卫 `guardRunawayLoop`(engine.go:138-…)。**具体边由已发布 flow 定义**(本机 demo 流:internal/seed/flows/novanet_support.json),装载期校验(internal/flow/load.go)。终态触发收官动作:orchestrator.afterMove(orchestrator.go:377-398,armed 收线)。

| 项目 | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| 阶段边(entry→…→terminal,按 novanet_support.json) | flow JSON(数据) | engine.fire/enter | S1, S3 | [FACT](机制)/[ASSUMPTION](具体边) | 具体边未逐条核对 JSON。验证:`jq '.nodes[] | {id, transitions}' internal/seed/flows/novanet_support.json` 与通话日志 "flow reached a terminal phase"(orchestrator.go:387)对照 |

## 6. aicall.Session 隐式回合机(布尔/代数状态,无枚举;列出以防"事实上的状态机"漏检)

| 项目 | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| 静默 → ResponseStarted → 说话中(beginTurn:turnSeq++,双代失效) | session.go:431-432,:538-546 | actions.onTurnDone/onPlaybackDone 用回合号排序(actions.go:166-185) | S1 | [FACT] | |
| 说话中 → ResponseDone → 排空监视(endTurn→playbackDone 邮箱) | session.go:434-437,:556-570 | watchPlayback(:589-609)→ PLAYBACK_DONE | S1 | [FACT] | TURN_DONE≠PLAYBACK_DONE(硬性不变量) |
| 排空完成 → PLAYBACK_DONE → 死气计时(idle 代) | session.go:600-607,:640-660 | orchestrator.handleDeadAir(:402-424) | S1 | [FACT] | 双代计数器分离是 2026-08 修过的现场缺陷(session.go:154-162 注释) |
| 任意播放态 → 说话检测/按键 → barge-in(本地先清队) | session.go:409-416,:475-515(守卫 800ms :496;尾音仅本地清 :509-513) | actions.onBargeIn(:195-206,已说完closing line则立即执行 armed) | S1, S2 | [FACT] | |
| armed 动作:tool 到达回合 n,等 turn>n 的 PLAYBACK_DONE,10s 封顶 | actions.go:139-176;上限 orchestrator.go:43 | 转接/挂断执行 | S1, S3 | [FACT] | |
| 主叫挂断 → watchLeg → Close(全 pump 收敛→ENDED) | session.go:728-739,:274-281 | orchestrator.onCallEnded(:187-204,RTP 统计入 obs) | S2 | [FACT] | |

## 缺口汇总

### 需删除(死代码/死状态)
- ~~Call 状态 `ENDING`(call.go:23)~~ —— D7① **已于 2026-08-24 执行**(C4):契约 → generate → 实现,`make api-breaking` 报无破坏(响应侧枚举收窄)。
- `Presence.RingNoAnswer` 全链——2026-08-20 决议 D7②:**实现**(消费 QUEUE_AGENT_STATE,含 missed_reason 互斥修复)→ TASKS W2。
- 转写状态 `ENDED`(actor.go:110)——2026-08-20 决议 D7③:**先删除** → TASKS C4(breaking)。

### 需实现
- RONA 信号消费——2026-08-20 决议 D7②:**实现** → TASKS W2(此前 S5 在应用侧不可观测的判断维持,作为 W2 的验收基线)。

### 需补场景(边存在、S1–S12 未覆盖)
- ~~Party:DIALING→RELEASE→RELEASED~~ 2026-08-20 勘误:VC-S1-03(未知号 answer 前挂断)已覆盖此边。
- Party:HELD→RELEASE→RELEASED(保持中挂断)→ 已在 ledger.yaml 补 VC-S7-03。
- Party:非法迁移拒绝路径(乱序事件仅告警)→ 已在 ledger.yaml 以 failure_looks_like/日志断言覆盖(VC-S4-02 的 grep)。
- TALKING→ANSWER 幂等自环:S4 天然触发但无断言 → VC-S4-02 的日志断言("rejected party transition" 计数为 0)间接覆盖。
