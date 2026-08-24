# SCREAMING_SNAKE 枚举覆盖清单(enums.md)

来源:① `docs/openapi.json` 全部 enum(jq 穷举,22 处,含 2 个内联);② 迁移文件全部 CHECK 约束;③ Go 内部 SCREAMING_SNAKE 枚举(boundary 词表)。producer=赋值/产生该值的代码;consumer=按值分支/展示/校验该值的代码。openapi 行号为 schema 定义首行。

## 1. Role(openapi.json:3592;DB CHECK 00001:15)

| 项目 | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| Role=AGENT | internal/seed/seed.go:61-63;cmd/aicc/useradd.go | internal/httpapi/events_handler.go:49(AtLeast);web/src/lib/guards.ts | S11 | [FACT] | |
| Role=SUPERVISOR | internal/seed/seed.go:60 | _app.supervisor.*.tsx:18/:24/:33(requireRole);events/hub.go:225(IsSupervisor 全量视图) | S3, S11 | [FACT] | |
| Role=ADMIN | internal/seed/seed.go:59;cmd/aicc/useradd.go | web/src/lib/guards.ts;admin 路由 beforeLoad | S11 | [FACT] | |

## 2. AgentState(openapi.json:3660;DB CHECK 00002:50)

| 项目 | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| LOGGED_OUT | internal/agents/state.go:119(Logout);零值归一 :90-95 | state.go:219-227(→"Logged Out");_app.supervisor.agents.tsx:54 | S11 | [FACT] | |
| NOT_READY | state.go:107(Login)、:147(NotReady)、:165(StartWrapUp) | state.go:203-206(availability);supervisor.agents.tsx:26 | S4, S11 | [FACT] | |
| READY | state.go:132(Ready) | state.go:221(→"Available");supervisor.queues.tsx:96 | S4, S11 | [FACT] | |

## 3. NotReadyReason(openapi.json:3669;DB CHECK 00002:52)

| 项目 | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| LOGIN | internal/agents/state.go:107 | web(reason 徽标,经 AGENT_NOT_READY payload service.go:647) | S11 | [FACT] | 签入即 NOT_READY(LOGIN) |
| BREAK | agent_handlers.go:106(API 入参)+ web/src/lib/agent.ts:30(SELECTABLE_REASONS) | state.go:203-206;roster 展示 | S11 | [FACT] | |
| LUNCH | 同上(lib/agent.ts:30) | 同上 | S11 | [FACT] | |
| TRAINING | 同上(lib/agent.ts:30) | 同上 | S11 | [FACT] | |
| AFTER_CALL_WORK | state.go:166(StartWrapUp) | state.go:178(IsInWrapUp)、:203(→WRAP_UP) | S4 | [FACT] | |
| SYSTEM | state.go:193(RingNoAnswer)——但 RingNoAnswer 无调用方 | state.go:203-206 | UNCOVERED | [FACT] | 运行时不可达(死值,见 fsm-edges.md) |
| SUPERVISOR | NOT FOUND | —(仅 CHECK/契约允许) | UNCOVERED | [FACT] | ForceLogoutAgent 走 Logout(agent_handlers.go:241-244),不产生此值 |

## 4. Availability(openapi.json:3682;派生,不入库)

| 项目 | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| LOGGED_OUT | internal/agents/state.go:199-200 | _app.supervisor.agents.tsx:28;_app.tsx:40 | S11 | [FACT] | 派生优先级见 state.go:197-214 |
| ON_CALL | state.go:201-202(IsOnCall) | supervisor.agents.tsx:24 | S4 | [FACT] | SetOnCall:coordinator.go:419/:258 |
| WRAP_UP | state.go:203 | supervisor.agents.tsx:25 | S4 | [FACT] | |
| NOT_READY | state.go:205-206 | supervisor.agents.tsx:26 | S11 | [FACT] | |
| DEVICE_UNREACHABLE | state.go:207-210 | supervisor.agents.tsx:27;_app.tsx:41 | S11, S12 | [FACT] | 崩溃的浏览器标签页与好电话长得一样(注释 :208-209) |
| READY | state.go:212 | supervisor.queues.tsx:96 | S4, S11 | [FACT] | |

