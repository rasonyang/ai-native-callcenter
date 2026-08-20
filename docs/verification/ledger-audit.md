# ledger.yaml 审计 — 23 个 TODO case(逐 case 完整版)

> 审计基线:工作树 HEAD a6ff7b9(2026-08-20 11:43),live 环境实测 2026-08-20 下午。
> 方法:只读。代码证据 = file:line;环境证据 = 实跑只读命令(psql SELECT / fs_cli list / global_getvar / 日志 grep);
> 未发起任何呼叫,未触碰 stale tier(VC-S3-02 现场),未改任何源码/账本。
> 问题类型:**L-a** collect 命令缺陷 / **L-b** 前提事实过期 / **L-c** expect 漂移 / **L-d** 静默失败采不到(覆盖缺口)。
> 修订建议一律 diff,待人工批准后由下一 session 统一应用。

---

## 0. 已核实的环境事实(采集即证据)

| 事实 | 实测值 | 证据 |
|---|---|---|
| wei 绑定分机 | **1008** | psql agents⋈users⋈extensions 实查;seed 源码默认确是 1001(seed.go:61),live DB 已改绑 |
| amy 的 agent 行 | **default_extension_id = NULL**;**分机 1000 属 uiagent** | psql 实查(agent-amy 存在但无绑定分机) |
| 有默认分机的坐席 | agent=1005、ben=1002、chen=1009、tester=1003、uiagent=1000、wei=1008;amy/cara 无 | psql 实查 |
| 队列域后缀 | `@192.168.31.55`(`global_getvar domain` 实测) | tier list 另有陈旧 `support-en@192.168.31.176` + 静态遗留 `support@default`(S3-02 FAIL 的既知污染,不触碰) |
| `agent list` 状态列 | **$6**(name\|instance_id\|uuid\|type\|contact\|status\|…) | fs_cli 实测表头;账本 S4-02/S4-03/S11-01/S12-03 已回修 ✓ |
| `tier list` 表头 | `queue\|agent\|state\|level\|position` | 实测;S12-03 的 awk `$2"\|"$1` 兼容 ✓ |
| `queue list` 首列 | 带域队列名(`support-en@192.168.31.55\|…`) | 实测;S3-01 的 `awk '/^support-en@/{print $1}'` ✓ |
| **裸 `fs_cli` 不在 PATH** | `command -v fs_cli` 空(login zsh 亦空) | 只有 `/usr/local/freeswitch/bin/fs_cli` |
| DID | 95001/95002/95011/95012,**全部 is_recording_enabled=t**;95999 不存在 ✓ | psql 实查 |
| dispositions | 恰好 4 行 RESOLVED/FOLLOW_UP_REQUIRED/NO_ANSWER/OTHER ✓ | psql(VC-S4-03 expect 成立) |
| 队列 | support-en ext=7001 sla=20;support-zh ext=7002 sla=0;均 enabled | psql 实查 |
| luacc.dids | 95001→fallback ext 7001(=support-en);列名 `fallback_queue_ext_number` | psql(VC-S12-01 前提成立) |
| 转写已启用 | `live transcription enabled provider=openai model=gpt-live-transcribe rateHz=24000` | logs/aicc-20260820-125146.log(VC-S9-01 前提成立) |
| 启动串 | `voice provider selected`(恰 1 次)/`ai voice leg listening`/`database ready` 均在 | 同上 |
| 容器/app | aicc-postgres、aicc-seaweedfs Up(healthy);app :8080 存活 | docker ps / curl |
| FS 录音目标 | `aicc_recordings_dir=http://127.0.0.1:8888/buckets/aicc-recordings` | global_getvar(VC-S4-04 前提成立) |
| transcripts 表 | **无 `is_final` 列**(有 provider/utterance_id) | information_schema;S1-02 collect 的 `\|\|` 兜底分支即真实路径 |
| wrap_ups 表 | 有 disposition_label、is_confirmed ✓ | information_schema |
| 今日实呼数据 | queue_events 事件名 JOINED/OFFERED/BRIDGED/ABANDONED/LEFT 全出现;recordings 有 `S3\|aicc-recordings\|…\|38/42/70s` 行;**纯 bot 呼叫(95001,bot_sec=4)的 transcripts 只有 `BOT\|MODEL\|TEXT`,零 CUSTOMER 行**;95002 有 ANSWERED 且 bot_sec=0 的行(见 S3-03 备注) | psql 实查 |

### 0.1 第二轮追检(2026-08-20,应审阅要求;全部只读)

| 问题 | 结论 | 证据 |
|---|---|---|
| ① stale tier 是否阻塞阶段 3/4 | **不阻塞(惰性)**:`support-en@192.168.31.176` 作为队列不存在(queue list 无此行),该 tier 永远收不到成员;agent-wei 在真实两 tier 上 state=Ready、agent status=Available/Waiting | fs_cli tier list / agent list / queue list 实测 |
| ② F7:队列编辑表单是否发全量 | **发全量,无产品缺陷**:编辑走 `setEditing(row)`(整行拷贝)+ 逐字段 spread,提交 `saveQueue.mutate(editing)` 携带完整对象 | _app.admin.routing.tsx:99/:119/:125-132 |
| ③ 第二坐席 chen/1009 | **chen 不可用**:login 401——chen/uiagent/liveagent 均非 seed 账号(seed 只建 admin/supervisor/wei/amy/ben,seed.go:56-63、demoPassword=aicc@12345 seed.go:42),密码未知。**改用 ben**:login 200,agent-ben/分机 1002,luacc.directory 1002(aicc@123)启用。同时注册无冲突(1008/1002 是不同 AOR),仅需第二个浏览器 profile 跑 web-sip-phone | curl login 实测;psql;fs_cli sofia reg(当前仅 1008 在注册) |
| ④ liveagent/uiagent 凭据 | **均 401 不可用**;且 liveagent **没有 agents 行**(无 callcenter_name/分机),即便有密码也走不了坐席流。T6.1 的双坐席物料用 **wei+ben** | curl login 实测;psql users⋈agents |
| F8:DID is_enabled=false 是否真拒接 | **是,机制在视图层**:luacc.dids 视图 `WHERE d.is_enabled`(pg_get_viewdef 实查),停用号从视图消失 → lua route=nil → "unknown number"+UNALLOCATED_NUMBER(lua:47-49)。视图暴露的 is_enabled 列恒为 t(装饰性) | pg_get_viewdef;aicc_inbound.lua:43-49 |

## 1. 系统性发现(跨 case;逐 case 章节引用编号,不重复展开)

