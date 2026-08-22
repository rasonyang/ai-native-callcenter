# 验证与跟进任务清单(正式版 v1,released 2026-08-20)

> 前身:TASKS-draft.md(草案),经 owner 两轮决策(DECISIONS-pending.md,2026-08-20)后转正。
> 输入:docs/verification/ledger.yaml(28 case:4 PASS / 1 FAIL / 23 TODO)+ ledger-audit.md(逐 case 审计,
> 含 §0.1 追检)+ coverage/*。基线:HEAD a6ff7b9。
> **D1–D7 全部已决**;实现任务在阶段 7 的 **W 系列**(W1–W9)。唯一残留决策:settings 死表处置。
>
> **当前状态(2026-08-22)**:账本 **39 case —— 25 PASS / 3 FAIL / 11 TODO**。
> 原 28 条已全部执行完毕(25 PASS / 3 FAIL:VC-S3-02→C1、VC-S9-01→C14、VC-S12-01→C26);
> 阶段 6 起草的 11 条已于同日并入,均为 **TODO,尚未执行**。
> 阶段 0–6 已完成。阶段 7:**W 系列(W1–W9)未开工**;
> **C 系列已修 12 项、余 15 项** —— C1 / C2 / C4 / C7 / C10(已决 defer 第二期)/ C14 / C23 / C24 / C26 / C27 / C28 / C29 / C30 / C31 / C32。
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
| C6 | 流程(flow)管理 | 现 CLI-only(flowadd) | — | **已决 D3:要做** → **W3**(`/admin/bots`,参考 ui-test) |
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
| F4 | ~~app 重启窗内 member-queue-start 是否丢失~~ | VC-S12-01 | **已闭(2026-08-21,结论是"本题不适用")**:主叫**从未进入队列** —— `hangup_after_bridge` 让主叫腿在 bot 腿死后 50ms 内被跟随挂断,Lua 的兜底块没机会执行(→ **C26**)。原"两侧皆空=重跑"的判定辅助**作废** |
| F5 | 队列停用后模型是否**主动**提出留言 | VC-S3-04 | **部分闭(2026-08-21)**:留言全链已证(callbacks 建行 → claim → complete 全通),但**本题未被压到** —— 按落实方式由人工直接说了 leave a message,模型是否**主动**提出仍无证据。要答此题需另造一次**不给提示**的执行 |
| F6 | ~~今日 95002 bot_sec=0 行的成因~~ | VC-S3-03 前置 | **已闭(2026-08-20,T1.2)——不是 lua fallback,是 S3-03 猎的真漂移在野实证**:call 01a01d3b-6e49-…(11:33)bot 全程服务(bridged/conversation started/handoff fired/"transferring the caller"),CDR 却 bot_sec=0、无 flow、无 BOT leg;stampChannel 失败会 Warn(actions.go:236)而日志无 Warn → 漂移点在 facts==nil 静默跳过(actions.go:83-86)或挂断读回(switchevent.go:81)之间——已在 VC-S3-03 加通话中 uuid_getvar 探针定位,执行时优先复现 |
| F7 | ~~队列表单是否发全量~~ | G-C4 | **已闭**:`setEditing(row)` 整行拷贝(_app.admin.routing.tsx:99/:119) |
| F8 | ~~DID is_enabled=false 是否真拒接~~ | G-C3 | **已闭**:视图层 `WHERE d.is_enabled`(pg_get_viewdef 实查) |
| F9 | ~~goose_db_version 实表~~ | 覆盖表 | **已闭(2026-08-20,T1.1)**:实查 version_id 13/12/11 均 is_applied=t;tables.md 该行升 [FACT] |
| F10 | 坐席取他人 recordingId 的拒绝路径 | G-A4/T6.1 | **半闭(2026-08-22)**:代码侧已读并写进 VC-S13-02 的 expect(`recording_handlers.go:46-50/:55-61/:91`,主管由 `:33` 的 `Role.AtLeast` 早退);**wei+ben 实测未做**,随 VC-S13-02 执行时落实 |
| F11 | 非 seed 账号(chen/uiagent/liveagent 等 12 个)口令与归属 | 环境卫生 | 问 owner;留证即可,不阻塞 |

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
- **W3 flows 管理面**(D3):**路由 `/admin/bots`**;交互参考 ui-test(admin/bots/index.tsx 列表 +
  $flowId.tsx 详情:spec JSON 查看器、节点可达性分析、transitions 摘要、publish 对话框);
  设计规范 web/CLAUDE.md。**范围含 UI 上传/编辑 spec**(§补充 S1)。顺序:openapi 契约
  (GET /flows、GET /flows/{id} 含 revisions、POST /flows 新建、PUT /flows/{id} 草稿编辑、
  POST /flows/{id}/publish;装载期校验 internal/flow/load.go 即编辑时校验)→ make api-generate →
  handlers(flows/flow_revisions 已有 store 层,sql/flows.sql)→ UI。CLI flowadd 保留。ADMIN guard。
- **W4 audit_logs 检索**(D5):**路由 `/admin/audit`**;参考 ui-test admin/audit.tsx(分类过滤由
  action 前缀派生)。顺序:openapi 契约(GET /audit-logs:分页 + action 前缀/操作者/时间过滤)→
  generate → handler(读侧 sqlc 新查询)→ UI。ADMIN guard。关闭 tables.md "只写不读"缺口。
- **W5 D4 记录**:在设计文档(docs/phase1-decisions.md 或 design 附录)记一行"dispositions 固定词表
  为产品决策(2026-08-20)";S4-03 断言转正式。
- **W6 D1 记录**:质检评审 UI defer 下一期——tables.md/quality_reviews 行注记决议日期。
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
  另见 **C28②**:`DEVICE_*` 那一对不是"没实现",是**发错了一个**,W7 要做的是改对而非补上。
  ① CALL_RECORDING_STARTED/STOPPED——RECORD_START/STOP 已归一化(switchevent.go:259-264),
  补 coordinator→Hub 一跳(scope 沿用 call 域);
  ② DEVICE_REGISTERED/UNREGISTERED——信号已达 ObserveDevice(main.go:399-405),补区分发布;
  ③ PARTY_DIALING(addParty 时对 originator 腿宣告)、CALL_USER_DATA(userData 独立变更事件)、
  SYSTEM_LINK(挂 esl.Link 断连/重连,S12 语义)——全新 publish 点;
  ④ BOT_SESSION_STARTED/INTERRUPTED/ENDED——需给 aicall 引入 Hub 依赖(现无 Publish 调用,
  events.md 实证),**W7 内单独架构评审**(经 orchestrator 回调转发可避免直接依赖)。
  合入后:events.md 十行缺口关闭 + T6.10 补最小断言。
- **W8 trunk(中继号)管理**(§补充 S3):trunks 表(00002:137)从死表转正——契约评审先行
  (trunk 与 FS gateway/luacc 视图的关系需要一次设计过目,direction 枚举 3 值现全死),
  然后 openapi → generate → handlers → admin UI。settings 表处置仍待决(唯一残留)。
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
- **C2** SYS-6 契约缺口:PartySnapshot 补 isBotLeg(`make api-breaking` 走查)
- **C4** 死状态删除(D7①③ 已决):删 Call.ENDING(call.go:23,契约 CallState 同步)与转写 ENDED
  (actor.go:110,契约 TranscriptionState 同步)——两处均 breaking,走 `make api-breaking`;
  fsm-edges.md/enums.md 对应行、VC-S9-02 expect 括注随之更新
- **C7** queue_events 读路径 or 修正 cdr.go:112 注释
- **C10** 202 契约核对(SYS-2 源头)——**defer 第二期(§补充 S5,第二期需要)**;本期账本 expect 维持 202
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
- **C14(new,2026-08-20 T3.4 执行发现,FAIL 立案)** ASR tap 摄取路径丢帧:一通 ~23s 的转写
  HUMAN_AGENT 丢 46/1146(4.0%)、CUSTOMER 丢 78/1084(7.2%)("transcribe: audio was dropped",
  pump.go:226),识别文本随之崩坏(fox 句 → "Butro focus jobs owing the lazy workin")。pump 计数器
  证明是"没送到"而非"听错"。候选:pump 背压/缓冲、双流并发写。修复后重跑 VC-S9-01。
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
  后继路由),不再归 T6.11;**C24 仍未修**。
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
- **C28(new,2026-08-22 VC-S13-05 执行发现,未修)** **话机没了,交换机不知道;而发出去的事件说的是反话。**
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
- **C32(new,2026-08-22 VC-S14-04 执行发现,FAIL 立案,未修)** **从浏览器话机发起的 click-to-dial
  拨不出去 —— 被叫从未响铃。**
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

### 排序总则
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
   **W9(账号清理)**:原写"硬性排在阶段 3–6 之后"。阶段 6 现已起草完毕,但 ⚠ **W9 仍不得开工** ——
   已起草的 **VC-S13-01**(ben 只配员 support-zh 的过滤)与 **VC-S13-02**(以 ben 的录音
   `01a02276-2a62-…` 作越权靶子)都以 ben 为执行物料,而它们**尚未执行**。
   W9 的真正前置是"依赖 ben 的用例**执行完毕**",不是"起草完毕"。
4. 每 case 执行后:evidence 落 `docs/verification/artifacts/<ID>/`,status 更新单独 commit
   (账本修订与执行结果不混提)。