## 5. CallType(openapi.json:3977;DB CHECK 00005:14)

| 项目 | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| INBOUND | telephony/coordinator.go:902-905(callTypeOf);aicall/ledger.go:217 | cdrs.call_type;web CDR 列表 | S1–S9 | [FACT] | |
| OUTBOUND | coordinator.go:908;aicall/orchestrator.go:241-244(X-Aicc-Call-Type);outbound | _app.agent.calls.tsx:227;agent.index.tsx:112 | UNCOVERED | [FACT] | 外呼不在 S1–S12;需补场景 |
| CONSULT | NOT FOUND | 契约/CHECK 允许 | UNCOVERED | [FACT] | callTypeOf 只产 INBOUND/INTERNAL/OUTBOUND;死值 |
| INTERNAL | coordinator.go:906(inbound 且非 public context) | 同上 | UNCOVERED | [FACT] | 分机互拨;需补场景 |

## 6. CallState(openapi.json:3987)

| 项目 | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| CREATED | telephony/call.go:198(NewCall) | Snapshot→GET /calls | S1 | [FACT] | |
| RUNNING | call.go:220-222(首腿加入) | 同上;web 呼叫视图 | S1, S4 | [FACT] | |
| ~~ENDING~~ | — | — | 已删除 | [FACT] | 2026-08-24 C4:定义、契约枚举、生成物一并删除。呼叫在最后一条腿释放时结束,是一个事件,从来没有一个时刻处在 ENDING |
| ENDED | call.go:314(Finish) | registry.go:350 后续钩子 | S2, S4, S6 | [FACT] | |

## 7. PartyState(openapi.json:4003)/ PartyRole(openapi.json:3996)

| 项目 | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| DIALING | call.go:209(originator 初值) | components/active-call.tsx;transition payload registry.go:405 | S1 | [FACT] | |
| RINGING | call.go:207(target 初值) | 同上 | S4, S5 | [FACT] | |
| TALKING | call.go:68/:72/:83(ANSWER/RETRIEVE) | 同上 | S1, S4, S7 | [FACT] | |
| HELD | call.go:76(HOLD) | 同上 | S7 | [FACT] | |
| RELEASED | call.go:69/:73/:77/:84(RELEASE) | cdr split(cdr.go:320-333) | S2, S4, S5, S6 | [FACT] | |
| ORIGINATOR | call.go:209;merge 归一 coordinator.go:857-874 | cdr.go:324(from_number 来源) | S1 | [FACT] | |
| TARGET | call.go:207;coordinator.go:871(降级) | cdr.go:328 | S4 | [FACT] | |

## 8. ExtensionKind(openapi.json:4327;DB CHECK 00002:19)

| 项目 | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| AGENT | seed.go:212-216('AGENT' 字面量);CreateExtension catalog_handlers.go:48 | luacc.directory(不过滤 kind,00002:157-165);admin.extensions.tsx | S4, S11 | [FACT] | |
| BOT | catalog/types.go:16-21 校验域;仅管理员手工建 | 同上 | UNCOVERED | [FACT] | seed 不产;bot 网关走 FS gateway 而非 extensions 行。需补场景或明确用途 |
| PLAIN | 同上 | 同上 | UNCOVERED | [FACT] | 需补场景 |

## 9. Strategy(openapi.json:4411;DB CHECK 00002:79)

| 项目 | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| LONGEST_IDLE_AGENT | DB 默认(00002:78)+ seed 队列 | luacc.queues→aicc_xml.lua:154(渲染 mod_callcenter) | S3, S4, S5 | [FACT] | 运行时实际生效的唯一值(seed 默认) |
| ROUND_ROBIN | catalog/types.go:63-76(校验域;管理员选) | 同上 | UNCOVERED | [FACT] | 其余 5 值需补场景(策略切换验证);策略语义属 mod_callcenter |
| TOP_DOWN | 同上 | 同上 | UNCOVERED | [FACT] | |
| AGENT_WITH_LEAST_TALK_TIME | 同上(web/src/lib/catalog.ts:35) | 同上 | UNCOVERED | [FACT] | |
| AGENT_WITH_FEWEST_CALLS | 同上(lib/catalog.ts:35) | 同上 | UNCOVERED | [FACT] | |
| RANDOM | 同上 | 同上 | UNCOVERED | [FACT] | |