### SYS-1 [L-a] 裸 `fs_cli` 在本机不可执行(9 个 case、11 处)
`command -v fs_cli` 交互/登录 shell 均为空;账本内 S1-03、S12-01 已用全路径,其余用裸名。
受影响:VC-S1-01(#1)、S2-01(#2)、S3-01(#4,两次)、S4-02(#5)、S4-03(#3)、S7-02(#3)、S12-02(#2)、S12-03(#2、#3)。
VC-S11-01(PASS)执行时是人工代换后跑的——账本原文仍是坏命令。

**建议修订**(账本头部加约定 + 全文替换):
```diff
 # 通用约定(每条用例的 collect 假定已执行过下面两行登录;为省篇幅不逐条重复):
+#   FS=/usr/local/freeswitch/bin/fs_cli(裸 fs_cli 不在本机 PATH;collect 中一律用 $FS)
```
```diff
-      - "fs_cli -x 'show channels' | cat"
+      - "FS=/usr/local/freeswitch/bin/fs_cli; $FS -x 'show channels' | cat"
```
(其余 10 处同型。)

### SYS-2 [L-c EXPECT-DRIFT] 呼叫控制操作实际返回 **202**,非 204/200(3 case)
`callOp` 成功统一写 `http.StatusAccepted`(internal/httpapi/call_handlers.go:141);契约同
(openapi:hold/retrieve/transfer/dtmf 成功码只有 202)。受影响:VC-S7-01、S7-02、S7-04 的 expect 首行。
```diff
-      hold/retrieve 均返回 204(或 200)
+      hold/retrieve 均返回 202(Accepted;callOp 统一码,call_handlers.go:141)
```
(S7-02 "transfer 返回 204(或 200)"、S7-04 "dtmf API 返回 204(或 200)" 同型改 202。)

### SYS-3 [L-a] 对整个日志文件 `grep -c` 断言 =0 没有基线;`…| tail; echo rc=$?` 的 rc 恒为 0
- 无基线假阳:S4-02 #4、S7-03 #2('rejected party transition')、S2-01 #4('transcript mailbox full')——
  同一实例先跑的 case 产生过该行,后跑的会误判 FAIL。
- rc 无效:S1-03 #2、S7-01 #7、S9-01 #6 的 `| tail -N; echo rc=$?` 取的是 tail 的退出码,恒 0,毫无判别力。

**建议修订**(基线差分,代表性 diff;三处 rc 行删 `echo rc=$?`,改由输出行数判定):
```diff
-      - "sleep 3; …; LOG=$(ls -t logs/aicc-*.log | head -1); grep -c 'rejected party transition' \"$LOG\""
+      - "LOG=$(ls -t logs/aicc-*.log | head -1); B=$(grep -c 'rejected party transition' \"$LOG\"); sleep 3; A=$(grep -c 'rejected party transition' \"$LOG\"); echo delta=$((A-B))"
```
(expect 相应改 "delta=0"。)

### SYS-4 [L-b] amy 无默认分机;分机 1000 属 uiagent(详见 VC-S7-02)

### SYS-5 [L-a] VC-S9-02 jq 字段名错:契约字段是 `state`,不是 `transcriptionState`(详见该 case)

### SYS-6 [观察,不改判] 契约缺口:线上有、schema 没有
- `/calls` 直接序列化 telephony.Snapshot(call_handlers.go:70/45),PartySnapshot(Go call.go:348)带
  `isBotLeg,omitempty` —— VC-S1-01 的 expect 可执行;但 openapi PartySnapshot **未声明** isBotLeg。契约漂移,归 TASKS。
- 录音条目字段是 `id`(无 `recordingId`);S4-04 collect 的 `.recordingId // .id` 兜底恰好可用,建议顺手删掉前半。

### SYS-7 [L-a MAJOR] `/queues/{id}` 与 `/dids/{id}` 只有 **PUT(全量替换)**,没有 PATCH(2 case)
openapi paths 实查:`/queues/{queueId}` → put/delete;`/dids/{didId}` → put/delete。handler 全量 decode
(catalog_handlers.go:89-97 decode 整个 catalog.Queue)。受影响:
- VC-S3-04 #1/#8:`-X PATCH /queues/$QID -d '{"isEnabled":false}'` → **405/不匹配路由**;若有人"顺手"改成单字段 PUT,会把
  strategy/moh/sla 等字段清零 —— **破坏性**。
- VC-S4-04 #2:`-X PATCH /dids/$DID -d '{"isRecordingEnabled":true}'` → 同型;单字段 PUT 会清掉 language/flowId/fallbackQueueId。

**建议修订**(读-改-写全量 PUT;S3-04 #1 示例,#8 与 S4-04 #2 同型):
```diff
-      - "QID=$(…); curl -s -b /tmp/vc-sup.jar -H 'X-AICC-Csrf: 1' -H 'Content-Type: application/json' -X PATCH http://127.0.0.1:8080/api/v1/queues/$QID -d '{\"isEnabled\":false}' | jq -c '.isEnabled'"
+      - "QID=$(…); Q=$(curl -s -b /tmp/vc-sup.jar http://127.0.0.1:8080/api/v1/queues | jq -c '.items[] | select(.id==\"'$QID'\")'); curl -s -b /tmp/vc-sup.jar -H 'X-AICC-Csrf: 1' -H 'Content-Type: application/json' -X PUT http://127.0.0.1:8080/api/v1/queues/$QID -d \"$(echo \"$Q\" | jq -c '.isEnabled=false')\" | jq -c '.isEnabled'"
```

### SYS-8 [L-c EXPECT-DRIFT] 纯 bot 呼叫没有 CUSTOMER 转写行(2 case:S1-02、S8-01)
三重证据:
1. OpenAIProfile **未设 TranscribeModel**(profile.go:71-84 无该字段;QwenProfile 同),
   realtime.go:367-368 只在 TranscribeModel 非空时开 input transcription → provider 永不产生
   `input_audio_transcription.*` → `CUSTOMER_SAID` 事件(session.go:440)在两个 stock profile 上都是死路径
   (唯一例外:按键会写 `CUSTOMER|MODEL` 的 "[keypad] …" 行,orchestrator.go:334)。
2. ASR tap 只在**坐席腿**上挂(coordinator.go:480 唯一 Attach 调用点,带 agentID)→ 纯 bot 呼叫无 ASR 行。
3. 实证:今日 95001 纯 bot 呼叫的 transcripts 只有 `BOT|MODEL|TEXT`;全天 CUSTOMER 行全部 source=ASR(转人工段)。

---

## 2. 已核实成立的关键点(逐 case 引用)

- **日志串逐字存在**(19 个,均已 grep 源码核对):`ai conversation started`(orchestrator.go:310,字段
  flowId/provider/language;flowId=spec.ID="novanet_support" slug ✓)、`ai call bridged`(session.go:258,字段
  law/toProvider/fromProvider/isPassthrough)、`caller leg ended, closing the model session`(session.go:733)、
  `model session closed`(session.go:461)、`ai call failed`(session.go:743)、`transcript mailbox full, line dropped`
  (actor.go:171)、`rejected party transition`(registry.go:400)、`recording booked`(cdr.go:140)、
  `no recording ingested`(cdr.go:122)、`transcription tap attached`(tap.go:182)、`tapped stream connected`
  (streamin.go:319)、`transcription live`(session.go:61)、`transcribe: audio was dropped for this speaker`
  (pump.go:226)、`transcription tap refused a command`(coordinator.go:146)、`agent presence mirrored to the switch`
  (service.go:549)、`registrations reconciled`(wiring.go:146)、`agent staffing reconciled|already matched`
  (catalog/service.go:310/:313)、Lua `aicc_inbound: unknown number …`(lua:21+48)、`aicc_inbound: bot leg failed for …`(lua:106)。
- **SSE 可见性**:supervisor/admin 收一切(hub.go:225-228 `if s.IsSupervisor return true`)→ sup jar 抓
  agent/queue 域事件成立;PARTY_RINGING scope=AgentIDs[wei](coordinator.go:418)→ wei jar 抓自己的成立;
  CALL_TRANSCRIPT scope=在场坐席(actor.go:155-162)→ S9-01 wei jar 成立。
- **payload 键名**:QUEUE_JOINED{queueName,fromNumber,joinedAt}(waiting.go:240-247);QUEUE_LEFT{queueName,fromNumber,
  waitSec,cause,cancelReason}(waiting.go:263-274);QUEUE_COUNT{queueName,waiting[,longestWaitAt]}(waiting.go:285);
  QUEUE_AGENT_OFFERED payload={queue,fromNumber},**agentId 在信封**(coordinator.go:590-600 + SseEvent.agentId);
  PARTY_RINGING{fromNumber,toNumber,extensionNumber}(coordinator.go:412-416);PARTY_DTMF{digit,durationMs}
  (registry.go:388-391;ticks/8 换算 switchevent.go:255-258);CALL_TRANSCRIPT{utteranceId,speaker,kind,isFinal,text,
  source[,seq,offsetMs,agentId]}(actor.go:264-284)。
- **队列名域后缀在归一化层剥除**(switchevent.go:336 `strings.Cut(CC-Queue,"@")`)→ 事件与 QueueByName 都用裸名。
- **queue_events 事件名** = JOINED/OFFERED/BRIDGED/ABANDONED/LEFT(ledgerstore.go:825-829;cdr.go:346-370;今日实证)。
- **missedReason 值域与判定**(cdr.go:251-277):ABANDONED_RINGING(需 queue.BridgedAt≠0)、NO_AVAILABLE_AGENT
  (Cause=Timeout)、SHORT_ABANDONED(<5s,cdr.go:45)、ABANDONED_WAITING(其余)、AGENTS_DID_NOT_ANSWER(仅非队列呼叫)。
- **CDR 归属**:纯 bot → aicall 写一行(ledger.go:130-186;IsContained=ANSWERED∧endReason=="HANGUP",:175);
  已转接 → recorder.finish 早退(:145-146),人侧凭 bot-share 戳写唯一一行(cdr.go:79);
  stampBotShare 盖 aicc_bot_sec/aicc_language/**aicc_flow_id**/aicc_did(ledger.go:223-231)→ botShare 读回
  (switchevent.go:81-95)→ cdr.FlowID/legs(cdr.go:162/:282)。
- **adopt 只发生在 CHANNEL_CREATE**(coordinator.go:173-174)→ S1-03 未知号短命通道会入账;S12-02 重启后进行中呼叫
  永不入账(Dispatch false,registry.go:140)——账本对该缺口的钉死方向正确。
- **Transfer 选"活着且 AgentID==nil"的腿**=主叫(coordinator.go:770-777)→ 打对腿;Hold/Retrieve 打坐席自己的腿
  (coordinator.go:640-651 agentChannel → adapter uuid_phone_event,adapter.go:169/:176-177);SendDTMF 用
  uuid_send_dtmf(adapter.go:203)。
- **wrap-up 链**:挂断→已应答坐席腿→BeginAfterCallWork(coordinator.go:246-264)→OpenWrapUp 平台代开、默认处置词=
  首个 enabled(ledgerstore.go:495-504 defaultDisposition;SQL ledger.sql:180-182,is_confirmed=false);
  确认= ConfirmWrapUp **先落库**、成功才 EndWrapUp→READY(agent_handlers.go:193-207)→ S4-03 的 expect 与实现一致,
  且其 failure_looks_like(确认失败但回 READY)在服务端次序上**不可达**(只剩 UI 乐观更新一种途径)。
- **CallcenterStatus 映射** READY→Available / NOT_READY→On Break / 其它→Logged Out(state.go:220-228)。
- **录音链**:record_session 目标 `{aicc_recordings_dir}/{UTC %Y/%m/%d}/{call_id}.wav`(lua:87-94)与
  recording.Key(storage.go:44-46)一致;flushWait=2s(cdr.go:35);ingest→InsertRecording{backend,bucket,size,
  duration=size换算}→MarkRecorded→"recording booked"(cdr.go:107-142);今日已有 S3|aicc-recordings 实例行。
- **restart.sh**:先 build 再 pkill,15 次重试起新实例,成功 `started: <log>`、失败 `failed to start; last log:`。
- **契约字段**:WaitingCall{queueName,fromNumber,slaThresholdSec,…} ✓;RosterEntry{username,availability,isOnCall,
  isRegistered,…}、Availability 含 ON_CALL ✓;CurrentWrapUp{callId,dispositionCode,dispositionLabel,note,isConfirmed} ✓
  (GET /agent/wrap-up 无单时 **204 空体**);Callback.status ∈ OPEN/CLAIMED/DONE/DISMISSED、complete 体
  {"status":"DONE"} 合法 ✓;Leg{kind,label,durationSec}、LegKind 含 BOT/QUEUE/AGENT ✓(store.Leg json tag 同名,
  ledgerstore.go:90-95);Speaker=CUSTOMER/BOT/HUMAN_AGENT、TranscriptKind 含 TEXT、TranscriptSource=MODEL/ASR ✓;
  uq_transcripts_call_id_seq 存在(00005:64)✓。
- **主机 dialplan**:stock `Local_Extension ^(10[01][0-9])$`(/usr/local/freeswitch/conf/dialplan/default.xml:265-266)
  覆盖 1000–1019 → 转接裸分机号可达;仓内 default 上下文只管 `^(7\d{3})$` 队列分机(conf/dialplan/default/05_aicc.xml:10)。
- **novanet_support.json**:id="novanet_support";global.voice 未设(回落 profile);alwaysAllowedTools=
  [transfer_to_agent, take_message, hangup];global.transitions 只有 result.ok=1 的成功边(transfer→handoff、
  hangup→farewell)——**没有 transfer 失败分支**,拒绝后靠 refusalHint 驱动模型继续对话(actions.go:58-64)。
- **findQueue 每次实时读库**(orchestrator.go:441-452 → catalog.Queues → store.ListQueues)→ 停用队列即时生效,
  S3-04 机制成立;停用返回 `flow.Failed("QUEUE_CLOSED", …)`(actions.go:62-64)。
- **ESL 重连回退** 500ms–30s(link.go:17-18);OnConnect→SyncSwitch+SyncTiers(wiring.go:99/:134-136)。

---

## 3. 逐 case 审计

图例:a=前提事实 / b=expect 对照 / c=collect 本机可执行 / d=静默失败可采集。✓ 通过;△ 通过但有备注;✗ 有缺陷(附修订)。

### VC-S1-01 — a✓ b△ c✗ d✓
- a ✓:precondition 两串实测在(§0);95001 在 dids ✓。refs 4 处全对位(coordinator.go:329 adopt/:297 reidentify、
  orchestrator.go:207 runCall、lua:56 create_uuid)。
- b △:全部 expect 有实现源——2 通道、/calls 单呼叫(reidentify 合并 :297-326)、state=RUNNING、language、
  parties role/state、`isBotLeg`(线上有,call.go:348;**契约没有**,SYS-6)、两条日志的字段名逐一核对(§2)。
- c ✗:**SYS-1**(#1 裸 fs_cli)。
- d ✓:failure(GET /calls 出现 2 呼叫)由 #2 直接采到。

### VC-S1-02 — a△ b✗ c△ d✗
- a △:transcripts 无 is_final 列(§0)——collect #2 的第一条 psql 必错、`||` 兜底即真实路径(可用,勿删兜底)。
- b ✗ **[L-c / SYS-8]**:"speaker=BOT 与 speaker=CUSTOMER 各≥1,source=MODEL" 不成立——本部署纯 bot 呼叫
  **没有任何 CUSTOMER 行**(证据链见 SYS-8)。BOT 行 source=MODEL ✓;seq 连续 ✓(actor 独占分配 actor.go:222-261);
  CDR 各字段 ✓(ledger.go:150-186:INBOUND/ANSWERED/bot_sec>0/did/language/is_contained=false【主叫先挂→endReason=""】/
  NORMAL_CLEARING 兜底 :154-156);日志计数 ≥1 ✓(session.go:733)。
  ```diff
  -      transcripts 该 call_id 至少 2 行:speaker=BOT 与 speaker=CUSTOMER 各≥1,source=MODEL,seq 从 1 起连续无洞
  +      transcripts 该 call_id 至少 1 行:speaker=BOT(kind=TEXT,source=MODEL),seq 从 1 起连续无洞;
  +      【既成事实】CUSTOMER 行不出现:stock profile 未设 TranscribeModel(profile.go:71-84,realtime.go:367),
  +      ASR tap 只挂坐席腿(coordinator.go:480)——若未来出现 CUSTOMER|MODEL 行,说明有人启用了 provider 侧转写,单独归档
  ```
- c △:兜底分支可用;首条报错落 stderr,判读时忽略即可。
- d ✗ **[L-d]**:failure 之一是"同一通话 2 行 CDR",但 #1 `limit 1` 只看最新一行,**双行采不到**。
  ```diff
  -      - "sleep 3; docker exec -i aicc-postgres psql -U aicc -d aicc -Atc \"select call_id, call_type, status, is_contained, bot_sec, total_sec, did, language, hangup_cause from cdrs order by started_at desc limit 1\""
  +      - "sleep 3; docker exec -i aicc-postgres psql -U aicc -d aicc -Atc \"select call_id, call_type, status, is_contained, bot_sec, total_sec, did, language, hangup_cause from cdrs order by started_at desc limit 1\""
  +      - "CALL_ID=$(docker exec -i aicc-postgres psql -U aicc -d aicc -Atc \"select call_id from cdrs order by started_at desc limit 1\"); docker exec -i aicc-postgres psql -U aicc -d aicc -Atc \"select count(*) from cdrs where call_id='$CALL_ID'\""
  ```
  (expect 增一行:"同一 call_id 的 cdrs 行数 = 1"。)

### VC-S1-03 — a✓ b✓ c✗ d✗
- a ✓:95999 不在 dids(实测);lua:47-49 逐字对位(`unknown number` + `session:hangup("UNALLOCATED_NUMBER")`);
  adopt 在 CHANNEL_CREATE 上无条件收养(coordinator.go:173-174/:329)→ 短命通道也入 registry → Finish → InsertCDR
  (无 bot 腿 → cdr.go:79 人侧写)。
- b ✓:status=NO_ANSWER(answered 空,cdr.go:229)、hangup_cause 透传、DIALING→RELEASE 合法(call.go:69)、
  console 前缀 `aicc_inbound:`(lua:21)。
- c ✗ **[SYS-3]**:#2 的 `echo rc=$?` 恒 0;`tail -3` 显示的是历史行,无从判"无新增"。改基线差分(SYS-3 diff 同型)。
- d ✗ **[L-d]**:failure 是"cdrs 里根本没有行",但 #3 读"最新一行"——没有新行时显示的是**上一通的行**,与"有新行"
  无法区分。需要呼叫前基线:
  ```diff
  +      - "docker exec -i aicc-postgres psql -U aicc -d aicc -Atc \"select count(*) from cdrs\"   # 拨号前跑,记 N0"
  -      - "docker exec -i aicc-postgres psql -U aicc -d aicc -Atc \"select status, hangup_cause, answered_at is null as never_answered from cdrs order by started_at desc limit 1\""
  +      - "docker exec -i aicc-postgres psql -U aicc -d aicc -Atc \"select count(*) from cdrs\"   # 挂断后跑,须为 N0+1"
  +      - "docker exec -i aicc-postgres psql -U aicc -d aicc -Atc \"select status, hangup_cause, answered_at is null as never_answered from cdrs order by started_at desc limit 1\""
  ```

### VC-S2-01 — a✓ b✓ c✗ d✓
- a ✓。b ✓:三条日志串在(§2);channels=0;CDR ANSWERED/is_contained=false/bot_sec≥1(说话中挂断>1s)/
  NORMAL_CLEARING(ledger.go:150-175);mailbox 串在(actor.go:171)。
- c ✗:**SYS-1**(#2)+ **SYS-3**(#4 mailbox 计数无基线)。
- d ✓:failure("model session closed" 缺失 / is_contained=true)#1、#3 均直接采到。

### VC-S3-01 — a△ b✓ c✗ d✓
- a △ **[L-b 轻]**:expect 括注 "seed 默认 1001" 已过期(live=1008;seed.go:61 原文确是 1001)。collect 以查询为准,
  自愈;建议括注改 "live 当前 1008,以查询为准"。
- b ✓:queueName/fromNumber/slaThresholdSec(=20)与 WaitingCall 契约对位;三事件 payload/信封与 §2 对位;
  sup 收 agent 域事件 ✓(hub.go:225-228)。members 表 "state=Trying|Waiting,cid_number" 为 mod_callcenter 格式
  [INFERENCE,空队列无法静态验证——保留,首跑时核]。
- c ✗:**SYS-1**(#4 两处)。另 **[L-a 轻]**:precondition 说"前 2 条可代做签入+READY",但 collect 没有 login 一条,
  wei 未签入时 #2 会回 AGENT_NOT_LOGGED_IN:
  ```diff
  +      - "curl -s -b /tmp/vc-wei.jar -H 'X-AICC-Csrf: 1' -H 'Content-Type: application/json' -X POST http://127.0.0.1:8080/api/v1/agent/login -d '{}' | jq -c '.state // .error.code'"
        - "curl -s -b /tmp/vc-wei.jar -H 'X-AICC-Csrf: 1' -X POST http://127.0.0.1:8080/api/v1/agent/ready | jq -c ."
  ```
- d ✓:两侧(members / /calls/waiting)都在 collect,failure 可采。

### VC-S3-03 — a✓ b✓ c✓ d✓
- a ✓(紧接 S3-01)。
- b ✓:单行 CDR(转接→aicall 早退 ledger.go:145-146,人侧凭戳写 cdr.go:79);bot_sec/flow_id 经
  aicc_bot_sec/aicc_flow_id 戳(ledger.go:223-231→switchevent.go:81-95→cdr.go:162);queue_events
  JOINED→OFFERED(agent)→BRIDGED(wait_ms>0)(cdr.go:346-361);legs BOT/QUEUE(label=队名)/AGENT
  (cdr.go:280-…;json tag kind/label/durationSec,ledgerstore.go:90-95)。
- c ✓。d ✓:failure(bot_sec=0∧flow 空)#1 直接采到。
- 备注:**今日 DB 已有 95002 的 ANSWERED 且 bot_sec=0 行**——要么是 bot 腿失败走了 lua fallback(合法 0),要么正是
  本 case 猎的静默漏戳。执行本 case 时顺带核对该历史行的成因(fs console / 日志),值得提前排。

### VC-S3-04 — a△ b✓ c✗ d✓
- a △ **[L-b/ASSUMPTION 判明]**:novanet_support.json **没有 transfer 失败分支**(global.transitions 只有
  result.ok=1 两条成功边);take_message 在 alwaysAllowedTools —— "bot 主动提出留言"依赖模型跟随 refusalHint
  (actions.go:58-64),非流程保证。note 中"若无此分支,直接对 bot 说 leave a message"应从备选升为主路径:
  ```diff
  -    note: "…flow 是否在队列停用时主动提出留言取决于 novanet_support.json 的转移规则([ASSUMPTION]——验证方法:jq …;若无此分支,直接对 bot 说 leave a message)"
  +    note: "…[已判明] novanet_support.json 无 transfer 失败分支(global.transitions 仅 result.ok=1 成功边);拒绝后 bot 是否主动提出留言取决于模型对 refusalHint 的跟随——人工步骤直接说 leave a message, please 最稳"
  ```
- b ✓:QUEUE_CLOSED 拒绝(actions.go:62-64,findQueue 实时读库 ✓);callbacks 列/回落号码(actions.go:106-108)/
  claim="CLAIMED"/complete 体与响应/CALLBACK_UPDATED=2(ledger_handlers.go:185/:217)/CREATED 广播(wiring.go:309)全对位。
- c ✗ **[SYS-7 MAJOR]**:#1、#8 的 PATCH 不存在且单字段 PUT 破坏性——用 SYS-7 的读-改-写 diff。
- d ✓:failure(口头答应、库无行)由 #3(callbacks 查询)+#4(SSE 计数)采到。

### VC-S4-01 — a✓ b✓ c✓ d✓
- a ✓(依赖 S3-01 建立)。b ✓:PARTY_RINGING payload.extensionNumber(coordinator.go:415)且 scope=wei ✓;
  QUEUE_AGENT_OFFERED scope 也含 wei(coordinator.go:600)→ wei jar 全收;SetOnCall→AGENT_AVAILABILITY
  (coordinator.go:419→service.go:350-364);roster ON_CALL/isOnCall ✓(Availability 枚举、RosterEntry 契约)。
- c ✓。d ✓:failure(agentForLeg 三候选全失手→无 PARTY_RINGING)#2 计数直接采到(coordinator.go:831-848 对位)。

### VC-S4-02 — a✓ b✓ c✗ d✓
- a ✓。b ✓:挂断→已应答坐席腿→BeginAfterCallWork(coordinator.go:246-264);OpenWrapUp 平台代开默认处置词
  (ledgerstore.go:495-504)、is_confirmed=false;CurrentWrapUp 字段 ✓;switch On Break(state.go:220-228)。
- c ✗:**SYS-1**(#5)+ **SYS-3**(#4 无基线)。
- d ✓:failure(镜像失败 switch 仍 Available,service.go:628-635 只 Warn)#5 直接采到。

### VC-S4-03 — a✓ b✓ c✗ d△
- a ✓:dispositions 恰 4 行(live 实测);wrap_ups 有 disposition_label(实测)。
- b ✓:ConfirmWrapUp 先落库、成功才 EndWrapUp→READY(agent_handlers.go:193-207;service.go:331-339);
  label 服务端解析 ✓。
- c ✗:**SYS-1**(#3)。
- d △:failure_looks_like(界面回 READY 而 is_confirmed=f)在服务端次序上不可达(确认失败即不 EndWrapUp)——
  只剩前端乐观更新一种途径;#1/#2 依然能采到,判定无碍。建议 failure 文案补一句"服务端次序已防,此形态只可能来自 UI"。

### VC-S4-04 — a△ b✓ c✗ d✓
- a △:本机 95001 已 is_recording_enabled=t(全 DID 皆 t)——#2 通常跳过;SeaweedFS Up ✓;
  aicc_recordings_dir 指向 filer bucket(实测,§0)。
- b ✓:key 契约一致(lua:89 ↔ storage.go:44-46,均 UTC);backend=S3/bucket=aicc-recordings(.env + 今日实证行);
  duration≥1(DurationSec 换算);has_recording(MarkRecorded);audio 下载 200/audio(recording_handlers.go:81-…);
  两日志串在。flush 2s 说法与 recordingFlushWait=2s(cdr.go:35)一致,collect sleep 8 富余 ✓。
- c ✗ **[SYS-7 MAJOR]**:#2 的 PATCH /dids → PUT-only,单字段 PUT 会清 language/flowId/fallbackQueueId。
  改为"仅当 =f 时读-改-写 PUT"(SYS-7 diff 同型);顺手删 `.recordingId //`(SYS-6)。
- d ✓:failure(key 不一致 → "no recording ingested … reason")#5 采到。

### VC-S5-01 — a✓ b✗ c△ d△
- a ✓:tiers 只有 agent-wei(实测)→ "amy 未签入" 实际无关紧要,前提自动满足;RONA 死边确证
  (state.go:189-194 无调用方;QUEUE_AGENT_STATE 归一化即弃,switchevent.go:368)。
- b ✗ **[L-c EXPECT-DRIFT]**:missed_reason 两个候选都到不了(cdr.go:251-277)——
  ABANDONED_RINGING 需 queue.BridgedAt≠0(坐席从未接→bridge-agent-start 从未发生);
  AGENTS_DID_NOT_ANSWER 需 queue.JoinedAt==0(本呼叫入过队)。实际落点:**ABANDONED_WAITING**
  (Cause=Cancel、wait≈60s≥5s 阈值)。
  建议把 expect **整体拆成"可验半"与"留证半"**(判定与记录分离,重派节奏这类环境事实只留证不判定):
  ```diff
  -    expect: |
  -      SSE:QUEUE_AGENT_OFFERED 计数 ≥2(mod_callcenter 重复派同一坐席或按策略轮派)
  -      queue_events:OFFERED ≥2,JOINED=1,无 BRIDGED
  -      cdrs:status=NO_ANSWER,missed_reason=ABANDONED_RINGING(挂断时坐席腿在响)或 AGENTS_DID_NOT_ANSWER
  -      wei 前后两次 agent_states 完全一致(state=READY)——【记录在案的已知缺口】app 侧
  -      RONA(RingNoAnswer→NOT_READY(SYSTEM))无调用方,坐席不接不会被摘出路由
  +    expect: |
  +      —— 可验半(判定 PASS/FAIL)——
  +      SSE:QUEUE_AGENT_OFFERED ≥1 且全程无 BRIDGED
  +      queue_events:JOINED=1,OFFERED ≥1,无 BRIDGED
  +      cdrs:status=NO_ANSWER,missed_reason=ABANDONED_WAITING(cdr.go:266-270:入过队且未 bridge、
  +      Cause=Cancel、wait≥5s;ABANDONED_RINGING 需 BridgedAt≠0、AGENTS_DID_NOT_ANSWER 需未入队——均不可达)
  +      wei 前后两次 agent_states 完全一致(state=READY)——app 侧 RONA 无调用方的既成事实
  +      —— 留证半(落档 evidence_dir,不判定)——
  +      OFFERED 实际次数与间隔(needs-FACT F3:max_no_answer=0/no_answer_delay_time=0 下的重派节奏);
  +      RONA 三处无痕(presence/报表/坐席屏)的截图或查询输出——已知缺口的钉子,变化即预警
  ```
  (人工步骤建议"响-停-再响后再挂"即 ≥90s,以给留证半采到 ≥2 次 OFFERED 的机会;SSE 抓流 `--max-time 180`。)
- c △:#3 位置在人工步骤后执行没问题;SSE 抓流 120s 窗对 90s+ 的等待偏紧,建议 `--max-time 180`。
- d △:本 case 本身在钉缺口 ✓;wei 状态前后对照可采 ✓。

### VC-S6-01 — a✓ b✓ c✓ d✓
- a ✓。b ✓:QUEUE_LEFT payload cause/waitSec(waiting.go:263-274;"Cancel" 与 cdr.go:361 同词表);
  ABANDONED(10s≥5s 阈值,cdr.go:45/:267;**若主叫 <5s 就挂会落 SHORT_ABANDONED——保持"默数 10 秒"不少于 8s**);
  missed_reason=ABANDONED_WAITING ✓;/calls/waiting 归零 ✓(CHANNEL_HANGUP 兜底在,waiting.go:189-195)。
- c ✓。d ✓:幽灵等待者由 #6 采到。

### VC-S7-01 — a✓ b✗ c△ d✓
- a ✓。b ✗:**SYS-2**(202)。其余 ✓:Hold/Retrieve 打坐席腿(coordinator.go:640-651→uuid_phone_event,
  adapter.go:169/:176-177);PARTY_HELD/RETRIEVED(registry.go:337-340);tap Pause/Resume 挂点在
  (coordinator.go:200-207),拒绝串在(:146)。
- c △:#7 `echo rc=$?` 恒 0(SYS-3),删掉、以输出行数判。
- d ✓:failure(switch 真保持但 SSE 无 PARTY_HELD)#5 采到。

### VC-S7-02 — a✗ b✗ c✗ d✓
- a ✗ **[L-b MAJOR / SYS-4]**:"amy…分机 seed amy=1000" 双重过期:live 上 amy 无默认分机(login `-d '{}'` 会失败),
  1000 属 uiagent。第 2 坐席改用 **ben=1002**(追检 §0.1③:唯一既是 seed 账号(aicc@12345 可登录)又有默认分机的
  第 2 坐席;chen/uiagent/liveagent 均 401 非 seed。stock Local_Extension `^(10[01][0-9])$` 覆盖 1002):
  ```diff
  -    precondition: "wei 与主叫通话中;amy 已签入 READY(第 2 部坐席,分机见 seed amy=1000;若无第 2 人,转 7002 队列亦可)"
  +    precondition: "wei 与主叫通话中;ben 已签入 READY 且第二个浏览器 profile 的话机注册分机 1002(live 实测:amy 无默认分机、1000 属 uiagent、chen 等非 seed 账号不可登录;若无第 2 人,转 7002 队列亦可)"
  ```
  ```diff
  -        …-X POST http://127.0.0.1:8080/api/v1/calls/$CALL/transfer -d '{\"destination\":\"1000\"}' -o /dev/null -w '%{http_code}\\n'"
  +        …-X POST http://127.0.0.1:8080/api/v1/calls/$CALL/transfer -d '{\"destination\":\"1002\"}' -o /dev/null -w '%{http_code}\\n'"
  ```
  (expect 中 "amy 的腿(user/1000…)" 同步改 ben/user/1002;cookie jar 增 /tmp/vc-ben.jar 登录约定。)
- b ✗:**SYS-2**(202)。机制 ✓:Transfer 选主叫腿(coordinator.go:770-777→TransferToExtension,default 上下文;
  stock Local_Extension 路由)。"agentId 非空" 要求目标坐席已签入——修订后前提已含。
- c ✗:**SYS-1**(#3)。
- d ✓:failure(主叫被挂)show channels 采到。

### VC-S7-03 — a✓ b✓ c✗ d✓
- a ✓。b ✓:HELD→RELEASE 合法(call.go:84);Finish→CDR ANSWERED/talk_sec>0;/calls 归零(registry.go:341-361)。
- c ✗:**SYS-3**(#2 无基线)。
- d ✓:failure(呼叫悬挂 RUNNING)#4 采到。

### VC-S7-04 — a✓ b✗ c✓ d✓
- a ✓。b ✗:**SYS-2**(202)。payload digit/durationMs ✓、ticks/8 ✓、SendDTMF 打远端腿 ✓(coordinator.go:710-735,
  adapter.go:203)。"FS 在主叫腿上生成 DTMF 事件(4/2/#)"为 [INFERENCE]——uuid_send_dtmf 是否对该通道回发
  DTMF 事件未能静态证实,首跑若只见 digit=5,按 L-d 补充判定路径(fs_cli events 或日志),不判 FAIL。
  建议在 expect 加括注"(若发送侧不产事件,以 digit=5 一条为准,发送效果由主叫听感确认)"。
- c ✓。d ✓:failure(打错腿)由 SSE 有无 + 主叫听感组合采到。

### VC-S8-01 — a✓ b✗ c✓ d✓
- a ✓:95002(zh)在;A1 表述与 main.go:267-275 一致。
- b ✗ **[L-c / SYS-8]**:"CUSTOMER 行为中文识别文本" 不成立——纯 bot 呼叫无 CUSTOMER 行(SYS-8;95002 不转人工,
  ASR tap 不挂)。其余 ✓:provider 恰 1 次/字段、语言链(lua:76→orchestrator.go:220-222)、cdrs did/language。
  改写为**留证式**(判定条款只保留可达的;CUSTOMER 现状不判 PASS/FAIL,只落档,变化即预警):
  ```diff
  -      transcripts:BOT 行文本为中文(问候/回答);CUSTOMER 行为中文识别文本
  +      transcripts:BOT 行(source=MODEL,kind=TEXT)文本为中文(问候/回答)——判定条款
  +      【留证,不判定】同一 call_id 的 CUSTOMER 行数一并落档 evidence_dir(当前实现应为 0:SYS-8——
  +      provider 侧转写未启用、ASR tap 只挂坐席腿;若>0,记录为实现变化并回溯 SYS-8,不作为本用例 FAIL)
  ```
  (collect 增一条留证命令:`docker exec -i aicc-postgres psql -U aicc -d aicc -Atc "select count(*) from transcripts where call_id='$CALL_ID' and speaker='CUSTOMER'"`。)
- c ✓。d ✓:failure(provider 随语言变 / bot 说英文)#1↔#2 对照 + BOT 行文本采到。

### VC-S9-01 — a✓ b✓ c△ d✓
- a ✓:转写在线(live 实测,§0);wei READY 依赖前置用例。
- b ✓:三段日志链串全在(tap.go:182→streamin.go:319→session.go:61);CALL_TRANSCRIPT payload isFinal/text
  (actor.go:269-276)、scope=在场坐席含 wei(actor.go:155-162);DB 断言与今日实证一致
  (HUMAN_AGENT|ASR / CUSTOMER|ASR;agent 行带 agent_id——tap Attach 带 agentID,coordinator.go:480);
  uq_transcripts_call_id_seq 存在(00005:64);dropped 串在(pump.go:226)。
- c △:#6 `echo rc=$?` 恒 0(SYS-3),删掉。
- d ✓:failure(attached 后永无 connected)#3 双串对照采到,ERROR(STREAM_NEVER_CONNECTED)路径在
  (streamin.go:415-431)。

### VC-S9-02 — a✓ b✓ c✗ d✓
- a ✓(复用 S9-01 的 sse.log)。
- b ✓:状态序列 CONNECTING→LIVE→STOPPED 的发布点全对位(streamin.go:391/:429、session.go:57/:60/:217/:232);
  ENDED 死值确证(无发布方)。
- c ✗ **[SYS-5]**:契约字段是 `state`(CallTranscript{items,nextSinceSeq,isLive,state}),
  `.transcriptionState` 恒 null:
  ```diff
  -      - "CALL_ID=$(…); curl -s -b /tmp/vc-sup.jar \"http://127.0.0.1:8080/api/v1/calls/$CALL_ID/transcript\" | jq '{state: .transcriptionState, n: (.items | length)}'"
  +      - "CALL_ID=$(…); curl -s -b /tmp/vc-sup.jar \"http://127.0.0.1:8080/api/v1/calls/$CALL_ID/transcript\" | jq '{state: .state, n: (.items | length)}'"
  ```
  (expect 文字 "transcriptionState 与最后一次事件一致" 同步改 "state 与…"。)
- d ✓:快照/流分歧由修正后的 #2 对照采到。

### VC-S12-01 — a✓ b✓ c✓ d△
- a ✓:fallback 链 95001→ext 7001→support-en(luacc.dids+queues 实测);restart.sh 语义核对
  (build→pkill→15 次重试;"started:"/"failed to start")。
- b ✓:lua 兜底(lua:104-116:`bot leg failed for …`→`transfer <ext> XML default`);等待名单**不依赖收养**
  (waiting.go:222-233:registry 不认识的成员照样入列,FromNumber 取 CC 侧)——expect 措辞"收养此呼叫"偏松,
  结果断言正确。
- c ✓(#2 已是全路径)。
- d △ **[L-d]**:存在启动窗竞态——旧实例死→lua 兜底入队的 member-queue-start 可能发生在新实例 ESL 订阅完成之前,
  事件丢失 → /calls/waiting 为空,与"收养失败"不可区分。补一条 switch 侧真值:
  ```diff
        - "curl -s -c /tmp/vc-sup.jar …; curl -s -b /tmp/vc-sup.jar http://127.0.0.1:8080/api/v1/calls/waiting | jq ."
  +      - "FS=/usr/local/freeswitch/bin/fs_cli; $FS -x 'callcenter_config queue list members support-en@192.168.31.55'"
  ```
  (判定:members 有而 /calls/waiting 空 = 应用侧缺口;两侧都空 = 事件落在断档窗,重跑。)

### VC-S12-02 — a✓ b✓ c✗ d✗
- a ✓。b ✓:钉死的缺口逐条有实现根据——adopt 只在 CHANNEL_CREATE(coordinator.go:173-174)、
  Dispatch 对未知 channel 返回 false 即弃(registry.go:140-…)、Restore 回灌(service.go:505-523)。
- c ✗:**SYS-1**(#2)。
- d ✗ **[L-a/L-d]**:#5 `count(*) … > now()-'10 minutes'` 无法把"这通没入账"从"窗口里别的 case 入了账"里分辨出来
  ——期望 0 会被同窗其它行为污染。改为围绕本通的差分:
  ```diff
  -      - "sleep 5; docker exec -i aicc-postgres psql -U aicc -d aicc -Atc \"select count(*) from cdrs where started_at > now() - interval '10 minutes'\""
  +      - "docker exec -i aicc-postgres psql -U aicc -d aicc -Atc \"select count(*) from cdrs\"   # 挂断前跑,记 N0"
  +      - "sleep 5; docker exec -i aicc-postgres psql -U aicc -d aicc -Atc \"select count(*) from cdrs\"   # 挂断后跑,期望仍为 N0(本通漏记即钉死的账面损失)"
  ```
- 备注:#2(channels=2)在 restart.sh 完成后执行,桥不受影响 ✓。

### VC-S12-03 — a✓ b✓ c✗ d✓
- a ✓:OnConnect 钩子在(wiring.go:99→onSwitchConnected:134-136:SyncSwitch+SyncTiers)。
- b ✓:三串日志在(§2);tier/agent list 版式实测对位;roster isRegistered/availability 契约在。
  注:陈旧 tier support-en@192.168.31.176 会一并列出,不影响"存在性"断言。
- c ✗:**SYS-1**(#2、#3)。另 [时序]:`sleep 20` 对 ESL 回退上限(30s,link.go:17-18)+ 浏览器话机重注册偏紧,
  建议 `sleep 20` → `sleep 45`,或对 #1 的 grep 加重试(最多 3 次、间隔 15s)。
- d ✓:failure(OnConnect 未跑)由 #1(无 mirrored)+#2(agent list 空)采到。

---

## 4. 汇总矩阵(23 case × 4 问)

✓ 通过 / △ 通过有备注 / ✗ 缺陷(见该 case 的 diff)

| case | a 前提 | b expect | c collect | d 可采集 | 问题标签 |
|---|---|---|---|---|---|
| VC-S1-01 | ✓ | △ SYS-6 | ✗ SYS-1 | ✓ | L-a |
| VC-S1-02 | △ is_final 兜底即真路径 | ✗ SYS-8 | △ | ✗ limit 1 藏双行 | L-c, L-d |
| VC-S1-03 | ✓ | ✓ | ✗ rc 恒 0+无基线 | ✗ 无行/旧行不可分 | L-a, L-d |
| VC-S2-01 | ✓ | ✓ | ✗ SYS-1+SYS-3 | ✓ | L-a |
| VC-S3-01 | △ 1001 括注过期 | ✓(members 版式待首跑核) | ✗ SYS-1+缺 login | ✓ | L-a, L-b |
| VC-S3-03 | ✓ | ✓ | ✓ | ✓(附今日 bot_sec=0 疑点) | — |
| VC-S3-04 | △ 假设已判明:无失败分支 | ✓ | ✗ SYS-7 | ✓ | L-a(major), L-b |
| VC-S4-01 | ✓ | ✓ | ✓ | ✓ | — |
| VC-S4-02 | ✓ | ✓ | ✗ SYS-1+SYS-3 | ✓ | L-a |
| VC-S4-03 | ✓ | ✓ | ✗ SYS-1 | △ 服务端次序已防 | L-a |
| VC-S4-04 | △ 全 DID 已开录音 | ✓ | ✗ SYS-7+SYS-6 | ✓ | L-a(major) |
| VC-S5-01 | ✓ | ✗ missed_reason 漂移;OFFERED≥2 时序假设 | △ 窗口偏紧 | △ | L-c, L-d |
| VC-S6-01 | ✓ | ✓(<5s→SHORT_ABANDONED 边界) | ✓ | ✓ | — |
| VC-S7-01 | ✓ | ✗ SYS-2 | △ rc 恒 0 | ✓ | L-c, L-a |
| VC-S7-02 | ✗ SYS-4(amy/1000) | ✗ SYS-2 | ✗ SYS-1 | ✓ | L-b(major), L-c, L-a |
| VC-S7-03 | ✓ | ✓ | ✗ SYS-3 | ✓ | L-a |
| VC-S7-04 | ✓ | ✗ SYS-2(+发送侧事件为推断) | ✓ | ✓ | L-c |
| VC-S8-01 | ✓ | ✗ SYS-8 | ✓ | ✓ | L-c |
| VC-S9-01 | ✓ | ✓ | △ rc 恒 0 | ✓ | L-a(轻) |
| VC-S9-02 | ✓ | ✓ | ✗ SYS-5 | ✓ | L-a |
| VC-S12-01 | ✓ | ✓ | ✓ | △ 启动窗竞态需 members 旁证 | L-d |
| VC-S12-02 | ✓ | ✓ | ✗ SYS-1 | ✗ 计数无法隔离 | L-a, L-d |
| VC-S12-03 | ✓ | ✓ | ✗ SYS-1+时序 | ✓ | L-a |

**统计**:23 case 中,4 个四问全过(S3-03、S4-01、S6-01——S4-01 全 ✓,S6-01 全 ✓,S3-03 全 ✓;S9-01 仅轻微 rc);
b 列漂移 7 个(SYS-2 ×3、SYS-8 ×2、S5-01 missed_reason、S7-02);c 列缺陷 15 个(SYS-1 为最大宗);
d 列缺口 4 个(S1-02、S1-03、S12-01、S12-02)。全部修订均为账本文本改动,无需改源码。
