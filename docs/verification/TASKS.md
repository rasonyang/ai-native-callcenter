# 验证与跟进任务清单(正式版 v1,released 2026-08-20)

> 前身:TASKS-draft.md(草案),经 owner 两轮决策(DECISIONS-pending.md,2026-08-20)后转正。
> 输入:docs/verification/ledger.yaml(28 case:4 PASS / 1 FAIL / 23 TODO)+ ledger-audit.md(逐 case 审计,
> 含 §0.1 追检)+ coverage/*。基线:HEAD a6ff7b9。
> **D1–D7 全部已决**;实现任务在阶段 7 的 **W 系列**(W1–W9)。唯一残留决策:settings 死表处置。
>
> **当前状态(2026-08-22 过期标记扫描,HEAD 8321675)**:账本 28 case 已全部执行完毕 ——
> **25 PASS / 3 FAIL / 0 TODO**;3 个 FAIL 是 VC-S3-02(→ C1)、VC-S9-01(→ C14)、VC-S12-01(→ C26)。
> 阶段 0–5 已完成,阶段 6 已起草待批准。阶段 7:**W 系列(W1–W9)未开工**;
> **C 系列已修 12 项、余 9 项** —— C1 / C2 / C4 / C7 / C10(已决 defer 第二期)/ C14 / C23 / C24 / C26。
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
| A3 | 看等待名单 | _app.agent.index;GET /calls/waiting(按 staffing 过滤,call_handlers.go:52-66) | VC-S3-01(sup 视角) | **G-A1**:agent 视角的 QueuesForAgent 过滤无 case → 已起草 **VC-S13-01**(待批准) |
| A4 | 弹屏 + 接听 | PARTY_RINGING(coordinator.go:404-418)+ incoming 卡 | VC-S4-01 | G-A2(低):UI 渲染 bot summary/userData 呈现层无断言 |
| A5 | 通话中:实时转写 | live-transcript.tsx;CALL_TRANSCRIPT(actor.go:261) | VC-S9-01/02 | — |
| A6 | 通话中:联系人卡 | _app.agent.index ⋈ lib/contacts;GET /contacts | — | **G-A3**:contacts 全链无 case(tables.md 已记)→ 已起草 **VC-S13-06**(待批准) |
| A7 | 保持/取回 | POST /calls/{id}/hold\|retrieve(202);uuid_phone_event | VC-S7-01 | — |
| A8 | DTMF | POST /calls/{id}/dtmf;uuid_send_dtmf 远端腿 | VC-S7-04 | — |
| A9 | 转接 | POST /calls/{id}/transfer;Transfer 选主叫腿(coordinator.go:770-777) | VC-S7-02 | — |
| A10 | 挂断→ACW(平台代开)→确认 | coordinator.go:246-264;OpenWrapUp 默认词;POST /agent/wrap-up(agent_handlers.go:193-207) | VC-S4-02/03 | — |
| A11 | 坐席不接(RONA) | mod_callcenter 重派;app 侧死边(state.go:189) | VC-S5-01 | 缺口已拍板实现 → **W2**(S5-01 已因 C22 于 08-21 重跑一次 PASS;W2 落地后仍须按 RONA 新行为整体改写再跑,见 T6.9) |
| A12 | 回访单认领/办结 | _app.agent.callbacks;claim/complete(ledger_handlers.go:171/:196) | VC-S3-04(后半) | — |
| A13 | 回看自己的通话 + 回放录音 | _app.agent.calls.tsx(RecordingPlayer,a6ff7b9);GET /cdrs(mine)+ /recordings/{id}/audio | — | **G-A4**:①列表只见自己 ②hasRecording 可播 ③**越权拒绝**(0639c56)→ 已起草 **VC-S13-02**(待批准) |
| A14 | 我的一天(my-day) | ledger.sql:222-240 CTE;_app.agent.index 概览 | — | **G-A5**:汇总数字无 case → 已起草 **VC-S13-03**(待批准) |
| A15 | 话机失联 | ObserveDevice→DEVICE_IN_SERVICE→DEVICE_UNREACHABLE(state.go:207-210) | S11/S12 只到事件层 | **G-A6**:失联→摘除→恢复 旅程无 case → 已起草 **VC-S13-05**(待批准) |

### 1.2 主管(supervisor)旅程

| # | 步骤 | 锚点 | 已有 case | 缺口 |
|---|---|---|---|---|
| B1 | 墙板(今日数字) | _app.supervisor.index;Report*(ledger.sql:133-161) | — | **G-B1**:报表数字对账无 case → 已起草 **VC-S13-04**(待批准) |
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
| C2 | 分机管理 | _app.admin.extensions;PUT(全量);luacc.directory | — | **G-C2**:建分机→注册鉴权→删除守卫 无 case → 已起草 **VC-S14-01**(待批准) |
| C3 | 号码(DID)管理 | _app.admin.numbers;PUT(全量);luacc.dids→lua:44 | VC-S8-01、S4-04 | **G-C3**(F8 已闭:视图 `WHERE d.is_enabled` 过滤):剩"建号→放号→拨通→停用→拒接" → 已起草 **VC-S14-03**(待批准) |
| C4 | 路由(队列)管理 | _app.admin.routing;saveQueue(lib/catalog.ts:51) | VC-S3-04 | ~~G-C4~~ **已闭**(F7:表单发全量) |
| C5 | 队列配员 | staffQueue/unstaffQueue;PUT /queues/{id}/agents | VC-S3-02(FAIL) | G-C5:配员→tier 生效→撤销 UI 全链无 case → 已起草 **VC-S14-02**(待批准) |
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

**起草进度(2026-08-22,草案待批准)**:G-A1→VC-S13-01、G-A3→VC-S13-06、G-A4→VC-S13-02、
G-A5→VC-S13-03、G-A6→VC-S13-05、G-B1→VC-S13-04、G-C2→VC-S14-01、G-C5→VC-S14-02、G-C3→VC-S14-03;
**G-C1 未起草**(与 W9 账号清理冲突)。另有两条不在上表内:C15 附带记的 **G-A7**(坐席外呼全链)
已起草为 **VC-S14-04**,T5.3 发现的重建缺口已起草为 **VC-S12-04**。
草案并入 `ledger.yaml` 之前,本节计数维持原值不动。

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

### 阶段 6 —— 新 case 起草(旅程缺口 → 账本追加)· **11 case 已起草,待批准后并入 ledger.yaml**

> **状态(2026-08-22)**:草案在 `docs/verification/ledger-draft-phase6.md`(commit 8321675),
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

**C 系列(既有立案):**
- **C1** VC-S3-02 根因修复:DeleteCallcenterTier 不对含域名字二次限定 + converge removed 以复查为准;
  修后重跑 S3-02。
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
  **已了结**:两型 live 复测均已通过,结果记在 **C17**;T6.11 已起草为 **VC-S14-04**(待批准)。
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
2. **验证先行于实现** —— **前置条件现已满足**:阶段 2(S1-02/S8-01)与 T4.1(S5-01)均已在
   **W1/W2 合入前**按旧行为执行留证,"账本钉住旧缺口 → 实现 → T6.9 重跑证明修复"的证据链已成立,
   **W1/W2 可以开工**。W7 组①③④ 涉及的事件断言仍在 T6.10 补。
   W3/W4/W5/W6/W8 与验证执行无交集,可并行。
3. C1:依赖 tier 正确性的 case 已全部跑完,**修复不再被阻塞**。但注意 **C1 的症状已潜伏** ——
   修后重跑 VC-S3-02 前须先按 C1 条目里的办法重造复现场景,否则会得到**假通过**。
   **W9(账号清理)**:原写"硬性排在阶段 3–6 之后"。阶段 6 现已起草完毕,但 ⚠ **W9 仍不得开工** ——
   已起草的 **VC-S13-01**(ben 只配员 support-zh 的过滤)与 **VC-S13-02**(以 ben 的录音
   `01a02276-2a62-…` 作越权靶子)都以 ben 为执行物料,而它们**尚未执行**。
   W9 的真正前置是"依赖 ben 的用例**执行完毕**",不是"起草完毕"。
4. 每 case 执行后:evidence 落 `docs/verification/artifacts/<ID>/`,status 更新单独 commit
   (账本修订与执行结果不混提)。