## 10. OverflowType(openapi.json:4423)

| 项目 | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| ANNOUNCE_HANGUP | 视图默认(00002:197)+ 管理配置 | freeswitch/scripts/aicc_queue.lua:77、:121-127 | S6(超时挂断路径) | [FACT] | |
| BOT_FLOW | 管理配置(web/src/lib/catalog.ts:38) | aicc_queue.lua:79-111(回手 bot,带全套 X-AICC 头) | UNCOVERED | [FACT] | 需补场景(队列溢出回 bot 留言) |
| FORWARD | 管理配置 | aicc_queue.lua:113-119 | UNCOVERED | [FACT] | 需补场景 |

## 11. CDRStatus(openapi.json:4819;DB CHECK 00005:33)

| 项目 | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| ANSWERED | telephony/cdr.go:213,:224;aicall/ledger.go:149 | _app.agent.calls.tsx:33;报表 SQL ledger.sql:133-161 | S1, S2, S4 | [FACT] | |
| NO_ANSWER | cdr.go:229 | 同上 | S5, S6 | [FACT] | |
| BUSY | NOT FOUND | 契约/CHECK 允许;web :33 有着色 | UNCOVERED | [FACT] | 死值(无赋值方) |
| FAILED | aicall/ledger.go:151(endReason=FAILED) | 同上 | S2(provider 故障变体) | [FACT] | |

## 12. LegKind(openapi.json:4829)

| 项目 | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| TRUNK | NOT FOUND | CDR 详情 UI | UNCOVERED | [FACT] | 死值 |
| DIALING | telephony/cdr.go:315 | _app.admin.cdr.$callId.tsx(journey) | S6 | [FACT] | 仅当无其它腿时兜底 |
| BOT | aicall/ledger.go:179;cdr.go:283 | 同上 | S1, S2, S3 | [FACT] | |
| QUEUE | cdr.go:286 | 同上 | S3, S6 | [FACT] | |
| AGENT | cdr.go:300 | 同上 | S4, S5 | [FACT] | |

## 13. TranscriptKind(openapi.json:5044;DB CHECK 00005:62)

| 项目 | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| TEXT | aicall/ledger.go:69(say);streamin/session.go:190(ASR) | live-transcript.tsx;actor.go:222(空 final 防烧 seq) | S1, S9 | [FACT] | |
| TOOL_CALL | ledger.go:74(toolCall) | live-transcript.tsx;cdr 详情 | S1, S3 | [FACT] | |
| TOOL_RESULT | ledger.go:80(toolResult) | 同上 | S1, S3 | [FACT] | |

## 14. CallbackStatus(openapi.json:5198;DB CHECK 00006:8)+ CompleteCallbackRequest.status(内联)

| 项目 | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| OPEN | DB 默认 00005:103;InsertCallback ledger.sql:79(aicall/actions.go:112) | _app.agent.callbacks.tsx:24,:135 | UNCOVERED | [FACT] | 需补场景(take_message) |
| CLAIMED | ClaimCallback ledger_handlers.go:171(ledger.sql:115) | callbacks.tsx:142 | UNCOVERED | [FACT] | 同上 |
| DONE | CompleteCallback ledger_handlers.go:196(入参枚举 openapi CompleteCallbackRequest) | callbacks.tsx:130,:145 | UNCOVERED | [FACT] | 同上 |
| DISMISSED | 同上 | callbacks.tsx:148 | UNCOVERED | [FACT] | 同上 |

## 15. CreateCallRequest.kind(内联,openapi)

| 项目 | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| AI_OUTBOUND | API 调用方(POST /calls 入参) | outbound_handlers.go:50(CreateCall)→ outbound/outbound.go:206(DialAI) | UNCOVERED | [FACT] | 无 UI 调用方(web 零引用);外呼机器人不在 S1–S12 |

