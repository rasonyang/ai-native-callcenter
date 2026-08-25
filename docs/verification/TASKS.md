# 验证与跟进任务清单(正式版 v1,released 2026-08-20)

> 前身:TASKS-draft.md(草案),经 owner 两轮决策(DECISIONS-pending.md,2026-08-20)后转正。
> 输入:docs/verification/ledger.yaml(28 case:4 PASS / 1 FAIL / 23 TODO)+ ledger-audit.md(逐 case 审计,
> 含 §0.1 追检)+ coverage/*。基线:HEAD a6ff7b9。
> **D1–D7 全部已决**;实现任务在阶段 7 的 **W 系列**(W1–W10)。~~唯一残留决策:settings 死表处置。~~ **2026-08-24 已决并完成(`00022`,删表 + 录音保留期);`DECISIONS-pending` 已无待决条目。**
> **W1 / W2 / W2.1 / W5 / W6 已完成(2026-08-23);W4 / W3 已完成(2026-08-24)**;
> **W11 管理面闭环已于 2026-08-24 全部完成**(六个子项;过程中 owner 三次收窄原规格);
> **W8 已撤销**(2026-08-24 owner 直裁:删表 + 只读状态,见条目);W7 / W10 未开工;
> **W9 前置已解除**(见排序总则 3,余一处取舍待 owner 定)。
>
> **当前状态(2026-08-23)**:账本 **39 case —— 39 PASS / 0 FAIL / 0 TODO。全部执行完毕。**
> 阶段 6 起草的 11 条已于同日并入并全部执行完毕;VC-S3-02 与 VC-S13-05 经修复后重跑转绿。
> **不再有 FAIL。** 最后一条 VC-S9-01 于 2026-08-23 转 PASS:C14 在切到 qwen 之后的路径上
> **不复现**(三通共约 4800 帧/侧,丢帧全为 0),**但未修** —— 丢弃策略一行没动,
> openai 路径是否仍丢帧未测。重跑途中另发现并修掉 **C40**(tap 与受众都困在 join 的合并分支里,
> 队列派单的电话两天没有转写且不报错)。
> **VC-S12-01 已于 2026-08-23 转 PASS**:C26(bot 腿死后主叫活下来)与 C36 两半
> (按交换机成员表重建等待名单、收养重启期间排队的主叫)均已修并现场证实。
> **VC-S14-01 已于 2026-08-23 修复并现场重跑转绿**(C29 可选布尔取默认值;C30 删分机由外键 RESTRICT 挡住并回 409)。
> 阶段 0–6 已完成。阶段 7:**W1–W6 与 W11 已完成,W8 已撤销,W7 / W10 未开工**(此行原写"W1–W9 未开工",是 v1 发布时的笔误,2026-08-24 更正);
> **C 系列已修 47 项、真开 1 项(C57)+ C10 已决 defer 第二期 + C42 已移出另立项目 + C32/C14 不复现**(47+1+1+1+2 = 52,与条目实数一致)—— **唯一待办是 C57**(CDR 时长字段是否构成划分;owner 已决:立案,以后再查)。C37 于 2026-08-24 查明成因(web-sip-phone 的 RESET 出路只有一个后台定时器)并由该仓修复,aicc 侧零改动;**遗留一个已知缺口:486 不计入 `max_no_answer`,交换机侧对持续拒绝的话机没有兜底**(见条目五);**C34 已于 2026-08-24 现场闭合**(实为四个接口在说谎,见条目);**C1 两半均已修**(后半 2026-08-24,但重跑 VC-S3-02 前仍须重造场景,否则假通过);**C24 已于 2026-08-24 现场闭合**(修它的是 08-22 的 aicc context 三连,见条目);**C32 已不再复现**(原因未证明,守卫为 VC-S14-04);**C14 在 qwen 路径上不复现**(配置变了,不是同配置下消失;openai 路径未测)。
> 上一行的 "4 PASS / 1 FAIL / 23 TODO" 是 v1 发布时的**输入基线**,作为历史保留不改。

## 0. CallType 判定口径与呼叫能力(owner 直裁,2026-08-20)

**判定以腿是否穿过外部 PSTN 网关为准,不看号码形态:**

| 情形 | callType |
|---|---|
| 第一条 leg 经 PSTN 网关**呼入** | `INBOUND` |
| 外呼的 leg 经 PSTN 网关**呼出** | `OUTBOUND` |
| 分机号互相拨打(不出网关) | `INTERNAL` |

**能力限制:`INTERNAL` 通话不支持转接(transfer)、保持(hold)、取回(retrieve)。**
影响面:①CDR 的 call_type 派生须按此实现(立案时 click-to-dial 分机互拨被记成 OUTBOUND,
见 C17 —— **2026-08-20 已修**);②呼叫控制接口对 INTERNAL 呼叫应拒绝上述三个操作(立案时无此判断,
见 **C18** —— **2026-08-21 已修**,409 `OPERATION_NOT_ALLOWED_FOR_CALL_TYPE`);
③账本相关 case(T6.11 外呼、S7 系列 hold/transfer)的 expect 须按此口径写。

---

## 1. 三条角色旅程(贴代码走查)

每步:界面/接口锚点 → 支撑代码 → 已覆盖的 case → 缺口(缺口按审计口径归 **L-d**)。

### 1.1 坐席(wei)旅程

| # | 步骤 | 锚点(路由 / API / 代码) | 已有 case | 缺口 |
|---|---|---|---|---|
| A1 | 登录 + 话机注册 | login.tsx;softphone-bar(_app.tsx);luacc.directory→aicc_xml.lua:103 | VC-S11-01/02(PASS) | — |
| A2 | READY / 小休 / 签出 | POST /agent/ready\|not-ready\|logout;agents/service.go | VC-S11-01(PASS) | — |
| A3 | 看等待名单 | _app.agent.index;GET /calls/waiting(按 staffing 过滤,call_handlers.go:52-66) | VC-S3-01(sup 视角) | **G-A1**:agent 视角的 QueuesForAgent 过滤无 case → 已起草 **VC-S13-01**(已并入) |
| A4 | 弹屏 + 接听 | PARTY_RINGING(coordinator.go:404-418)+ incoming 卡 | VC-S4-01 | G-A2(低):UI 渲染 bot summary/userData 呈现层无断言 |
| A5 | 通话中:实时转写 | live-transcript.tsx;CALL_TRANSCRIPT(actor.go:261) | VC-S9-01/02 | — |
| A6 | 通话中:联系人卡 | _app.agent.index ⋈ lib/contacts;GET /contacts | — | **G-A3**:contacts 全链无 case(tables.md 已记)→ 已起草 **VC-S13-06**(已并入) |
| A7 | 保持/取回 | POST /calls/{id}/hold\|retrieve(202);uuid_phone_event | VC-S7-01 | — |
| A8 | DTMF | POST /calls/{id}/dtmf;uuid_send_dtmf 远端腿 | VC-S7-04 | — |
| A9 | 转接 | POST /calls/{id}/transfer;Transfer 选主叫腿(coordinator.go:770-777) | VC-S7-02 | — |
| A10 | 挂断→ACW(平台代开)→确认 | coordinator.go:246-264;OpenWrapUp 默认词;POST /agent/wrap-up(agent_handlers.go:193-207) | VC-S4-02/03 | — |
| A11 | 坐席不接(RONA) | mod_callcenter 重派;app 侧死边(state.go:189) | VC-S5-01 | 缺口已拍板实现 → **W2**(S5-01 已因 C22 于 08-21 重跑一次 PASS;W2 落地后仍须按 RONA 新行为整体改写再跑,见 T6.9) |
| A12 | 回访单认领/办结 | _app.agent.callbacks;claim/complete(ledger_handlers.go:171/:196) | VC-S3-04(后半) | — |
| A13 | 回看自己的通话 + 回放录音 | _app.agent.calls.tsx(RecordingPlayer,a6ff7b9);GET /cdrs(mine)+ /recordings/{id}/audio | — | **G-A4**:①列表只见自己 ②hasRecording 可播 ③**越权拒绝**(0639c56)→ 已起草 **VC-S13-02**(已并入) |
| A14 | 我的一天(my-day) | ledger.sql:222-240 CTE;_app.agent.index 概览 | — | **G-A5**:汇总数字无 case → 已起草 **VC-S13-03**(已并入) |
| A15 | 话机失联 | ObserveDevice→DEVICE_IN_SERVICE→DEVICE_UNREACHABLE(state.go:207-210) | S11/S12 只到事件层 | **G-A6**:失联→摘除→恢复 旅程无 case → 已起草 **VC-S13-05**(已并入) |

### 1.2 主管(supervisor)旅程

| # | 步骤 | 锚点 | 已有 case | 缺口 |
|---|---|---|---|---|
| B1 | 墙板(今日数字) | _app.supervisor.index;Report*(ledger.sql:133-161) | — | **G-B1**:报表数字对账无 case → 已起草 **VC-S13-04**(已并入) |
| B2 | live 呼叫墙 | GET /calls(requireSupervisorRole) | VC-S1-01、S7-03 | — |
| B3 | 等待名单 | GET /calls/waiting | VC-S3-01、S6-01、S12-01 | — |
| B4 | 坐席花名册 | _app.supervisor.agents;GET /agents | VC-S4-01(ON_CALL)、S12-03(isRegistered) | G-A6 同源(DEVICE_UNREACHABLE 展示) |
| B5 | 队列概览(配员数) | _app.supervisor.queues(只读) | VC-S3-02(FAIL:DB↔switch 对账) | 对账缺陷已立案 → C1(⚠ 症状已潜伏,直接重跑会**假通过**) |
| B6 | CDR 检索/详情 | _app.admin.cdr.*(guard=SUPERVISOR);legs/transcript/RecordingPlayer | VC-S3-03、S4-04、S9-02 | G-B2(低):详情页 wrapUp 展示无断言 |
| B7 | 质检评审 | API 在(recording_handlers.go:118/:152),web 零引用 | — | **已决 D1:下一期**(本期不补 UI、不起草 case) |
| B8 | 实时转写旁听 | LiveTranscript 仅坐席 cockpit | — | **已决 D2=B:by design**(坐席工具;主管看事后 CDR 详情)——不补 UI、不起草 case |
| B9 | SSE 断线恢复 | hub replay / SYSTEM_RESET | VC-S10-01/02(PASS) | — |

### 1.3 管理员(admin)旅程

| # | 步骤 | 锚点 | 已有 case | 缺口 |
|---|---|---|---|---|
| C1 | 账号/坐席管理 | _app.admin.agents;POST/PUT /agents | — | **G-C1**:建坐席→绑分机→签入→接听 全生命周期无 case → **未起草**(与 W9 账号清理冲突,排 W9 之后) |
| C2 | 分机管理 | _app.admin.extensions;PUT(全量);luacc.directory | — | **G-C2**:建分机→注册鉴权→删除守卫 无 case → 已起草 **VC-S14-01**(已并入) |
| C3 | 号码(DID)管理 | _app.admin.numbers;PUT(全量);luacc.dids→lua:44 | VC-S8-01、S4-04 | **G-C3**(F8 已闭:视图 `WHERE d.is_enabled` 过滤):剩"建号→放号→拨通→停用→拒接" → 已起草 **VC-S14-03**(已并入) |
| C4 | 路由(队列)管理 | _app.admin.routing;saveQueue(lib/catalog.ts:51) | VC-S3-04 | ~~G-C4~~ **已闭**(F7:表单发全量) |
| C5 | 队列配员 | staffQueue/unstaffQueue;PUT /queues/{id}/agents | VC-S3-02(FAIL) | G-C5:配员→tier 生效→撤销 UI 全链无 case → 已起草 **VC-S14-02**(已并入) |
| C6 | 流程(flow)管理 | ~~现 CLI-only(flowadd)~~ → `/admin/bots` + `/flows` 五个操作 | — | **已闭(2026-08-24,W3)**;`flowadd` 保留为自动化入口 |
| C7 | 处置词管理 | dispositions 无 CRUD | VC-S4-03(读侧) | **已决 D4:固定词表** → W5(记录);S4-03 断言转正式 |
| C8 | 报表 | _app.admin.reports(guard=SUPERVISOR) | — | 并入 G-B1 |
| C9 | 审计日志 | audit_logs 只写不读(httpapi/audit.go:33) | — | **已决 D5:要做** → **W4**(`/admin/audit`,参考 ui-test) |

### 1.4 辅助两条泳道(把"孤儿 case"接住)

- **呼叫者×bot 泳道**:VC-S1-01/02/03、S2-01、S8-01、S3-03(前半)、S3-04(前半)。
- **平台/恢复泳道**:VC-S10-01/02(PASS)、S12-01/02/03。

**双向对账结论**:28 个 case 全部有旅程归属(无孤儿);旅程缺口 14 个 G-*:7 个补 case
(G-A1/A3/A4/A5/A6 + G-C1/C2/C5 可执行部分 + G-C3 DRAFT)、已决 5 个(G-B3 defer、G-B4 by design、
G-C6/C7/C8 → W3/W5/W4)、已闭 1 个(G-C4)、低优先呈现层 2 个(G-A2、G-B2)。

**起草进度(2026-08-22,已并入 `ledger.yaml`)**:G-A1→VC-S13-01、G-A3→VC-S13-06、G-A4→VC-S13-02、
G-A5→VC-S13-03、G-A6→VC-S13-05、G-B1→VC-S13-04、G-C2→VC-S14-01、G-C5→VC-S14-02、G-C3→VC-S14-03;
**G-C1 未起草**(与 W9 账号清理冲突)。另有两条不在上表内:C15 附带记的 **G-A7**(坐席外呼全链)
已起草为 **VC-S14-04**,T5.3 发现的重建缺口已起草为 **VC-S12-04**。
本节上一段的缺口计数是 v1 发布时的口径,作为历史保留;当前实际覆盖以 `ledger.yaml` 为准。

---

## 2. needs-FACT 清单(执行前/执行中要落实的事实)

| # | 事实问题 | 归属 | 状态/落实方式 |
|---|---|---|---|
| F1 | ~~members 列名与 state 词表~~ | VC-S3-01 | **已闭(2026-08-20)**:实测表头 17 列,state=Trying 实见(VC-S3-01/verdict) |
| F2 | ~~`uuid_send_dtmf` 是否在目标通道产生 DTMF 事件~~ | VC-S7-04 | **已闭(2026-08-20):不产生** —— 发出的 4/2/# 在 SSE 里一条都没有,同一抓流里主叫按的 5 正常到达;PARTY_DTMF 只覆盖**收到的**按键。坐席界面若要回显自己按了什么,须由应用在 `SendDTMF` 成功后自行 publish(建议记入 events.md 语义说明) |
| F3 | ~~max_no_answer=0 下的重派节奏~~ | VC-S5-01 | **已闭(2026-08-21)**:1 次派单 → **60 秒**振铃超时 → 约 0.1 秒间隔的 **11 连发** → 再 60 秒 → 再 11 连发。那 60s 是 `agent-originate-timeout` 未设时的默认值;推荐参数见阶段 7 的 **W2.1** 表 |
| F4 | app 重启窗内 member-queue-start 是否丢失 | VC-S12-01 | **【2026-08-23 重开并已闭,结论相反】** 上一次判"不适用"的理由(主叫从未进队)已被 C26 推翻:主叫现在真的进队,而入队事件**确实**落在断档窗里丢失(09:59:11–09:59:17 重启,入队 09:59:14.388,`/calls/waiting` 空)。**这就是 C36 的前一半**,已修:连接钩子按交换机成员表重建等待名单。原判定辅助"两侧皆空=重跑"随之作废 —— 现在两侧皆空是缺陷。旧记录保留如下:**已闭(2026-08-21,结论是"本题不适用")**:主叫**从未进入队列** —— `hangup_after_bridge` 让主叫腿在 bot 腿死后 50ms 内被跟随挂断,Lua 的兜底块没机会执行(→ **C26**)。原"两侧皆空=重跑"的判定辅助**作废** |
| F5 | 队列停用后模型是否**主动**提出留言 | VC-S3-04 | **部分闭(2026-08-21)**:留言全链已证(callbacks 建行 → claim → complete 全通),但**本题未被压到** —— 按落实方式由人工直接说了 leave a message,模型是否**主动**提出仍无证据。要答此题需另造一次**不给提示**的执行 |
| F6 | ~~今日 95002 bot_sec=0 行的成因~~ | VC-S3-03 前置 | **已闭(2026-08-20,T1.2)——不是 lua fallback,是 S3-03 猎的真漂移在野实证**:call 01a01d3b-6e49-…(11:33)bot 全程服务(bridged/conversation started/handoff fired/"transferring the caller"),CDR 却 bot_sec=0、无 flow、无 BOT leg;stampChannel 失败会 Warn(actions.go:236)而日志无 Warn → 漂移点在 facts==nil 静默跳过(actions.go:83-86)或挂断读回(switchevent.go:81)之间——已在 VC-S3-03 加通话中 uuid_getvar 探针定位,执行时优先复现 |
| F7 | ~~队列表单是否发全量~~ | G-C4 | **已闭**:`setEditing(row)` 整行拷贝(_app.admin.routing.tsx:99/:119) |
| F8 | ~~DID is_enabled=false 是否真拒接~~ | G-C3 | **已闭**:视图层 `WHERE d.is_enabled`(pg_get_viewdef 实查) |
| F9 | ~~goose_db_version 实表~~ | 覆盖表 | **已闭(2026-08-20,T1.1)**:实查 version_id 13/12/11 均 is_applied=t;tables.md 该行升 [FACT] |
| F10 | 坐席取他人 recordingId 的拒绝路径 | G-A4/T6.1 | **半闭(2026-08-22)**:代码侧已读并写进 VC-S13-02 的 expect(`recording_handlers.go:46-50/:55-61/:91`,主管由 `:33` 的 `Role.AtLeast` 早退);**wei+ben 实测未做**,随 VC-S13-02 执行时落实 |
| F11 | 非 seed 账号(chen/uiagent/liveagent 等 12 个)口令与归属 | 环境卫生 | 问 owner;留证即可,不阻塞 |
| F12 | ~~历史表是按分机外键还是按号码文本记的~~ | W11 分配器(D3) | **已闭(2026-08-24,实查)——按 agent uuid,号洞可以复用**:`agent_state_logs.agent_id`/`agent_states.agent_id` 是 FK CASCADE,`wrap_ups` 是 `(call_id, agent_id)` 主键,`queue_agents` 是 `(queue_id, agent_id)` 主键,`cdrs` 存 `agent_ids uuid[]` + `primary_agent_id`。**没有一张表拿分机 id 或号码当身份键**;唯一的号码文本是 `agent_states.extension_number`,那是登录时覆写的当前态,不是历史。所以最小空闲号复用**不会**让新坐席继承离职坐席的历史行。**次级影响另记**:`cdrs.from_number`/`to_number`/`legs` 是号码文本,按号码检索 CDR 会把复用前后混在一起 —— 这在今天"删掉分机再建同号"时就已成立,不是复用策略引入的 |

---

## 3. 任务清单(带排序约束)

### 阶段 0 —— 账本修订 ✅ 完成(2026-08-20;owner 全批后应用)
- **T0.1a 机械修订(零判断)**:SYS-1 fs_cli 全路径 ×11、SYS-2 期望码 202 ×3、SYS-3 `echo rc=$?` 删除 ×3
  与基线差分 ×3、SYS-5 jq `.state`、SYS-6 删 `.recordingId //`、S12-03 sleep 45、S3-01 补 login、
  S12-01 补 members 旁证、S1-02/S1-03/S12-02 的基线/计数补采。
- **T0.1b 语义修订(逐条过目)**:SYS-8 两处留证式(S1-02、S8-01)、S5-01 拆可验半/留证半、
  S7-02 改 ben/1002、SYS-7 读-改-写全量 PUT ×3、S3-04 note 判明、S3-01 括注以查询为准。
- **T0.2** ~~F7/F8 核读~~ 已完成(审计 §0.1);F10 代码侧核读并入 T6.1 前置。

### 阶段 1 —— 纯脚本/静态 ✅ 完成(2026-08-20)
- **T1.1** ✅ F9 已闭;覆盖表勘误已应用——除计划内各项外,另发现并修正两处真勘误:events.md 的
  PARTY_DTMF 与 fsm-edges.md 的 DIALING→RELEASE 原标 UNCOVERED,实际已分别由 VC-S7-04 / VC-S1-03 覆盖;
  三份覆盖表的决议注记(D1–D7、W 系列指针)已补。
- **T1.2** ✅ F6 已闭(重大发现):三行不是 lua fallback——call 01a01d3b-6e49-…(11:33)bot 全程服务、
  handoff 已 fire、"transferring the caller" 已打,CDR 却 bot_sec=0/无 flow/无 BOT leg,即 VC-S3-03
  猎的静默漂移的在野实证;stampChannel 失败会 Warn 而日志无 Warn → 漂移点在 facts==nil 静默跳过
  (actions.go:83-86)或挂断读回(switchevent.go:81)之间;已在 VC-S3-03 增加通话中 uuid_getvar 探针,
  T3.1 执行时定位。

### 阶段 2 —— 呼叫者×bot 泳道执行 ✅ 完成(2026-08-20 17:25–17:56,5/5 PASS)
- **T2.1** ✅ VC-S1-01、VC-S1-02 PASS(单呼叫合并/isBotLeg/CDR 单行 bot_sec=125/留证 CUSTOMER=0)
- **T2.2** ✅ VC-S1-03 PASS(.5 链路自动重试 ×6,6 次拒接 ↔ 6 行 CDR 全部入账——计数断言的差分化立了功)
- **T2.3** ✅ VC-S2-01 PASS(附发现:"model session closed" 在主叫先挂路径缺席是常态,failure 叙述待修)
- **T2.4** ✅ VC-S8-01 PASS(provider 恒 openai 不随语言变——A1 在线证实)
- 执行注记:首拨曾因代理断线致 OpenAI dial timeout → orchestrator 侧 fallback 入队(在野实证已档于
  VC-S1-01/verdict);环境观察:主叫链路(1000@ws.aicc.test→192.168.31.5)对失败呼叫自动重试、
  cdrs 存在来源不明测试行(tone-a/5900)——均记档,不阻塞。W1 合入前置条件已满足(旧行为留证完成)。

### 阶段 3 —— 人工坐席链执行 ✅ 完成(2026-08-20 18:10 – 2026-08-21 10:34;12 case:11 PASS / 1 FAIL)
- **T3.1** ✅ VC-S3-01 PASS;VC-S3-03 首跑 FAIL(bot 份额与队列等待账丢失 → **C11**),
  修后 08-21 12:06 重跑 **PASS**
- **T3.2** ✅ VC-S4-01 / S4-02 / S4-03 PASS。途中发现 **C13**(PARTY_RINGING 报浏览器注册标识而非分机号),
  已修;S4-01 的该条款**待重跑时转正**
- **T3.3** ✅ VC-S4-04 PASS
- **T3.4** VC-S9-01 **FAIL**(ASR 摄取丢帧 4.0%/7.2% → **C14**,至今未修)、VC-S9-02 PASS
- **T3.5** ✅ VC-S7-01 / S7-04 / S7-02 / S7-03 全 PASS;途中现场发现并修 **C15 / C17 / C18 / C19 / C20**
  (click-to-dial 全链 + INTERNAL 能力限制 + 队列派单腿归属)
- **T3.6** ✅ 已产出 ben 的带录音呼叫 `01a02275-8884-…`(recordingId `01a02276-2a62-…`,zh,39 秒)
  —— 即 VC-S13-02 的越权靶子
- 约束(已履行):ben 仅 T3.5/T3.6 需要;T3.6 排 T3.5 后。

### 阶段 4 —— 队列负路径 ✅ 完成(2026-08-21 12:34–16:05;3 case 全 PASS)
- **T4.1** ✅ VC-S5-01 首跑 **FAIL**(bot 接过的电话一律记 ANSWERED,队列放弃在报表里全线不可见
  → **C22**),修后 08-21 13:46 重跑 **PASS**。**W1/W2 合入前的旧行为留证已完成**;
  F3 的重派节奏抄录见 needs-FACT
- **T4.2** ✅ VC-S6-01 PASS。当场坐实 **C12**(`/calls/waiting` 拒绝主管,已修),
  并暴露账本 collect 三型缺陷 → **C23**
- **T4.3** ✅ VC-S3-04 PASS;放行门已满足(结束实测 `isEnabled=true`,队列已在交换机侧恢复)
- 约束(已履行):T4.3 最后;窗口内无并行呼叫。

### 阶段 5 —— 恢复泳道 ✅ 完成(2026-08-21 20:09–20:50;3 case:2 PASS / 1 FAIL)
- **T5.1** VC-S12-01 **FAIL** —— bot 腿死后主叫腿在 50ms 内被跟随挂断,Lua 兜底块不可达
  (→ **C26**,未修)
- **T5.2** ✅ VC-S12-02 PASS。媒体不中断;重启窗内的在途呼叫对新实例不可见、也不进账本(已知缺口)。
  另记一层比 expect 更重的:**正在通话的坐席在 app 里读作 READY**,下一通排队呼叫会派给他
- **T5.3** ✅ VC-S12-03 PASS,但**没有压到重建路径** —— mod_callcenter 的 agents/tiers 存在它自己的
  sqlite 库,交换机重启时自行恢复,我们的 reconcile 只是**确认**(`already matched … added=0`)
  → 因此追加 **T6.12 / VC-S12-04**

### 阶段 6 —— 新 case 起草 ✅ 起草并并入完成(2026-08-22,账本 28 → 39;G-C1 排 W9 后,T6.9/T6.10 归 W 系列后)

> **状态(2026-08-22,owner 已批准)**:11 条草案已按原样并入 `ledger.yaml` —— 新场景 **S13**(坐席与主管旅程缺口,
> 6 条)、**S14**(管理面生命周期 + 坐席外呼,4 条),另 **VC-S12-04** 并入既有 S12 段;
> 全部 `status: TODO`。草案原件保留在 `docs/verification/ledger-draft-phase6.md`(commit 8321675),
> 38 条 `file:line` 已机械校验(文件存在、行号在范围内),其中 6 条抽查过指向内容。
> **T6.1 / 6.2 / 6.3 / 6.4 / 6.5 / 6.7 / 6.8 / 6.11 / 6.12 已起草**。
> **T6.6 只完成三分之二** —— G-C2→VC-S14-01、G-C5→VC-S14-02 已起草,
> **G-C1(坐席账号全生命周期)未起草**:它要建号删号,与 **W9 的账号清理**直接冲突,排在 W9 落地后。
> **T6.9 / T6.10 阻塞于 W1/W2/W7**,不在本阶段完成。
> 三处静态确认不了的(删除已绑定分机的行为、停用 DID 后的拒接形态、三处请求体字段名)写在 case 的
> `note` 里:实测不符即立案,**不得为了让用例通过而降低 expect**。

- **T6.1** DRAFT G-A4:坐席回放 + 越权拒绝(wei+ben;靶子=T3.6;前置 F10 核读)
- **T6.2** DRAFT G-A3:contacts 全链 **T6.3** DRAFT G-A5:my-day 对账 **T6.4** DRAFT G-A6:话机失联
- **T6.5** DRAFT G-A1:agent 视角等待名单过滤 **T6.6** DRAFT G-C1/C2/C5:管理面生命周期三连
- **T6.7** DRAFT G-B1:墙板/报表对账 **T6.8** DRAFT G-C3:DID 生命周期(停用=UNALLOCATED_NUMBER,引 S1-03 形态)
- **T6.11(新)** DRAFT G-A7:坐席外呼全链(click-to-dial:先振坐席腿(auto-answer)→桥出→
  CDR→ACW)——click-to-dial 媒体链已通(2026-08-20 实测 1008→1007),但账面见 C17。
  **外呼一型已实测(2026-08-20)**:1008→18688886669 经 `pstn_sim_outbound` 出网关,call_type=OUTBOUND
  正确,但账面同 C17 三条失真。case 需三型断言(INBOUND/OUTBOUND/INTERNAL,判定口径见 §0)
  + INTERNAL 的能力限制(C18)
- 约束:阶段 2–5 经验之后起草;expect 全给 file:line。
- **T6.9(新)** W1/W2 合入后的账本改写:S1-02/S8-01 留证条款→正式断言(CUSTOMER|MODEL ≥1)并重跑;
  S5-01 按 RONA 新行为整体改写(agent_states 将不再"前后一致"!)并重跑;S3-01 members 词表(F1)回填。
- **T6.12(新,2026-08-21 T5.3 执行发现)** DRAFT **VC-S12-04:交换机侧状态真丢失后的重建**。
  S12-03 通过了,但**没有压到重建路径**:mod_callcenter 的 agents/tiers 存在它自己的
  `/usr/local/freeswitch/db/callcenter.db`,交换机重启时自行恢复,我们的 reconcile 只是**确认**
  (`already matched … added=0`),没有**恢复**。即 S12-03 证明的是"重连钩子会触发且两侧一致",
  未证明"switch 侧真丢了时应用能推回去"。
  起草要点:停机后清空该库(`delete from agents; delete from tiers;`)再启动,
  断言 `added=` 为正、tier 与 agent status 由应用重新建立、且**不出现** S12-03 的
  `failure_looks_like`(app 里人人 READY 而 switch 里谁都不存在)。
- **T6.10(新)** W7 合入后:events.md 十行缺口关闭 + 为新事件补**最小断言**——优先挂进既有 case 的
  SSE grep(如 CALL_RECORDING_* 挂 S4-04、SYSTEM_LINK 挂 S12-03、BOT_SESSION_* 挂 S1-01/S2-01),
  而非新建 10 个 case;PARTY_DIALING/CALL_USER_DATA 若无既有挂点再单独起草。

### ⚠ 计划缺口(2026-08-22 owner 提问暴露)—— 新并入的 11 条没有执行阶段

阶段 2–5 执行原 28 条,阶段 6 **起草**(每条 T6.x 都写着 DRAFT),阶段 7 实现 W/C。
**计划里没有任何一个阶段负责执行阶段 6 产出的那 11 条。** 它们并入后全是 `TODO`,
按现有排序会一直挂到 W 系列改完代码 —— 那时它们钉的已经不是今天的行为了。

按排序总则 2 的同一条规则,这 11 条应当**在会改动它们的 W 项之前执行**。
建议补一个执行阶段(暂记 **T6.13**),排在阶段 7 之前或与之并行:
- **必须先于 W2**:VC-S13-04(带 W2 前的留证条款)
- **必须先于 W9**:VC-S13-01、VC-S13-02(以 ben 为执行物料)
- 其余 8 条与 W 项无交集,可与阶段 7 并行执行
**此缺口待 owner 定夺如何排期**,本文件先记下,不擅自改动阶段编号。

### 阶段 7 —— 工程实现与跟进(spec-first;W=已拍板,C=既有立案)

**W 系列(2026-08-20 两轮决议产物):**
- **W1 TranscribeModel 实现**(D7④):端对端模型自带 transcript,**stock profile 默认开**——
  两个 profile 设 TranscribeModel 默认值(profile.go:71-108),realtime.go:367 分支激活,
  CUSTOMER_SAID(session.go:440→orchestrator.go:325)全链贯通;AICC_* 配置可覆盖/关闭。
  qwen 侧 owner 已确认支持(§补充 S2:response.text.delta 流式文本片段);实现期以
  `AICC_LIVE_PROVIDER_TEST=1` 双 provider 复核。无契约面(纯后端)。
  验收 = T6.9 重跑 S1-02/S8-01 见 CUSTOMER|MODEL 行。
- **W2 RONA 全链实现**(D7②):消费 QUEUE_AGENT_STATE(switchevent.go:368 已归一化,现被弃)→
  RingNoAnswer(state.go:189/service.go:342 已有,接上调用方)→ NOT_READY(SYSTEM)+AGENT_NOT_READY SSE+
  switch 镜像;**含 missed_reason 条件互斥修复**(cdr.go:255-258:ABANDONED_RINGING 需 BridgedAt≠0
  与"振铃中放弃"语义矛盾——修成互斥可达)。验收 = T6.9 重跑 S5-01(新 expect:坐席不接→被摘出路由、
  missed_reason 语义正确)。fsm-edges.md/events.md 死边条目同步更新。
  **W2.1 队列超时参数(2026-08-21 owner 提问触发的配置核查,与 W2 同批落地)** ——
  现状是 RONA 被整个关掉了,且从配置痕迹看是**漏配而非决策**:

  | 参数 | 现值 | 来源 | 推荐 |
  |---|---|---|---|
  | `agent-originate-timeout` | 未设 → 默认 **60s**(与 VC-S5-01 实测 60.08s 间隔吻合) | callcenter.conf.xml 无此 param | **15–20s** —— 收益最大的一项;60s 意味着一次漏接让主叫白等一分钟 |
  | `max_no_answer` | `0`(无限) | 从未设置;**adapter 连 setter 都没有** | **2** —— 取 1 太敏感,WebRTC 话机一次网络抖动就被踢下线 |
  | `no_answer_delay_time` | `0`(立即重派) | `adapter.go:98` 有 setter,**无人调用** | **60s** —— max_no_answer 生效后退居兜底 |
  | `reject_delay_time` | `0` | `adapter.go:93` 有 setter,**无人调用** | **60s** |
  | `busy_delay_time` | `0` | 无 setter | **60s** |
  | `wrap_up_time` | `0` | `service.go:604` 主动置 0 | **保持 0** —— ACW 归应用,现有决策正确 |

  判定为漏配的三个证据:①两个队列都已配好 `agent_no_answer_status='On Break'`,只等
  `max_no_answer` 触发;②adapter 写好了三个 setter 却没有调用方;③**`queues.rona_delay_sec`
  是死配置** —— DB(support-en=10)、`internal/catalog/types.go:122`、OpenAPI 契约里都有,
  能读能改,但 `freeswitch/scripts/aicc_xml.lua:152-174` 从不下发,永不生效。

  **落地次序上的硬约束**:光调 `max_no_answer` 只能修好一半。mod_callcenter 命中后把坐席置为
  `agent_no_answer_status`,而应用会把自己的 presence 反向镜像回交换机(`service.go:628`
  `mirrorStatus`),下一次镜像就把人推回 Available,应用自己的 `agent_states` 仍是 READY
  —— VC-S5-01 实测到的"前后完全一致"正是这个。**所以状态转移必须由应用拥有、交换机只做通知**,
  参数调优必须与 W2 主体同批,否则得到一个交换机与应用互相打架的半成品。

  实现面因此含三件事:①给 adapter 补 `max_no_answer` / `busy_delay_time` 的 setter;
  ②`mirrorRegistration`(service.go:595-612)在 add 之后一并下发这批参数,取值来自队列/坐席配置
  而非硬编码;③把 `rona_delay_sec` 接上(要么下发,要么从契约里删——不留死配置)。
  队列侧另有一处不一致待一并处理:support-zh 的 `rona_delay_sec`/`sla_threshold_sec`/
  `discard_abandoned_after_sec` 均为 0,而 support-en 是 10/20/60,seed 两边不同口径。

  **【2026-08-23 落地,三个提交;现场重跑待做】**
  - **`cbf0391` 交换机侧的守卫**:四个**每坐席**参数随 `mirrorRegistration` 一并下发 ——
    `max_no_answer=2` / `no_answer_delay_time=60` / `reject_delay_time=60` / `busy_delay_time=60`;
    补写了 `max_no_answer` 与 `busy_delay_time` 两个 setter(另两个此前有 setter 无调用方)。
    **`agent-originate-timeout` 是全局参数**,由 `aicc_xml.lua` 的 `<settings>` 下发,60 → **15 秒**。
    连带:`reject_delay_time=60` 正是 **C37**(被拒的派单 70 毫秒一次空转三分钟)的直接对策,
    该条状态随之改为"参数已改,待复现验证"。
  - **`27be54c` 应用侧的消费**:`KindQueueAgentStatus`(此前归一化后被丢弃)→
    交换机把坐席置 `On Break` = **通知**,应用据此 `RingNoAnswer` → NOT_READY(SYSTEM) +
    `AGENT_NOT_READY` + 反向镜像。**状态转移归应用所有,交换机只做通知** —— 这正是 W2.1
    点名的硬约束,VC-S5-01 实测到的"前后完全一致"就是两边打架的结果。
    **两道守卫都在 presence 服务里**,因为只有它知道自己已经相信了什么:
    ①**仍为 READY** —— 已经不接单的人没有什么可改,这同时切断了"我们自己的镜像回声被当成新的漏接";
    ②**话机可达** —— 响不响得起来是好话机才有的事;READY 但话机失联的人是**我们故意**镜像成
    On Break 的(C28),把它读回来会夺走一个他从未改变过的状态。
  - **`88a636f` missed_reason 的矛盾**:`ABANDONED_RINGING` 原本要求队列**已桥接**,
    而"响铃中放弃"的人根本走不到桥接 —— 该分支对它自己描述的情形不可达,
    这类主叫**全被记成 `AGENTS_DID_NOT_ANSWER`**:本该是主叫的选择,记成了坐席的过失,
    而主管正是靠这份报表分辨两者。仅仅反转条件会矫枉过正 —— 8 月那次事故的 fixture 证明了:
    23 次派单无人接听、随后主叫又静默等了两分钟才挂断,**响过,但不是他离开时响的**。
    故判据是"他离开那一刻还活着的那次振铃"(腿的释放不早于主叫的离开)。
    "谁结束的"取队列自己的词汇(`Cancel` vs `Timeout`),不靠推断 —— 其余一律**不算**主叫的选择,
    因为把没挂机的主叫记成放弃,是报表上看不出来的那种错。
    第三种情形随之补名:队列没说为什么走(这种缺失本轮 C36 见过),而话机确实响过 →
    `AGENTS_DID_NOT_ANSWER`,记**录到的事**,不发明一个主叫未必做过的选择。
    **摘除验证抓到我自己刚写的死代码**:这个新分支在补 fixture 之前没有任何用例能到达。
  - **support-zh 的 `0|0|0` 已修复**:live 库 UPDATE 成 `10|20|60` 与 support-en 一致;
    **seed 也一并改**(`seed.go`)—— 原先只显式写 `sla_threshold_sec`,其余靠列默认值,
    而经 API 建的队列会拿到 0(C33),两条本该一样的队列因此按"从哪扇门进来"分了岔。
    ⚠ 这动了 **C33 的现场证据**:support-zh 那行不再是受害者,C33 的证据以
    `VC-S12-01/verdict.md` 里抄下的读数为准。
  - **仍未决(须 owner 定)**:`queues.rona_delay_sec` **在交换机里没有位置** ——
    `callcenter_config queue list` 的列里根本没有 RONA 延迟,它是**每坐席**的
    `no_answer_delay_time`;一个坐席同时配员两条队列时 per-queue 的值无解,
    这正是 mod_callcenter 把它放在坐席上的原因。所以"下发"这条路走不通,
    剩下的是**从契约里删**(改契约 + 迁移,spec-first),或者重新定义它的含义。
    本轮**没有动它**,它仍是死配置。
- ~~**W3 flows 管理面**(D3)~~ **【已完成 2026-08-24】** `/admin/bots` 与五个 ADMIN 操作
  (`GET /flows`、`POST /flows`、`GET /flows/{flowId}` 含草稿与修订、`PUT /flows/{flowId}`、
  `POST /flows/{flowId}/publish`)。契约 → generate → store → handlers → UI,
  `api-lint`/`api-check`/`api-breaking` 全绿,CLI `flowadd` 保留为自动化入口。
  **spec 在契约里是不透明文档**(`type: object`)。方言归 `internal/flow` 的装载器 ——
  那是**活着的通话解析已发布修订时跑的同一段代码**;在 openapi.json 里再写一遍这个形状,
  就是第二份定义,而它随时可以和真正在跑的那份漂移。所以契约只管"这里有个文档",
  校验归装载器,**被拒时回 422 并带上它找到的全部问题**(`params.problems`)——
  作者改一份 spec 该一次看完整份报告,而不是存一次发现一条。为此给 `flow.Load` 加了
  `ValidationError`(问题列表 + 原样的拼接消息,启动日志要的还是后者)。
  **`hasUnpublishedChanges` 交给 PostgreSQL 判定**:jsonb 相等是语义的,重排键、重新缩进
  都不是改动,`IS DISTINCT FROM` 又让"从未发布"成为它本来就是的那个真值;在 Go 里比字节两头都错。
  真库测试抓到了另一半 —— 写成普通 `<>`,未发布的流程那一列是 NULL,扫进 `bool` 直接失败
  (与 W4 的 NULL 扫描是同一类,fake 复现不了)。**前端同一个坑**:草稿保存回来的是服务端的键序,
  按文本比较会让每个刚存过的流程永远显示"未保存",而 Publish 要求草稿干净 —— 它就永远按不下去。
  **顺带修好 `flowadd` 的更新分支绕过校验**:slug 撞车时它直连生成的 query 写草稿,
  一份坏文件能覆盖掉正在用的流程,直到 Publish 才报错;现在两条路都走会校验的
  `FlowStore.UpdateDraft`。
  **可达性分析放在前端**:装载器不拒绝孤立阶段,这是对的——通话只是永远走不到那里,
  运行时不坏;但它几乎总是一次改了一半的重命名,所以由这一屏提示,**不在服务端规则旁边
  再发明一条**。参考 UI 的 `validateFlow` 把装载器的规则用 TS 又写了一遍,没有照抄:
  那正是会漂移的第二份定义。
  **故意不做**:没有 DELETE(号码的外键本来就挡着,而这一屏不是用来删流程的);
  **旧修订的快照不回吐** —— 把它读回来就是回滚,那是另一个决定。
- ~~**W4 audit_logs 检索**(D5)~~ **【已完成 2026-08-24 `f0328b6` + `6158eaa`】**
  `GET /audit-logs`(ADMIN,最新在前,`actorId`/`actionPrefix`/`from`/`to` + limit-offset)
  与 `/admin/audit` 页面。契约 → generate → handler → UI,`api-lint`/`api-check`/`api-breaking` 全绿。
  **过滤按 action 前缀,而不是"派生的分类"**:存下来的 action 就是 `METHOD /route/template`,
  所以一个字面前缀同时选方法与资源(`DELETE ` 选全部删除,`PUT /api/v1/queues` 选队列编辑)。
  参考 UI 的分类是从点号命名(`queue.update`)派生的,我们没有那种值 —— **不在存下来的值上面
  再发明一层分类,那层一定会和它漂移。**
  **操作者名在读时解析**(LEFT JOIN + COALESCE):账号可以被删除而它做过的事留在账上,
  行始终保留 `actorId`,所以清理花名册不会抹掉它的账号做过什么;页面在这种行上显示 id 而不是空白 ——
  空白会被读成"没人做过这件事",而那正是这一行否证的东西。真库测试抓到了第一版把这个 NULL
  扫进 `string` 的 bug(fake 复现不了)。
  **⚠ 开工时先修了一个安全缺陷,见下条 C58** —— 不修就等于把一个休眠的泄漏变成可浏览的。
  `coverage/tables.md` 的"只写不读"缺口随之关闭。
- **C58(new,2026-08-24 做 W4 时发现,已修 `74cce21`)**
  **审计行里存着明文的分机 SIP 密码。**
  `auditTrail` 中间件把请求体原样存进 `audit_logs.detail`,而唯一的守卫是路径前缀
  `/api/v1/auth/`。契约里带密码字段的请求体有三处:`LoginRequest`(被守卫挡住)与
  **`ExtensionWrite`(`POST /extensions`、`PUT /extensions/{id}`,没挡住)**。
  开发库里实测 **4 行**含明文,例:
  `{"request": "{\"number\":\"1099\",\"password\":\"vc-probe-pass\",…}"}`
  —— 那是话机的**注册凭据**,拿到就能以该分机注册并接走它的呼叫。
  **路径前缀正是会过期的那种守卫**:它覆盖 `/auth/` 是因为写它的时候密码在那里,
  而将来任何带密码的新端点都不在其内。**改为按字段名脱敏、递归到任意深度**
  (`password`/`secret`/`token`/`apikey`/`credential` 子串匹配,覆盖 `newPassword`、`apiKeyId`)。
  **用标记而不是删掉键**:"这个字段发过、我们没留"与"这个字段没发过"是两件不同的事,
  审计不该把它们混同 —— 故写 `"[redacted]"`。
  **解析不出 JSON 对象的请求体一概不存**:看不懂就担保不了;契约里没有这样的写端点,
  而畸形请求体本就回 400、根本不会落审计行。
  现场复验:新建分机落的是 `"password":"[redacted]"`。
  **已存的 4 行是数据**,归入 owner 那批待定的清理决定,**但标注为凭据、优先级高于其余几项**。

- ~~**W5 D4 记录**~~ **【已完成 2026-08-23】**:决议记进了**设计集**本身
  (`docs/design/03-data.md` 的 `dispositions` 行,带日期与理由:四个词全中心认同,
  胜过谁都对不上账的四百个;因此"恰好这四个"是**契约级**断言,加第五个是要重开的决议、
  不是配置变更)。`coverage/tables.md` 的 D4 注记本就已在。S4-03 的该行确认为硬断言
  (原本就不是留证),并补上了它为何是契约级的出处。
- ~~**W6 D1 记录**~~ **【已完成 2026-08-23】**:同样记进设计集
  (`docs/design/03-data.md` 的 `quality_reviews` 行):API 完整并保留契约、前端零引用,
  **这张表只进不出是带日期的决定,不是没人发现的疏漏**;并指回
  `coverage/tables.md` 中该行的 UNCOVERED 标注。tables.md 侧的注记(含决议日期)本就已在,
  缺的是设计文档这一半 —— 两处都记,才不会有人在下一期读设计文档时把它当成缺口去补。
- **W7 十个零生产者 SSE 类型全部实现**(D6),按难度四组:
  **【2026-08-22 现场证据,owner 提问触发】** `PARTY_DIALING` 的缺席在事件流上是看得见的:
  一通分机互拨(1002→1008)的完整流里,主叫 party 的**第一次出现就是 `PARTY_ESTABLISHED`** ——
  ```
  9800005 PARTY_RINGING     partyId=…b32e (wei, TARGET)      ← 被叫宣告了 RINGING
  9800007 PARTY_ESTABLISHED partyId=…b32e (wei)   RINGING→TALKING
  9800008 PARTY_ESTABLISHED partyId=…b305 (ben, ORIGINATOR)  ← 它的 DIALING 从未被宣告
  ```
  两条 `PARTY_ESTABLISHED` 是**两个不同的 party**,各自合法迁移,不存在 `TALKING→TALKING`;
  但 owner 读流时的疑问是对的 —— **从流上看不到 `DIALING`,party 一出现就已经在通话**。
  originator 腿确实以 DIALING 创建(`call.go:209`),只是没有事件宣告它(`events.md:11`)。
  **实现 `PARTY_DIALING` 应优先于其余九个**:它是唯一一个让 FSM 的起点在流上隐形的缺口,
  而且订阅方(如坐席自己的工作台)要靠它才能在对方接起之前知道这条腿存在。
  另见 **C28②**:`DEVICE_*` 那一对不是"没实现",是**发错了一个**。
  **2026-08-22 已随 C28 改对** —— `ObserveDevice` 现在按方向发 `DEVICE_UNREGISTERED` / `DEVICE_IN_SERVICE`,
  `DEVICE_UNREGISTERED` 从此有生产者,W7 的清单据此减一。
  ① ~~CALL_RECORDING_STARTED/STOPPED~~ **【已做 2026-08-23】** —— 信号早已归一化,
     只是从来没人消费。改在 **call actor** 里发(不是 coordinator):那里
     `publish(t, nil, …)` 天然就是**通话作用域**,而"这通电话在不在录音"正是关于通话、
     不是关于某条腿的。**不带交换机的文件路径** —— 那是交换机自己磁盘上的路径,
     浏览器拿着没用,录音按 call_id 走 recordings API 取;屏幕要的只是"发生了、什么时候",
     而这两样信封本来就带着。两条反向摘除验证:去掉两条宣告 → 用例超时;
     改成带路径的腿事件 → 报"录音被当成某条腿的事"并揪出路径外泄。
     零生产者 8 → 6。
  ② ~~DEVICE_REGISTERED/UNREGISTERED——信号已达 ObserveDevice(main.go:399-405),补区分发布~~
  **已做(C28,2026-08-22)**;`DEVICE_REGISTERED` 仍无生产者 —— 恢复走的是 `DEVICE_IN_SERVICE`;
  ③ ~~PARTY_DIALING(addParty 时对 originator 腿宣告)~~ **【已做 2026-08-23】** ——
     发起腿(`Role == ORIGINATOR`)在 `addParty` 时宣告 `PARTY_DIALING`,
     **作用域严格照"腿事件私有于其坐席"办**:有坐席只发他本人,无坐席(主叫自己的腿)
     不进任何坐席的流。四个子例钉住,含两条反向 —— 给每条发起腿都安上坐席、
     把派单腿也当成发起腿宣告,各自立刻失败(**派单腿是被响,不是在响别人**)。
     零生产者从 9 个降到 8 个。
     **现场验证用了三通分机互拨(2026-08-23),前两通各揪出一处载荷错向**:
     ①`1008→1002` 报 `{from:1002,to:1008}`(方向反了)、`1002→1008` 报 `{from:1002,to:1002}`
     (自己的号出现两次)—— 根因是我照搬了 `PARTY_RINGING` 的号码对,而**那是被叫视角**
     ("谁在响你、响你哪个分机"),在被叫腿上对,在发起腿上正好倒过来:
     读出来是"被拨的号在拨号",坐席工作台会把自己打出去的电话显示成对方打进来的。
     改用**这条腿自己的号 + 它要拨的目的号**。
     ②改完再验,`fromNumber` 对了,但 `1008→1002` 的 `toNumber` 是 **`doskp0mj`** ——
     浏览器话机注册时的随机 contact 令牌;**桌机那通(1002)是干净的,只跑一通根本看不见**。
     旁边 `PARTY_RINGING` 的注释早写过这个坑(*"telling an agent they are being rung at
     'g7bih4lv' is telling them nothing"*),同一个坑隔了一条腿。
     故目的号**只在它是数字时才发** —— 不发,被叫自己的号几秒后随他的腿就到;
     发了,那是个**长着正确答案样子的错误答案**。
     ③第三轮三通全对,`PARTY_DIALING` 在每条 party 的最前,发起腿只进它自己坐席的流。
  ③b **CALL_USER_DATA** —— **做不了,而且原因不是"还没接线"(2026-08-23 查明,立案 C42)**。
     `userData` **没有任何写入路径**:`CreateCallRequest` 里没有这个字段、没有任何接口能改它、
     `MergeUserData` **只有测试在调用**(`grep` 全仓可证)。它是一个挂在 Call 与快照/CDR 上、
     随每条事件信封下发、契约里写着 *"merge-patched, survives transfers"*、
     而**生产中永远为空**的字段。**没有变更就没有变更事件可发** ——
     补一个 publish 点等于为一个不存在的功能伪造生产者。
     要做就得先有**改 userData 的能力**(契约 + handler + 协调层),那是产品决策不是接线任务,
     **留给 owner 定**:要么建这条写入路径,要么把它从契约里摘掉。
  ③c ~~SYSTEM_LINK(挂 esl.Link 断连/重连,S12 语义)~~ **【已做 2026-08-23】** ——
     两个方向都发(`{isUp:true|false}`),**广播作用域**:交换机没了是坐席既看不见、
     也绕不过去的那种故障(不响、按什么都没反应),而在此之前唯一的迹象就是"什么都不再发生了"。
     `esl.Link.OnLost` 这个钩子**写好从没被调用** —— 本周第四个同型的
     (`ListQueueMembers` / `ShowChannels` / `LoadPresence` / `OnLost`)。
     摘除验证:只发重连那一半 → 用例报"只宣告了 1 条,两个方向各要一条";
     只会报"回来了"的事件分不清重连与首次启动。零生产者 6 → 5。
  ④ BOT_SESSION_STARTED/INTERRUPTED/ENDED——需给 aicall 引入 Hub 依赖(现无 Publish 调用,
  events.md 实证),**W7 内单独架构评审**(经 orchestrator 回调转发可避免直接依赖)。
  合入后:events.md 十行缺口关闭 + T6.10 补最小断言。
- **`settings` 死表(§补充的唯一残留)** **【已决并完成 2026-08-24 `00022`】** —— **删表 + 顺手把保留期做了**。
  `settings` 是 key/value 表,0 行、**没有任何 Go/Lua/sqlc 查询碰它**。而本仓的配置答案早已定下:
  `AICC_*` 环境变量、`.env.example` 是唯一登记册(49 条,CLAUDE.md 明文要求与 `config.go` 同步)。
  一张 settings 表就是**第二套配置系统**在和它竞争 —— 今天清了一整天的那种两处真相。
  **但它和 trunks 不同**:它有一项**有设计读者**的条目 —— 设计 03 写着录音保留期
  "daily job deletes storage objects + stamps `deleted_at` where `age > settings.retentionDays`"。
  顺着它查下去,那个功能有**三处遗迹而只有一处是死的**:`settings.retentionDays` 空表无人读;
  但 **`recordings.deleted_at` 是承重的**(每一条录音读都在 `WHERE deleted_at IS NULL`)、
  `recording.Storage.Delete` 也在(注释写着 "for retention")—— **只差那个 job,它从来没建**。
  故本次一并建成:`AICC_RECORDING_RETENTION_DAYS`(**默认 0 = 永久保留**,
  升级进这个功能的部署不会因为没人填数字就开始删录音)、`recording.Sweeper` 每小时扫一批(每批 500)。
  **顺序是先删对象再盖章**:反过来会丢失字节 —— 行标了删除而对象还在,那是一份再也没人会去找、
  却要一直付费的孤儿。这个顺序下,两步之间失败留下的是"列得出来但放不出来"的录音,
  看得见,而且下一轮自愈(删一个已经不在的对象不算失败,而正是想要达到的状态)。
  **行保留、只删音频**:没有录音的 CDR 仍是一通电话的记录,连行一起消失则是"这通电话没发生过",
  那是另一句话,而且是假的。
  单测钉住四条(0 不删任何东西 / 对象删不掉就不盖章 / 对象本来就没了仍要盖章 /
  一个失败不阻断其余),真库测试钉住年龄比较与"已扫过的不再来"。
  **`DECISIONS-pending` 至此没有待决条目。**

- **`missedReason` 词表四层不一致** **【已完成 2026-08-25 `00023`】** —— **契约补枚举 + 删掉一个没人能拿到的理由**。
  一个 missed reason 要穿过四层、以同一份词表的四种写法存在:装配器决定它(`cdr.go` 的
  `missedReason`)、`cdrs` 的 CHECK 存它、契约声明它、控制台翻译它。**四层没有一个地方对得上**:
  代码能给出 5 个值,CHECK 允许 6 个,契约把它写成裸 `string`(什么都不承诺),两份 locale 只译了 4 个。
  后果是**已经上线的**:`AGENTS_DID_NOT_ANSWER` 自账本建成起就在产,而中英文都没给它名字,
  于是主管打开"为什么这通电话没人接"的那张报表,读到的是
  `cdr.missedReasons.AGENTS_DID_NOT_ANSWER` —— 一个 i18n 裸键。**没有任何测试失败**,
  react-i18next 对不认识的键的回答就是把键本身还回去。
  处置分两头。**契约补 `MissedReason` 枚举**(照 `CDRStatus` 的样子命名,`$ref` 进 `CDR`),
  于是 Go 侧拿到 `api.MissedReason` 及其 `Valid()`,TS 侧拿到五值联合,
  `StatusCell` 的 `status`/`missedReason` 与 `STATUS_COLOR` 一并从 `string` 收紧到枚举 ——
  再多一个状态就是编译错,而不是一个没人注意的灰点。描述同时改正:
  原文写"empty when the call was not missed",但 `store.CDR` 带 `omitempty`,空串根本上不了线,
  枚举也没有 `""` 这个成员,该说的是**缺席**。
  **`OUT_OF_HOURS` 出局**(`00023`)。它只活在 00005 的 CHECK 和设计 03 里,**没有任何代码决定它** ——
  本产品不知道一个队列开几点到几点:队列上没有排班、没有日历、也没有哪条 dialplan 会因为来得晚而拒接。
  留着它比不用更糟:契约现在把这份词表声明成枚举,**词表里的每个值都是对客户端的承诺**
  (某通电话某天会带着它来),也是控制台欠下的两份翻译。为一个代码给不出的值付这两笔,不值。
  真做了排班,难的是排班本身,这一行是最后一步而不是第一步,到时随功能一起回来。
  **改写在收紧 CHECK 之前**,如例。真实部署一行也不可能有 —— 从没有东西写过它 ——
  但 **CHECK 是关于表的断言,不是关于恰好填了它的代码的断言**,诚实的收紧方式是先让断言为真。
  `migrate_test` 的 fixture 钉在 v22 插一行 `OUT_OF_HOURS`,把顺序颠倒过来就复现出
  "is violated by some row"(已实测)。
  两侧各留一个钉子把词表拉在一起:Go 侧 `TestEveryMissedReasonIsOneTheContractNames`
  (装配器 ↔ 契约,双向);前端 `src/test/locales.test.ts`(两份 locale ↔ 同一份五值,且互等)。
  前端为什么不从生成物枚举:`openapi-typescript` 只出类型,运行时已被擦除,没有东西可遍历。
  **顺手**:`Extension` 的 `required` 里还留着 W11 删掉的 `kind`,`api-lint` 一直在报这条 warning
  (CLAUDE.md 要求零 warning)。已摘,lint 归零。
  **未纳入**(仍待 owner):`plan-cdr-anchors.md:185` 记的那个洞 —— 没进过队、也没有坐席腿的
  `NO_ANSWER`,`missedReason` 为空,账本说不出理由。那是**给词表加值**,与本条"删掉一个多余的值"
  方向相反,需要先定"未接但没进队"该叫什么,不在本次范围内。

- ~~**W8 trunk(中继号)管理**(§补充 S3)~~ **【已撤销 2026-08-24,owner 直裁;`00021`】**
  **删表 + 只读状态**,而不是管理面。查清的事实:`trunks` 表 0 行、**没有任何 Go/Lua/SQL 读它**、
  `luacc` 里没有 trunks 视图、`aicc_xml.lua` 只服务 directory/dialplan/configuration(仅 callcenter.conf,
  其余 configuration 键一律 fall through)。**网关定义在交换机自己的 sofia profile XML 里、
  profile 加载时读取,应用不写那个文件** —— 要让一行变成网关,得再加 `luacc.trunks` 视图、
  让 `aicc_xml.lua` 接管 configuration 的 sofia 部分、每次改动触发 `sofia profile external rescan`,
  为一个单机部署只有一个、部署时配一次的东西。
  而 **D6 早已定下 gateway 是系统级配置**,一个编辑它的界面正说反了;它也兑现不了承诺 ——
  **W11.1 改名时仓内只能改一半**,另一半在不受版本控制的宿主机 XML 里。
  **留下的是那真实的一半**:`GET /system/health` 增加 `trunks[]`(名字/profile/地址/state/isUp),
  经 `sofia status` 读取,Overview 的 SYSTEM HEALTH 卡片逐条显示。
  **state 原样透传交换机自己的词**(REGED/NOREG/DOWN/FAIL_WAIT),不映射成"好/坏":
  down 得有名字的时候就该说那个名字。**`NOREG` 算 up** —— IP 中继按设计从不注册,
  把它算成 down 会在每个健康的部署上报一个不存在的故障(单测钉住这一条)。
  交换机连不上时给空列表而不是报错:这份回答的其余部分仍然是真的。
  **顺带修**:Overview 的分机 KPI 显示 `admin.boundExtensions` 原始 key —— 上一批改名时漏了翻译。

- **W9 账号清理**(§补充 S4,含 seed 名单同步):live DB 只保留 **wei、agent、supervisor、admin**,其余
  (amy、ben、chen、uiagent、liveagent、livesup、qa-sup、lin、sam、cara、tester、m45check)删除。
  **seed 名单一起改**(owner 2026-08-20):demoPeople(seed.go:56-63)删去 amy、ben 两行,使
  `AICC_SEED=demo` 不再把它们建回来;随之核对 seed 内对这两个账号的连带引用(queue_agents 配员、
  agent_state_logs/cdrs 演示历史的 agent 归属)与受众文档里的演示账号清单(README/deploy/demo/README
  及 zh-CN 版),同步收敛。注意 `agent` 账号本就不在 seed 内(手工建),保留只针对 live DB。
  **连带的账本影响**:VC-S11-02(PASS)的分机抢占守卫用 amy 登录——清理后该 case 无法原样重跑,
  W9 内一并把它改写为用 `agent` 账号(或届时保留的第二坐席)作抢占方。
  **硬性排序:阶段 3–6 之后执行**——ben 是第 2 坐席物料(T3.5/T3.6/T6.1);清理前确认无 case
  再需要它们。清理后花名册类断言的环境噪音消失(F11 关闭)。
  **【2026-08-24 复核】前置已满足**:依赖 ben/1002 的八条用例全部 PASS,S13-01/S13-02 的判词
  确认是用 ben 真跑完的。**但新增一项须 owner 定夺**:1002 是本机唯一的**原生软电话**,
  而 C37 的定性完全依赖"浏览器话机 7 : 原生软电话 0"这个对照;C24/C52/C55 的现场验证也用两部话机
  互为对照。**建议:账号清掉,但把分机 1002 保留并挂到 `agent` 名下** ——
  花名册照清,不同实现的第二部话机这个诊断能力也留住。

- ~~**W11 管理面闭环**(owner 直裁 2026-08-24)~~ **【全部完成 2026-08-24】**
  六个子项全部落地,管理面从"能配置一个呼叫中心但配不出人"变成可用。
  **过程中 owner 三次收窄了原规格,每一次都让形状更小**:①`agents` 改名作废(本仓早已拆分,
  `users` 名字被占),换成新建 `/users` 写侧;②`extensions.kind` 从四值收到两值再删掉整列
  (BOT 路由不了任何东西、PLAIN 是 `AGENT 未绑定` 的第二种拼法、剩一个值的列什么也没分类);
  ③分机与队列号都不再手输,随账号/队列自增。**净结果:`extensions` 回到"一个号码 + 归谁"**,
  队列号仍归 `queues.ext_number`(队列是被拨的,不是被注册的)。
  管理面现在不闭环:**没有任何接口能创建账号**(`/users` 只有 GET,`POST /agents` 还要求先有
  `userId`),账号只能用 `aicc useradd` 建。补齐到可用为止。

  **前置"`agents` 表改名 `users`"作废(owner 2026-08-24)** —— 本仓不存在那张混装表:
  身份早已在 `users`(username / password_hash / role / status / locale),`agents` 是纯 ACD
  (`callcenter_name` / `is_auto_answer` / `default_extension_id` / `user_id`)。改名要达成的拆分
  已经做完,而 `users` 这个名字正被身份表占着。**换成"新建 `/users` 写侧"**,这才是不闭环的根。
  (本仓没有承载那条原表述的文档,故无处可标 superseded —— 记在这里就是它的墓碑。
  D8 的 superseded 另有其处:`docs/design/m4-cleanup-findings.md` 的 F-01。)

  **已定决策(owner 2026-08-24):**
  - **D1** user↔extension 是 **1:0..1**,DB 强制。**注意本仓的关联方向是反的**:现为
    `agents.default_extension_id` + `uq_agents_default_extension UNIQUE … WHERE NOT NULL`,
    抢占已被占用的分机已回 409(VC-S11-02 覆盖)。D1 要的 `extensions.agent_id` +
    `UNIQUE (agent_id) WHERE agent_id IS NOT NULL` 落地时**必须撤掉原来那一处** ——
    两个方向同时存在就是两处真相,而它们会各自漂移。这是 W11.4 最大的一块。
  - **D2** 号段来自 `AICC_EXTENSION_RANGE`(默认 1000-1999),AGENT/SUPERVISOR 共用一池。新增配置。
  - **D3** 分配取号段内**最小空闲号** + advisory lock;禁止 `MAX+1`。**前置已查清,见 F12:安全。**
  - **D4** SIP 口令**明文存**。realm 绑宿主 IP,本环境已漂移两次;a1-hash 会随域变更集体失效且
    无法重算,只能全员重置口令、打断当班注册。代价记录在案:能调 reveal 端点即可取明文。
  - **D5** 口令读取走独立端点,ADMIN-only + 审计;schema 中永不含口令字段。
    ⚠ 与 **C58** 同源:`auditTrail` 已按字段名递归脱敏,reveal 端点的**响应**不经审计,
    但它的**调用**必须留在账上 —— 这正是 D5 说"+ 审计"的地方。
  - **D6** gateway 是系统级配置,不进 `dids` 表。同时改名 `pstn_sim` → `pstn_gateway`。
    **本仓只能改一半**:`pstn_sim` 只存在于宿主机 `/usr/local/freeswitch/conf/vars.xml:454-459`
    与 `sip_profiles/external/pstn_sim.xml`(**不在版本控制内**),仓内只有 `.env` 的
    `AICC_OUTBOUND_ENDPOINT` 和测试字面量。宿主机那半边手工改 + `sofia` 重载,改完才生效。
  - **D7** 方向用两个布尔列(`allow_inbound` / `allow_outbound`),不用枚举数组。`dids` 现无方向列。
  - **D8** `flow_id` 约束改为 `CHECK (NOT allow_inbound OR flow_id IS NOT NULL)` ——
    纯呼出号码没有 bot 可绑。horizon 上"恢复 NOT NULL"的原表述**作废,记 superseded**。
  - **D9** 缺省外呼号:部分唯一索引保证全局至多一条;无缺省时**快速失败,不得回落到分机号**。
    **今天就在静默回落**:`outbound.go` 的坐席点击外呼用 `cfg.OutboundCallerID`
    (`AICC_OUTBOUND_CLID`),为空时**什么都不设**,由 gateway 的 `pstn_sim_caller_id=95001` 兜底。
    AI 外呼走 `did.Number`,那一条是对的。
  - **D10** bots 的 extensions 列是 join 出来的,不建冗余列。
  - **kind 词表:只把 `PLAIN` 改名 `QUEUE`(owner 2026-08-24)** —— 不把 `queues.ext_number`
    迁进 `extensions`。现状:`extensions.kind` 是 `AGENT | BOT | PLAIN`,库里只有 `AGENT` 20 条,
    `BOT`/`PLAIN` 各 0 条;队列的分机号是 `queues.ext_number` 这一列,不是 extensions 行。
    **因此 `kind=QUEUE` 的目标选择器指向什么、与 `queues.ext_number` 如何不打架,是 W11.4
    开工时唯一待定的细节** —— 到那一步再定,不在这里猜。
  - **Auto answer:只从表单摘掉(owner 2026-08-24)** —— 列与交换机接线保留。它现在是通的:
    admin UI → API → `agents.is_auto_answer` → `luacc.directory` 视图 → `aicc_xml.lua` 下发
    `sip_auto_answer` → 并传给 mod_callcenter 的 agent contact。**摘掉表单后新建坐席一律 false**,
    这是行为变更,验收时要确认没有人依赖它为 true。

  **执行顺序(依赖决定,不可换):**
  1. ~~**W11.1 `pstn_sim` → `pstn_gateway`**(D6)~~ **【已完成 2026-08-24 `f2c3457`】**
     **宿主机实为五个文件,不是条目原估的两个**:`vars.xml` 的三个变量、
     `sip_profiles/external/pstn_sim.xml`、以及 **三个** dialplan 目录各一份
     (`default` / `public` / `aicc`,产品真正走的是 `aicc` 那份)。各自备份为
     `*.pre-gwrename-20260824` —— 后缀不以 `.xml` 结尾,include 的 `*.xml` 通配装不进去,
     否则会得到两个同名网关。`reloadxml` + `sofia profile external restart reloadxml` 生效。
     **真机验证(wei/1008 click-to-dial → 18688886669,接通后对端挂断)**:
     `[aicc->pstn_gateway_outbound]` 正则 PASS →
     `bridge(…origination_caller_id_number=95001…sofia/gateway/pstn_gateway/18688886669)` →
     18:34:00 对端应答 → 18:34:04 NORMAL_CLEARING 双向拆线。
     CDR `01a03355`:OUTBOUND / ANSWERED / ring 1 / talk 4 / total 6 / `legs=[TRUNK|18688886669]`
     / `switchBillSec=4` —— 与改名前那通外线同型(VC-S14-04)。改名后日志里 `pstn_sim` 出现 **0 次**。
     **变量改名是会静默失败的那一处**:dialplan 引用未定义的 `$${pstn_gateway_caller_id}` 会渲染成
     空 caller id 而电话照打,日志里它解析成 `95001`,才说明变量是**跟着引用一起**改的。
     **仓内那一半由第二通电话闭合(同日)**:click-to-dial 不读 `EndpointFormat` ——
     坐席腿是 `sw.Endpoint(分机)`,应答后 `uuid_transfer` 进 dialplan;
     `AICC_OUTBOUND_ENDPOINT` 只有 **AI 外呼 `DialAI`**(`outbound.go:281/342`)才用。
     故补打一通 AI 外呼(`POST /calls` AI_OUTBOUND,did=95002,`45e04474`):
     交换机侧出的正是 `sofia/gateway/pstn_gateway/18688886669`,
     **两条路径的网关名至此都跑过了**。这通同时把 AI 外呼全链又走了一遍 ——
     应答 → bot(qwen/zh,`novanet_support` 的 `welcome`)→ 主叫要人工 →
     `transfer_to_agent` 进 `support-zh`(ext 7002)→ 1002 接起 → 挂断。
     CDR:`OUTBOUND / ANSWERED / 95002 → 18688886669`,`legs=[BOT 12s, QUEUE support-zh 2s,
     AGENT 1002 5s]`,`bot_sec=12` / `talk_sec=4` / `total=22`,零幽灵行。
  2. ~~**W11.2 号段分配器**(D2/D3)~~ **【已完成 2026-08-24】** `AICC_EXTENSION_RANGE`
     (默认 `1000-1999`,写进 `.env.example`)+ `CatalogStore.AllocateExtension`。
     **分配与插入是同一个事务**:单给一个"空闲号"是假的 —— 它在返回的那一刻就可能被别人拿走。
     事务里先取池锁(`pg_advisory_xact_lock`),再 `generate_series` 找最小空闲号,再插入。
     **池锁的键必须不同于实例锁**(`0x414943430001` vs `0x41494343`):会话级与事务级 advisory
     锁共用一个键空间,而本进程终生持有实例锁 —— 复用那个键会让第一次分配永远等自己,
     无报错、无超时。
     **配置解析失败直接拒绝启动**,不回落默认值:症状会是"分机发在了没人选的池子里",
     而一个能用的分机看不出它来自哪个池。
     **真库测试四条**,含并发一条 —— 它的断言是"八个全部成功",不只是"号码互不相同":
     没有锁时两个事务读到同一个最小空闲号,失败的那个撞 `uq_extensions_number`,
     **症状是请求报错而不是重复行**。摘除验证:去掉池锁 → 八个里五个当场
     `duplicate key value violates unique constraint`。
     另外三条:**空洞复用**(建 1000-1002、删 1001,下一个必须是 1001 而不是 1003 ——
     这是 D3 禁止 `MAX+1` 的全部理由)、**池外号码不受影响**(队列的 7002 既不会被发出去、
     也不会把分配推过头)、**池满回 `ErrPoolExhausted`**(不是冲突:没有任何东西撞上,
     是这个部署的号段用完了)。
     顺带把 `Extension.validate` 拆出 `validateApartFromNumber`:分配时号码要到事务里才有,
     而**握着池锁去发现口令太短**会让一个坏请求变成所有人的问题。
     **范围止于 service 方法**,没有契约、没有 handler、没有 UI —— 那是 W11.3 的消费者。
  3. ~~**W11.3 Users 写侧**~~ **【已完成 2026-08-24 `d8931da`】** `POST /users`、
     `PUT /users/{userId}`、`POST /users/{userId}/password` + `/admin/users` 页面。
     **建账号是一个事务写三行**(账号 / ACD 身份 / 话机):接电话的账号缺一不可用,
     而且**重名必须一分钱不花** —— 这条才是这个设计的收益,测试就钉它:
     重名被拒后下一次分配拿到的仍是同一个号。没有回滚的话,每一次填错表单都会
     **无声地**烧掉一千个号里的一个(烧掉的号和用掉的号长得一模一样)。
     **先插 user、后锁池**:重名要由唯一约束拒绝,不能握着所有人排队的那把锁去发现它。
     **主管有身份有话机,管理员两样都没有** —— "ADMIN 是 user 但不是 agent"是双向的:
     接不了电话的主管不是在管一个呼叫中心。**代价说清楚:主管会出现在 `/agents` 花名册、
     看板和 mod_callcenter 里(不配员,所以没有电话会派给他)。**
     **降级永不删 agents 行**:`agent_state_logs` / `wrap_ups` 挂在它上面级联,
     删掉就是毁掉这个人做过什么的记录 —— 而"改一下这个人的角色"没有要求这个。
     升级才是配的那个方向,问两次不会配出第二部话机。
     **最后一个在岗管理员不能被降级或停用**,计数与变更在同一事务内,
     两个管理员不能靠同时动手互相降级。独立错误码,因为运营能对它做点什么:先提拔一个人。
     **供应绕过了 agents 服务,所以那个服务替自己做的镜像要显式调用** ——
     否则坐席在产品里存在、在 mod_callcenter 里不存在,而**在队列派不出单之前没有任何东西会说**。
     话机的 SIP 口令是生成的、任何响应都不返回(D5),读取走 W11.4 的 reveal 端点;
     **Auto answer 只离开表单**,列与接线保留,新建坐席一律 false。
     `/admin/agents` **退役而不是改名**:路由叫 agents、标签叫 Users 正是本仓在清的那种漂移;
     **代价:重绑话机在 W11.4 给分机装上目标选择器之前没有界面。**
     真库测试六条(三行齐全 / 管理员只有账号 / 重名不烧号 / 降级保住身份与历史 /
     升级配话机且幂等 / 最后一个管理员的守卫)。
     **现场验证**:建主管 → `1020`(号段里最小空闲号,1000-1019 已占)、
     `luacc.directory` 里看得到它(交换机据此放行注册)、用新账号登录 200、
     审计行里初始口令是 `"[redacted]"`(C58 的按字段名脱敏接住了它)。
     **话机注册的真机验证归 W11.4** —— 生成的 SIP 口令在 reveal 端点做出来之前没人读得到。
  4. ~~**W11.4 Extensions**~~ **【已完成 2026-08-24 `813f619` + `c990676` + kind 收窄】**
     `GET /extensions/{extensionId}/password`(ADMIN,每次读都记审计)、按 kind 切换的目标选择器、
     号码预填下一个空闲号(可编辑,越界/占用**在输入时**报而不是提交后)、口令三态。
     **D1 的关联翻转没有做,而且不该做**:`agents.default_extension_id` 那一侧已经有 D1 要的
     两道保障(`uq_agents_default_extension` 一人一机 + `fk_agents_extensions RESTRICT`
     拒绝删除有人在用的分机),而 **00015 是特意把删除守卫放进数据库的** —— 起因是分机被静默
     解绑后坐席仍 READY、队列继续派单,那次迁移的原话是"the delete must fail even when it
     arrives by psql"。翻过来会把这道守卫连同 VC-S14-01 一起拆掉,换不来任何行为收益。
     故 `00017` 只加了没有住处的 `flow_id`/`queue_id`。
     **口令三态,没有一态往 input 里渲染占位密文** —— 一排点是对"里面有什么"的谎言,
     而浏览器会把它原样提交回来。新建:生成→只显示一次+Copy,**明文只存在 ref 里**,
     不进 form state、不进序列化草稿、不进 input 的 value,关闭表单即遗忘;
     已有:**不渲染输入框**,Copy 走审计端点、Reset 替换。非 ADMIN 由路由守卫挡住,按钮不渲染。
     **reveal 在 handler 里审计而不是中间件**:中间件保证的是**变更**的结构性覆盖,
     按路径往里加一条读正是 C58 那种会过期的守卫形状;泄露是另一类事件,
     而知道自己泄露了的是那个 handler。
     **`00018` 把 kind 收成 `AGENT | QUEUE`(owner 直裁,选项 B)**:`PLAIN` 是 `AGENT 未绑定`
     的第二种拼法(00017 自己的 CHECK 就把两者当同一件事,而 live 库 21 个分机全是 AGENT、
     只有 7 个真绑了人 —— "没人用的裸话机"已经被表达了 14 次);**`BOT` 路由不了任何东西** ——
     来电经 `aicc_inbound.lua` 桥到 `sofia/gateway/aicc_bot/<did.number>`,应用再按 DID 解析
     `dids.flow_id`,**分机表在这条路上一次都没出现**。`flow_id` 随 BOT 一同删除;
     指向 flow 的是 DID,而那一列 W3 的 flows 列表已经有了。
     **现场**:BOT 分机绑 flow→201 / 不绑→422 / 改 PLAIN 清空目标 / psql 造矛盾被 CHECK 拒绝;
     收窄后 `BOT` 回 422、live 的 21 行毫发无损、`flow_id` 列已消失;
     **真 SIP 客户端用 reveal 到的口令注册成功(`Registered(UDP)`)—— W11.3 欠的那条话机注册验证随之闭合。**
     **`00019` 把分机收回它本来的样子(owner 直裁,选项 C)**:`kind` 与 `queue_id` 双双删除。
     一个只剩一个合法值的列什么也没分类,而 `queue_id` 从来没有用途 —— 顺着它走能看清整个想法
     错在哪:**这张表喂的是 `luacc.directory`,也就是 SIP 注册**,一行就是一个话机可以注册的账号
     (带口令)。队列不是被注册的东西,它是被拨的,`aicc_queue.lua` 经
     `luacc.queues WHERE ext_number` 找到它,**从不查这张表**。给队列建一行不会让它可达,
     只会让它的号码可注册 —— 白买一个暴露面。
     队列号仍归 `queues.ext_number`(那里本来就有唯一约束),改成**从 `AICC_QUEUE_RANGE`
     (默认 7000-7999)自增**,与分机同一套分配器,共用同一把池锁(两个池、一列写者:
     建队列和招人一样罕见,而第二把锁是第二件要推理的事)。
     `POST /extensions` 保留为自动化入口(与 `flowadd` 同例),但**页面去掉了"新建"** ——
     话机随账号而来。Extensions 页变成只读 + 凭据操作,列显示"已分配/未分配"而不是 kind。
     **现场**:建队列不给号码 → `extNumber=7000`;`extensions` 只剩
     `id,number,password,display_name,is_enabled,created_at,updated_at`;
     **开发库里 14 个无主分机已清掉**(21 → 7,全部有主) —— 它们是手工建的残留,
     `luacc.directory` 却一直把它们当可注册账号下发。
  5. ~~**W11.5 Numbers**(D7/D8/D9)~~ **【已完成 2026-08-24 `00020`】**
     **两个布尔而不是枚举或数组(D7)**:既能被打进来又能打出去的号码是常态,枚举说不出来
     除非再加一个"both",而数组让每个读者为一个是非题去解析一个集合。
     **`flow_id` 从列级规则变成条件规则(D8)**:要求每个号码都有 flow 会禁掉一条合法的行 ——
     只用来外拨的号码没有 bot 可绑,因为没人会打它。P4 真正管的是**能被打进来**的号码要有 flow 来接,
     CHECK 现在就是这么写的。**这条 supersede 了 `m4-cleanup-findings` F-01 里
     "等有了 flow 选择器就恢复 NOT NULL"的计划** —— 选择器有了(W3),而更强的列现在反而是错的。
     **改写在收窄之前跑,而且真的有一行要接**:95099 启用、默认呼入、既无 flow 也无兜底队列,
     今天打过去什么也到不了。新规则下它不能声称能接呼入,于是它不接 —— 变成纯呼出,
     那是无 flow 号码唯一允许的形状。**没有凭空发明什么**:它在这次迁移之前也没在接呼入电话。
     **缺省外呼号是部分唯一索引(D9)**,"至多一条"由数据库回答,而不是三个调用点各自记得去检查;
     不能外呼的号码不能当缺省,由第二条 CHECK 定死而不是交给表单。
     **坐席外呼取缺省号,取不到就快速失败,不回落**:以前 `effective_caller_id_number` 不设,
     中继就presents 网关里配的那个号 —— **运营从未选过、每通都在用、只有问被叫看到了什么才查得出来**。
     拒绝会说出来,回落不会。(`AICC_OUTBOUND_CLID` 设了的部署仍以配置为准:那个变量早于这个标志,
     悄悄移除会改变一个正在工作的部署呈现的号码。)
     **现场**:live 库迁移后 95001/95002/95011/95012 仍是呼入、95099 变成纯呼出;
     把 95099 设为缺省外呼号 200;呼入不给 flow → 422;第二个缺省外呼号 →
     `uq_dids_default_outbound` 拒绝。单测钉住"没有缺省号时坐席的话机根本不响"。
     **W8(trunk 管理面)排在其后**,免得中继号的契约评审和这里的方向列打架。
  6. ~~**W11.6 Bot Flows 加"指向该 flow 的分机"列**(D10,join)~~ **【W3 已做,2026-08-24 复核确认】**
     那一列就是 flows 列表的 **Numbers**,`useDIDs()` 客户端 join 出来的,没有冗余列(D10 满足)。
     **条目原写"分机",而指向 flow 的是 DID**:来电经 `aicc_inbound.lua` 桥到
     `sofia/gateway/aicc_bot/<did.number>`,应用再按 DID 解析 `dids.flow_id` ——
     分机表在这条路上一次都没出现(见 W11.4 的 `00018`,BOT kind 因此被删)。
     复核现场:`novanet_support` 一行显示 `95001 · 95002 · 95011 · 95012`,
     而 **95099 正确地不在其中**(它没有 flow、是纯呼出号)—— 这同时证明 join 走的是
     `flow_id` 而不是更松的条件。

  **验收(硬性):**
  - 改名提交(W11.1)的验收是"行为不变",**含一通真实呼叫走完全链**。
  - **呼入过滤、缺省外呼、话机注册三条必须真机验证,单测不算。**
  - lua 里的裸 SQL 与写死的 domain 字面量漏改会编译通过、单测全绿、真机注册失败 ——
    每一步都要过一遍 `luacc.*` 视图与 `freeswitch/scripts/*.lua`。

- **W10(new,owner 直裁 2026-08-22)** **生产禁用 `loopback`。** AI 外呼路径目前默认
  `AICC_OUTBOUND_ENDPOINT=loopback/%s/aicc/XML`(本次已把 `default` 改成 `aicc`,但 loopback 本身还在)。
  真实部署把它设成自己的中继 `sofia/gateway/<gw>/%s` 即可绕开,**但代码不该以 loopback 为默认形态** ——
  应改成 click-to-dial 已在用的那套:originate + park,应答后 `uuid_transfer` 进 aicc 拨号方案。
  相关约定同批落地(已生效):**所有交换机命令的 context 默认 `aicc`**
  (`adapter.go` `TransferToExtension`、`outbound.go`、两个 lua 的 `transfer … XML aicc`);
  **用户目录的默认 context 也是 `aicc`**(`aicc_xml.lua`)。见设计 01 §7 **D6a**。

**C 系列(既有立案):**
- **C1** VC-S3-02 根因修复。~~DeleteCallcenterTier 不对含域名字二次限定~~ + converge removed 以复查为准;
  修后重跑 S3-02。
  **【修法变更 2026-08-22,owner 直裁:"queue name 应该不需要 @domain,从源头就应该去掉"】**
  原修法是让删除**处理**含域的名字;新修法是**让这类名字不存在** —— 队列名不再带 `@domain`。
  那个后缀本就是我们加的(`tiers.go` 原注释:*"The domain suffix is ours, not mod_callcenter's…
  The switch appends nothing"*),单租户下不携带任何信息,却嵌进了**会变的主机地址**:
  本机由 `.176` 变 `.55`,旧名字下的每一条 tier 就成了 converge 既匹配不到、也删不掉的东西。
  已从源头四处去掉:`adapter.go` `QueueName`、`tiers.go` `bareQueueName`、
  `aicc_xml.lua`(渲染 callcenter.conf)、`aicc_queue.lua`(送主叫进队列)。
  `bareQueueName` 改为在**第一个 `@` 处截断**,好让 2026-08-22 之前写下的旧行**仍能被认出**
  并由 converge 清掉 —— 认不出的 tier 就是删不掉的 tier,这正是那一行活过一次改址的原因。
  测试:`TestAQueueNameCarriesNoDomain`、`TestATierUnderAnOldAddressIsStillThatQueue`。
  ~~**待办**:`aicc_fs` 库里现存的旧名字行需清掉并让应用重建(与 VC-S12-04 同一动作,见该例)。~~
  **已完成(2026-08-22,随 VC-S12-04)**:清库重建后两侧带 `@` 的行数均为 **0**,
  `.176` 那行消失;VC-S3-02 **重跑 FAIL→PASS**,且这次的通过是结构性的而非 08-21 那种假通过。
  **⚠ 但 C1 仍开启:第二半未修。** 直接探针(2026-08-22):
  ```
  callcenter_config tier del does-not-exist agent-wei  → +OK
  callcenter_config tier del support-en agent-nobody   → +OK
  ```
  交换机对 miss 的删除同样回 `+OK`,而 `converge`(`catalog/service.go:288-295`)只要
  `DeleteCallcenterTier` 返回 nil 就 `removed++`,**不复查** —— `removed=` 依旧可能谎报。
  **剩余修法**:删除后复读 `tier list` 再计数。**验收不能用 VC-S3-02**(它断言的是全量一致性,
  谎报不体现在那里),需要一条新用例:令 converge 删一条不存在的 tier,断言 `removed=0` 而非 1。
  **【状态变更 2026-08-21,阶段 5 开跑前复查】症状已潜伏,缺陷未动。**
  陈旧行 `support-en@192.168.31.176|agent-wei` **仍在 `/usr/local/freeswitch/db/callcenter.db`**,
  但因同名队列早已不存在(本机 IP 固定为 …55),mod_callcenter 只列已加载队列的 tier,
  故它从 `callcenter_config tier list` 中消失;converge 读不到它,`removed=1` 的谎报随之消失
  (今日日志为 `already matched … removed=0`)。
  **代码缺陷一字未动**,只要该域的队列再次出现(IP 换回、或多机部署里另一台的域进入视野),
  行与谎报会一并回来。**⚠ 此时重跑 VC-S3-02 会得到假通过**,判定维持 FAIL。
  原"禁止手工清除 stale tier"的约束**作废**——它已不在 `tier list` 里,无可清除;
  需要复现时改为:往 `callcenter.db` 的 tiers 插一行异域条目,或临时建一个该名字的队列。
  阶段 5 的 S12-03 重启 FreeSWITCH 会让 mod_callcenter 从该 db 重新加载,那行**有可能重新浮现**
  —— 若浮现,C1 即重获活的复现场景,是好事。
  **【后半已修 · 2026-08-24 `799963e`,留证 `artifacts/C1/verdict-second-half-2026-08-24.md`】**
  **前提今日重探仍成立**:`tier del does-not-exist agent-wei → +OK`、
  `tier del support-en agent-nobody → +OK`。
  **修法**:两个循环只记**尝试次数**;有过尝试才再读一次交换机自己的 tier 表,
  按**前后差集**得出 added/removed。没有尝试就不重读 —— 常态(两侧本就一致)的
  交换机读取次数不变,仍是一次,不是两次。
  第二次读失败**不静默回落**到尝试数,而是照报尝试数并标 `isVerified=false`;
  静默回落等于回到原点 —— 一个没有东西支撑的数字。
  **现场(诚实路径未改坏)**:给 `agent-ben` 手工插一条库里没有的 tier
  (`tier add support-en agent-ben 1 9`),重启触发对账得
  `agent=agent-ben desired=1 actual=2 added=0 removed=1 failed=0`,
  且 `tier list` 确认真的没了;全程日志 `isVerified` 出现 **0 次**。
  **谎报本身仍无现场用例**,理由与"症状已潜伏"同 —— 能回 +OK 却删不掉的是异域陈旧行,
  而它不出现在 `tier list` 里,converge 根本读不到、也就不会去删。
  复现需先造场景(往 `callcenter.db` 插异域 tier,或临时建同名队列),不是修复的前置。
  谎报路径由单测钉住,直接构造"答 +OK 但 tier 表不变"的开关。
  **新用例**(印证"验收不能用 VC-S3-02")`TestConvergeCountsWhatTheSwitchDidNotWhatItAccepted`
  四型:①答 +OK 却没删 → `removed=0`;②真删了 → `removed=1`;③真加了 → `added=1`;
  ④复读失败 → `isVerified=false` 且仍给出尝试数。已验证去掉复读后 ①④ FAIL。
  **⚠ VC-S3-02 的假通过风险不变**:它断言的是事后一致,谎报不体现在那里;
  重跑它之前仍须按上面的办法重造场景,否则拿到的仍是无判别力的 PASS。
- **C2(已修 2026-08-24 `7c10489`,留证 `artifacts/C4/verdict-2026-08-24.md`)**
  SYS-6 契约缺口:PartySnapshot 补 isBotLeg。**纯契约缺口** —— Go 的 `PartySnapshot`
  早就带着 `isBotLeg`(`call.go:459`)、线上一直在发,只有 `docs/openapi.json` 没声明;
  实现侧一行未动。它必须有名字的理由:**和 bot 通话的主叫,这通电话有两条腿、只有一个人**,
  而这个对象里没有别的字段能把那条腿与"一个恰好没有 agentId 的坐席腿"分开。
  `make api-breaking` 无破坏(新增可选字段)。
- **C4(已修 2026-08-24 `7c10489`;后半撤回,留证 `artifacts/C4/verdict-2026-08-24.md`)**
  死状态删除(D7①③ 已决)。
  **前半成立并已执行**:`Call.ENDING` 删除(定义、契约枚举、生成物)。呼叫在**最后一条腿释放**时
  结束,那是一个事件,所以**从来没有一个时刻处在 ENDING**;定义过、从未赋值、从未读取。
  `make api-breaking BASE=main` → **无破坏**(响应侧枚举收窄)。
  **⚠ 后半撤回:转写 `ENDED` 不是死值。** 删除时**编译锁拒绝了构建**
  (`transcript_handlers.go:68: undefined: api.TranscriptionStateENDED`)。
  它是**快照对一通已结束呼叫的回答** —— 呼叫不再存活、也没有 actor 可问时由 REST 合成;
  前端消费它(`live-transcript.tsx:35` 上色、`lib/transcript.ts:103` 推导),
  并有一条**以它命名的测试**(`freezes at ENDED after a call rather than falling back to IDLE`)。
  删掉它会打断一个自带测试的行为。
  **这条最该记的是:证据早就在账本里。** VC-S9-02 在 **2026-08-20** 执行时当场查明并写进了 status
  ——"发现 ENDED 是 REST 收官合成值(handlers:69)→ D7③ 前提推翻待重议"——
  而本条目直到 2026-08-24 仍写着"死值",**整整四天**。与 C49 同型:话已经说出来了,没人回去改结论。
  **处置**:契约保留 `ENDED` 并在 description 写明生产者是 REST 快照而非 actor;
  `actor.go` **保留** `StateEnded` 并写明它在此处永不赋值的原因 —— 镜像要完整,
  否则下一个人比对契约时会得出和本条一样的结论。
  **连带订正(两个方向都改)**:01-telephony.md 的 Call FSM;enums.md 的 ENDING 行(已删除)
  **与 ENDED 行(由"死值"改为记明生产者与消费者)**;fsm-edges.md 的 ENDING 边与 D7①;
  ledger.yaml 的清单行;以及 **VC-S9-02 的 expect** —— 它原写"契约有值但系统从不发布",
  改为"**流上**不出现,由 REST 快照合成"。
- **C7(已修 2026-08-24 `6078fe3`,留证 `artifacts/C7/verdict-2026-08-24.md`)**
  queue_events 读路径 or 修正 cdr.go:112 注释。
  **owner 选定选项 1、最小面积** —— 只做 `GET /calls/{callId}/queue-events`,不做面板、不做聚合;
  理由(owner):符合 event sourcing 原则,后期 supervisor 与可观测性都用得上。
  **决策时才发现两个选项的差距比条目写的大**:这张表在无人读的这段时间里积下了
  **273 行 OFFERED 对 89 通排队呼叫**,其中**七通被派单 10–33 次,且每一通都只派给同一个坐席**。
  最重的那通 CDR 只说 `ABANDONED_WAITING / 220 秒 / 一个 agent_id`;
  经新端点读出来是:**33 次派单、三分半钟、全部派给 807b2164、主叫在 208 秒时放弃**。
  **与 C37 的关系如实记**:这是 C37 的形状,把**一次轶事变成七个有时刻有计数的样本**,
  但**不证明** C37 的成因(那条挂死的 INVITE 仍未查清)—— **C37 维持开启**。
  **权限主管专属且是刻意的**:这些行点名了通话曾派给哪些同事、谁没接,
  正是"一条腿的事件只发给它自己的坐席"要挡在坐席视野外的东西;坐席读得 403。
  空数组是**答案**不是缺失。
  **实现要点**:契约先行(`listCallQueueEvents`,api-lint/check/breaking 全绿,新增端点无破坏);
  新端点出参用 `api.*` 不外泄 `store.*`;迁移 `00016` 建**部分索引**
  `(call_id, occurred_at, id) WHERE call_id IS NOT NULL`(call_id 可空 —— 交换机归属不了的移动照样记);
  排序 `(occurred_at, id)`,因为同一通的两次移动可能同毫秒,**两次读顺序会变的旅程不是旅程**。
  测试 `TestACallCanBeAskedForItsQueueJourney` **需真实 PostgreSQL**(部分索引与排序 fake 复现不了)。
  **`cdr.go` 那句注释同时改掉了** —— 它现在描述的是事实。
- **C10(defer 第二期不变,但待办内容已订正 —— 2026-08-24)**
  202 契约核对(SYS-2 源头)。本期账本 expect 维持 202。
  **【owner 2026-08-24】**"客户端是通过 SSE 事件的 correlation id 来关联的"——**这是设计意图,不是现状。**
  **核实结果:该机制目前不存在,一处都没有。**
  - `hold/retrieve/transfer/dtmf` 的 **202 没有响应体**(契约里该响应只有 description,无 `content`),
    调用方拿不到任何句柄;
  - **SSE 信封没有这个字段**:`version / seq / type / occurredAt / callId / callType /
    partyId / agentId / queueId / payload / userData`,没有请求侧的关联标识;
  - `middleware.RequestID` 确实挂着(`server.go:118`),但它**只进日志**,既不进响应体也不进事件。
  故本条第二期的待办由"核对 202 是否合适"订正为:**建立请求↔事件的关联标识** ——
  202 回一个句柄、事件信封带上同一个句柄,两处都是契约变更;之后才谈得上写那条闭环用例。
  **202 本身没有问题**:语义就是"收下了,结果稍后由事件送达",契约与实现一致。
  缺的是让调用方**认得出**那条结果的东西。

  **【设计已定稿 2026-08-24,与 C42 另立的 webhook / 推送项目一起做,本期不动代码】**

  **命名取 `correlationId`,不取 `requestId`。** 理由不是"更好懂":
  ①`requestId` 在本仓**已经被占**——`middleware.RequestID`(`server.go:118`)已经在往日志
  上下文里放一个 requestId,线上再来一个同名不同来源的,比两个名字都糟;
  ②语义也反了——requestId 通常是**服务端铸**的、给日志用,而这里这个是**客户端铸**的;
  `correlationId` 说的是"拿来把两样东西系在一起",且是消息/事件系统的既有词
  (JMS `JMSCorrelationID`、AMQP `correlation-id`、CloudEvents),使用者不用学。

  **① 请求侧:一个可选的头** `X-AICC-Correlation-Id`,客户端自己铸值。
  **为什么是头不是 body**:`hold/retrieve/answer/hangup/mute/unmute` 今天**没有请求体**,
  为带一个 id 给它们加 body 等于为所有现存调用方改契约形状;而 `transfer/dtmf` 有 body,
  那就会出现两种写法。头对**经 `callOp` 的全部八个操作**是同一种写法,与既有 `X-AICC-Csrf` 一致,
  `oapi-codegen` 直接支持 header param。
  **不传就什么都不变**(owner 口径)——现有调用方一字不改,事件上也不多出谁都没要的字段。

  **② 响应侧:原样回显同一个头。** 202 **保持无 body**。回显有两个作用:确认收下了、
  并**确认这个服务端支持关联**(老版本不会回显)。

  **③ 事件侧:信封加一个可选 `correlationId`**,**只打在这次请求预期产生的那一条事件上**,
  不是那通电话之后的所有事件。

  **④ 中间这一段是难点:事件不是请求发出来的。** 事件由呼叫的 actor 发布、被**交换机的事件**驱动,
  actor 对那次 HTTP 请求毫无记忆。所以要有寄存处,**键必须是回来的交换机事件也带着的东西**——
  **channel id + 期待的事件种类**:`Hold(callID, agentID)` 本就经 `handledChannel` 解析出
  `channelID`,回来的 `CHANNEL_HOLD` 带的正是它。
  `correlations[key{channelID, KindChannelHold}] = {id, expiresAt}`,在 `transition` 发布前取出。
  **这不是新模式**:`outbound.arm()`(按 channel 键、带 `expiresAt`、单发)就是同一个形状,复用它。
  三条规则:**单发**(取出即删,否则下一次无关的 hold 会被贴上陈旧 id);
  **TTL 30 秒**(交换机的事件通常一秒内回来),**过期就丢**——事件照发、只是不带 id;
  **同步发布的走直路**——`mute` 的 `PARTY_CHANGED` 在命令路径里同步发出、不经交换机事件,
  直接挂上即可。统一入口是 `r.Context()`:`callOp` 放进去,协调层取出,同步的直接用、异步的寄存。

  **⑤ 这个设计不解决的事,必须写明**:**"命令被接受、事件永远没来"仍然分不出来。**
  关联 id 让你认出**来了的**那条,它不会替你产生**没来的**那条。
  **C37 那部卡死的话机就是这一型**:`+OK`、202,然后什么都没有。
  要闭合这一半,得在**寄存条目过期时发一条带同一个 correlationId 的失败事件**——
  那是新增事件类型,影响面大得多。**分两期:先做关联,再看要不要做超时告知。**

  **⑥ 值不值得,如实说**:**本仓自己的前端不需要它**——`useCallActions` 拿到 202 就
  `invalidateQueries` 重拉快照(`agent.ts:200`,注释写明"状态以交换机为准,所以重取而不是猜"),
  绕开了这个问题。真正需要它的是**外部 API 使用者**(即 C42 另立的 webhook / 推送项目)
  与**诊断"接受了但什么也没发生"**。故与那个项目同批做,而不是单独立项。

  **⑦ 工作量**:契约(8 个操作各加一个 header param、信封加一个可选字段、202 加一个响应头)
  → generate → `callOp` 透传 → 协调层寄存 → 测试。**不改任何现有行为**,不传头就完全等价。

- **C11(new,2026-08-20 T3.1 执行发现;2026-08-21 已修并现场复验)** 转接呼叫的 CDR 组装丢失 bot 份额与队列等待账:
  通话中探针证明 aicc_bot_sec/aicc_flow_id/aicc_language/aicc_did 四戳都在主叫通道上,挂断后 CDR 仍
  bot_sec=0/flow 空 → 丢失在读回/快照侧(botShare switchevent.go:81 / registry.go:346 / 合并路径);
  同时 queue_wait_sec=joined→left(把通话时长计入等待,真实等待应为 joined→bridged;queue_events 的
  BRIDGED wait_ms 反而正确)——疑似 Queue.BridgedAt 与 Bot 同因丢失。在野样本:上午 95002 三行 +
  本次 01a01ea6-76d1。修复后重跑 VC-S3-03。
  **根因(2026-08-21 实测,与立案时的猜测不同——读回侧无罪)**:是两条独立缺陷。
  ①`aicc_inbound.lua:64` 的 `export_vars` 把 `aicc_did` 导出到 bot 腿;转接时 `session.Close()`
  让 bot 腿**立刻**挂断,带来一份只有 DID 的 share,而 registry 当时的规则是"先到者胜"
  (`if a.call.Bot.IsZero()`),于是一两分钟后主叫挂断带来的完整 share 被整份丢弃 —— 这正好解释了
  那个一直没被解释的分界:Lua 导出的 did/language 有值,app 盖的 bot_sec/flow_id/user_data 全空。
  改为 `BotShare.Merge` 按字段填补。②mod_callcenter 的 `bridge-agent-start` 抛在**坐席腿**上,
  `normalizeCallcenter` 只在 `ChannelID==""` 时才回落 member 通道,于是 `Queue.BridgedAt` 落到坐席腿
  所在的 call(修 C20 前那是幽灵 call),merge 时被 `if call.Queue.JoinedAt.IsZero()` 挡掉而整份丢失。
  改为 member 相关六个 kind 一律按 member 通道路由。
  **复验(95002→ben)**:bot_sec=17(原恒 0)、flow_id 非空、botSummary/botReason 落库、
  queue_wait_sec=4 与 `queue_events.BRIDGED wait_ms=4513` 吻合(LEFT 为 14000)、
  legs 首次出现 `BOT 17s`;两通产 2 行 CDR 无重复,幽灵计数不动。
  单测三条:`TestBotShareMergesAcrossLegs`、`TestQueueEventsRouteToTheWaitingCaller`、
  `TestTheBotShareSurvivesTheLegThatHangsUpFirst`(摘掉修复即报线上那三个值)。
  详见 `docs/verification/artifacts/C11/verdict.md`。**VC-S3-03 已于 2026-08-21 12:06 用 95001 重跑,FAIL→PASS**(bot_sec=13、has_flow=t、legs 含 BOT、单行 CDR)。
- **C13(new,2026-08-20 T3.2 执行发现;2026-08-21 已修,S4-01 条款待重跑转正)** PARTY_RINGING payload 的 extensionNumber/toNumber 携带
  浏览器话机的 WS 注册标识(实测 "g7bih4lv")而非坐席分机号:coordinator.go:412-416 直取
  ev.DestinationNumber,而 :829-848 的 agentForLeg 早已把腿正确归户——归户成功后应以坐席绑定分机
  回填屏显字段。修复后 S4-01 的该条款转正。
  **已修(2026-08-21)**:`agentForLeg` 现在把**匹配到的那个候选**一并返回
  (`dialed_user` / `aicc_extension` / `DestinationNumber` 三选一),`addParty` 透传,
  PARTY_RINGING 的 `toNumber` 与 `extensionNumber` 都改用它。回归测试
  `TestARingingLegNamesTheExtensionNotTheContactToken` 以实测那个 `"g7bih4lv"` 为原型,
  摘掉修复即报出该 token。**S4-01 的该条款可转正**(下次重跑时核)。
- **C14(new,2026-08-20 T3.4 执行发现,FAIL 立案;2026-08-23 复测【在 qwen 路径上不复现】,未修)**
  ASR tap 摄取路径丢帧:一通 ~23s 的转写
  HUMAN_AGENT 丢 46/1146(4.0%)、CUSTOMER 丢 78/1084(7.2%)("transcribe: audio was dropped",
  pump.go:226),识别文本随之崩坏(fox 句 → "Butro focus jobs owing the lazy workin")。pump 计数器
  证明是"没送到"而非"听错"。候选:pump 背压/缓冲、双流并发写。修复后重跑 VC-S9-01。
  **【2026-08-23 复测结论】** 三通 tap 在线的电话(19s / 41s / 36s,每侧合计约 4800 帧),
  **丢帧全部为 0**。立案那次是 **openai** 路径,而本轮是 **qwen**:
  openai 每帧 base64 + JSON 封装、24 kHz;qwen 是裸二进制帧、16 kHz —— 约三分之一的字节量,
  外加每秒每路 50 次 JSON 编码的省去。
  **不写成"已修"**:丢弃策略、队列深度、`dsWriteWait`/`oaWriteWait` 一行没改。
  准确说法是 **"在当前部署的 provider 上不复现"** —— 与 C32 那种"同配置下不再复现"**不同**,
  **这里是配置本身变了**;openai 路径是否仍丢帧**未测**(测它要花 OpenAI 的钱,由 owner 定)。
  **顺带证伪了立案时的一个推断**:原文把崩坏归因于丢帧,而 8-23 第二通**零丢帧照样崩坏**
  (后查明是执行者同房间同时扮演主叫与坐席的串音,把手机拿开即一字不差)——
  崩坏与丢帧本来就不是同一件事的两面。另注 qwen 的 `OwnsEndpointing: false`,
  切分是引擎自己做的,不是我们的 600ms 静音规则。
  **已留下诊断**(`diag(transcribe)` 提交,行为未变):真再遇上,pump 会说出是三种里的哪一种 ——
  `dropRuns`(丢帧**开始**了几次,而不是丢了几帧)、`slowSends`(单帧发送 ≥20ms,即一帧的时长,
  过了就是按定义在落后)、`maxSendMs`(最慢那一次;平均值恰好会藏住那个一口气清空两秒队列的卡顿)。
  这条诊断的测试在打第一通电话之前就抓到了我自己的错:第一版 `dropRuns` 在"驱逐一帧后重试成功"时清零 ——
  而那次重试**必然成功** —— 于是一次卡顿会被记成每帧一次,**那个数字会长得像个答案**。
- **C15(new,2026-08-20 T3.5 执行发现,即时根因;2026-08-20 已修并上线 b174a5c)** click-to-dial 从未能工作:outbound.go:164 设
  `origination_caller_id_name = "Dial "+destination`(带空格),renderVars(adapter.go:309-321)不对值
  加引号 → FS originate `{…}` 段 "Parse Error!" → DESTINATION_OUT_OF_ORDER(fs 日志 19:02/19:04 两次实证,
  audit_logs 167/165 对应请求)。**已修并上线(commit b174a5c,2026-08-20)**,三处一并:
  ①`origination_caller_id_name` 去空格("Dial-1007");②renderVars 维持裸拼接并写明约束——加引号会
  破坏 inline transfer 的外层单引号,实测报 "Invalid Application 1007"(第二形态);
  ③**click-to-dial 改 transfer 形态**(owner 指示:loopback 难追踪)——坐席腿应答后
  `uuid_transfer <leg> <destination> XML default`,路由交还 dialplan(内部/外线由它决定),
  BridgeToEndpoint+loopback 退出 click-to-dial 路径;AI 外呼仍用 loopback,默认值已钉 `/XML`
  (loopback b 腿继承 a 腿 dialplan,继承到 inline 就把号码当应用名——第三形态)。
  ~~待办:live 复测(1008→1007 内部、1008→外线号)后 T6.11 起草 case。~~
  **已了结**:两型 live 复测均已通过,结果记在 **C17**;T6.11 已起草为 **VC-S14-04**(已并入)。
  **附带旅程缺口 G-A7**:坐席外呼(DIAL OUT / POST /calls/dial,internal/outbound 整个服务)不在三旅程
  与任何 case 内。
- **C16(2026-08-21 `go test -race` 实证,已修;2026-08-22 补立案 —— 修好了却一直没有条目)**
  `internal/streamin` 的会话拆除与会话启动争同一个 `pumps` 数组:`Server.Stop` 从关服的 goroutine
  关闭每一个活会话,而仍在启动中的那个会话正由自己的 goroutine 半写着该数组;`sync.Once` 只保证
  拆除不跑两遍,从未给拆除与启动定序。**已修(commit `68948a7`)**:`pumps` 上锁,两侧都取;
  帧路径在锁内把两个指针拷出,不再就地 range —— 一次无竞争加锁加两个字的拷贝,对着相隔二十毫秒的帧。
  意义在门本身:`go test -race ./...` 自此**首次整体通过**;此前几乎每次运行都报这条,
  即"仍是红的、且已没人在看"的最坏状态。
- **C17(new,2026-08-20 click-to-dial 复测,内部/外呼两型均已实证;2026-08-20 已修,残留另立 C24)** 媒体链正常(内部 1008→1007 响铃、
  双向通话、任一侧挂断双向拆线;外呼 1008→18688886669 经 dialplan `pstn_sim_outbound` 正确出网关),
  但**账面三处失真**,两型皆然:
  ① **call_type 判定与 §0 口径不符**:coordinator.go:900-909 只看腿方向(outbound→OUTBOUND),
     click-to-dial 的坐席腿恒为 originate 方向 → 分机互拨被记成 OUTBOUND(应 INTERNAL);
     外呼记成 OUTBOUND 只是恰好撞对。正解须按"被叫腿是否经 PSTN 网关"判定,
     而这依赖 ②。
  ② **dialplan 抬起的被叫腿从未被收养**:legs 恒为 [DIALING/1008] 单条,无目标腿;
     talk_sec=0(实测通话十几秒)、ring_sec=0、agent_ids 空、primary_agent_id 空——
     发起坐席在自己发起的呼叫里查无归属(与 C11 的"读回/快照侧丢失"同族)。
  ③ **status 恒 ANSWERED**:远端 NO_USER_RESPONSE(对端未响应)的那通同样记 ANSWERED——
     坐席腿 auto-answer 被当成整通已接听;应按被叫腿结果判定(NO_ANSWER/FAILED)。
  实测样本:01a01f03-ca0d(外呼成功 13s)、01a01f03-6dd9(NO_USER_RESPONSE)、01a01ef9-1440(内部)。
  **已全部修复并实测通过(2026-08-20)**:①`aicc_call_type` 由 click-to-dial 按位数判定(4 位=INTERNAL,
  owner 简化口径)盖在坐席腿上,归一化进 SwitchEvent.CallTypeHint,callTypeOf 优先采信;
  ②`aicc_extension` 一并盖上,坐席在自己发起的呼叫里可被归属;③assemble 新增"坐席发起"分支——
  被叫腿决定 answered/ring/talk,坐席腿的 auto-answer 不再算接通,legs 渲染被叫腿(出网关=TRUNK、
  内部=AGENT)。复测:INTERNAL 1008→1007 ring=2/talk=6/agents=1/legs=[AGENT/1007];
  OUTBOUND 1008→18688886669 ring=2/talk=9/legs=[TRUNK/…];未接一型由单测钉住(NO_ANSWER)。
  **残留**:b 腿在 bridge 前失败时会自成一通 CDR(样本 01a01f03-728b)。
  ~~归 T6.11 起草时一并覆盖。~~ **其真身已于 2026-08-21 查明并另立 C24**(拨号方案跌落 voicemail 的
  后继路由),不再归 T6.11;**C24 已于 2026-08-24 闭合**(见其条目)。
- **C18(new,2026-08-20 owner 直裁;2026-08-21 已修,409 `OPERATION_NOT_ALLOWED_FOR_CALL_TYPE`)** INTERNAL 呼叫的能力限制未实现:transfer / hold / retrieve
  对分机互拨的呼叫应被拒绝(接口层给出明确错误码,UI 相应禁用),现状三者一律放行。
  依赖 C17 的 call_type 正确派生先落地(否则判据本身不可靠)。补 case 归 T6.11 同批。
  **【owner 裁决 2026-08-21】接口层拒 + UI 禁用。已修**:规则落在 telephony 层
  (呼叫模型才知道 `CallType`),新 sentinel `ErrNotForCallType`;`Hold`/`Retrieve` 走新的
  `handledChannel()`(先查类型再取腿),`Transfer` 在取到主叫腿后同样判一次。
  httpapi 映射为 **409 `OPERATION_NOT_ALLOWED_FOR_CALL_TYPE`**(契约新增该错误码,
  三个操作的 description 同步写明理由)。UI 两处:工作台控制键组把保持/取回、转接置灰
  (静音与挂断仍可用),通话卡直接不渲染这三个按钮。
  测试:`TestInternalCallsRefuseTheControlsThatMeanNothingOnThem`(含反例——inbound 呼叫
  返回的是 `ErrNoAgentLeg` 而非类型拒绝,以此证明类型闸放行了它)+ 前端两条
  (INTERNAL 置灰、INBOUND 仍可用)。
- **C19(new,2026-08-20 owner 现场发现,已修)** cockpit 把坐席**自己拨出**的呼叫渲染成来电:
  `isRinging = state==='RINGING' || state==='DIALING'`(_app.agent.index.tsx、components/active-call.tsx
  两处同源),于是 DIALING 期间出现"接听/拒接",还允许在未接通时转接。
  **已修**:RINGING(呼叫送到坐席)与 DIALING(坐席自己发起)分开——DIALING 显示"正在外呼"标题
  与单个挂断按钮,不再提供接听/保持/转接;新增 i18n `call.outgoing`(en/zh)与一条 cockpit 回归测试。
  **同源余波(同批已修)**:拨出瞬间通话只有坐席一条腿,取"对端 party"取不到号 → 通话卡与客户卡
  双双显示 "Unknown number"(owner 现场发现)。三处显示层(cockpit 通话卡、客户卡、软电话条)改为
  回落到坐席腿的 `otherNumber`(即被叫号码,CDR 的 to_number 同源);回归测试用单腿的
  placedCall fixture 钉住"两个面板都报出被叫号、不出现 unknown"。
  **第二轮(owner 又抓到)**:顶部软电话条第一轮漏改,通话已建立仍显示 Unknown number;同处另有
  方向图标按 callType 判定(INTERNAL 一律画成来电箭头)——两者一并修:号码同样回落 `otherNumber`,
  方向改按坐席自己的 role(ORIGINATOR=外呼箭头),并补 softphone-bar 回归测试。
- **C20(new,2026-08-21 T3.5 途中 owner 现场发现,已修)** 队列派单腿被当成坐席自己发起的外呼:
  mod_callcenter originate 派单腿时,coordinator 见到陌生 outbound channel 先铸一个 provisional
  agent-only call,要等 `CHANNEL_BRIDGE` 才由 `join` 并入主叫。**振铃期间两个 call 并存**,
  `/calls/mine` 两条都返回,工作台在两者之间翻转 —— owner 报的"Ringing 变为 Dialing"即此,
  **不是 FSM 违规**(`call.go` 的 partyTransitions 本就没有这两个状态之间的边)。派单腿在幽灵 call
  里是第一个 party,按 `AddParty` 规则拿到 ORIGINATOR/DIALING;在真实 call 里是后来的,拿到
  TARGET/RINGING。二级后果:派单在接通前被取消时,幽灵 call 从未 bridge 就死,**却照样写 CDR**
  ——单通排队电话实测产出 **12 条** `坐席分机 → 主叫号` 的 NO_ANSWER OUTBOUND 记录
  (即 C17 残留项"b 腿自成一通 CDR"的真身,规模是每次派单重试一条)。
  历史核查该型记录最早见于 2026-08-19,**早于 08-20 的 CallType/CDR 改动**,故为既有缺陷非回归。
  **已修**:`strings mod_callcenter.so` 实证派单腿带 `cc_side` / `cc_member_session_uuid`(主叫 channel);
  ①`normalizeChannel` 读出后填 `SwitchEvent.MemberChannelID`(判据用 `member != ChannelID`,
  主叫自己那条腿在同一变量里放的是自己的 id,故该比较即使 `cc_side` 缺失也成立);
  ②`adopt()` 见到带该印记的腿**直接挂到主叫的 call 上**,不再铸 provisional call。
  派单腿由此从出生就是 TARGET/RINGING、callType 保持主叫的 INBOUND。
  复测(VC-S7-03 同场):振铃期 `/calls/mine` 2→**1** 条、role/state 由 ORIGINATOR/DIALING →
  **TARGET/RINGING**、callType OUTBOUND → **INBOUND**、幽灵 CDR 增量 +12 → **+0**、
  CDR 总增量 +13 → **+1**。单测两条:`TestNormalizeReadsTheQueueDeliveryStamp`(主叫自己那条腿
  不得被当成派单)、`TestQueueDeliveryLegJoinsTheCallerImmediately`。
  **余留**:派单腿的 CHANNEL_CREATE 若抢在主叫入册之前到达,仍会走旧路(铸 call → bridge 合并),
  该竞态未消除,只是回到修改前的行为。
- **C22(new,2026-08-21 T4.1 执行发现,FAIL 立案;2026-08-21 已修账目,S5-01 已重跑 PASS ——
  ABANDONED_RINGING 条件互斥与 RONA 状态机仍归 W2)** 一通"bot 接了 → 转队列 → 无人应答 → 主叫放弃"
  的电话被记成 `ANSWERED`,`missed_reason` 为空 —— **队列放弃在报表里全线不可见**
  (本部署所有入站电话都先过 bot,因此是全线,不是个例)。
  根因:`cdr.go:246` 的 `snap.Bot.Sec > 0` **无条件**判 ANSWERED,不问之后是否进队列、是否有人接。
  **这条分支在 C11 修好之前打不到**(转接呼叫的 `Bot.Sec` 恒 0),故账本 VC-S5-01 的
  `expect: status=NO_ANSWER` 是照着**由缺陷造成的**行为写的;C11 修复让缺陷显形,不是制造。
  同一行 CDR 另两处失真(C20 修复后才落到这一行上,此前 23 条派单腿各自成幽灵 call):
  `agent_ids` 23 个元素去重后 1 个(cdr.go:206-210 逐腿 append 无去重)、
  `legs` 含 23 段 0 秒的 AGENT、`ring_sec=0`(响了 159 秒;cdr.go:238 只在 `answered != nil` 时才算)。
  修复须同时给出"bot 接过但最终无人应答"的收官口径,并与 W2 的 missed_reason 互斥修复对齐;
  T4.2(VC-S6-01,排队中放弃)大概率命中同一分支,执行时留证对照。
  证据:`docs/verification/artifacts/VC-S5-01/verdict.md`。
  **已修(2026-08-21,commit `71ec949`)**:①`cdr.go` 引入 `soughtAPerson`(有坐席腿或进过队列)
  —— bot 接听只在呼叫仍属于 bot 时才判 ANSWERED,一旦要找人就由那个人来回答;
  ②`agent_ids` 用 `slices.Contains` 去重(23 次重试是同一个人;`legs` 里的 23 段 AGENT **有意保留**,
  详情页应看得见交换机试了多少次,两者口径不同);③新增 `ringSpan()`,无人接听时按
  "第一条派单腿创建 → 最后一条释放"计 `ring_sec`。回归测试 `TestACallNobodyAnsweredIsNotAnsweredByTheBotHavingSpoken`
  以实测那通电话为原型,摘掉修复即报出线上那四个值;另加 `TestABotServedCallThatTimedOutInQueueIsMissed`
  (队列超时→NO_AVAILABLE_AGENT)。**复验**:VC-S5-01 重跑 FAIL→PASS(NO_ANSWER/ABANDONED_WAITING、
  agent_ids=1、ring_sec=121、bot_sec=12 保留、单行 CDR、幽灵计数不动)。
  **未动**:`ABANDONED_RINGING` 的条件互斥错误仍归 W2;RONA 状态机(坐席被摘出路由)仍归 W2 ——
  C22 修的是账目,不是状态机。
- **C12 补充(2026-08-21 T4.2 实证)** `/calls/waiting` 拒绝主管已当场坐实:
  `403 FORBIDDEN {"code":"FORBIDDEN","message":"this account is not an agent"}`,坐席会话 200。
  连带发现**账本 collect 缺陷**:VC-S6-01 第 6 条写的是 `/tmp/vc-sup.jar`,在 C12 修复前
  **永远取不到**。修复前该条须改用坐席会话(diff 见 `artifacts/VC-S6-01/verdict.md`)。
- **C23(new,2026-08-21 阶段 4 执行发现,账本 collect 缺陷三型;新起草的 case 已避开,既有 case 未回修)**
  三处都会让 collect 静默失效,非产品缺陷但会误导判定。
  **状态(2026-08-22)**:阶段 6 的 11 条草案在写的时候已按这三条办(抓流 1800、按 `"type":"X"` 计数、
  按端点真实要求的角色选会话),但**既有 28 条 case 的 collect 一条都没改** —— 回修仍待做,
  排在草案并入 `ledger.yaml` 的那一批:
  ①**角色门比账本假设的细**:队列启停(`PUT /queues/{id}`)需 ADMIN,主管会话 403
  `{"requiredRole":"ADMIN"}`;`/calls/waiting` 拒绝主管(C12)。账本多处默认"主管会话万能"。
  ②**`grep -c '事件名'` 把一个 SSE 事件数成两个** —— SSE 每事件产生 `event:` 与 `data:` 两行;
  应改为 `grep -c '"type":"事件名"'`。影响 VC-S3-04 的 CALLBACK_CREATED/UPDATED 计数,
  以及其他用裸串计数的条款。
  ③**`--max-time 120` 太短**:从开启抓流到人工拨号、对话、挂断超过 120 秒是常态,
  VC-S6-01 首跑因此漏抓 SSE 条款。建议 S3/S5/S6/S7 统一 1800。
  **【已修 · 2026-08-24 `a41f9f6`】按**类**回修,不按点名回修。**
  条目点名了每一条的发生处,但缺陷是**类**;只改点名处就会重蹈 C34 的覆辙(点名三个、实为四个)。
  实际改动:
  ①**角色**:3 条命令由主管会话改为 `vc-admin.jar` —— 队列启停两条、号码录音开关一条。
  `PUT /queues/{id}`、`GET/PUT /dids` 同在 `requireRole(auth.RoleAdmin)` 组下(server.go:204-222),
  而 `requireRole` 用的是 `AtLeast`,故 ADMIN 也能读 `GET /queues`,整条命令换会话是安全的。
  ②**事件计数**:2 处 `grep -c 'CALLBACK_*'` 改为按 `'"type":"X"'`。
  S3-04 expect 里的 1 与 2 **本就是事件条数**(执行时当场折半后写进 verdict),故 expect 不动,
  现在是命令向它对齐。
  ③**抓流窗口**:**8 处**改为 1800 —— 条目点名 4 处,按"用例含人工步骤且窗口 < 1800"扫全库
  **又找出 4 处**(VC-S3-01 / S5-01 / S7-04 / S9-01,原为 90/180/90/180)。
  `VC-S10-01/02`、`VC-S11-01` 窗口短但**无人工步骤**,是刻意的,不动。
  **回修时发现这个缺陷已经真的发生过一次而没人察觉**:
  **VC-S4-04** 打开 95001 录音开关那步用的是主管会话,`GET/PUT /dids` 同在 ADMIN 组下 ——
  该步当时**必然 403、什么也没做**,只因录音本来就是开着的,断言才照样成立,用例照常 PASS。
  已写进它的 status。**这正是 C23 的本体**:一条静默失效的 collect 与一条真正执行过的 collect,
  从输出上分辨不出来。
  **同批完成 C31 的连带待办**:VC-S1-03 补了 `to_number=95999` 断言(collect 与 expect 各一处),
  并在 status 标明**本条需重跑** —— 旧 PASS 不覆盖一条它当时没有做的断言。
  这条待办在 C31 修复前补不了:字段本身是空的。
- **C24(new,2026-08-21 整体回归发现)** 一通**无人接听的 click-to-dial 产生 4 行 CDR**。
  实测 `01a023b6-1384`(INTERNAL 1008→1007 NO_ANSWER,真实的那条)之外,另有三条多余:
  `OUTBOUND 1007→1007`、`OUTBOUND voicemail→(空)`、`INTERNAL 1007→(空)`,均 NO_ANSWER/29 秒。
  机制:1007 不接时**拨号方案跌落到 voicemail**,后继腿各自被 `adopt()` 铸成独立呼叫并各写一行。
  与 C20 同族但来源不同 —— C20 是 mod_callcenter 的派单腿,本条是**拨号方案的后继路由**。
  这就是 C17 当时记的残留("b 腿在 bridge 前失败时会自成一通 CDR")的真身,规模是每次未接 3 行。
  **【owner 方向 2026-08-21】aicc 相关的拨号方案应当有自己专属的 context,不落在 `default` 上。**
  跌落 voicemail 是 `default` context 的既有行为,aicc 的呼叫不该继承它 —— 从源头上就不会
  产生这些后继腿,而不是事后在 `adopt()` 里认领或过滤。归入后续优化,不在本期修。
  次要候选(若 context 隔离不可行):给拨号方案后继腿一个指回原呼叫的印记,同 C20 的思路。
  **在修复之前**:幽灵计数会被这类行污染,账本里以 `from_number ~ '^[0-9]{4}$'` 统计幽灵的口径
  需同时排除 `to_number=''` 的行。
  **【2026-08-22 VC-S14-04 补记:还有一层比数量更重的质量问题】**
  跌落 voicemail 的那一腿被记成 **`OUTBOUND | ANSWERED | 1002->voicemail | talk_sec=9`,
  且挂在一个坐席名下(`agent_ids` 非空)**。也就是说**语音信箱的问候语被计成一通已接通的坐席通话**。
  它会进 `callsHandled`、进 `talkSec`、进队列与坐席报表 —— **把从来没人说过话的 9 秒算成坐席工时**。
  VC-S13-03(my-day)与 VC-S13-04(报表)读的都是这个数。同一窗口实测两次点击各产一行。
  故 C24 不只是『多几行脏数据』,它**污染工时与服务水平统计**;修复优先级应据此上调。
  **【已修 · 2026-08-24 现场闭合】** 修它的不是一次针对 C24 的改动,而是 2026-08-22 处理
  cockpit 方向错乱时落地的三个 commit(`646fe7d` aicc 自有 context、`1ca8675` 第二条腿指回第一条、
  `80ecc66` 两个文件不能同开一个 context)。owner 当时给的方向"从源头隔离,不要事后认领或过滤"
  正是这么兑现的:`aicc.xml` 的内部呼叫规则 `continue_on_fail=false` 且没有 voicemail 回落,
  后继腿从不诞生,`adopt()` 里一行都不必改。
  **账面证据(context 落地后 88 行)**:`voicemail` 行 **11→0**、`INTERNAL` 且 `to_number=''`
  **15→0**;而触发条件在这段窗口里真实发生过 9 次(9 通未接的 INTERNAL),不是"没机会犯错"。
  **现场复验(2026-08-24 07:45,`01a03104-0c32-7501-a95a-a2882346073f`)**:wei 1008 → ben 1002
  click-to-dial,响 30 秒无人接。交换机全程只有 **2 条 channel**,一起消失,无任何后继腿;
  账本恰好 **1 行** `INTERNAL | NO_ANSWER | 1008→1002 | total=30 talk=0 bill=0`,
  legs 只含 `{AGENT, "1002", did not answer, 0s}`。对照立案时同一动作产 4 行。
  **残留的 3 行 `from=to` 不属于本条**(病因不同,见 C52),故 C24 的检测口径
  (`voicemail` / 空被叫)此后应恒为 0。
  **历史脏数据未清**:11 行 voicemail 被记成已接通的坐席通话、15 行空被叫、16 行自己打给自己,
  仍在库里喂 `callsHandled`/`talkSec` 与队列报表。清理还是重新 seed,连同"方向拧反的 AI 外呼
  CDR 要不要回填",**并作一个问题等 owner 定**。
- **C25(new,2026-08-21 整体回归发现,已修)** **转接走的电话不离开第一个坐席的屏幕。**
  `CallsForAgent`(coordinator.go)只匹配 `p.AgentID == agentID`,**不看这条腿死没死**,
  于是坐席只要曾经有过一条腿,这通电话就一直留在他的 `/calls/mine` 上,直到整通结束。
  实测:转给 ben 之后 party 模型正确(1008 `RELEASED`、1007 `TALKING`)、交换机侧只剩 2 条 channel,
  但 wei 的 `/calls/mine` 仍返回 1 条 —— 屏幕上是一通他已经交出去的电话,还带着一条交换机早已
  挂断的腿的控制按钮。这就是 owner 在 T3.5 与本次回归两度报的"**1008 没有挂断**"。
  **已修**:`CallsForAgent` 增加 `p.IsActive()`(新增 `PartySnapshot.IsActive()`,与 `Party` 对称)。
  回归测试 `TestACallPassedOnLeavesTheFirstAgentsScreen`。
- **C26(new,2026-08-21 T5.1 执行发现,FAIL 立案)** **bot 腿死后的 fallback 兜底对最可能的故因不可达。**
  `aicc_inbound.lua:69` 设 `hangup_after_bridge=true`,bot 腿一挂,主叫腿在 **50 毫秒**内被跟随挂断
  (FS 日志实证:`20:09:31.106` bot 腿 hangup → `20:09:31.156` 主叫腿
  `Overriding SIP cause 480 with 200 from the other leg` → hangup),
  第 104 行 `if session:ready()` 的兜底块**没有机会执行**。owner 听感:"直接断了",无保持音。
  第 70 行的 `continue_on_fail=true` 覆盖的是**另一种**故障——桥接**压根没接通**(拨号时网关就是死的);
  它不覆盖"接通了、然后对端消失",而 **app 重启 / 进程崩溃 / provider 掉线全是后者**,
  正是这个兜底最该管的那些情况。
  **修法有两难,这是它不能一行改掉的原因**:直接把 `hangup_after_bridge` 改 `false`,
  会让 **bot 正常收官的每一通电话**也落进 `session:ready()` 从而被塞进人工队列。
  兜底必须能分辨"bot 把事办完了"与"bot 消失了"。建议形态:bot 关闭自己那条腿之前,
  往主叫通道盖一个收官印记(与转接时盖 `aicc_bot_sec` 同一手法),Lua 见印记则挂断、
  无印记则转 fallback 队列。修复后重跑 VC-S12-01。
  证据:`docs/verification/artifacts/VC-S12-01/verdict.md`。
  **【已修 2026-08-23,owner 于当日批准修法】** 按立案时建议的形态实现,两处细化写在这里:
  - **Lua**:`hangup_after_bridge` 由 `true` 改 **`false`**,主叫因此活过 bot 腿;
    bridge 之后的块先读印记 `aicc_bot_finished` —— **有印记**就是这通电话谈完了,
    `session:hangup("NORMAL_CLEARING")`;**无印记**就是 bot 消失了,照旧走 fallback。
    日志按 `bridge_uuid` 是否存在分成 `bot leg failed`(压根没接通)与 `bot leg vanished`
    (接通后消失)两句 —— **分支不依赖这个判断**,判错只影响日志措辞。
  - **Go 只在两处盖印**:`Hangup` 工具(`actions.go` 的 armed 闭包)与
    **流程走到终态**(`orchestrator.go` `afterMove`)。这两处"关掉自己那条腿"本身就是收官动作。
    **转接一律不盖** —— 这是对立案建议("bot 关闭自己那条腿之前盖印")的细化,理由:
    若在 `uuid_transfer` 之前盖印而**转接失败**,Lua 见印记就会把主叫挂掉,
    等于把一次失败的转接变成了正在修的那个掉线;而转接最可能失败的时刻,
    恰恰就是交换机出问题的时刻。**不盖印,失败的转接反而自愈**:主叫落进 fallback 队列。
    同理 `rescueCaller` 与"没有 fallback 可去"那条也都不盖。
  - **主叫交出去之前要把交换机的收尾规则放回去**:`hangup_after_bridge=false` 是留在
    **主叫通道**上的,会跟着他进队列;若不还原,坐席挂机后主叫可能不跟着结束。
    新增 `Adapter.EndCallerWithTheirBridge`(FS 词汇留在 adapter 里,不外泄进 aicall),
    `TransferToAgent` 与 `rescueCaller` 转接前各调一次;Lua 的 fallback 分支同样先还原再 transfer。
  回归:`TestTheBotSaysWhetherItMeantToEndTheCall`(3 子例)、`TestAFlowThatConcludesAlsoSaysSo`。
  **四处逐一摘除验证**,含一条**反向**:摘 HANGUP 印 → *"aicc_bot_finished = "", want HANGUP —
  unmarked, the dialplan sends a caller who heard goodbye to a queue"*;摘 FLOW_END 印 → 同形;
  摘转接前的规则还原 → *"the caller's teardown rule was not restored before the transfer"*;
  **给转接加上印记**(反向)→ *"a transfer marked the call as finished ("TRANSFER"). If the
  transfer fails the caller is then hung up instead of rescued"*。全部还原后 `go test -race ./...` 0 FAIL。
  **Lua 那半只能现场证**,这正是 VC-S12-01 重跑的意义;重跑同时要证**反方向** ——
  一通正常收官的电话仍旧只是挂断,不会掉进队列。
  **【2026-08-23 现场复验,两半均成立】** 通话中重启:`11:12:57 bot leg vanished` →
  `11:12:59.384 adopted a caller queued across a restart callId=01a02c9a-d6fb-…`
  (**正是重启前从通道上抄下的那一个**)→ `11:12:59.387 waiting line reconciled restored=1 dropped=0`。
  `/calls/waiting` 有他且 `joinedAt` 是**真实入队时刻** `03:12:57Z` 而非发现时刻;
  `/api/v1/calls` 是**一通 INBOUND、`ORIGINATOR TALKING 18688886669`**
  (上一轮此处是每派单一通的假 OUTBOUND);录音 1 条与转写 18 行都挂在同一个原生 call_id 下。
  坐席接起后合并正确、等待名单随之清空,与"没重启过的对照组"同形。
  `uuid_getvar` 的格式也在同一轮现场核对(见上)。**VC-S12-01 随之转 PASS。**
  ⚠ 本机 FreeSWITCH 是原生安装,`/usr/local/freeswitch/scripts/` 里是**副本不是软链**
  (改前已核对三个脚本与仓库一致,无本地漂移),改完须 `cp` 过去;mod_lua 每通电话读一次文件,不需要 reload。
- **C27(new,2026-08-22 VC-S13-04 执行发现,未修)** **同一个 SLA 字段,两块屏用两个不同的分母,
  谁都没标口径。** 后端 `answeredWithinSla` 是一个**计数**(`ledger.sql:143`:排队等待 ≤20 秒
  且已接听),不是比率;分母由前端自己选,而两处选得不一样:

  | 位置 | 分母 | 含义 | support-zh 2026-08-21 |
  |---|---|---|---|
  | `web/src/routes/_app.admin.reports.tsx:128` | `row.totalCalls` | 及时接通 ÷ 呼入总数(行业通行的 service level) | **31%** |
  | `web/src/components/queue-performance.tsx:58` | `row.answeredCalls` | 及时接通 ÷ 已接通数 | **57%** |

  两者各自自洽 —— 后者的注释明写 "share of answered calls that beat the threshold",
  是有意为之而非笔误。问题在于两块屏都只写 "SLA"/服务水平,**不标是哪一个定义**:
  主管在墙板上看到 57%,管理员在报表里看到 31%,同一个队列、同一天、同一份数据。
  未接通的呼叫在后者的分母里消失了,于是**队列越差,后者显得越好**(全员不接 → 分母趋近于
  及时接通数 → 趋近 100%)。这是比"算错"更难发现的一类:数字始终自洽,只是回答了另一个问题。
  归呈现层(G-B2 族),不影响后端判定。
  **修法**:先定口径(建议取行业通行的 ÷ 呼入总数,即 reports.tsx 那个),两处统一,
  并在表头标出定义;若确需保留两个指标,就给它们两个名字,不要都叫 SLA。
  证据:`docs/verification/artifacts/VC-S13-04/verdict.md` §4②。
  **【已修 · 2026-08-24 `064eade`,留证 `artifacts/C27/verdict-2026-08-24.md`】**
  **口径裁定**:取 `answeredWithinSla ÷ totalCalls`,两处统一。
  理由不是"哪个更常见",而是**另一个会为失败加分**。
  **今日实库把这一点摆得比立案时更清楚**:
  | queue | total | answered | inSLA | ÷total | ÷answered |
  |---|---|---|---|---|---|
  | support-en | 68 | 53 | 42 | 62% | 79% |
  | support-zh | 19 | 9 | 9 | **47%** | **100%** |
  `support-zh` 19 通打进来、**10 通等不及挂了**,旧的主管墙板给它打 **100%**。
  **标签也统一了**:英文原先两处都只写 "SLA",中文两处词还不一样(达标率 / 服务水平目标)
  却都不含分母;现在同名 **Service level / 服务水平**,并各带一句写明分母的 `title`。
  **测试** `web/src/components/queue-performance.test.tsx` 用的就是 support-zh 的真实数字:
  渲染 47% 且 **100% 不得出现**;已验证分母改回 `answeredCalls` 后 FAIL。
  **未做(记明)**:后端 `answeredWithinSla` 仍是**计数**而非比率,分母仍由前端选。
  两处现已同源同义且有测试守着,但第三块屏若要用它,仍需自觉取同一个分母;
  把比率下沉到后端更彻底,那是契约变更,不在本条范围。
- **C28(new,2026-08-22 VC-S13-05 执行发现;2026-08-22 已修,待重跑 VC-S13-05 转正)** **话机没了,交换机不知道;而发出去的事件说的是反话。**
  根因同一个函数 `internal/agents/service.go:371-396` 的 `ObserveDevice`,后果两条:
  ① **交换机镜像根本没被调用**。应用侧正确地把坐席记成 `availability=DEVICE_UNREACHABLE`、
     `isRegistered=false`(且 `state` 仍为 READY —— 意愿与可达性分开,这一点是对的),
     但全函数没有一处触及交换机状态,`callcenter_config agent list` 里 **agent-wei 持续 `Available`**
     (实测持续观察 60+ 秒未追上)。**队列会继续把电话派给一部不存在的话机**,
     每通振铃到超时再重派;主管墙上看到"有人在线却没人接",而坐席早已下线。
  ② **掉线时发出的事件类型是反的**。第 394 行**上线掉线一律** `publish(events.TypeDeviceInService)`,
     于是掉线那一条顶着 `DEVICE_IN_SERVICE` 的名字、载荷里写着 `DEVICE_UNREACHABLE`。实测三条:
     ```
     seq=9200420 02:54:22Z availability=READY               ← 续注册
     seq=9200421 03:04:16Z availability=READY               ← 续注册
     seq=9200422 03:10:37Z availability=DEVICE_UNREACHABLE  ← 掉线,类型却仍是 IN_SERVICE
     ```
     按 `type` 过滤的消费者被告知了相反的事;只有忽略类型去读载荷的才对。
     契约里的 `DEVICE_UNREGISTERED` / `DEVICE_REGISTERED` **一次都没出现过**(实测计数 0/0)。
  **与 W7 的关系要更正**:events.md 把这两个类型列为"十个零生产者"之一,读起来像"还没实现";
  **实测是发错了一个**。W7 该做的不是"补一个生产者",而是**把现有这个改对** —— 建议同批修 ①。
  证据:`docs/verification/artifacts/VC-S13-05/verdict.md`。

  **已修(2026-08-22)**,三处,各自独立:
  - `internal/agents/state.go` `CallcenterStatus()` —— 意愿与可达性分开的前提下,把**可达性**读进来:
    READY 但 `!IsRegistered || !IsDeviceInService` 映射为 `On Break`。
    坐席在应用里仍是 READY(掉线不说明意愿),交换机看到的是"现在别派给他"。
  - `internal/agents/service.go` `ObserveDevice` —— 补上原本整段缺失的镜像调用 `s.mirrorStatus(profile, snapshot)`。
    ①的根因不是映射错,是**这条路径压根没往交换机写过**。
  - 同一函数,事件类型按方向取:掉线发 `TypeDeviceUnregistered`,恢复发 `TypeDeviceInService`,
    不再一律 `IN_SERVICE`。**W7 的记录随之要改**:`DEVICE_UNREGISTERED` 从此有生产者。
  回归:`TestALostPhoneReachesTheSwitchAndIsNamedForWhatHappened`。三处**逐一摘除验证**过 ——
  摘①报 `the switch was last told "status agent-1001 Available", want On Break`;摘②直接 FAIL;
  摘③报 `the phone's loss was announced as DEVICE_IN_SERVICE, want DEVICE_UNREGISTERED`;
  三处还原后全套 0 FAIL。另有三条既有用例同批改写(`TestCallcenterStatusMapping` /
  `TestReadyMirrorsAvailableToTheSwitch` / `TestWrapUpDoesNotEndByItself`):它们把"登录但从未观测过话机"
  当常态,而生产路径在 `Login` 里就用 `applyDeviceLocked` 施加已知话机状态 —— 那个函数自己的注释写着
  *"Without this an agent signing in at a perfectly good phone reads as unreachable until the phone happens to re-register."*
- **C29(new,2026-08-22 VC-S14-01 执行发现,未修)** **建分机不显式写 `isEnabled`,建出来的是停用的,
  而 API 只回 201。** 同一分钟的 A/B:
  ```
  POST {number,password,displayName}                → isEnabled:false,luacc.directory 计数 0
  POST {number,password,displayName,isEnabled:true} → isEnabled:true, luacc.directory 计数 1
  ```
  根因:契约里 `isEnabled` 是**可选**的,而 `CreateExtension`(`catalog_handlers.go:48-55`)
  `decode` 进普通结构体 `catalog.Extension`,**省略即 Go 零值 `false`** 并直接写库 ——
  `extensions.is_enabled` 的列默认值 `true` 永远轮不到生效(佐证:库里原有 20 个分机全是 `t`)。
  后果:`luacc.directory` 带 `WHERE e.is_enabled`,这部分机 **Lua 查不到、永远注册不上**,
  而管理员在 API 侧看不出任何异常 —— 排查会指向话机或网络,不会指向这里。
  **同型风险已坐实,不是分机接口独有** —— 2026-08-22 在 `POST /dids` 上复现同一形态
  (VC-S14-03 执行时顺带验证):不带 `isEnabled` 建出 `false` 且 `luacc.dids` 查不到;
  带 `isEnabled:true` 则正常。**凡"可选布尔 + 非指针字段"的写接口都要一并排查**
  (catalog 各 Create/Update)。修法建议:请求体改用指针或 `*bool`,区分"未提供"与"显式 false"。
  **【已修 2026-08-23】** 修法与立案时写的建议(`*bool`)**不同**,理由记在这里,不让偏离无声发生:
  可选布尔的默认值**在解码前先种进结构体**,而不是把字段指针化。
  `catalog.NewExtension/NewQueue/NewDID`(types.go,紧挨各自的 `validate`)返回"默认值已经施加好"的空壳,
  6 处 handler 的解码目标由 `var in catalog.X` 改成 `in := catalog.NewX()`。
  `encoding/json` **不碰报文里没有的字段**,所以省略保留默认、显式 `false` 照旧生效 —— 与指针法的分辨力等价。
  **为什么不用 `*bool`**:①三条 Write schema 的散文一直写着 *"Omitted fields take server defaults"*,
  PUT 是整体替换;指针会诱导出"省略=保持原值"的第二套语义,与契约冲突。
  ②响应里这几个字段是 `required` 且非空,把领域类型指针化会让它们能 marshal 成 `null`。
  ③默认值因此和 `validate` 里其它默认(kind、mohSound、strategy、language…)待在同一个文件里,不散进 handler。
  **8 个写接口 × 可选布尔全部排查**(不止分机):

  | 字段 | 列默认 | 修前建出 | 处置 |
  |---|---|---|---|
  | `extensions.is_enabled` | true | **false** | 已修 |
  | `queues.is_enabled` | true | **false** | 已修 |
  | `queues.is_recording_enabled` | true | **false** | 已修 |
  | `queues.is_abandoned_resume_allowed` | false | false | 与 Go 零值一致,**不动**(并加反向断言,防止被顺手种成 true) |
  | `dids.is_enabled` | true | **false** | 已修 |
  | `dids.is_recording_enabled` | true | **false** | 已修 |
  | `agents.is_auto_answer` | false | false | 与 Go 零值一致,**不动** —— 这是排查走完,不是跳过 |

  另两张带 `is_enabled DEFAULT true` 的表(`trunks`、`dispositions`)**没有写接口**
  (契约里 `/dispositions` 只有 GET,trunks 一条路由都没有),不在本次面内。
  **契约变更只有三行 description**,把布尔默认写进各 Write schema 的默认清单。
  **没有加 `default` 关键字** —— 试过并撤回:`openapi-typescript` 会把带 `default` 的可选字段在 TS 里升成**必填**
  (`isEnabled?: boolean` → `isEnabled: boolean`),等于要求客户端必须发送一个契约里 optional 的字段,
  与本次修复的意思正好相反;全库既有 12 处 `default` 全在 query 参数上(生成器对 parameters 有豁免),
  请求体里从来没有过。`make api-lint` 通过,`api-breaking` 无破坏性变更。
  **PUT 语义就此记明**:省略即回到服务端默认(整体替换),**不是**"保持原值" —— 依契约散文,是决定不是意外。
  回归:`TestAnOmittedBooleanTakesTheDeclaredDefault`(`internal/httpapi/catalog_handlers_test.go`,5 子例)。
  五处 seed **逐一摘除验证**:摘分机报 *"the extension was created disabled; the phone would never register"*
  (并连带 *"an update that omitted isEnabled disabled the extension"*);摘队列两处分别报
  *"the queue was created disabled"* / *"the queue was created without recording; calls would go unrecorded"*;
  摘号码两处均报 *"the number was created disabled or unrecorded: {…}"* 并打出整个结构体指出是哪一个;
  反向那条(把 `isAbandonedResumeAllowed` 也种成 true)报 *"isAbandonedResumeAllowed defaulted true, want false"*。
  全部还原后 `go test -race ./...` 0 FAIL。
  **前端未受影响也不会被反向影响**:三处表单新建时自带 `isEnabled: true`
  (`_app.admin.extensions.tsx:39` / `_app.admin.numbers.tsx:49` / `_app.admin.routing.tsx:48`),
  编辑时 `setEditing(row)` 整行带上 —— SPA 从来没触发过这个洞,修后 PUT 也始终显式发送。
  **后续(未做)**:catalog 组的请求体仍直接解到 `catalog.*` 而非生成的 `api.*` 类型。
  CLAUDE.md 指的方向是后者,但那是一次跨 18 个字段的搬迁,验收途中不动;留作后续。
  **现场复验(2026-08-23,VC-S14-01 重跑)**:`POST /extensions` 不带 `isEnabled` → 201 且
  `isEnabled:true`,`luacc.directory` 查得到,话机随后经 WSS 真的注册上 ——
  8-22 同一条命令建出的是停用分机、目录零行、永远注册不上。
  同一轮 `POST /queues` 不带布尔 → `isEnabled:true`、`isRecordingEnabled:true`,队列侧同样成立。
- **C30(new,2026-08-22 VC-S14-01 执行发现,未修)** **删除分机没有任何守卫,坐席被静默解绑。**
  `DeleteExtension`(`catalog_handlers.go:67-69`)→ `catalog/service.go:128-130` → store,
  **中间没有任何检查**;唯一可能的保护是外键,而它是
  `fk_agents_extensions … ON DELETE SET NULL`(**不是** `RESTRICT`)。
  实测(amy 绑到该分机、话机正在注册时删):`DELETE → http=204`,分机行没了,
  **amy 的绑定变成 NULL,无错误无提示**,`luacc.directory` 随之清空。
  后果即 `failure_looks_like` 第一种:那名坐席的话机下次重注册就失败,而应用里他仍是 READY,
  队列继续给他派单,直到有人发现"这个人接不到电话"。
  **契约层面也缺位**:`DELETE /extensions/{extensionId}` 只声明 `204,400,401,403,404,503`,
  **没有 409** —— "拒绝"这条路在契约里就没有位置,修复须**先改契约**(spec-first)。
  证据:`docs/verification/artifacts/VC-S14-01/verdict.md`。
  **【已修 2026-08-23】** 守卫落在**外键**上,不是 service 里的预检 —— 立案原文点名的就是
  `ON DELETE SET NULL`,那不是保护而是"把伤害做得安静一点"的指令。
  迁移 `00015_an_extension_in_use_cannot_be_deleted.sql` 把它换成 **`ON DELETE RESTRICT`**。
  这样**每条路径都挡得住**,包括不走 API 的 psql;service 预检只挡得住 handler 那一条。
  **契约先改**(spec-first):`DELETE /extensions/{extensionId}` 增 409,
  新错误码 **`EXTENSION_ASSIGNED_TO_AGENT`** 进 `ErrorCode` enum 与 `Conflict` 响应的码清单,
  operation description 写明何时被拒、以及"先解绑"这个动作。
  **没有复用 `EXTENSION_IN_USE`** —— 它的含义是"另一名坐席已在该分机签入"
  (签入冲突,`agent_handlers.go:367`,en/zh 两份文案都是这么写的);
  拿它表示"被绑定"会把两件事混成一件,前端还会给出错的指引。
  边界翻译:`violatesConstraint(err, "fk_agents_extensions")`(`errors.go`)把 23503 认成冲突,
  **按约束名匹配而不是笼统认 23503** —— 别的外键失败不该冒充这一条。
  不认的话它会落进 default 变成 **503 STORAGE_DOWN**,那会教操作员去重试,而重试永远不会成功。
  **迁移纪律**:有 Down(退回 SET NULL);关键是**带历史的库**那一档 ——
  `TestMigrationsRefuseToDeleteAnExtensionAnAgentWorksAt` 先迁到 14、写入一行"已绑定"的 agent,
  再迁完,断言四件事:绑定**没被改写**、删被绑定的分机报 `fk_agents_extensions`、
  删没人用的分机**照常成功**(守卫不能变成阻碍)、解绑后可删。
  **摘除验证**:把 00015 改回 `SET NULL`,该用例报
  *"deleting an extension an agent works at succeeded; they were just unbound in silence"*;还原后通过。
  边界两条:`TestDeletingAnExtensionAnAgentWorksAtIsRefusedAsAConflict`(409 + 码 + 文案里有 "unbind")、
  `TestAnUnrelatedDeleteFailureIsNotTheBindingConflict`(换个约束名不得冒充本冲突)。
  两语种文案已加。**影响面核对**:全库只有一个外键引用 `extensions`;
  `AICC_SEED=fresh` 的删除顺序是先 users(级联 agents)后 extensions,不受 RESTRICT 影响。
  `make api-lint` 通过;`api-breaking` **0 error / 303 warning**
  (新增 enum 值在每个错误响应上各报一次 warning,与历次加码同形),`--fail-on ERR` 放行。
  **【重跑当场又抓到一处并修掉 —— `10ca069`】** 守卫成立,**但报错报错了**:
  第一次删除返回的是 **503 `STORAGE_DOWN`** 而不是 409 —— 正是上面那段注释里写着要避免的那件事。
  根因:**显式 `ON DELETE RESTRICT` 抛的是 SQLSTATE `23001 restrict_violation`**,
  不是 `23503 foreign_key_violation`(后者用于 `NO ACTION` 与插入侧);边界只认 23503,遂落进 default。
  **单元测试没拦住的原因值得单记**:那条测试是我用**自己以为的错误码**造出 `PgError` 喂给 handler 的,
  与服务端犯同一个错,于是两边一致通过 —— **桩件不会在世界的问题上反驳你**。
  现改成两个 SQLSTATE 都认,测试里的 code 与 message **抄自这次真实失败**;
  摘除 23001 那一支即报 `SQLSTATE 23001: http = 503, want 409`。
  这是"现场重跑"相对"跑测试"的价值:代码、单测、契约三方一致,却一致地错着。
  **现场复验(2026-08-23)**:amy 绑到 1099 后删 → **409 `EXTENSION_ASSIGNED_TO_AGENT`**,
  报文写明先解绑,amy 仍绑着、分机行与注册都还在;`psql` 直删同样被拒(证明守卫不在 handler);
  解绑后删 → 204,`luacc.directory` 归零;坐席会话建分机 403。
  夹具改用 psql 直写,**交换机侧零残留**(对比 8-22 走 API 绑定留下指向已删分机的悬空 contact,事后需清理)。
  **同族疑点(未取证,未立号)**:删队列没有对应守卫 —— `queue_agents` 是 `ON DELETE CASCADE`
  (静默清空配员),`dids.fallback_queue_id` 是 `ON DELETE SET NULL`(号码静默失去兜底队列)。
  形态与本条相同但**尚未实测**,若要比照办理需另起一条用例取证,不夹带进本条。
- **C31(new,2026-08-22 VC-S14-03 执行发现,未修)** **被拒的来电入了账,却记不出对方拨的是哪个号。**
  一通打向未配置/已停用号码的呼叫会正确落一行 CDR,`hangup_cause=UNALLOCATED_NUMBER` 也精确 ——
  但 `did` 为 **NULL**、`to_number` 为**空**:

  ```
  call_id        | call_type | did    | status    | to_number | hangup_cause
  01a027ea-4c17… | INBOUND   | (null) | NO_ANSWER | (空)      | UNALLOCATED_NUMBER
  ```

  **全库 11 行 `UNALLOCATED_NUMBER` 无一例外**(2026-08-19 至今),含 VC-S1-03 当时产生的 6 行。
  机制:`internal/telephony/cdr.go:199-201` —— `ToNumber = originator.OtherNumber`,
  取不到则回落 `cdr.DID`;被拒呼叫两者皆空。
  **而交换机是知道的**:同一时刻 FS 日志打着
  `aicc_inbound: unknown number 95009 from 18688886669`(`aicc_inbound.lua:49`)。
  后果:运营看得到"有人被拒",看不出**他拨的是什么** ——
  "有人一直打一个已停用的号"与"有人在扫号"在账本里长得一模一样,
  而前者要通知客户、后者要告警,处置完全不同。
  **连带**:这说明 **VC-S1-03 的覆盖比它看起来薄** —— 那条只断言了行数(+6 对应 6 条日志),
  从未断言号码,所以这个洞在它眼皮底下 PASS 了两天。C31 修复后应给 S1-03 补一条号码断言。
  证据:`docs/verification/artifacts/VC-S14-03/verdict.md`。
  **【已修 · 2026-08-24 `0f4c388`,与 C53 同一次修复,留证 `artifacts/C31/verdict-2026-08-24.md`】**
  **修法**:`addParty` 原先只给**坐席腿**记 `OtherNumber`。腿面对哪个号是**腿自己的事实**,
  与上面坐没坐着坐席无关 —— 而唯一没人可问的那条腿,正是最需要它的那条。改为**每条腿都记**。
  **现场**:向 `95009` 发起的呼叫落行为
  `INBOUND | from=18688886669 | to=95009 | UNALLOCATED_NUMBER`(此前 `to_number` 恒为空)。
  **连带待办现在可以做了**:给 VC-S1-03 补一条号码断言(修复前补不了,因为字段本就是空的)。
- **C32(new,2026-08-22 VC-S14-04 执行发现;同日重测【不再复现】,原因未证明)**
  **【状态变更 2026-08-22 19:10】** 今日交换机侧改造(aicc context 全链、internal profile context、
  `TransferToExtension` 目标 context 由 default 改 aicc、FreeSWITCH 重启、`internal_auth_calls=true`)
  之后重测:wei 的腿到达 `CS_EXECUTE`、transfer 真实执行、被叫接通,**症状消失**。
  **VC-S14-04 已随之重跑转 PASS**(两型账面 + 409 能力限制全通)。
  **但记为『不再复现』而非『已修』**:当时的怀疑(`absolute_codec_string=PCMU` 在 DTLS/WebRTC 腿上
  使媒体协商无法收官)**从未被验证**,而其间的变更有多项,无法归因。未经解释的痊愈会静默复发,
  守卫交给可重跑的 VC-S14-04。若再现,优先验证那个 PCMU 猜想(同一部浏览器话机,去掉该 var 再拨)。
  **同期查明并修掉的一个真缺陷(我今日引入)**:部署方的中继规则放在第二个 `<context name="aicc">`
  文件里,FreeSWITCH **只用第一个、静默忽略其余** —— 规则对 `xml_locate` 可见、对呼叫不可达,
  每次外呼都 `NO_ROUTE_DESTINATION`。已给 aicc context 加 `<X-PRE-PROCESS include "aicc/*.xml">`
  扩展点(同原厂 `default.xml` 收 `default/*.xml`),部署规则以裸 `<extension>` 放入。
  ~~原文如下~~ **从浏览器话机发起的 click-to-dial 拨不出去 —— 被叫从未响铃。**
  `POST /calls/dial` 返回 201、`callType` 判定正确,但交换机侧**全程只有坐席那一条腿**。
  FS 状态机(通道即 originate 指定的 `origination_uuid`):

  ```
  CS_NEW → CS_INIT → CS_ROUTING → CS_CONSUME_MEDIA
  [proceeding][180] → [completing][200] → [ready][200]     ← 200 回来了,sip_auto_answer 生效
  ……此后停在 CS_CONSUME_MEDIA,从未进入 CS_EXECUTE
  ```

  **拿到 200 却从未到达"已应答"**:`&park()` 没执行,`CHANNEL_ANSWER` 没发出。
  而转接正挂在这个事件上 —— `outbound.go:213` 的 `arm(agentLeg, onAnswer)` 由
  `KindChannelAnswer`(`:347→362`)触发才调 `TransferToExtension`(`:214`)。
  事件不来,**被叫号码从头到尾没有被拨过**。
  佐证:`click-to-dial transfer failed` 日志计数 **0** —— 不是转接失败,是根本没调用。
  **判别实验已做**:改由 ben 的**原生软电话**(Telephone 1.6/UDP)发起,一次就通
  (`CS_EXECUTE` + 对端 `CS_EXCHANGE_MEDIA`,账面全对)。**变量锁定在发起腿**。
  ⚠ **不能直接断言"WebRTC 一向如此"**:C17 于 2026-08-20 复测这条路时是通的,当时 1008
  也是这部浏览器话机;今日 wei 的话机因 VC-S13-05 被 Sign Out 后重新注册过,中间有变量。
  **怀疑但未证实**:originate 里钉的 `absolute_codec_string=PCMU` 在 DTLS/opus 的 WebRTC 腿上
  使媒体协商无法收官。修前应先做一次最小复现(同一部浏览器话机,去掉该 var 再拨)。
  **连带两条**:
  ①**应用侧把这通电话当成已接通**(party 全程 `state=TALKING`),交换机侧却连应答都没到;
  ②**UI 给它渲染了"接听"按钮** —— owner 截图里同一屏同时是
  `CALLING OUT / Dialling` 与 `Inbound`,顶栏还有绿色 Answer。点下去之后炸出 C24 全套
  (本窗口 10 行 CDR、真实通话仅 2 通)。C19 修的是"外呼被画成来电",这里是它的变种。
  证据:`docs/verification/artifacts/VC-S14-04/verdict.md`。
- **C21(new,2026-08-21 修 C11 时发现;2026-08-21 已修 —— 原头部标注"未修"与正文矛盾,
  2026-08-22 扫描时更正)** CDR 归属靠一场静默竞态决出:`CallFinished`
  用 `!snap.Bot.IsZero()` 判断"bot 已交接、人工路径拥有这一行",但 `IsZero()` 把 `DID` 也算在内,
  而 bot 腿总带着 `export_vars` 导出的 DID —— 于是**纯 bot 呼叫(contained)时人工路径也会尝试写行**,
  与 aicall recorder 抢同一个 `call_id`,靠 `ON CONFLICT (call_id) DO NOTHING`(ledger.sql:19)
  静默决胜。近三日两通 contained 呼叫都是 recorder 赢(bot_sec>0),但这是运气不是保证:人工路径
  的那一行没有 bot 的转写、时长与 containment。真正的交接标记应是 `Bot.Sec > 0`(只有
  `stampBotShare` 会设)。修复须同时核对 contained 呼叫的 CDR 归属,故未在验收途中动。
  **已修(2026-08-21)**:判据改为"**bot 是否盖过章**"本身,而不是章的内容 ——
  `BotShare.IsStamped` 在 `variable_aicc_bot_sec` **存在**时置位(注意不是 `>0`:
  第一秒内决定的转接会盖出 0),`CallFinished` 改用 `snap.Bot.HandedOver()`。
  contained 呼叫因此不再进人工路径,竞态消失。两条回归测试:
  `TestTheBotOwnsItsFinishedCalls`(fixture 补上 contained 也带导出 DID 的真实形态)、
  `TestAnImmediateHandoverIsStillAHandover`(Sec=0 仍须落库,否则该通电话会一行都没有)。
- **C12(new,2026-08-20 T3.1 执行发现;2026-08-21 已修并现场复验)** GET /calls/waiting 拒绝 supervisor(403 "this account is
  not an agent",call_handlers.go:52-66 agent 视角实现)——与旅程 B3 及账本多 case 的 sup 假设冲突。
  决策+修复:handler 补 supervisor 分支(全队列)or 契约明确 agent-only 并改 UI/账本口径;
  账本 S6-01/S12-01/S12-02 的该行 collect 先改用 wei jar(下轮修订批)。
  **【owner 裁决 2026-08-21】补主管分支(看全部队列)。已修**:`ListWaitingCalls` 认出
  `Role.AtLeast(RoleSupervisor)` 即走 `AllWaitingCalls()`(该方法本已存在),坐席仍按配员归属;
  与 `ListCalls`("for supervision",无限制)口径一致。契约 description 同步写明两种视角。
  实测三角色均 200(supervisor / admin / agent)。回归测试
  `TestTheWaitingListIsEveryQueueForASupervisor`(用一个"查无此坐席"的目录桩,
  确保主管不是靠碰巧有坐席身份才通过)。**账本 S6-01/S12-01/S12-02 的 collect 可改回主管会话**
  —— 但 C23① 记的另一半(队列启停需 ADMIN)仍然成立,不要一并改。

- **C33(new,2026-08-23 修 C29 时发现,未修)** **同一个病的整数版:队列的三个非零列默认同样永不生效。**
  `queues` 有三列的默认值不是零 —— `discard_abandoned_after_sec DEFAULT 60`、
  `rona_delay_sec DEFAULT 10`、`sla_threshold_sec DEFAULT 20` —— 而 `Queue.validate()`
  只给 `mohSound`/`strategy`/`displayName`/`tierRules.waitSec` 兜底,**这三个没有**;
  `CreateQueue` 的 INSERT 又把列名一一写出(telephony.sql:26),列默认因此轮不到生效。
  机制与 C29 **完全相同**,只是零值这次是 `0` 而不是 `false`,所以不显示为"停用",而显示为
  "RONA 不等待"、"SLA 门限 0 秒"、"放弃呼叫立即丢弃"。
  与 C29 的差别在于:布尔那半是**建出来就不通**(Lua 查不到),整数这半是**建出来就在跑,只是参数不是文档说的那个**,
  更难被发现。**未修 —— 是否修、修到哪一层由 owner 定**;本条只立案,不夹带进 C29。
  **已取证(2026-08-23,VC-S14-01 重跑顺带)**:`POST /queues {"name":…,"extNumber":"7099"}`
  → 201,`discardAbandonedAfterSec:0` / `ronaDelaySec:0` / `slaThresholdSec:0`,
  而同一响应的 `isEnabled` / `isRecordingEnabled` 都是 true —— 布尔那半已修,整数这半没有。
  **库里已有真实受害者**:`support-zh` 现为 `0|0|0`,`support-en` 为 `60|10|20`,
  而种子对两条队列用的是**同一条 INSERT**(`seed.go:252`,只显式写 `sla_threshold_sec=20`)——
  说明 support-zh 是后来被某次 API 写操作抹平的。
  **连带**:凡在 support-zh 上量过的 SLA 数字,门限都是 **0 秒**而不是 20 秒,读旧结论时要当心。
  证据:`docs/verification/artifacts/VC-S14-01/verdict.md`。
  **【已修 · 2026-08-24 `d680e79`,留证 `artifacts/C33/verdict-2026-08-24.md`】**
  **修到哪一层:沿用 C29 在同一文件里立下的先例** —— 三个值在 `NewQueue()` 里**播种**,
  不在 `validate()` 里兜底。理由与 C29 一字不差:不是零值的默认值同样无法事后补,
  `validate` 分不清"没写"和"写了 0"。这也是本条唯一有争议的一点,故按已有先例决定而非另立一套。
  **没有第四个**:catalog 三张表里非零默认的整数列**只有这三个**,
  `queues` 另外七个非零默认此前已各有归属(避免重蹈 C34"比立案时宽"的覆辙,这次先查了全表)。
  **现场**:`POST /queues {"name":…,"extNumber":"7098"}` → `60 / 10 / 20`
  (对照 08-23 同一请求给的是 `0 / 0 / 0`);显式 `slaThresholdSec:0` 仍压过默认 → `60 / 10 / 0`。
  **顺带修掉一半没立案的**:`NewQueue()` 同供 create 与 update,
  所以**没写这些字段的 PUT 现在恢复默认而不是写 0** —— 正是 support-zh 当初被怀疑的致零路径。
  **"库里的受害者"一节已作废**:2026-08-23 `f227bb1` 已就地 UPDATE 修好 support-zh 并
  自陈"That edits C33's live evidence";今日复查两条队列均为 `60|10|20`,
  且 support-zh 的 `updated_at` 仍是 08-13 的创建时刻(修复走直接 UPDATE,不经 API)。
  **历史告诫不变**:在它读作 `0|0|0` 的那段时间里量过的 SLA,门限是 0 秒。
  **测试** `TestAnOmittedIntegerTakesTheDeclaredDefault` 三型(不写 / 显式 0 / PUT 不写),
  已验证去掉播种后 ①③ FAIL;与 C29 的 `TestAnOmittedBooleanTakesTheDeclaredDefault` 并排。

- **C34(new,2026-08-23 补跑 VC-S14-01 的 403 断言时发现,未修)**
  **删一个不存在的东西,照样回 204 —— 契约里的 404 是死路。**
  三个目录删除接口一致:
  ```
  DELETE /extensions/00000000-0000-0000-0000-000000000000  → 204
  DELETE /queues/00000000-…                                → 204
  DELETE /dids/00000000-…                                  → 204
  ```
  而契约给这三条都声明了 **404**。根因:`writeDeleted`(`catalog_handlers.go:190-196`)
  只看 err、不看**删掉了几行**,而 `DELETE FROM … WHERE id=$1` 删零行不是错误;
  store 层也没有把 rows affected 交上来。于是 404 那一支**永远走不到**。
  后果:管理员打错一个 id,得到的是"已删除",他会认为那个分机/队列/号码没了 ——
  与 **C1** 的 `removed=` 假报告是同一个病(『不复查就报告成功』),
  也与 C30 同源:**静默地什么都没做,和静默地做错,对操作员是一回事**。
  **连带(方法论)**:这也说明**凭 `http=204` 判定删除成功的 collect 都不足信** ——
  本次 VC-S14-01 第 9 条若单看 204,分机根本不存在时长得一模一样;
  真正把它钉住的是第 10 条 `luacc.directory` 计数归零。
  与 C28 那轮"SPA 兜底把未知路径也回 200"是同一类陷阱:**判写操作不能只看状态码**。
  修法无需改契约(404 已经声明好了),只需 store 交出 rows affected、`writeDeleted` 据此分流。
  证据:`docs/verification/artifacts/VC-S14-01/verdict.md`。
  **【已修 · 2026-08-24 现场闭合 `9e35930`,留证 `artifacts/C34/verdict-2026-08-24.md`】**
  **比立案时记的宽**:探针跑了全部五个删除接口,说谎的是**四个** —— 立案的三个之外
  **`DELETE /agents/{id}` 同样回 204**。第五个(取消配员)之所以回 404,只是因为
  `UnstaffQueue` 先查了队列在不在;**队列真实存在而坐席根本没配在上面时,它同样回 204**。
  同一个病,一半被一次无关的查询挡住了 —— 正是"不复查就报告成功"最典型的伪装。
  **修法**:五条 sqlc 查询 `:exec` → `:execrows`;`catalogstore.go` 新增 `deleted()`
  把行数翻译成"那里本来有东西吗";坐席那条用它自己包的词汇(`pgx.ErrNoRows`),
  两者各经既有 handler 到 404。**契约一字未改**。
  **现场**:五个接口打不存在的 id 全部 404;正路径未被误伤(建一个真实 DID,
  第一次删 204、第二次删 404)。
  **测试**:`internal/store/catalogstore_delete_test.go` **需真实 PostgreSQL** ——
  缺陷本身就是 `DELETE … WHERE id=$1` 匹配不到行时的行为,**没有 fake 能复现**;
  已验证去掉修复后五个子用例全部 FAIL。另有 handler 层四个接口的 404 映射测试。
  **影响面已核**:这五个删除的调用方只有 HTTP handler,没有内部对账器,
  `AICC_SEED=fresh` 也不走这条路,故"删不到就报 404"不会打断任何自动流程。
  **连带的方法论仍然成立且更硬**:凡凭 `http=204` 判定删除成功的 collect 都不足信 ——
  本条正是被一条这样的断言放过去的。

- **C35(new,2026-08-23 VC-S12-01 重跑时 owner 手动复测转接暴露;当场已修)**
  **bot 把主叫转进了错误的 dialplan context,于是"转接成功"等于什么都没发生。**
  现场对照,同一个队列分机、相差三分钟:
  ```
  09:59:14  Transfer …18688886669 to XML[7001@aicc]     ← 今天新写的 Lua 兜底
            → mod_callcenter: Member 18688886669 joining queue support-en
  10:02:44  Transfer …18688886669 to XML[7001@default]  ← bot 的 transfer_to_agent
  10:03:40  Transfer …18688886669 to XML[7001@default]  ← 同上,第二次
            → **一行 joining queue 都没有**
  ```
  `7001` 只在 **aicc** context 有匹配(`aicc.xml` 的 `aicc_queue`,7xxx → aicc_queue.lua);
  `default` 里没有任何队列分机。后果是**静默的**:主叫听到保持音、以为在排队,
  实际**没有进任何队列**,坐席话机永远不会响,墙板上也不会多一个人。
  owner 手测时的原话是"只听到保持音,并没有看到 1008 响铃"。
  **根因是一个没人用的默认值**:`Adapter.TransferToExtension` 在 8-22 那批 aicc context 改造里
  已经把**空 context 默认成 `aicc`**,注释还写着 *"landing a transfer in `default` would route it
  by rules written for a different product"* —— 但**四个调用点全都显式传 `"default"`**,
  把那个默认顶掉了:`aicall/actions.go`(transfer_to_agent、rescueCaller)、
  `aicall/orchestrator.go`(bot 起不来时的 fallback)、`telephony/coordinator.go`(坐席发起的转接)。
  **为什么两天没被发现**:原厂 `default.xml` 有一条 `^(10[01][0-9])$` 的本地分机规则,
  于是**转到坐席分机(1008)在 default 里碰巧能走通**,VC-S14-04 那类用例照样通过;
  只有**队列**分机在 default 里无处可去。这也正是 owner 定的
  "原厂拨号方案和 aicc context 尽量解耦"要防的事 —— 我们一直在借原厂的规则活着。
  **已修**:四处一律改传空 context,由 adapter 决定;并在两处写下为什么不在这里点名 context。
  `go test -race ./...` 0 FAIL(单测覆盖不到这条 —— 它是 ESL 命令的一个字符串参数,
  桩件不会说 `default` 里没有 7001;**这条只有现场能证**)。
  **后续(未做,须 owner 定)**:`TransferToExtension` 的 `context` 参数现在**没有任何调用点在用**,
  留着就是同一个陷阱的下一次机会,建议**直接删掉这个参数**;那要动 `aicall.Switch` 接口与各桩件,
  不夹带进本次修复。
  证据:`docs/verification/artifacts/VC-S12-01/verdict.md`。

- **C36(new,2026-08-23 VC-S12-01 重跑发现,未修)**
  **重启后新实例不收养队列里的主叫,而把每一次派单的坐席腿当成一通新外呼。**
  主叫在交换机上确实在队列里:
  ```
  callcenter_config queue list members support-en
  support-en|…|session_uuid=01a02c57-a227-…|18688886669|state=Trying|serving_agent=agent-wei
  ```
  而应用侧:`/api/v1/calls/waiting` → `{"items":[]}`;
  `/api/v1/calls` → 只有一通 **`callType: OUTBOUND`**、唯一 party 是 `1008` 的 `DIALING` 腿,
  `otherNumber` 才是主叫号码。**主叫本人从头到尾不在应用里**,每派单一次就多开一通假外呼
  (实测 RONA 后重派,又是一通新的假 OUTBOUND)。
  后果即本用例 `failure_looks_like` 的原话:*"这通电话直到有人接起前对所有屏幕都是隐形的"* ——
  主管看不到有人在等、报表少一通、坐席弹屏没有来电上下文。
  **为什么现在才看见**:C26 修好之前主叫在 bot 腿死后 50 毫秒内就被挂断,根本活不到进队列,
  这个洞一直躲在它后面。**这是 VC-S12-01 目前判 FAIL 的唯一原因**(C26 那半已证实修好)。
  方向:重启后的通道对账(`ShowChannels` 收养)没有把"已在 callcenter 里排队的主叫通道"认回来;
  而 `cc_member_session_uuid` 本来就是为这件事准备的(见 `aicc.xml` 关于第二条腿的长注释)。
  证据:`docs/verification/artifacts/VC-S12-01/verdict.md`。
  **【前一半已修 2026-08-23:等待名单】** 根因比"没收养"更朴素:**入队只被宣告一次**。
  `member-queue-start` 落在重启的 ESL 断档窗里(09:59:11–09:59:17,入队在 09:59:14.388),
  此后**没有任何事件会再说一遍** —— 这正是 needs-FACT **F4** 的形态,而 F4 当初被判"本题不适用",
  理由是"主叫从未进入队列";**C26 修好后这个理由不成立了,F4 就此重开**(表格那一行须更正)。
  更要命的是:把主叫从垂死 bot 腿上救下来的兜底,**恰恰就在这一刻触发** ——
  最需要被看见的那通电话,正好落在最看不见的那个窗口里。
  **修法**:连接钩子里加第三项对账 —— `Coordinator.ReconcileWaiting`,
  按交换机自己的成员表重建等待名单(`callcenter_config queue list members`)。
  三件事值得记:
  - **两个读取器早就写好了,只是从没被接上**:`Adapter.ListQueueMembers` 的注释写着
    *"used to reconcile after a reconnect"*,`ShowChannels` 的写着
    *"the source of truth when reconciling after a restart or a reconnect"* ——
    **两个都没有任何调用点**。C36 是接线缺口,不是设计缺失。
  - **是收敛不是补齐**:交换机是真相,两个方向都算 —— 它有我们没有的,加;
    我们有它没有的,删。只加不删会留下永远排队的幽灵,与 C1 那个"不复查就报成功"同形。
    但**读失败的队列不动它的条目** —— 读不到不等于知道它空。
  - **等待起点取交换机的 `joined_epoch`,绝不取 `time.Now()`**:用当下时间恢复,
    一通已经等了两分钟的电话会显示成刚来的,**队列的服务水平反而因为我们弄丢了它而变好看** ——
    与 C27/C33 police 的是同一种数字不诚实。解析不出可用的入队时间就丢弃该行,
    宁可等下一次事件重建,也不发明一个等待时长。
  恢复走的是 `queueJoined` 本身,所以发布与去重和真实入队完全一致;去重键是成员通道,
  于是**进程没死的普通重连是彻底的 no-op**(有回归用例钉住)。
  回归:`TestTheSwitchsOwnMemberListingIsRead`(fixture 是**本次现场原样抄下来的**成员行,
  连表头和 `+OK` 一起 —— 凭记忆敲的 fixture 会和自己的 bug 一起通过,这是 C30 那次
  SQLSTATE 23001 的教训)、`TestACallerQueuedWhileWeWereDownIsFoundAgain`、
  `TestACallerWhoLeftWhileWeWereDownIsNotStillWaiting`、`TestReconnectingWithNothingChangedSaysNothing`、
  `TestAQueueWeCouldNotReadKeepsItsCallers`、`TestAnAnsweredCallerIsNotRestoredToTheLine`、
  以及连接钩子那条 `TestReconnectRebuildsPresenceStaffingAndDevices`。**五处逐一摘除验证**过。
  **后一半仍未修(假 OUTBOUND)**:主叫的 Call 不在 registry 里,于是每条派单腿都自成一通外呼。
  `coordinator.go` 里那段守卫早就预见了这件事(注释原文:*"the stray call ends unmerged and
  reaches the ledger as an outbound CDR with caller and agent reversed, one per retry"*),
  它只是**没有可绑的东西**。方向已定:在 `ReconcileWaiting` 里顺带**收养**主叫的通道 ——
  从通道上取回 `aicc_call_id`(不是新铸一个,否则录音与转写会成孤儿),
  起始时间取成员行的 `system_epoch`,主叫方 party 直接置 ESTABLISHED(他早就接通了),
  不重放事件(重放会把"两分钟前就接通的人"说成正在振铃);屏幕靠重启本就会发的
  `SYSTEM_RESET` 重新拉快照。**`ShowChannels` 全量清扫暂不做**:它会在每一次瞬时重连时运行,
  而那时 registry 正持有活着的通话,收养一个正在与 bot 通话的主叫会重新打开 C21 关掉的
  "两个写入者抢同一行 CDR"的竞态。
  **【后一半也已修 2026-08-23:收养】** 收养做在 `ReconcileWaiting` 里,不做在派单守卫上 ——
  它只在连接时跑一次、不在事件路径上加 ESL 往返;而且顺带把 CallID/CallType/Language
  给了等待条目(否则只有交换机报的号码),重启后**放弃排队的主叫也终于有 CDR**
  (在此之前那次挂断落在未知通道上,直接消失)。
  - **身份是取回来的,不是新铸的**:`uuid_getvar <chan> aicc_call_id`。
    拨号方案在任何腿存在之前就铸好了这个 id,录音、转写、已有的 CDR 全都指着它;
    新铸一个会让这三样同时成为孤儿。**通道说不出自己是哪通电话就不收养** ——
    主叫仍然按交换机报的号码留在等待名单里,也就是今天的行为。
  - **不重放任何事件**:主叫几分钟前就接通了,他的 party 直接以 `TALKING` 加进去,
    而不是沿着一段没发生过的历史走一遍(那会把"两分钟前就接通的人"当场宣告成正在振铃)。
    屏幕靠重启本就会发的 `SYSTEM_RESET` 恢复 —— 已核对前端确实是"全量失效重取快照"
    (`web/src/lib/use-event-stream.ts`),这正是它存在的理由。
  - **起始时间取成员行的 `system_epoch`**,与 `joined_epoch` 同理:被恢复的通话不能从恢复那一刻算起。
  - `callType` 读 `aicc_call_type`,读不到才默认 INBOUND —— 它一经创建不可改,不能靠猜。
  回归三条:`TestAdoptingAQueuedCallerGivesTheDeliveryLegSomethingToBindTo`、
  `TestAChannelThatCannotNameItsCallIsNotAdopted`、`TestAdoptingIsIdempotent`;
  **五处逐一摘除验证**(去掉收养 / 用恢复时刻当起点 / 不置 TALKING / 新铸 id / 说不出 id 也照收)。
  **`uuid_getvar` 的返回格式已现场核对(2026-08-23,对着一通活着的通话)**,三种回答与实现一致:
  已设置 → 裸值(`01a02c97-ae6b-…` / `en` / `95001`);未设置 → `_undef_`;通道没了 → `-ERR No such channel!`。
  已抄进 `TestWhatTheSwitchAnswersForAChannelVariable` 当 fixture。
  同一次还证实两件事:入呼的 `aicc_call_type` 就是 `_undef_`(所以默认 INBOUND 是对的),
  以及**转接的那一通 `aicc_bot_finished` 确实是 `_undef_`** —— C26"转接一律不盖印"的设计在真实转接上成立。
- **C37(new,2026-08-23 VC-S12-01 重跑发现,未修;观察一次,机制未独立复现)**
  **一次被取消的派单把坐席话机卡死,之后每 70 毫秒被重试一次,持续到主叫放弃。**
  ```
  09:59:14.428  第一次派单 agent-wei
  09:59:16.128  Agent agent-wei Origination Canceled : ORIGINATOR_CANCEL
  09:59:16.2 起  USER_BUSY … 一簇一簇地重试,簇内约 70ms 一次;三分钟里共 42 次拒绝
  10:02:25      Member … abandoned waiting in queue support-en
  ```
  第一条振铃腿没被拆干净(通道在 `CS_CONSUME_MEDIA/RINGING` 一直挂着),
  浏览器话机此后对每个新 INVITE 回 486;而**拒绝不是无应答**,不走 RONA 退避,
  `reject_delay_time=0`(`callcenter_config agent list` 实测,由应用镜像写入)于是立即重试。
  owner 当时的观感:"只听到保持音,并没有看到 1008 响铃"。
  **与 C36 同源但后果独立**:即使收养修好,这个忙等循环仍会让一名坐席在整通电话里
  既接不到、也不显示为忙。**只观察到一次**(重启后的状态下),机制未独立复现 ——
  若要处理,先补一条能稳定重现它的用例。
  **【2026-08-23 参数已改,待复现验证】** W2.1 随 `mirrorRegistration` 下发了
  `reject_delay_time=60`(此前是 0 = 立即重试),这正是本条的直接对策:被拒之后要等一分钟才会再派。
  **但本条不改判为"已修"** —— 触发条件(那条挂死的 INVITE)本身还没弄清,
  而且没有能稳定重现它的用例。下次遇到时先看重试间隔是不是变成了 60 秒。
  证据:`docs/verification/artifacts/VC-S12-01/verdict.md`。
  **【2026-08-24 成因查明并已在 web-sip-phone 侧修复;aicc 侧零代码改动。
  留证 `artifacts/C37/investigation-2026-08-24.md`】**
  调查用的是 C7 当天刚建的 `GET /calls/{callId}/queue-events` —— **没有那条读路径就没有样本**。
  **一、七个样本的共性**:全部是 wei 的**浏览器话机**,ben 的原生软电话 **0 次**(7:0);
  节奏是**簇**(一秒约 10 次、间隔约 61 秒),不是均匀的 70ms;
  触发前置是**一次被主叫中途挂断打断的派单**(不是 RONA 超时);
  **一旦卡住跨通话持续不自愈**(四通紧跟前一通被放弃之后,一通在前一场风暴未结束时就开始)。
  另两条排除项:应用侧 wei 的状态**是对的**(确实 READY,不是镜像漂移);
  **应用在整场风暴里一个字都没记** —— 派单只进 `queue_events` 不进日志。
  **二、成因(读 web-sip-phone 代码得出)**:`handleInvite` 只在
  `session === null && state === Idle` 时受理,否则回 486;而**离开 Ended/Failed 只有两条路** ——
  `setState` 挂的那一个 `setTimeout`(1s/3s),或 `forceIdle()`,
  **而后者唯一的调用方是传输重建**。那个定时器没执行,话机就对之后每个 INVITE 回 486,
  直到传输重建才好。这解释了七次的全部共性。
  **三、复现实验的结果是"复现不出来"**,而这本身是证据:
  触发前置完整重演(主叫在振铃中挂断,连 `CS_CONSUME_MEDIA` 都一样),第二通**正常振铃 15 秒**;
  当时**标签页前台活跃**,定时器正常触发。与"后台冻结才卡死"一致。
  顺带实证了 W2.1 的守卫**对无应答有效**:15 秒超时 → wei 立刻 `NOT_READY(SYSTEM)` → 停止派单。
  **四、web-sip-phone 的处置**:`handleInvite` 惰性补跑 RESET(受理不再依赖任何后台定时器);
  `SipRuntime.start()` 开头 `forceIdle()`(堵住 stop→start 继承 Ended 的第二条路径);
  486 按状态拆分(真在通话/振铃仍 486,槽被已进终态的 session 占着改 **480**);
  那个 1s/3s 的合法 486 窗口**取消**;新增 `overdueByMs` 诊断字段让下一次现场自证。
  **五、⚠ aicc 侧留下一个已知缺口**:**486 没有兜底**。
  实测两次(占住 wei 的话机 → 送主叫进只配了 wei 的 support-en):
  `[terminated][486]` → `USER_BUSY`,而 wei 全程 `Available` / `no_answer_count=0` ——
  **486 不计入 `max_no_answer`,坐席不会被挪出轮转**;对照无应答一型是立刻挪走。
  修复之后这不成问题(话机不再从已死状态发拒绝,此后的 486 意味着真有会话占用,不挪是对的),
  但**若将来出现别的成因让话机持续 486,交换机侧拦不住**。
  **未观察到的部分如实记**:两次合成主叫只待了 14 秒与 41 秒,不足 `busy_delay_time=60`,
  **没看到第二次派单**;"会不会一分钟一次无限重试"是从参数推的,不是测出来的。
  **六、我方观测缺口**:应用只记 `registrations reconciled` 汇总行,
  **不记单个话机的注册/注销**,所以对方提出的 stop→start 继承路径在我们日志里
  **既不能证实也不能排除**。

- **C38(new,2026-08-23 VC-S12-01 第二次重跑发现,未修)**
  **被兜底救回来的通话,账本只记到重启那一秒 —— 坐席那四分钟不存在。**
  ```
  被收养  01a02c9a-d6fb-…  bot_sec=36  talk_sec=0    bill_sec=36  queue_id=NULL
          started 03:12:20   ended 03:12:57   ← bot 腿死掉的那一刻
  对照组  01a02c97-ae6b-…  bot_sec=12  talk_sec=161  bill_sec=177 queue_id 有值
  ```
  坐席实际通了四分多钟(11:14:02 接起,约 11:17 挂断)。
  `tech` 字段指认了写入者:被收养那行是 `{codec, sipCallId, remoteRtpAddr}` —— **bot recorder 的形状**;
  对照组是 `{switchBillSec, callerChannelId}` —— 人工路径的形状。
  旧实例关闭时把这通电话当"到此为止"落了一行,随后人工阶段那一行被
  `ON CONFLICT (call_id) DO NOTHING`(`ledger.sql:19`)**静默丢弃**。
  **这是 C21 竞态的另一副面孔**:C21 关掉的是"两个写入者抢同一行",这一条是
  **bot 先写下的那行没人能再纠正** —— 同一个 `DO NOTHING` 在两种情形里做的事相反。
  后果:一通被兜底救回、坐席真的接了的电话,在账本里长得像一通在重启那秒就结束的纯 bot 通话 ——
  坐席工时不见了,队列不见了,计费短了几分钟;而这恰恰是**最需要被算对的那种电话**。
  **只在"重启穿过一通电话"时出现**,所以在 C26/C36 修好之前根本走不到这一步。
  方向待定:要么人工阶段允许**补写**(把 DO NOTHING 换成有条件的 UPDATE,须先想清楚哪些列可覆盖),
  要么 bot recorder 在"腿是被我们自己关停杀死的"时不落终局行。**两条都要先想清楚再动 C21 的那把锁。**
  **【已修 2026-08-23】取第一条,但判据不是"哪些列可覆盖",而是"哪一行看到的通话更完整"** ——
  `ON CONFLICT (call_id) DO UPDATE … WHERE EXCLUDED.ended_at > cdrs.ended_at`。
  **为什么不取第二条**(bot 在自己被关停时不落行):进程被 `SIGKILL` 时本来就什么都不写,
  而**优雅关闭与崩溃在账本上应当没有区别** —— 但"不落行"会在另一个方向丢数据:
  没有配 fallback 的号码,主叫会被 Lua 挂断、根本进不了队列,于是也不会被 C36 收养,
  那通电话将**一行都没有**。让人工阶段补写则两种都覆盖:主叫真的跟着结束,bot 那行就是对的、留着;
  主叫活了下来,人工那行更完整、盖过去。
  **为什么这没有重开 C21 的竞态**:C21 关的是"两个写入者抢同一行",而这条规则是**单调**的 ——
  行只可能被"看得更远"的那一行替换,永远回不去,所以两个写入者不会来回倒。
  两条路平时也不重叠(转接归人工、收官归 bot,各自都拒绝写对方的),重叠只发生在本条这一种情形。
  回归:`TestALaterEndingReplacesTheRowThatSawLessOfTheCall`(`internal/store/workspace_test.go`,
  **走的是出货的那条 `InsertCDR`,不是手写的 upsert** —— 规则只有在真正发货的查询里实现了才算数;
  第一版我写成了内联 SQL,自测通过而证明不了任何事,已改)。
  **两处摘除验证**:改回 `DO NOTHING` → *"talkSec = 0, want 240 —— 坐席那四分钟被丢弃了"*;
  去掉单调性守卫(无条件覆盖)→ *"一行看得更少的到达之后 talkSec 又变成 0"*。
  **现场复验(2026-08-23 16:42–16:44,完整编排:通话中重启 → 兜底进 support-zh → 收养 →
  1008 接起 → 主叫挂断)**:

  | | 今早(未修) | 修后 |
  |---|---|---|
  | `talk_sec` | **0** | **11** |
  | `queue_id` | NULL | 有值 |
  | `agent_ids` | — | 1 |
  | `ended_at` | 停在重启那一秒 | **主叫挂断那一刻** |
  | `tech` | `{codec, sipCallId, remoteRtpAddr}`(bot recorder) | `{switchBillSec, callerChannelId}`(人工路径) |

  `tech` 是决定性的:人工那行确实盖过了 bot 那行。`bill_sec=123` 亦对。
  **⚠ 同时暴露一处遗留,主动记下**:`bot_sec=0`,而 bot 实际说了十几秒。
  原因是 bot 的时长是**转接/收官时盖在主叫通道上的**(`aicc_bot_sec`),
  而重启在它盖章之前就把它杀了 —— 这一份**真的没有幸存证据**。
  可以从"入队时刻 − 通话开始"推算,但那是**推算不是记录**,与今天一路 police 的
  `joinedAt`/`system_epoch` 同一类问题(宁可缺,不可编)。
  故此处**留 0 并记明含义:0 表示"这通电话的 bot 时长没有幸存下来",不是"bot 没说话"**。
  是否要改成可空、或从入队时刻推算,**由 owner 定**。
  证据:`docs/verification/artifacts/VC-S12-01/verdict.md`。

- **C39(new,2026-08-23 VC-S12-01 第二次重跑发现,未修;机制两说,未判定)**
  **重启后坐席在交换机上被判 `On Break` 达 63 秒,而应用侧的注册对账只差 6 毫秒就读完了。**
  ```
  11:12:59.329  esl connected
  11:12:59.368  (FS) Updated Agent agent-ben set status = On Break
  11:12:59.377  agent presence mirrored to the switch  agents=3      ← SyncSwitch
  11:12:59.388  (FS) Updated Agent agent-wei set status = On Break
  11:12:59.394  registrations reconciled endpoints=2                 ← ObserveDevice ×2
  11:14:02.328  (FS) Updated Agent agent-wei set status = Available   ← 63 秒后
  ```
  代价是**这位主叫多等了一分钟**才被派单 —— 而他正是被兜底救回来的那一通。
  形态正是 C28 那段注释警告过的:*"an agent signing in at a perfectly good phone reads as
  unreachable until the phone happens to re-register"* —— 真正让 wei 恢复的像是话机自己的续注册。
  **不是 `Registrations()` 读错**:同一台机器现在 `sofia status profile internal reg` 报的是
  `Ping-Status: Reachable`,而解析器只在**显式 `Unreachable`** 时才判不可达
  (`registrations.go:71`),所以 `ObserveDevice(1008, true, true)` 本该映射成 `Available`。
  **两种候选机制,日志分不出来,都没有证据**:
  ① `ObserveDevice` 在"观测结果与已持久化的 presence 一致"时短路,不再镜像 ——
     于是 `SyncSwitch` 先写下的 `On Break` 没人纠正;
  ② `SyncSwitch` 与注册对账之间存在**写入顺序/异步竞态**(FS 侧的写发生在 .368 与 .388,
     跨在应用的 .377 与 .394 两侧),后写的把 `Available` 盖回了 `On Break`。
  **判定前不要改** —— 这两条的修法方向相反(一个要去掉短路,一个要调顺序或加重试)。
  下一步:在 `ObserveDevice`/`mirrorStatus` 上加一条能分辨"短路了"与"写了但被盖"的日志,
  再重启一次即可定案;不需要真实通话。

  **【2026-08-23 已定案并修复 —— 两条候选都不对,真机制更糟】**
  按上面的办法加了镜像诊断(`mirroring presence to the switch`,含 status 与三个来源字段),
  一次重启就说清了:

  ```
  16:42:44.063  (FS) Updated Agent agent-wei set state = Receiving   ← 交换机已在派单给 wei
  16:42:46.263  (FS) agent-wei set status = On Break                 ← 重启对账,派单还在飞
  16:42:46.333  (FS) agent-wei set status = Available                ← 60 毫秒后就纠正了
  16:44:07      下一次派单                                            ← 主叫多等 81 秒
  ```

  **不是"卡在 On Break"**(应用 60 毫秒就纠正了,两次重启实测都是几十毫秒),
  **而是重启时的 presence 镜像打断了一次正在进行的派单** —— mod_callcenter 随后按
  `no_answer_delay_time=60` 退避才再试。原来记的"63 秒"是**症状的时长,不是状态的时长**,
  归因错了。
  **根因**:`Restore` 在启动时从库里载回 presence 并调 `applyDeviceLocked`,
  而那一刻 `s.devices` 是空的 —— **不知道 = 不可达**,于是一个好端端的坐席算出 `On Break`;
  `SyncSwitch` 随即把这个错值广播出去,**在读注册表之前**。
  **修法**:连接钩子里**先安静地学话机,再镜像 presence**。新增 `Service.NoteDevice`
  (只记录,不镜像、不发事件),连接后先跑一遍;随后 `SyncSwitch` 拿到的就是真值,
  **错的 On Break 一次也不会发出去**。原有的 `ObserveDevice` 循环留在镜像之后,
  仍然发 `DEVICE_*` 事件,让离线期间变过的话机能到达屏幕。
  顺带修正一处:注册表**读失败时把返回值当无效**(此前失败仍会用它带回的行)。
  回归:`TestThePhonesAreKnownBeforePresenceIsMirrored`(断言顺序,不是断言调用次数);
  摘除验证 → *"no phone was recorded before the mirror"*。
  **现场复验**:重启后第一条镜像就是 `agent-wei status=Available isRegistered=true`,
  错值不再出现。
  证据:`docs/verification/artifacts/VC-S12-01/verdict.md` 末节。

- **C40(new,2026-08-23 开跑 C14 时发现;当场已修)**
  **一次修复顺手关掉了人工阶段的实时转写,静默两天。**
  重跑 VC-S9-01 的第一通电话:通话正常、坐席通了 28 秒、CDR 正确,
  **而应用日志里连一行 `transcription tap attached` 都没有** —— 转写从头到尾没有开始。
  根因在 `Coordinator.join`(`coordinator.go`):tap 挂在**合并分支里面**,
  而入口第一行是 `if !ok || !otherOK || callID == otherID { return }`。
  **`dec47ad`(2026-08-21,"a queue delivery leg joins the caller it was dialled for")**
  让派单腿在 `CHANNEL_CREATE` 时就绑进主叫那通电话 —— 那个改动是对的,它正是阻止
  "派单读成一通外呼"的东西(见 C36 后一半)—— 但从此桥接时两条腿**已经在同一通电话里**,
  `callID == otherID` 直接 return,`tapAgentLeg` 再也够不着。
  **时间线正好错开一天**:VC-S9-01 最后一次执行是 2026-08-20,绑定次日落地,
  此后没人重跑过这条 —— 于是**每一通经队列派单的电话都没有转写,而没有任何东西报错**。
  **为什么单测没拦住**:既有那条 `TestTheTapGoesOnTheAgentLegAtTheBridge` 造的派单腿
  **只带 `variable_dialed_user`、不带 `cc_member_session_uuid`** —— 那是 8-21 之前的形态。
  测试模型停在旧世界,于是它一直绿着,而生产早已换了形状。
  **已修**:合并与否是**身份**问题,挂不挂 tap 是**桥接**问题,两者不是同一个问题 ——
  `join` 改成"需要合并才合并",tap 无条件在桥接时挂上;`announceAudience` 仍留在合并分支里
  (没有合并就没有 party 迁移,不必重播受众)。
  回归:`TestTheTapGoesOnEvenWhenThereIsNothingToMerge` —— **按真实派单形态**
  (带 `variable_cc_member_session_uuid`)造腿。摘除验证:把早退还原,该用例报
  `condition not reached in time`,旧用例照旧通过 —— 正是它两天来的表现。
  **对 C14 的意义**:在此之前 VC-S9-01 **根本无法重跑** —— 没有 ASR,就没有丢帧可测。
  **【当天补修:第一版只修了一半】** tap 挪出去了,**`announceAudience` 还留在合并分支里**,
  理由写的是"没有合并就没有 party 迁移,不必重播受众" —— **这个理由是错的**。
  转写 actor 按 `agentIDs` 给自己定 scope(`transcript/actor.go` 的 `scope()`),
  受众没被宣告过就是空,于是**每一行都写进了库、发给了没有人**:
  现场抓 wei 的 SSE,整通电话 `QUEUE_JOINED`/`PARTY_*`/`CALL_CDR` 都在,
  **一条 `CALL_TRANSCRIPT` 都没有**,而库里 ASR 行好好地躺着 —— 坐席面板全程空白且不报错。
  受众和 tap 是同一个理由、同一个时刻:**派单腿在创建时就绑进来了,没有任何东西迁移,
  但这通电话上确实多了一个人**。两者一起挪到合并分支之外。
  回归用例同步补上受众断言;摘除验证(只在合并时宣告)报
  `audience = [] (announced=false)`。
  这一条是**现场重跑相对跑测试的又一次兑现**:我自己的修复自洽、测试全绿,
  只有真实的 SSE 流说了不。
  证据:`docs/verification/artifacts/VC-S9-01/verdict.md`(重跑记录)。

- **C41(new,2026-08-23 W2.1 落地时发现,未解)**
  **`agent-originate-timeout` 送到了、被接受了、不起作用 —— 振铃仍是 60 秒。**
  ```
  xml_locate configuration … callcenter.conf
    <settings><param name="agent-originate-timeout" value="15"></param></settings>   ← 交换机确实看到了
  实测振铃  16:09:07.33 → 16:10:07.02 = 59.7 秒（CDR ring_sec=59）
  ```
  已排除的解释:①**参数名没写错** —— `strings mod_callcenter.so` 里有这个字面量;
  ②**Lua 确实在供这份配置** —— 同一次 `reload mod_callcenter` 之后 support-en/support-zh
  两条队列都从 Lua 重建了出来,而它们只可能来自这份 XML;
  ③**不是没重读** —— 已执行 `reload mod_callcenter`(不是只 `reloadxml`),模块重新装载过。
  剩下的候选没有证据分辨:该参数或许只在**队列**块里被读、或许被每次派单的其它取值覆盖、
  或许是模块本身的问题。**没有继续猜** —— 参数留在 `<settings>` 里(无害,且与文档一致)。
  **影响**:一次漏接仍然让主叫白等一分钟。W2.1 表格里"收益最大的一项"因此**尚未兑现**;
  而同批的其余四个**每坐席**参数(`max_no_answer` / `no_answer_delay_time` /
  `reject_delay_time` / `busy_delay_time`)**实测已生效**(交换机侧 agent list 可见,
  且 "sleeping for 60 seconds" 现场可见)。
  **【2026-08-23 已修 —— 绕开该参数,不是让它生效】**
  按建议把 param 同时写进每个 `<queue>` 块并重载,**第三次实测仍是 59 秒**。
  至此四条替代解释全部排除、两个位置都试过,判定为
  **`agent-originate-timeout` 在本版本 mod_callcenter 上不起作用**,不再猜下去。
  **改走我们自己控制得了的那条路**:振铃时长挂在**坐席自己的拨号串**上 ——
  `callcenter_config agent set contact` 的值是我们下发的,而这段代码本就示范了
  `{...}` 前缀可用(`sip_auto_answer=true`),于是 contact 变成
  `{leg_timeout=15}user/1008@…`(需要自动应答时是 `{leg_timeout=15,sip_auto_answer=true}…`)。
  **现场实测**:`17:14:13.115` 起振 → `17:14:28.003` `Origination Canceled`,**14.9 秒**;
  CDR `ring_sec=14`(此前三次都是 59)。W2 的 RONA 链在其上照常闭合
  (`a delivered call rang out unanswered` → wei `NOT_READY/SYSTEM`)。
  两条被钉住的命令串同批更新(`TestCommandStrings`)。
  **W2.1 表格里"收益最大的一项"至此兑现**:一次漏接从让主叫白等一分钟,变成十五秒。
  证据:`docs/verification/artifacts/VC-S5-01/verdict.md` 末节。

- **C42(2026-08-24 移出 C 系列 —— 置呼时的写入路径已建成并闭合;呼叫中修改另立项目)**
  **【owner 2026-08-24】** 本条作为缺陷已了结:契约描述的能力现在真实存在了(置呼时可写)。
  **剩下的"呼叫中修改"(TAttachUserData 等价物)不再计入 C 系列**,另立项目 ——
  owner 同时指出该项目还包含 **webhook / 推送给客户** 的需求,规模已超出一条缺陷。
  `CALL_USER_DATA` 至今零生产者**仍然是对的**:数据只在置呼时给定,没有"变更"可宣告;
  产生它的正是那个项目。**以下为立案与实现的原始记录,保留不改。**
- **C42(原始记录:new,2026-08-23 做 W7③ 时查明)**
  **`userData` 是一个永远为空的字段,而契约说它是可以被合并修改的。**
  - `CreateCallRequest` 的字段只有 `callId / kind / to / did / language` —— **建呼叫时设不了**;
  - 全仓没有任何接口能修改它(OpenAPI 里没有一个写操作的请求体提到 `userData`);
  - `Call.MergeUserData` 的**调用者只有测试**(`internal/telephony/call_test.go`、`registry_test.go`);
  - 生产里唯一碰它的是 `waiting.go:233`,而那是**读出来做展示**,不是写。
  与此同时:它挂在 `Call`、快照、CDR 上,随**每一条事件信封**下发,
  契约的描述写着 *"Business data attached to the call; merge-patched, survives transfers."* ——
  **"survives transfers" 是真的**(`merge()` 会带过去),**"merge-patched" 从来没有发生过**。
  后果不大但性质明确:**契约在描述一个不存在的能力**,而任何据此写集成的人都会以为自己能带业务数据进来。
  这也是 `CALL_USER_DATA` 这个 SSE 类型至今零生产者的**真实原因** ——
  不是漏了一跳,是**没有变更可宣告**。
  **两条路,须 owner 定**:①建写入路径(契约 + handler + 协调层 + 事件),让它成为真功能;
  ②从契约里摘掉 `userData` 与 `CALL_USER_DATA`(breaking change,走 `make api-breaking`)。
  **不建议的第三条**:为它补一个 publish 点 —— 那是给一个不存在的功能伪造生产者。

  **【2026-08-23 owner 选①,已建置呼时的写入路径】** `POST /calls` 增加可选 `userData`:
  扁平 KV(值为字符串)、最多 32 键、单值 ≤1024 **字节**,超限 **400 `USER_DATA_TOO_LARGE`**
  —— 拒绝而不截断:一块只显示了半份客户资料、又不说另一半去哪了的屏幕,比一个失败的请求更糟。
  **限额是字节不是字符**,而本部署的验收材料是中文(一字三字节),按字符算会放进三倍。
  上限写进 schema(`maxProperties` / `additionalProperties.maxLength`)而不是只写在描述里,
  但生成代码不强制,handler 才是执行者。
  **承载复用既有的,没有第二套**:值最终落在 `Call.UserData` →
  事件**信封**上的 `userData`(`events/event.go:98`,弹屏消费点
  `web/src/routes/_app.agent.index.tsx:617`)→ 快照 → `cdrs.user_data`
  (`00005_call_ledger.sql:43`,写在 `ledgerstore.go:118/166`)。
  **不写 FreeSWITCH channel var**:业务数据一旦上通道变量,就同时进了交换机的日志、库和事件流,
  而坐席屏幕本来就从这一侧到达。因此请求把它留在应用里,由**两个读者**取:
  协调层(建通话时挂上,弹屏与人工 CDR 靠它)与 bot 会话
  (**AI 外呼被 bot 收官时,落库的是 bot 那一行,而 `internal/aicall` 完全不接触 registry** ——
  没有这个读者,数据会在转接时到得了屏幕、在不需要转接的通话里从账本消失)。
  是"读"不是"取走",因为两个读者都要读到。
  载体带 TTL(10 分钟)与容量上限:originate 失败会立刻 `Drop`,但"交换机收下了却什么也没发生"
  的通话会留下孤儿条目,而那正是最不该长期留存的数据。
  **两个建通话点都挂**(`coordinator.go` 的 `adopt` 与 reidentify 两处)——
  一通电话的哪条腿先报出 minted id 是不确定的,只挂一处会让业务数据取决于交换机先announce 了谁。
  **前提核实的两处出入**(记录在案,不是假设):①**没有 `calls` 表**,活着的通话只在内存里,
  落库点是结束时的 CDR,所以"写 calls.user_data"落到 **`cdrs.user_data`** —— 同一列、同一读路径;
  ②`cdrs.user_data` 是 `jsonb **NOT NULL** DEFAULT '{}'`,**存不了 NULL**;
  "null 与空对象等价"因此实现为**都不记录**(空 map 不入载体,列保持其 `'{}'` 默认值),
  语义一致而未改列约束 —— 改成可空需要迁移,不在本次范围。
  **`CALL_USER_DATA` 事件仍无生产者,这是对的**:数据现在只在置呼时给定,**没有"变更"可宣告**;
  产生它的是**呼叫中修改**,而那一条 owner 已明确另行立项(TAttachUserData 等价物)。

- **C43(new,2026-08-23 现场验 userData 时发现,未修)**
  **AI 外呼的 CDR 主被叫是反的。**
  ```
  call_type=OUTBOUND  from_number=18688886669  to_number=95002  did=95002
  （实际是 95002 呼出到 18688886669）
  ```
  根因:`internal/aicall/ledger.go` 里 `FromNumber: call.fromNumber`(取自 `X-AICC-ANI`)、
  `ToNumber: call.did`。这对**呼入**是对的(to = 被拨的那个 DID),对**外呼**正好倒过来 ——
  外呼时 DID 是我们的主叫号,而客户号在 `req.To`。
  **人工路径没有这个问题**:同一天同一条编排下的点击拨号 CDR 是
  `from=1008 to=18688886669`,正确。所以这是 aicall 那一份独有的。
  与今天 `PARTY_DIALING` 那次同型:**借用了另一个方向的语义**,键名对、值也像模像样。
  影响:外呼报表里"谁打给谁"是反的,而且 `to_number` 恒等于 `did`,
  按被叫号检索 AI 外呼**永远查不到**。
  **【已修 2026-08-23】** 判据是一条对称的事实:**`DID` 永远是我们这一侧,`ANI` 永远是对端**。
  呼入时对端是主叫(`from=ANI, to=DID`),外呼时对端是被叫(`from=DID, to=ANI`)。
  原代码把 ANI 当成"永远是主叫"、DID 当成"永远是被叫",对呼入成立、对外呼正好翻转。
  `DID` 列本身两个方向都不变 —— 它说的是这通电话属于我们哪个号,不是哪一端。
  回归 `TestTheLedgerKnowsWhichEndOfAnOutboundCallIsWhich`(两个方向各一例),
  并单独断言"外呼的 `to_number` 不得等于 `did`" —— 那正是让按被叫号检索失效的形态;
  摘除验证两条同时失败。
  **历史行不会自动更正**:此前所有 AI 外呼的 CDR 都是反的(本机 2026-08-23 18:04 那一行可查),
  要不要回填由 owner 定 —— 回填能从 `did` 与 `from_number` 无歧义地重建,
  但那是改历史,不在本次范围。
- **C44(new,2026-08-23 同上,未修)** **两条 `PARTY_CHANGED` 不带信封该带的上下文。**
  `coordinator.go:761-767`(`reason=CALL_MERGED`)与 `:900-905`(`isMuted`)发布时
  **没有 `UserData`**,后者连 `CallType` 也没有。而 `SseEvent` 的契约描述写着
  call 事件重复 `callType, userData` 是 *"for a screen-pop without further requests"*。
  现场可见:同一通电话的十条事件里,八条带着 `orderId`,这两条是 `{}`。
  今天前端不受影响(弹屏读的是查询缓存,事件只用于失效),但**契约在这两条上不成立**,
  而任何直接照信封渲染的消费者都会在这两条上看到上下文凭空消失。
  修法一行:与同文件其它 publish 一样带上 `CallType` 与 `UserData`。
  **【已修 2026-08-23,而且不止一行 —— 查下去发现更深的一处】**
  两条 publish 补上 `CallType` / `UserData` 之后回归用例**仍然失败**,因为
  **`merge()` 根本没有把 `UserData` 搬过去**(`coordinator.go` 的 merge:parties、
  队列事实、bot 份额都搬了,唯独业务数据没有)。两通电话合并时哪一通被保留,
  取决于交换机先announce 了哪条腿 —— 所以这份数据会**在一部分通话里丢失、另一部分里不丢,
  而没有人看得出规律**。本次现场那一通是碰巧数据在被保留的那一通上。
  已改为 `call.MergeUserData(movedUserData)` —— 这也让 `MergeUserData`
  **有了第一个生产调用者**(C42 记的"只有测试在调用"到此为止)。
  合并而非覆盖:被保留那通自己的数据不是别人的可以盖掉的。
- **C45(new,2026-08-23 owner 读事件流指出,已修)**
  **坐席自己拨出去的那条腿,被宣告了两次。**
  现场流里(点击拨号,callId `01a02e26-2c3b…`):
  ```
  13000009 PARTY_DIALING  payload={"fromNumber":"1008"}
  13000010 PARTY_RINGING  payload={"extensionNumber":"1008","fromNumber":"18688886669","toNumber":"1008"}
  ```
  **同一条腿、同一个 partyId、相隔 3 微秒**,而且第二条是**被叫视角**的措辞:
  把正在被拨的号码说成"在呼叫你"。
  这是今天上午补 `PARTY_DIALING`(W7③)**带出来的**:在那之前,坐席自己拨出的腿
  只有 `PARTY_RINGING` 这一条宣告,所以它兼任了"这条腿开始了";
  有了 DIALING 之后,RINGING 就成了同一次转移的第二次宣告。
  **已修**:`PARTY_RINGING` 只发给**被拨的腿**(`isAgentLeg && !isOriginator`)——
  发起腿说 DIALING,被叫腿说 RINGING,各说各的。
  ⚠ **`SetOnCall` 必须留下**:它原本在同一个分支里,把整段跳过会让坐席不再显示 ON_CALL。
  已挪到分支外,并有断言钉住(摘除 → "dropping the ringing event must not drop that with it")。
  同批改写一条既有用例:`TestARingingLegNamesTheExtensionNotTheContactToken` 原来造的是
  **孤零零一条坐席腿**(那在今天之后是发起腿),而它的主题其实是**被派单的腿** ——
  fixture 停在旧形态,与今天早上 tap 那次同型,已改成真实派单形状。

- **C46(new,2026-08-23 owner 读事件流指出,未修)**
  **一次点击拨号里,同一条腿的 `CALL_MERGED` 被宣告了两次,通话 id 变了又变。**
  ```
  13000015 PARTY_CHANGED callId=01a02e26-3102-…  partyId=…d46d  reason=CALL_MERGED
  13000016 PARTY_CHANGED callId=01a02e26-2c3b-…  partyId=…d46d  reason=CALL_MERGED
  ```
  **同一个 partyId、相隔 12 毫秒、两个不同的 callId**,而 `2c3b` 才是拨号方案铸的那个
  (随后的 `PARTY_RELEASED` / `CALL_CDR` 都在它名下)。也就是说坐席先被告知
  "你这通电话现在叫 3102",十二毫秒后又被告知"不,叫 2c3b"。
  这条事件存在的理由是**告诉持有旧 id 的人换号了**(见 `announceMerge` 的注释:
  转写面板会因为 id 过期而把后续的行全丢掉),所以**多余的那一次不是无害的噪音** ——
  它让面板按一个即将作废的 id 重新拉取,而正确的那次紧随其后。
  未查明的是**为什么会发生两次合并**:`join` 的保留规则(agent-only 让位、
  minted id 优先)看起来应该一步到位。需要读 `merge`/`join` 在点击拨号这条链上的实际时序,
  或加一条日志跑一次点击拨号即可定案 —— **不需要真实通话之外的东西**(本机自拨即可)。
  **【当日查明并修复】** 原因是 `join` 里保留哪一通的**优先级反了**:
  ```go
  case c.isAgentOnly(callID):            // ← 先问这个
  case c.isAgentOnly(otherID):
  case c.isMintedID(otherID) && !c.isMintedID(callID):
  ```
  "agent-only 的那通是临时的,让位" —— 这条规则对**队列派单**是对的
  (坐席那条腿确实自成一通临时通话);对**坐席自己拨出的电话正好相反**:
  它在桥接那一刻**只有坐席一条腿**,于是被判为临时的、让位给了对端,
  **而它才是拨号方案铸的那个身份**。随后 `reidentify` 发现通道上的 `aicc_call_id`
  与所属不符,又把它并回来 —— 那就是第二条 `CALL_MERGED`。
  **修法:minted 身份优先于 agent-only,先问。** 队列派单不受影响
  (那边主叫那通本来就是 minted,两种排序结论一致)。
  **修完的结果比"合并一次"还好:一条都不发** —— 被搬走的变成了远端那条**无坐席**的腿,
  而坐席自己的 party 从未换过通话,**没有什么要通知他**。
  回归 `TestACallTheAgentPlacedKeepsItsMintedIdentityInOneMove` 断言的正是"零条 id 变更"
  与"最终落在 minted 那通上";摘除验证(把 agent-only 放回前面)两条断言同时失败。
  **现场复验(2026-08-23 18:46,同一条点击拨号)** —— C44/C45/C46 三条一起看:
  ```
  PARTY_DIALING → AGENT_AVAILABILITY(ON_CALL) → PARTY_ESTABLISHED → PARTY_RELEASED → CALL_CDR
  ```
  与修前那十条相比:自拨腿的 `PARTY_RINGING` 没了、`PARTY_CHANGED` **一条都没有**、
  callId 全程是 minted 的那一个(修前是 `2c3b → 3102 → 2c3b`)、
  **每一条 call 事件的信封都带 `orderId`**(修前 8/10)。
  `AGENT_AVAILABILITY(ON_CALL)` 仍在 —— 去掉振铃事件没有把它一起带走。
  CDR:`OUTBOUND | from=1008 | to=18688886669 | user_data={"orderId":"9999000000000000"}`。

- **C47(new,2026-08-23 验 C43 时撞出,未修 —— 这是 W10 的具体危害)**
  **AI 外呼根本转不到人工:`X-AICC-Channel-ID` 指向的是一条已经消失的 loopback 腿。**
  ```
  18:52:33  could not stamp the caller's channel  variable=aicc_did
            error="uuid_setvar 5789e101-… aicc_did 95002: -ERR No such channel!"   ×6
  18:52:43  transfer failed
            error="uuid_transfer 5789e101-… 7002 XML aicc: -ERR No such channel!"
  ```
  交换机日志里 `5789e101-…` 是 **`loopback/18688886669-a`**。
  `AICC_OUTBOUND_ENDPOINT` 默认 `loopback/%s/aicc/XML`,`Originate` 把
  `origination_uuid` 钉在 loopback 的 **a 腿**上,而真正打给客户的是 **b 腿**经 pstn_sim 桥出去的那条;
  a 腿在真实通话建立之后就不在了。于是 `DialAI` 写进 `sip_h_X-AICC-Channel-ID` 的那个 id
  **从 bot 拿到它的那一刻起就已经是个死引用**。
  后果:**AI 外呼永远无法转人工**,也永远盖不上 bot 的份额
  (`aicc_bot_sec` / `aicc_bot_summary` / `aicc_flow_id` 六个变量全部写失败)。
  呼入不受影响 —— 那条链的 `X-AICC-Channel-ID` 由 `aicc_inbound.lua` 用
  `session:getVariable("uuid")` 取的是真通道。
  **这正是 W10(生产禁用 loopback)要防的事,而且它不只是"生产"的问题** ——
  开发环境同样跑不通 AI 外呼转人工。
  修法方向:外呼不要经 loopback(直接 `sofia/gateway/…` 或让 originate 落在真实腿上),
  或在 b 腿建立后把真实通道 id 回填给 bot。**须与 W10 一起决定,不要各修各的。**
- **C48(new,2026-08-23 同上,未修)**
  **转接失败的 AI 通话,账本里一行都没有。**
  上面那通:bot 接了、说了话、请求转接、转接失败、主叫挂断 —— **`cdrs` 里查无此行**。
  机制:`internal/aicall/ledger.go` 的 `finish` 在 `isTransferred` 为真时**直接 return**
  (注释写着"转接意味着人工路径拥有那唯一一行 CDR"),而转接**失败**时人工路径
  从来没有拿到这通电话,于是**两边都不写**。
  与 C38 是一对:C38 是"bot 写了、人工那行被丢弃",这一条是"bot 不写、人工也没有"。
  后果比 C38 重:C38 至少留下一行残缺的,这一条是**整通电话从账本上消失**,
  计费、报表、质检全都看不到它发生过。
  修法方向:`isTransferred` 不该是"我不写"的理由,而应是"我写一行**待人工补全**的",
  或者转接失败时把 `isTransferred` 撤回。C38 的 upsert(看得更远的行胜出)已经为前者铺好了路。

  **现场复验(2026-08-23 19:05,`01a02e4b-d4d7-70c3-a403-875ffb88bce1`,95002 → 18688886669,qwen)**
  —— loopback 默认值移除后(`06c0eae`)的第一通 AI 外呼,一次跑出四条结论:
  - **C47 已修。** 六条 `could not stamp` 与 `transfer failed` 全部消失;
    19:05:31 `transferring the caller`,1008 接起,`aicc_bot_summary` /
    `aicc_bot_reason` 落进了 `cdrs.user_data`。**AI 外呼能转人工了。**
  - **C48 不再复现,但机制未动。** 这通有 CDR 是因为转接**成功**了,人工路径拿到了它。
    `internal/aicall/ledger.go` 的 `isTransferred → return` 一行未改 ——
    **转接一旦再失败,这通电话仍会从账本上整通消失**。C48 保持未修。
  - **C43 只修了一半(已补全,`84777b2`)。** 这一行是**人工路径**写的,主被叫仍然反着:
    `from=18688886669 to=95002`。`internal/telephony/cdr.go` 有一份和 aicall 里
    一模一样的方向假设("有 DID 就是被拨的号"),而它**只在转接成功后才拥有那一行** ——
    在盖印和转接跑通之前,没有一通 AI 外呼走到过这里,所以它一直没被看见。
    判据与 aicall 那份相同:我们打出去的通话,DID 是**主叫**,而注册表里的"发起腿"是客户的。
- **C49(new,2026-08-23 同一通撞出,已修 `25fca24`)**
  **每一通 AI 外呼都按 0 秒计费。**
  ```
  19:05:40  WARN the ledger and the switch disagree on billable time
            billSec=0 switchBillSec=21
  ```
  `billedLeg` 的 OUTBOUND 分支只写了一种情形:坐席点击拨号 —— 坐席自己坐在发起腿上,
  面向运营商的是**被拨出的那条腿**,而坐席话机的自动应答**不能**算作通话开始计费
  (所以"没有一条拨出腿被接起"时返回 nil,是对的)。
  但这台平台自己发起的通话里**没有那样一条腿可找**:面向中继的**就是发起腿**,是我们创建的。
  越过它去找,结果是 `nil` → `bill_sec=0`,而交换机自己的 `bill_sec=21` 就写在那条腿上。
  值得记一笔的是:**这台机器早就在每一通这样的电话上报告过它和交换机的分歧**,
  只是没人读那条 WARN —— 它是自己把自己的缺陷讲出来的。
- **C50(new,2026-08-23 同一通,未修 —— 噪音,非数据)**
  **每一通转接过的电话都会报一次"bot 盖印的时长和它的桥接不一致"。**
  ```
  19:05:26  tool ran (transfer_to_agent)          ← 盖印写在这一刻:stampedSec=7
  19:05:31  transferring the caller               ← 桥接结束在这一刻:bridgedSec=12
  19:05:40  WARN the bot's stamped duration disagrees with its bridge
  ```
  两者相差的正是**告别语播完前的那几秒** —— 而"转接只在 PLAYBACK_DONE 之后执行"
  是本仓库的既定不变量,所以这个差**必然存在**,阈值 ±2s 必然被突破。
  数据是对的(代码本就取 `bridgedSec`),错的是这条 WARN:
  一条在每通正常电话上都响的告警,会把人训练成忽略它 —— 而它本来是用来抓真分歧的。
  修法方向二选一:转接**执行时**重新盖印一次(让两个数字真的相等),
  或让这条 WARN 认得"转接"这一种正常差异。**建议前者** —— 相等的数字比放宽的阈值可信。

  **现场复验(2026-08-23 20:01,`01a02e7e-f4e6-704e-9311-6c9fb1096d73`,同一条链路)**
  —— C43(第二份)/ C49 / C50 三条修复一并证实,且**整通电话零 WARN 零 ERROR**,
  这是本仓库第一通从头到尾干净的 AI 外呼:
  ```
  from_number=95002  to_number=18688886669     ← C43 人工路径方向已正
  bill_sec=32        switchBillSec=32          ← C49 与交换机分秒不差(此前 0 vs 21)
  bot_sec=12         (无 disagrees 告警)        ← C50 盖印与桥接一致
  user_data.orderId=9999000000000002           ← 业务数据随转接进了账本
  20:01:16 tool ran → 20:01:21 transferring    ← 中间 5 秒告别语,现已计入 bot 份额
  ```
  **C43 / C49 / C50 关闭。** C47 亦由 19:05 那通证实关闭;**C48 仍未修**
  —— 这两通都是转接**成功**的,而 C48 讲的是转接失败那条路。

  **C48 已修并现场复验(2026-08-23 20:21,`01a02e91-5b4f-76e1-a0f5-7b28c7d9a290`)。**
  修法取了条目里写的第一条路 —— **总是写一行,让看得更远的那行覆盖**(C38 的 upsert 已铺好)。
  判据比原条目更准:`markTransferred` 在 **bot 调用工具那一刻**置位,而交出主叫要等
  **告别语被听完**;主叫在告别语中途挂断,标记留着、转接**从未发生** ——
  这时连"转接失败"这个钩子都没有,所以"失败时撤回标记"那条路只能抓 ESL 报错,抓不住这一类。
  复现法即此:接起 → 说"转人工" → bot 一开口说告别语就挂断。
  ```
  status=ANSWERED  is_contained=f  queue_id=019ffaae-…(本要去的队列)
  bot_sec=9  bill_sec=9  total_sec=9  orderId=9999000000000005
  ```
  旧代码下这通电话**在 cdrs 里查无此行**。
  已知代价(接受):该行从转接那一刻起就存在,所以**通话进行中跑报表会把它算作一通短的 bot 通话**,
  直到人工路径覆盖它。已查:没有任何视图把"存在 CDR"当作"通话已结束"。
- **C51(new,2026-08-23 验 C48 时撞出,已修 `e04a8e2`)**
  **主叫在告别语中途挂断后,10 秒上限仍会对一条已死的通道执行转接。**
  ```
  20:21:22  tool ran (transfer_to_agent)      ← 主叫在这前后挂断
  20:21:32  WARN running an armed action at the cap; playback never finished
  20:21:32  transferring the caller
  20:21:32  WARN could not stamp the caller's channel
  20:21:32  WARN could not restore the caller's teardown rule
  20:21:32  ERROR transfer failed … No such channel!
  ```
  armed action 的上限是为"播放卡住"准备的,它**分不清卡住和主叫挂断**,
  而后者里它是唯一还在跑的东西。后果有两层:
  一是我们的 SIP 腿白占 10 秒;
  二更要命 —— 它报出来的正是**一小时前意味着"AI 外呼根本转不到人工"的那三条告警**(C47)。
  **一个没出事时也会响的告警,比没有告警更坏。**
  修法:通话结束(`drive` 返回)即作废 armed action。
  **代价(已权衡,接受)**:`drive` 返回不只意味着"主叫挂了",也意味着"会话因任何原因结束"。
  常见的 provider 掉线仍被覆盖 —— drain watch 会撑到已入队的告别语放完,
  `PLAYBACK_DONE` 先于 `drive` 返回触发 armed action。
  真正变掉的是**告别语还没生成完 provider 就死了**这一小片:
  此前上限会在 10 秒后把主叫补送进目标队列,现在不会,主叫改走 Lua 的兜底队列
  (bot 腿死亡且无 `aicc_bot_finished` 标记 → fallback)。这是设计好的降级路径,故接受。
  **现场复验(2026-08-23 20:30,`01a02e99-ba29-7cec-a376-321cf38c1418`,同一复现法)** ——
  20:30:32 工具执行、主叫随即挂断,10 秒上限于 20:30:42 静静过去:
  **整通电话的日志除 `tool ran` 一行外再无一字**,三条告警全部消失;
  同时 C48 的那行账仍在(`bot_sec=12`,队列归属完好,`is_contained=f`)——**两条修复互不干扰。**
- **C52(new,2026-08-24 闭合 C24 时数幽灵数出来的,已修 `77e5a85`)**
  **没人接的内部呼叫记不住拨给了谁 —— 主叫被写进了被叫栏。**
  C24 的检测口径里第三项 `from_number = to_number` 在 context 落地后仍有 3 行,
  全是 ben 从话机拨给 wei 的通话,账本写作 `INTERNAL | NO_ANSWER | 1002 → 1002`,
  而 legs 里明明躺着 `{AGENT, "1008", did not answer}`。
  病因:`addParty` 对每条坐席腿一律 `p.OtherNumber = ev.ANI`。
  ANI 在**交换机派给坐席的腿**上确实是对方(队列派单、click-to-dial 的坐席腿),
  在**坐席自己话机拨出的腿**上就是坐席自己。
  **它潜伏了十一天,是因为桥接一建立就会把这个字段覆盖掉** ——
  `registry.go:372` 在 `CHANNEL_BRIDGE` 上双向重写 OtherNumber。
  于是只有**从未接通**的通话会把错答案留到落账,而那恰恰是**唯一没有别处能说明"拨给了谁"**的一通:
  接通的通话有 legs、有 bridge,未接的通话只有这一列。
  同一个字段也是坐席工作台"正在通话/正在呼叫"那一行的号码来源
  (`active-call.tsx:48`、`softphone-bar.tsx:231`、`_app.agent.index.tsx:167`,
  均为 `other?.number ?? mine?.otherNumber`),所以坐席从话机拨出后、**在对方那条腿出现之前
  的那个窗口里**,屏幕上显示的是他自己的号码。对方腿一加入(`aicc_parent_channel`,几乎是
  紧接着),`other.number` 就接管了显示 —— 所以这一条不能靠肉眼复现,别照字面去试。
  **修法**:`otherNumber(ev)` 做成 `legNumber(ev)` 的镜像 —— 腿拿了哪个号做自己的,
  另一个就属于它面对的那一方(inbound 取 `DestinationNumber`,outbound 取 `ANI`)。
  **给后来人的一条提醒(已写进注释)**:click-to-dial 走 outbound 分支之所以一直是对的,
  只是因为 `Dial()` 把**目的号**塞进了坐席腿的 `origination_caller_id_number`
  (为了让坐席话机显示"正在拨给谁")。**哪天把这个显示技巧拿掉,这里会无声地退回坐席自己的号。**
  回归测试 `TestALegKnowsWhoItFacesBeforeAnythingHasBridged` 三型同钉(话机拨出 / 队列派单 /
  click-to-dial),已验证旧代码下第一型 FAIL、后两型 PASS(即不是把另两型改坏换来的)。
  **现场复验(2026-08-24 07:47,`01a03105-f782-769f-a447-aac670ce50b3`)**:
  ben 1002 从软电话拨 1008,响 29 秒无人接 → `INTERNAL | NO_ANSWER | 1002 → 1008`,1 行。
  三行历史脏数据不回改,并入 C24 末节那个待定的清理问题。
- **C53(new,2026-08-24 查 C24 数据时顺手看见,未修)**
  **AI 外呼没接通时,写下的是一行不知道打给了谁的账。**
  aicc context 落地后的 88 行里有 3 行
  `OUTBOUND | NO_ANSWER | from=18688886669 | to_number='' | did='' | total_sec=0`。
  被叫号与 DID 双空,`total_sec=0` —— 从账本看不出这通电话打给了谁、从哪个号打出去的。
  这是 **C31 的同一个病换到外呼侧**:C31 是被拒来电记不下对方拨的哪个号,本条是外呼记不下拨给了谁。
  两者的后果也同型 —— "外呼系统在空转"和"某个号码一直打不通"在账本里长得一模一样。
  注意与 C47/C50 区分:那两条是**接通之后**的账错,本条是**根本没接通**时账里没有号。
  怀疑方向:`AIDial` 的 originate 失败/无人应答时,主叫腿从未带上 `aicc_did`,
  而 `assemble` 的 `cdr.DID == ""` 分支取 `originator.OtherNumber`,那条腿也没有对端。
  修法应与 C31 合并考虑:**号从请求里来,不从腿上来** —— 置呼时就知道 To 与 DID,不必等交换机说。
  **【已修 · 2026-08-24 `0f4c388`,与 C31 同一次修复,留证 `artifacts/C53/verdict-2026-08-24.md`】**
  **怀疑方向是对的**:`aicc_did` 原先只在**桥接到 bot 时**才打到通道上,
  而没人接的呼叫根本走不到那次桥接。改为**置呼那一刻就打在客户腿上**。
  这同时让未接通的外呼拿到"本平台自己置的呼"该有的方向(从 DID 到客户)——
  走的是 **C43 已经修好的那个分支**,不是新逻辑:那个分支要求 `cdr.DID != ""`,而 DID 正是此前缺的。
  **`aicc_did` 打在客户腿上不会让它被当成 bot 腿**:`isBotLeg` 问的是"这条腿是不是**拨向** DID 的",
  这条拨向的是客户。已由 `TestTheCustomerLegOfAnAIOutboundIsNotMistakenForTheBots` 钉住 ——
  否则账本会把一通 bot 从未接手的呼叫交给 bot 那个写入方。
  **现场**(`01a03161-b16f-79a4-826d-f7362c980bbb`,owner 不接、响到超时):
  `OUTBOUND | from=95002 | to=18688886669 | did=95002 | NO_ANSWER | NO_USER_RESPONSE`。
  此前同一动作产出的是 `from=18688886669 | to=(空) | did=(空)` —— 主叫栏坐着客户、被叫栏空着,方向也反。
- **C54(new,2026-08-24 A 通复验时确认,未修 —— 噪音,非数据)**
  **计费比对告警在每一通未接的 click-to-dial 上都必然响,而账本是对的。**
  A 通(`01a03104-0c32`,无人接)照例报
  `WARN the ledger and the switch disagree on billable time billSec=0 switchBillSec=30`。
  这次**账本没有错**:没人接就不该计费。交换机那 30 秒是**坐席自己那条 auto-answer 的腿**
  ——click-to-dial 先响坐席、`sip_auto_answer=true`,所以 originator 腿一定被应答、一定被计费。
  `cdr.go:354` 拿 `cdr.BillSec` 与 `originator.BilledSec` 比,分不清"我们算错了"和
  "坐席腿自动应答了但没人来"。
  **对照**:B 通(话机拨出、未接)**不报** —— 那条 originator 腿从未应答,`BilledSec=0`,比对相等。
  即误报面恰好等于"未接的 click-to-dial",一个天天发生的场景。
  **这已经是本轮第三次遇到"没出事也报警"**(C50 的 stamp 漂移告警 → 盖住了 C49 两天 →
  C51 的 armed-action 告警与 C47 的灾难同文)。**C49 当初就是被这类噪声盖住没人读的**,
  而现在盖它的换成了这一条。
  ~~修法很轻:通话从未接通(`cdr.AnsweredAt` 为零 / `status != ANSWERED`)就不做这个比对。~~
  **【已修 · 2026-08-24 `6fb0d0a`,留证 `artifacts/C54/verdict-2026-08-24.md`】**
  **立案时那个修法是错的,不该照着做。** 它按"通话有没有接通"加静音条件,
  而真正的病是**比错了对象**:拿**我们的数**(来自 `billedLeg` 选中的腿)去比
  **交换机对 originator 腿的数**。两者在呼入、以及本平台自己置的呼上是同一条腿,
  **坐席外拨时是两条不同的腿** —— 坐席话机 auto-answer,交换机对他那条腿从头计费,
  而我们计费的是面向运营商的那条:没人接就是 0。
  **修法**:比**同一条腿**(`billed != nil && billed.BilledSec > 0`)。
  **这不是把告警关小,恰恰相反** —— AI 外呼上被计费的腿**就是** originator,
  C49 那一类仍在监视之下;彻底没人计费的场合(两部分机通话)才没有可核对的主张,整块跳过。
  照立案时的写法反而会连 C49 一起静音掉,因为 C49 的现场也是"从未接通"之外的一型。
  **现场对照(同一动作、同样无人接)**:
  修前 `01a03104-0c32` 报 `billSec=0 switchBillSec=30`;
  修后 `01a03154-9fff` 整个日志文件里 `disagree on billable time` **0 次**,
  而账行不变仍正确(`NO_ANSWER / total=30 / talk=0 / bill=0`)。
  **可见变化**:内线与未接外拨的 `tech.switchBillSec` **不再出现** ——
  那是拿一条没人计费的腿做比对,消失是对的,不是数据缺失。
  `ledger.yaml` 唯一断言该键的是 VC-S14-04 的**外线一型**,不受影响。
  **测试** `TestTheBillingAlarmRingsForTheLegItActuallyBilled` 三型
  (未接外拨静默 / 内线无该键且静默 / C49 所在的腿仍被监视),已验证改回比 originator 后 ①② FAIL。

- **C55(new,2026-08-24 owner 读实时流发现,已修)**
  **`PARTY_ESTABLISHED` 在没有人接听的通话上照样发出 —— 坐席被告知"正在通话",而对方还在响铃。**
  owner 现场证据(`01a0310d-5613-737e-9d0c-750b093ae993`,wei 1008 拨 ben 1002,ben 全程未接):
  ```json
  {"type":"PARTY_ESTABLISHED","occurredAt":"2026-08-23T23:55:53.913745Z",
   "agentId":"807b2164-…(wei)","payload":{"role":"ORIGINATOR","state":"TALKING"}}
  ```
  通话 23:55:52 建立、23:55:53 就发出 ESTABLISHED,而账本这一行是
  `NO_ANSWER / talk_sec=0 / answered_at=23:55:53`。
  **原因**:click-to-dial 先响坐席、`sip_auto_answer=true`,坐席自己那条腿一秒内自动应答;
  而 `registry.go` 把 `CHANNEL_ANSWER` 直接接到 `TriggerAnswer → TALKING`。
  于是坐席工作台整整三十秒显示"正在通话",直到响铃超时。
  **【owner 直裁 2026-08-24】**
  ① "established 只有在双方都接听的情况才会产生";
  ② "`PARTY_ESTABLISHED` 大部分基于 freeswitch bridged esl 事件";
  ③ "dialing/ringing/established/held/released 这些 party 事件要基于 FSM"。
  **讽刺的是这份代码早就知道**:`registry.go` 的 `KindChannelBridge` 分支写着
  "a bridge tells us who is talking to whom … it is the only moment that says a conversation
  actually started — answering does not, since a phone can answer with nobody in front of it";
  `cdr.go` 也写着"whether a *person* was reached is a question the bridge answers and the
  answer does not"。**账本一直按桥接算 `talk_sec` 与 `status`,只有 party 的状态没照做。**
  **修法**:`CHANNEL_ANSWER` 只记 `party.AnsweredAt`(计费事实),不再迁移状态、不再发事件;
  新增 `establish()` 在 `CHANNEL_BRIDGE` 上把**两条腿**一起送进 TALKING。
  仍然走 `transition → apply → partyTransitions`,**转换表依旧是唯一能移动 party 的东西**
  (owner 的 ③);`establish` 只放行 DIALING/RINGING 两个来源,
  已 TALKING 的重复桥接静默(转接、re-invite),HELD 仍只能由 RETRIEVE 回来。
  **`answered_at`/`bill_sec` 刻意不动** —— 那问的是"这条腿何时应答",是承运商计费的问题,
  与"有没有人在对面"是两件事,C49 刚把它修对,不能被这次改动带偏。
  **顺带修掉一处绕过 FSM 的赋值**:`waiting.go` 收养重启期间排队的主叫时直接
  `party.State = PartyTalking`。排队等待的主叫并没有在跟谁通话,现在只恢复 `AnsweredAt`,
  状态留在 `AddParty` 给的 DIALING,等真正桥接到坐席时按 FSM 迁移。
  **事件的条数没变,时刻变了**:接通的通话仍然两条 `PARTY_ESTABLISHED`,只是都在桥接那一刻;
  唯一少掉事件的是**从未接通的通话**,那正是缺陷本身。载荷与事件名未动,不涉及契约破坏。
  **测试**:`TestAnAgentIsNotToldTheyAreTalkingUntilSomebodyIsThere` 完整复现 owner 那一通
  (自动应答 → 对方响铃 → 零 ESTABLISHED 且不 TALKING 但 `AnsweredAt` 已记 → 桥接后恰好两条),
  已验证旧代码下 FAIL。另有三处夹具按真实时序订正:
  `answerAndEndAnAgentLeg` 原先"先桥接、再选择性应答"—— 交换机不会桥接一条没人接的腿,
  未接那一型现在两个事件都不发(它同时暴露了 after-call-work 依赖的正是"有没有进过对话",
  按新语义反而更准:自动应答却没接通的腿不该产生话后处理)。
  **设计已同步修订**:01 §Party FSM、04 §SSE 事件表、08 §16/§17 三处原文都写着 answer-driven,
  已按 m4-findings 的惯例就地改写并回指本条。
  **现场闭合(2026-08-24,两通对照,留证 `artifacts/C55/verdict-2026-08-24.md`)**:
  未接那通(`01a03124-6b2a`)wei 的流只有 `PARTY_DIALING → PARTY_RELEASED`,**零条 ESTABLISHED**,
  30 秒状态未离开 DIALING;接通那通(`01a0312f-64c6`)`answeredAt=00:33:05.9`(坐席腿自动应答)
  与 `PARTY_ESTABLISHED=00:33:09.8`(桥接)**相差 4 秒,正是被叫响铃的时间** —— 改动前两者同刻。
  CDR `ANSWERED/ring=3/talk=11/total=16`。wei 流上只 1 条 ESTABLISHED 而非 2,符合"腿的事件
  只发给它自己的坐席";ben 那条在 ben 的流上,单测断言 2 条是因测试 publisher 不做范围过滤。
  **残留(未修,待定)**:接通那行的 `answered_at` 仍记坐席腿自动应答的时刻,早 4 秒。
  `cdr.go` 该分支的理由是"内线没人计费,但通话确实接通了,行里该说何时" —— 没有计费理由,
  按本条规则应记桥接时刻;计费分支有理由,不动。
  **另记(未修)**:两通的 `PARTY_RELEASED` 都带 `isTransferredAway: true` 而都没转接过 ——
  click-to-dial 用 `uuid_transfer` 送坐席腿进拨号方案,交换机据此打了标记。
  工作台若拿它区分"电话转走了"和"通话结束",会判错。

- **C56(new,2026-08-24 owner 给出 party FSM 规格后立案,未修)**
  **实现的转移表与规格状态图之间有四处差;当日全部裁定 —— 三处"实现对、图错",一处形状差。本条已闭合。**
  owner 2026-08-24 给出 party FSM 的 mermaid 状态图并要求"记录在文档中",
  已按原样落进 `docs/design/01-telephony.md` §2,并声明**该图即规格**,
  `internal/telephony/call.go` 的 `partyTransitions` 是它的实现、且是**唯一能移动 party 的东西**
  (转移表的注释与 `fsm-edges.md` 表头都已回指)。差异如下:
  | 规格 | 实现 | 判断 |
  |---|---|---|
  | `Idle` 既是起点也是终点 | 无 `Idle`;party 就是一条 channel,出生即 `DIALING`/`RINGING`,终于 `RELEASED`(终态) | **形状差,非行为差** —— `RELEASED` 就是图里那个终止的 `Idle` |
  | ~~`Queued`~~ **已从图中删除** | 没有 | **2026-08-24 已裁定**:owner 复看本节时提出,**它根本不是 party 的状态**。party 就是一条 channel,排队等待的主叫是一条**开着、交换机在放音乐**的 channel —— 腿本身什么都没变。**电话在哪**是呼叫的事实,系统已经记在那里了(`call.Queue` 的 name/joinedAt/bridgedAt、`queue_events` 表、CDR 的 `queue_wait_sec`);在 party 上再存一份只可能与它们不一致。它也**没有生产者**:排队是 mod_callcenter 的事,经 `callcenter::info member-queue-start/-end` 观察,没有任何 `CHANNEL_*` 说得出"已排队"。而且**两条腿哪条都对不上这个形状**:`Queued → Ringing` 需要一条先等待、后响铃的腿,可排队的主叫和派给坐席的那条腿是**两条不同的 channel**,后者**生下来就是 RINGING**,呼叫等待期间它根本不存在。**代价只有措辞**:听着保持音的主叫显示为 `Dialing` 确实别扭 —— 那是既有状态的**命名**问题,不构成新增状态的理由;真要解决也是改名,不是加一个。**实现对、图错,代码一行未动。** |
  | ~~`Dialing → Ringing`~~ **已从图中删除** | **禁止** | **2026-08-24 已裁定**:owner "Dialing → Ringing 是必须禁止的"。首版图带着这条边,与 `call.go` 的 deliberately absent 正面冲突;裁定的结果是**实现对、图错**,图已修订,代码一行未动。理由也一并写进两处:一条腿不会中途换角色 —— DIALING 是发起方,RINGING 是被叫方,这条边意味着一条腿变成了另一个人;主叫听到的回铃属于**对方**那条腿的 RINGING |
  | ~~`EventAbandoned`~~ **已从图中删除** | 没有对应 trigger | **2026-08-24 已裁定**:owner"用 EventReleased 替换 EventAbandoned,都是挂断"。放弃就是一次挂断,cause 已经说明是哪一种 —— `cdrs.missed_reason` 今天就在读它,区分 `ABANDONED_WAITING`/`ABANDONED_RINGING`/`SHORT_ABANDONED`。独立 trigger 买不到新信息,只多一条转移表与规格图都要同步的边。**实现对、图错,代码一行未动。** `EventQueued` 与 `EventDestinationBusy` 随 `Queued` 一并退出 |
  **【已闭合 · 2026-08-24,零代码改动】** 四行全部裁定完毕,**没有一行需要改实现**:
  三行是图错(`Dialing→Ringing`、`Queued`、`EventAbandoned`),一行是形状差
  (`IDLE` —— `RELEASED` 就是图里终止的 `Idle`)。
  **契约一字未动**:退出的三样都不是 `PartyState`。
  原先写的"整条按一次设计变更处理"作废 —— 它最后是一次**规格订正**,不是一次实现变更。
  这条的价值也就在这里:**规格图第一版有三处与实现不符,而每一次都是图错**,
  说明转移表是被现场逼出来的,而图是事后画的。

- **C57(new,2026-08-24 我自己踩进去才发现,未查 —— owner:立案,以后再查)**
  **CDR 的几个时长字段不是对通话时长的划分,却长得像可以相减。**
  起因是我拿 `total_sec - bot_sec - queue_wait_sec - ring_sec - agent_sec > 20` 去筛"坐席腿被异常拆掉"
  的通话,筛出四条,全部是**正常电话**(两条是坐席按了转接、两条的坐席腿本就在通话末尾)。
  详见 `artifacts/C37/investigation-2026-08-24.md` 末节的撤回。
  **但那次误判暴露的是字段本身的问题,不只是我的查询**:
  ```
  call          type     status     total  bot  qwait  ring  talk   四段之和
  01a02729      INBOUND  NO_ANSWER    220   13    207   207     0      427
  01a0272c      INBOUND  NO_ANSWER    158   13    145   145     0      303
  01a01eae      INBOUND  ANSWERED     186    0    164     2   156      322
  01a022da      INBOUND  NO_ANSWER    142   12    121   121     0      254
  01a01ea6      INBOUND  ANSWERED     139    0    120    57    63      240
  ```
  **2026-08-19 以来 320 行中有 48 行"四段之和 > total_sec"** —— 最极端的近两倍。
  另有 52 行 `INBOUND` 而 `bot_sec=0`(每通来电都先见 bot),17 行有 `queue_id` 而 `queue_wait_sec=0`。
  **看得出的重叠**:`queue_wait_sec` 与 `ring_sec` 在未接通话上几乎相等(207/207、145/145、121/121)——
  它们量的是**同一段时间的两种说法**(等待 vs 振铃),不是先后两段。
  **要查清的是口径,不是数字**:这几个字段是按"**事件发生了就写**"填的,还是按"**这段时间归谁**"填的?
  前者不构成划分,不能相减;后者才能。`bot_sec + queue_wait_sec + ring_sec + talk_sec` 与 `total_sec`
  之间**是否存在过一个应当成立的恒等式**,需要从设计与实现两侧确认 ——
  **如果从来没人这样设计过,那么这个减法从第一天起就不成立**,而不只是在我筛的那几通上不成立。
  **影响面**:`docs/design/06` 的容量口径、报表、以及任何"用时长字段做减法找异常"的分析。
  **在查清之前,任何基于它们相减得出的"异常"都不可信** —— 包括我 2026-08-24 发给 web-sip-phone 的那份
  (已撤回)。
  **附带的方法论,值得单独记**:那次误判里我**从头到尾没查 `audit_logs`**,
  而坐席按了什么(answer / transfer / hold / hangup)逐条记在那里,四条中两条的答案就在其中。
  **先看系统自己记下了什么,再去推断。**

### 排序总则
-1. **W11(管理面闭环)优先于 W7 / W8 / W10(owner 直裁 2026-08-24)** —— 前者是"能不能用",
   后三者是"更完整"。它与验证执行无交集(39 条已全部跑完),不受总则 2 约束。
   **内部顺序由依赖锁死**:W11.1 改名 → W11.2 分配器 → W11.3 Users → W11.4 Extensions →
   W11.5 Numbers → W11.6 bots 分机列;**W8(trunk)排在 W11.5 之后**,它们动的是同一块地。
0. ~~追检①已确认阶段 3/4 可开跑(stale tier 惰性;agent-wei Available/Ready)。~~ **已作废**:两阶段均已跑完。
1. ~~T0 先于一切;阶段 2→3→4→5 顺序固定(负路径与重启放后)。~~ **已履行**:阶段 0–5 全部按序完成。
2. **验证先行于实现** —— 原 28 条的留证已完成:阶段 2(S1-02/S8-01)与 T4.1(S5-01)均已在
   **W1/W2 合入前**按旧行为执行,"账本钉住旧缺口 → 实现 → T6.9 重跑证明修复"的证据链已成立。
   **⚠ 更正(2026-08-22):本条 2026-08-22 一度写作"W1/W2 可以开工",那是只看了原 28 条得出的,错。**
   同一条规则也管阶段 6 新并入的 11 条,而其中一条**已经在钉 W2 之前的行为**:
   **VC-S13-04** 的 expect 写着"`ABANDONED_RINGING` 目前**不可达**(条件互斥,归 W2),
   故实测该项应只由 SHORT_ABANDONED 与 ABANDONED_WAITING 贡献" —— 这是一条**留证条款**,
   与 S1-02/S8-01/S5-01 同型。**W2 先落地,这条就永远拿不到旧行为的证据。**
   故:**W1 可以开工**(11 条中无一条断言转写内容);**W2 必须等 VC-S13-04 执行完毕**;
   W7 不阻塞(它加的是新事件类型,VC-S13-05 断言的 DEVICE_* 是既有的),但先跑 VC-S13-05
   能给 T6.10 一个干净的前后对照。W3/W4/W5/W6/W8 与验证执行无交集,可并行。
3. C1:依赖 tier 正确性的 case 已全部跑完,**修复不再被阻塞**。但注意 **C1 的症状已潜伏** ——
   修后重跑 VC-S3-02 前须先按 C1 条目里的办法重造复现场景,否则会得到**假通过**。
   **W9(账号清理)**:原写"硬性排在阶段 3–6 之后",后改为"依赖 ben 的用例**执行完毕**"。
   **【2026-08-24 复核:这道闸已经解除,但**又长出一条新的**】**
   闸的原文条件已满足:账本里依赖 ben / 1002 的**八条全部 PASS**
   (S5-01、S7-02、S12-04、S13-01、S13-02、S13-03、S14-02、S14-04);
   S13-01 与 S13-02 的判词都确认是**用 ben 真跑完的**(2026-08-22)。
   **⚠ 但删掉 ben 会失去一个当天刚被证明有用的东西:原生软电话对照组。**
   2026-08-24 查 C37 时,决定性的一条证据是 **7:0** —— 七次派单风暴全部在 wei 的**浏览器话机**上,
   ben 的**原生软电话**一次都没有。没有 1002,那七次只能得出"派单会风暴",
   得不出"问题在浏览器话机那一侧",而后者才是把调查引向 web-sip-phone 的关键一步。
   同样的对照在 C24/C52/C55 的现场验证里也用到了(两部话机分别发起、分别抓流)。
   **所以 W9 开工前需要 owner 再定一件事**:1002 这个**第二部、且是不同实现的话机**要不要保留。
   保留它不等于保留 ben 这个账号 —— 可以把分机 1002 挂到 `agent` 名下,
   账号花名册照清、对照能力照留。**这是取舍,不是阻塞。**
4. 每 case 执行后:evidence 落 `docs/verification/artifacts/<ID>/`,status 更新单独 commit
   (账本修订与执行结果不混提)。