## 16. Speaker(openapi.json:5918;DB CHECK 00009:28)

| 项目 | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| CUSTOMER | aicall/orchestrator.go:325(模型转写);streamin/streamin.go:370(右声道) | live-transcript.tsx(讲话人徽标) | S1, S9 | [FACT] | |
| BOT | orchestrator.go:330;ledger.go:74/:80 | 同上 | S1 | [FACT] | |
| HUMAN_AGENT | streamin/streamin.go:368(左声道) | 同上;session.go:202(挂 agentId) | S9 | [FACT] | 声道→讲话人为实测映射(streamin.go:360-366 注释) |

## 17. TranscriptSource(openapi.json:5927;DB CHECK 00009:35)

| 项目 | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| MODEL | aicall/ledger.go:96;transcript/actor.go:233-235(默认) | live-transcript.tsx;DB CHECK | S1 | [FACT] | |
| ASR | streamin/session.go:194 | 同上 | S9 | [FACT] | |

## 18. TranscriptionState(openapi.json:5935;常量 transcript/actor.go:103-111)

| 项目 | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| IDLE | actor.go:117-124(快照默认,从不上流) | transcript.ts:108-134;GET transcript 快照(transcript_handlers.go:29) | S9 | [FACT] | 只出现在 REST 快照,永不作为事件发布 |
| CONNECTING | streamin/streamin.go:391(Expect) | transcript.ts:173;live-transcript.tsx:30 | S9 | [FACT] | |
| LIVE | streamin/session.go:60 | 同上 | S9 | [FACT] | |
| DEGRADED | session.go:217(单侧 ASR 失败) | live-transcript.tsx:32 | S9 | [FACT] | 带 degradedSpeakers(actor.go:187) |
| ERROR | session.go:57(ASR_START_FAILED/ASR_NEVER_STARTED);streamin.go:429(STREAM_NEVER_CONNECTED) | 同上 | S9 | [FACT] | |
| STOPPED | session.go:232(流关闭) | 同上 | S9 | [FACT] | |
| ENDED | **httpapi/transcript_handlers.go:68** | live-transcript.tsx / lib/transcript.ts:103 | S9-02 | [FACT] | **2026-08-24 更正:不是死值。** actor 从不赋值,但它是**快照**对一通已结束呼叫的回答(REST 收官合成),前端据此渲染且有专测。VC-S9-02 的 status 早在 2026-08-20 就写下了这一点,而 C4 条目直到 2026-08-24 仍写着死值 |

## 19. ErrorCode(openapi.json:3537;常量 httpapi/errors.go:21-37)

| 项目 | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| INVALID_CREDENTIALS | auth_handlers.go(Login 失败) | web/src/lib/errors.ts | S11 | [FACT] | |
| SESSION_EXPIRED | middleware.go:36-…(requireSession);events_handler.go:37 | 同上(前端登出跳转) | S10, S11 | [FACT] | |
| FORBIDDEN | 角色守卫(writeError 各处) | 同上 | S3(坐席访问主管资源) | [FACT] | |
| VALIDATION_FAILED | writeParamError(api_server.go)及各 handler | 同上 | S11 | [FACT] | |
| NOT_FOUND | 各 handler(如 GetCDR ledger_handlers.go:122) | 同上 | S4 | [FACT] | |
| CONFLICT | 7 处(唯一键冲突,errors.go:72 pg 23505 映射) | 同上 | S11 | [FACT] | |
| EXTENSION_IN_USE | agent_handlers.go(Login,ErrExtensionInUse 映射) | 同上 | S11 | [FACT] | |
| AGENT_ALREADY_LOGGED_IN | agent_handlers.go(Login) | 同上 | S11 | [FACT] | |
| AGENT_NOT_LOGGED_IN | agent_handlers.go | 同上 | S11 | [FACT] | |
| AGENT_NOT_IN_WRAP_UP | agent_handlers.go:172 区域(1 处) | 同上 | S4 | [FACT] | |
| CALL_NOT_FOUND | call_handlers.go:132(callOp) | 同上 | S7 | [FACT] | |
| NOT_CALL_PARTY | call_handlers.go(3 处;coordinator ErrNotCallParty 映射) | 同上 | S7 | [FACT] | |
| USER_SUSPENDED | auth(2 处) | 同上 | UNCOVERED | [FACT] | SUSPENDED 用户无产生途径(users 表行无写入方),运行时不可达 |
| SWITCH_DOWN | 2 处(ESL IsUp 失败映射) | 同上 | S12 | [FACT] | |
| STORAGE_DOWN | 31 处(DB 错误统一映射) | 同上 | S12 | [FACT] | |
| RATE_LIMITED | NOT FOUND(errors.go:36 仅定义,0 使用) | 契约允许 | UNCOVERED | [FACT] | 死值 |
| INTERNAL | writeError 兜底(events_handler.go:43 等) | 同上 | S12 | [FACT] | |

## 20. SseEventType(openapi.json:5734)——33 值逐值产/消见 `events.md`

此处只登记枚举归属与死值结论,避免双份维护:PARTY_DIALING、CALL_USER_DATA、CALL_RECORDING_STARTED、CALL_RECORDING_STOPPED、DEVICE_REGISTERED、DEVICE_UNREGISTERED、BOT_SESSION_STARTED、BOT_INTERRUPTED、BOT_SESSION_ENDED、SYSTEM_LINK 共 10 值 producer NOT FOUND([FACT],逐值证据在 events.md);其余 23 值有生产者且逐值定位于 events.md。

## 21. 仅存在于 DB CHECK 的枚举

| 项目 | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| users.status=ACTIVE | 默认值 00001:16;useradd | auth 登录校验 | S11 | [FACT] | |
| users.status=SUSPENDED | NOT FOUND | auth(USER_SUSPENDED 分支) | UNCOVERED | [FACT] | 无停用 API;死值 |
| cdrs.missed_reason=SHORT_ABANDONED | telephony/cdr.go:267-268 | CHECK 00005:36;CDR 详情 | S6(快挂变体) | [FACT] | <5s(cdr.go:45) |
| cdrs.missed_reason=ABANDONED_RINGING | cdr.go:255-259 | 同上 | S6 | [FACT] | |
| cdrs.missed_reason=ABANDONED_WAITING | cdr.go:270 | 同上 | S6 | [FACT] | |
| cdrs.missed_reason=AGENTS_DID_NOT_ANSWER | cdr.go:273-275 | 同上 | S5 | [FACT] | |
| cdrs.missed_reason=NO_AVAILABLE_AGENT | cdr.go:264-266(queue.Cause=="Timeout") | 同上 | S5, S6 | [FACT] | |
| cdrs.missed_reason=OUT_OF_HOURS | NOT FOUND | CHECK 允许 | UNCOVERED | [FACT] | 死值(营业时间路由未实现) |
| queue_events.event=JOINED | cdr.go:350(store 常量 ledgerstore.go:825) | httpapi ListCallQueueEvents | S3 | [FACT] | |
| queue_events.event=OFFERED | cdr.go:352 | httpapi ListCallQueueEvents | S4, S5 | [FACT] | |
| queue_events.event=BRIDGED | cdr.go:354 | httpapi ListCallQueueEvents | S4 | [FACT] | |
| queue_events.event=ABANDONED | cdr.go:361-362(Cause=="Cancel") | httpapi ListCallQueueEvents | S6 | [FACT] | |
| queue_events.event=LEFT | cdr.go:364 | httpapi ListCallQueueEvents | S3 | [FACT] | |
| recordings.backend=FS | internal/recording/fs.go(Backend()) | CHECK 00005:70;cdr.go:128 | UNCOVERED | [FACT] | 录音无场景;需补 |
| recordings.backend=S3 | internal/recording/s3.go(Backend());dev 默认(memory:project-dev-recording-seaweedfs) | 同上 | UNCOVERED | [FACT] | 同上 |
| trunks.direction=INBOUND/OUTBOUND/BIDIRECTIONAL | NOT FOUND | NOT FOUND | UNCOVERED | [FACT] | 表本身零使用(tables.md) |
| transcripts.role=BOT/CALLER(00005:61) | 已被 00009 迁移为 speaker(00009:57 仅 Down 保留) | — | — | [FACT] | 历史列;当前写路径走 speaker |

## 22. 内部 Go SCREAMING_SNAKE 枚举(boundary 词表,不上线契约)

| 项目 | producer 位置 | consumer 位置 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|
| SwitchEventKind=CHANNEL_CREATE | switchevent.go:227 | coordinator.go:173(adopt) | S1 | [FACT] | |
| CHANNEL_ANSWER | switchevent.go:229 | registry.go:335;outbound.go:313 | S1, S4 | [FACT] | |
| CHANNEL_PARK | switchevent.go:231 | NOT FOUND | UNCOVERED | [FACT] | 归一化后无消费者 |
| CHANNEL_BRIDGE | switchevent.go:233 | coordinator.go:175(join);registry.go:362 | S3, S4 | [FACT] | |
| CHANNEL_UNBRIDGE | switchevent.go:239 | coordinator.go:206(tap detach) | S7, S9 | [FACT] | |
| CHANNEL_HOLD / CHANNEL_UNHOLD | switchevent.go:241/:243 | registry.go:337-340;coordinator.go:202-205(tap 暂停/恢复) | S7 | [FACT] | |
| CHANNEL_HANGUP | switchevent.go:245 | registry.go:341;coordinator.go:206,:246;waiting.go:189;outbound.go:329 | S2, S4, S6 | [FACT] | |
| DTMF | switchevent.go:253 | registry.go:387 | UNCOVERED | [FACT] | 同 PARTY_DTMF |
| RECORD_START / RECORD_STOP | switchevent.go:259-264 | NOT FOUND | UNCOVERED | [FACT] | 订阅+归一化但无消费者(events.md 录音事件缺口的根) |
| DEVICE_REGISTERED / DEVICE_UNREGISTERED | switchevent.go:288-300 | cmd/aicc/main.go:399-400 | S11 | [FACT] | |
| DEVICE_STATE | switchevent.go:302-308 | main.go:401-405 | S11, S12 | [FACT] | OPTIONS ping |
| AUDIO_STREAM_CONNECTED/DISCONNECTED/ERROR | switchevent.go:310-323 | coordinator.go:186-194(仅日志) | S9 | [FACT] | |
| QUEUE_MEMBER_JOINED | switchevent.go:352 | waiting.go:185;registry.go:370;cdr.go:349;coordinator.go:225 | S3, S6 | [FACT] | |
| QUEUE_MEMBER_LEFT | switchevent.go:354 | waiting.go:187;registry.go:379;cdr.go:358 | S3, S5, S6 | [FACT] | |
| QUEUE_AGENT_OFFERED | switchevent.go:359 | coordinator.go:177,:225;cdr.go:351 | S4, S5 | [FACT] | |
| QUEUE_BRIDGE_START | switchevent.go:361 | waiting.go:187;registry.go:377;cdr.go:353 | S4 | [FACT] | |
| QUEUE_BRIDGE_END / QUEUE_BRIDGE_FAILED | switchevent.go:363/:366 | NOT FOUND | UNCOVERED | [FACT] | 归一化后无消费者(FAILED 含 CC-Hangup-Cause) |
| QUEUE_AGENT_STATE / QUEUE_AGENT_STATUS | switchevent.go:368/:372 | NOT FOUND | UNCOVERED | [FACT] | RONA 的原始信号在此,但无人消费(S5 缺口的根) |
| QUEUE_MEMBERS_COUNT | switchevent.go:374 | NOT FOUND | UNCOVERED | [FACT] | 计数实际由 waiting.go 自算 |
| PartyTrigger=ANSWER/HOLD/RETRIEVE/RELEASE | call.go:54-58 | call.go:66-87(转移表);registry.go:336-349 | S4, S7 | [FACT] | |
| CallDirection=INBOUND/OUTBOUND | switchevent.go:102-105 | coordinator.go:899-909;isBotLeg :290 | S1 | [FACT] | |
| aicall.EventType=READY/CUSTOMER_SAID/BOT_SAID/TOOL_CALL/TURN_DONE/PLAYBACK_DONE/NO_INPUT/BARGE_IN/DIGIT/FAILED/ENDED | aicall/session.go:29-57(定义);发出点 :283,:428-:456,:514,:603,:658,:706,:744,:277 | aicall/orchestrator.go:321-369(drive) | S1, S2 | [FACT] | 进程内事件;不是 SSE。READY 无消费分支(drive 无 case)——无害但可注记 |
| transcribe.EventType=PARTIAL/FINAL | dashscope.go / openairt.go(emit);消费 streamin/session.go:188 | session.go:188-210 | S9 | [FACT] | |
| transcribe.EventType=SPEECH_STARTED | openairt.go:218 | NOT FOUND | UNCOVERED | [FACT] | streamin 消费 switch 无此 case(session.go:187-218) |
| transcribe.EventType=ERROR | 两客户端 | session.go:212-218 | S9 | [FACT] | |
| transcribe.EventType=CLOSED | dashscope.go:491;openairt.go:279 | NOT FOUND | UNCOVERED | [FACT] | range 循环随通道关闭退出,值本身无人分支 |
| provider.InterruptReason=SPEECH | session.go:416(bargeIn) | provider 客户端 Interrupt | S1 | [FACT] | |
| provider.InterruptReason=DTMF | session.go:705(pumpDigits) | 同上 | S1(按键打断) | [FACT] | |
| provider.InterruptReason=SYSTEM | NOT FOUND(provider/session.go:150 仅定义) | — | UNCOVERED | [FACT] | 死值 |
| aicall endReason="HANGUP"/"TRANSFER"/"FAILED" | ledger.go:105,:113,:119 | ledger.go:144-156(CDR status/contained) | S1, S2, S3 | [FACT] | 内部字面量(非导出枚举) |

## 缺口汇总

### 需删除(死值:契约/CHECK 允许、代码永不产生)
- ~~CallState=ENDING(call.go:23)~~ —— 2026-08-24 C4 已删除
- CallType=CONSULT(event.go:81)
- CDRStatus=BUSY(ledgerstore.go:101)
- LegKind=TRUNK(openapi:4829)
- TranscriptionState=ENDED(actor.go:110)
- NotReadyReason=SUPERVISOR(state.go:37);NotReadyReason=SYSTEM 运行时不可达(RingNoAnswer 无调用方——若 S5 要做 app 侧 RONA 则改归"需实现")
- ErrorCode=RATE_LIMITED(errors.go:36,0 使用)
- users.status=SUSPENDED(无停用 API——或改归"需实现")
- cdrs.missed_reason=OUT_OF_HOURS(营业时间路由未实现——BusinessHours schema 已在契约,若要做则归"需实现")
- provider.InterruptReason=SYSTEM(session.go:150)
- trunks.direction 全部 3 值(随 trunks 表)
- SseEventType 10 个零生产者值 → 处置见 events.md 缺口汇总

### 需实现(值有明确用途、链路断在中间)
- SwitchEventKind=QUEUE_AGENT_STATE / QUEUE_AGENT_STATUS 的消费者——这是 S5(RONA)在应用侧可观测的唯一信号源,现被丢弃。
- SwitchEventKind=RECORD_START / RECORD_STOP 的消费者(接通 CALL_RECORDING_* SSE)。
- SwitchEventKind=QUEUE_BRIDGE_END / QUEUE_BRIDGE_FAILED 的消费者(桥接失败当前不可观测)。
- transcribe.EventType=SPEECH_STARTED 的消费者(或从客户端删除发射)。

### 需补场景(值可产生、S1–S12 未覆盖)
- CallType=OUTBOUND / INTERNAL;CreateCallRequest.kind=AI_OUTBOUND(外呼/内呼场景)。
- CallbackStatus 全部 4 值(take_message→claim→complete)。
- ExtensionKind=BOT/PLAIN;Strategy 除 LONGEST_IDLE_AGENT 外 5 值;OverflowType=BOT_FLOW/FORWARD。
- recordings.backend=FS/S3(录音入账)。
- PartyDTMF / SwitchEventKind=DTMF(人工通话按键)。
