# 数据表覆盖清单(tables.md)

来源:`internal/store/migrations/00001–00013` 的全部 CREATE TABLE / CREATE VIEW(注意 `disposition_categories` 由 00010 创建、00012 删除,当前 schema 不含它;`goose_db_version` 由 goose 自建)。写/读路径给 sqlc 源(`internal/store/sql/*.sql`)行号 + 关键 Go 调用方;API handler 给 `internal/httpapi` 行号;UI 页面给 `web/src/routes|components` 文件。

| 表 | 写路径 | 读路径 | API handler | UI 页面 | 覆盖场景 | 证据等级 | 备注 |
|---|---|---|---|---|---|---|---|
| users(00001:10) | sql/users.sql:4(cmd/aicc/useradd.go);internal/seed/seed.go:200-204 | sql/users.sql:9,12,15;:18,:21(passwd/last_login) | Login auth_handlers.go:25;GetMe :67;ListUsers agent_handlers.go:335 | login.tsx;_app.admin.agents.tsx | S11 | [FACT] | status=SUSPENDED 无任何写入方(无停用 API),见 enums.md |
| sessions(00001:24) | sql/sessions.sql:4(auth login) | sql/sessions.sql:10;删除 :15,:18,:21(purgeSessions main.go:361-379) | Login/Logout auth_handlers.go:25,:57 | login.tsx(cookie 由浏览器持有) | S10, S11 | [FACT] | cookie 名来自 cfg.SessionCookie(auth_handlers.go:85) |
| settings(00001:39) | NOT FOUND | NOT FOUND | NOT FOUND | NOT FOUND | UNCOVERED | [FACT] | 建表后零引用;2026-08-20:处置仍待决(唯一残留决策) |
| seq_blocks(00001:47;初始行 :52) | sql/seq.sql:4(ReserveSeqBlock,main.go:458-462) | 同一 UPDATE…RETURNING | 无(内部) | 无 | S10 | [FACT] | SSE 全局 seq 的 hi/lo 块(events/seq.go:14) |
| extensions(00002:16) | sql/telephony.sql:4,:15,:21,:24;seed.go:212-216(kind='AGENT') | sql/telephony.sql:9,:12;luacc.directory(00002:157-165)→ freeswitch/scripts/aicc_xml.lua:103 | ListExtensions catalog_handlers.go:43;Create :48;Update :57;Delete :67 | _app.admin.extensions.tsx | S4, S11 | [FACT] | 目录鉴权即注册的口令来源 |
| agents(00002:29;00007 唯一分机索引;00012:27 删 wrap_up_time_sec) | sql/agents.sql:4,:15,:21;seed.go:219-225 | sql/agents.sql:9,:12,:40(roster) | CreateAgent agent_handlers.go:252;Update :267;Delete :279;ListAgents :229 | _app.admin.agents.tsx;_app.supervisor.agents.tsx | S11 | [FACT] | callcenter_name 是 mod_callcenter 侧标识(agents/service.go:451-461 校验) |
| agent_states(00002:47;00010:23 加 wrap_up_call_id) | sql/agents.sql:50(SavePresence) | sql/agents.sql:47(LoadPresence);Restore(agents/service.go:505-523) | GetAgentPresence agent_handlers.go:221;Login/Logout/Ready/NotReady :67-:130 | _app.tsx(状态条);softphone-bar.tsx | S4, S11, S12 | [FACT] | 每坐席一行,持久化真值;内存 live 为快路径(service.go:129-131) |
| agent_state_logs(00002:61) | sql/agents.sql:61(LogStateChange,service.go:586);:66(补 exited_at);seed.go:110-116 | sql/ledger.sql:230(my-day CTE) | GetMyDay ledger_handlers.go:94 | _app.agent.index.tsx(今日概览) | S11 | [FACT] | 写失败不阻塞状态变更(service.go:586-590) |
| queues(00002:73) | sql/telephony.sql:27,:44,:55;seed.go:248-… | sql/telephony.sql:38,:41,:77;luacc.queues(00002:183-201)→ aicc_xml.lua:154、aicc_queue.lua:36 | ListQueues catalog_handlers.go:75;Create :80;Update :89;Delete :99 | _app.supervisor.queues.tsx;_app.admin.routing.tsx | S3, S5, S6 | [FACT] | overflow jsonb 在视图内摊平(00002:197-199) |
| queue_agents(00002:104) | sql/telephony.sql:58-61(upsert),:64(删) | sql/telephony.sql:69(ListQueueAgents);catalog/service.go:346(desiredTiers) | StaffQueue catalog_handlers.go:108;Unstaff :126;ListQueueAgents :103 | _app.supervisor.queues.tsx | S3, S11 | [FACT] | 与 mod_callcenter tier 双向收敛(catalog/service.go:236-315)——S3 对账的 DB 侧 |
| dids(00002:121;00003/00008 改 flow 约束) | sql/telephony.sql:83,:95,:102;seed.go(demoNumbers 95001/95002) | sql/telephony.sql:89,:92;luacc.dids(00002:171-180)→ aicc_inbound.lua:44;orchestrator findDID(aicall/orchestrator.go:427-438) | ListDIDs catalog_handlers.go:134;Create :139;Update :148;Delete :158 | _app.admin.numbers.tsx | S1, S8 | [FACT] | flow_id ON DELETE RESTRICT(00008:17-20) |
| trunks(00002:137) | NOT FOUND | NOT FOUND | NOT FOUND | NOT FOUND | UNCOVERED | [FACT] | 建表后零引用(仅迁移命中);direction 枚举 3 值全死;2026-08-20 决议:需要(中继号管理)→ TASKS W8 |
| flows(00004:7) | sql/flows.sql:10,:15,:26(publish);cmd/aicc/flowadd.go(唯一入口) | sql/flows.sql:4,:7,:37 | NOT FOUND(openapi paths 无 /flows) | NOT FOUND | S1 | [FACT] | 流程管理现仅 CLI;2026-08-20 决议 D3:要做(含 UI 上传/编辑 spec)→ TASKS W3(/admin/bots) |
| flow_revisions(00004:19) | sql/flows.sql:21(publish 时插入) | sql/flows.sql:32(PublishedSpec join)→ aicall/orchestrator.go:226 | NOT FOUND | NOT FOUND | S1 | [FACT] | 仅发布修订可被呼叫使用 |
| cdrs(00005:7) | sql/ledger.sql:4(InsertCDR ← telephony/cdr.go:81 与 aicall/ledger.go:184 与 seed/history);:104(MarkRecorded ← cdr.go:137) | sql/ledger.sql:21,:24,:37;报表 :133-:161;my-day :222;contacts last_call :26 | ListCDRs ledger_handlers.go:20;ListMyCDRs :51;GetCDR :122;Report* :252-:272 | _app.admin.cdr.index.tsx;_app.admin.cdr.$callId.tsx;_app.agent.calls.tsx;_app.admin.reports.tsx | S1, S2, S3, S4, S6 | [FACT] | 归属规则:有 bot 腿且未盖 bot-share 戳→bot 侧写(cdr.go:79);disposition 列自 00010 起无人写有效值(00010 注释:6) |
| transcripts(00005:56;00009 加列/改 speaker) | sql/ledger.sql:51(InsertTranscriptLine ← transcript/actor.go:253) | sql/ledger.sql:57,:62(增量 after seq) | GetCallTranscript transcript_handlers.go:29 | components/live-transcript.tsx;_app.admin.cdr.$callId.tsx | S1, S9, S10 | [FACT] | seq 每呼叫独占分配(actor.go:226),partial 不入库 |
| recordings(00005:67) | sql/ledger.sql:68(InsertRecording ← telephony/cdr.go:126-133) | sql/ledger.sql:73,:76 | ListCallRecordings recording_handlers.go:64;GetRecordingAudio :81 | components/recording-player.tsx;_app.admin.cdr.$callId.tsx;_app.agent.calls.tsx | S4(VC-S4-04 已挂,TODO) | [FACT] | 2026-08-20 勘误:场景已挂 VC-S4-04;坐席回放/越权另由 TASKS T6.1 起草 |
| quality_reviews(00005:82) | sql/ledger.sql:107(CreateRecordingReview recording_handlers.go:118) | sql/ledger.sql:112(ListCallReviews :152) | recording_handlers.go:118,:152 | NOT FOUND | UNCOVERED | [FACT] | API 存在但 web 零引用——写得进、看不见;2026-08-20 决议 D1:UI defer 下一期 |
| callbacks(00005:97;00006 加 CLAIMED) | sql/ledger.sql:79(InsertCallback ← aicall/actions.go:112);:90,:115(claim/complete ← ledger_handlers.go:171,:196) | sql/ledger.sql:84 | ListCallbacks ledger_handlers.go:156;Claim :171;Complete :196 | _app.agent.callbacks.tsx | S3(VC-S3-04 已挂,TODO) | [FACT] | 2026-08-20 勘误:take_message 全链已挂 VC-S3-04 |
| queue_events(00005:114) | sql/ledger.sql InsertQueueEvent ← telephony/cdr.go QueueEvent();seed.go:107 | **QueueEventsByCall ← httpapi ListCallQueueEvents(`GET /calls/{callId}/queue-events`,主管)** | NOT FOUND | NOT FOUND | S3, S5, S6 | [FACT] | **2026-08-24 C7 已建读路径**(最小面积:一通电话的队列旅程,无面板无聚合)。报表仍读 cdrs —— 那是**结果**,本表是**过程**(跨队列、逐次派单、每次的等待)。cdr.go 那句失实注释已改 |
| audit_logs(00005:127) | sql/ledger.sql:100(auditTrail 中间件 httpapi/audit.go:33) | NOT FOUND | NOT FOUND | NOT FOUND | UNCOVERED | [FACT] | 只写不读;2026-08-20 决议 D5:补检索 → TASKS W4(/admin/audit) |
| dispositions(00010:31;00012 扁平化为 4 词) | 迁移内种子 00012:40-44 | sql/ledger.sql:171(ListDispositions),:174(校验) | ListDispositions ledger_handlers.go:80;AgentWrapUp agent_handlers.go:172 | _app.agent.index.tsx(ACW 卡片) | S4 | [FACT] | 无管理 CRUD;2026-08-20 决议 D4:固定词表为产品决策(TASKS W5 入档),"恰好 4 词"断言转正式 |
| wrap_ups(00010:63;00012 删 category;00013 加 is_confirmed) | sql/ledger.sql:180(OpenWrapUp ← agents/service.go:289);:188(确认 ← agent_handlers.go:172) | sql/ledger.sql:198,:204;my-day :240 | GetAgentWrapUp agent_handlers.go:132;AgentWrapUp :172 | _app.agent.index.tsx(确认卡) | S4 | [FACT] | 平台开单(is_confirmed=false)/坐席确认(true)的两段写 |
| contacts(00011:19) | sql/contacts.sql:7,:12,:19 | sql/contacts.sql:22,:26(带 last_call_at),:38 | ListContacts contact_handlers.go:31;Create :47;Update :58;Delete :69 | _app.agent.contacts.tsx;_app.agent.index.tsx(来电卡) | UNCOVERED | [FACT] | S1–S12 无联系人场景;需补场景 |
| luacc.directory(视图,00002:157) | (由 extensions/agents 派生) | freeswitch/scripts/aicc_xml.lua:103 | — | —(FreeSWITCH 目录) | S4, S11 | [FACT] | 分机注册鉴权 + auto_answer |
| luacc.dids(视图,00002:171) | (由 dids/queues 派生) | freeswitch/scripts/aicc_inbound.lua:44;aicc_queue.lua:89 | — | — | S1, S8 | [FACT] | |
| luacc.queues(视图,00002:183) | (由 queues 派生) | freeswitch/scripts/aicc_xml.lua:154;aicc_queue.lua:36 | — | — | S3, S5, S6 | [FACT] | mod_callcenter 队列配置的唯一来源 |
| goose_db_version(goose 自建) | goose(store.Migrate,main.go:108) | goose | — | — | S12(启动迁移) | [FACT] | 2026-08-20 实查(F9):version_id 13/12/11 均 is_applied=t,与 00013 对齐 |

## 缺口汇总

### 需删除 →(2026-08-20 决议更新)
- settings(00001:39)— 全仓零读零写。**仍待决**(唯一残留决策,DECISIONS-pending.md)。
- trunks(00002:137)— ~~删除候选~~ **已决 S3:需要,中继号要管理** → TASKS **W8**(契约评审先行)。

### 需实现 →(2026-08-20 决议更新)
- ~~queue_events **读路径**~~ —— **2026-08-24 C7 已闭合**:`GET /calls/{callId}/queue-events`(主管专属)。无人读期间表中已积下 273 行 OFFERED、七通被派单 10–33 次且每通只派给同一坐席,见 `artifacts/C7/verdict-2026-08-24.md`。
- quality_reviews **UI** — **已决 D1:defer 下一期**(TASKS W6 记录)。
- audit_logs **读路径/查询 API** — **已决 D5:要做** → TASKS **W4**(`/admin/audit`,参考 ui-test)。
- flows / flow_revisions 的 **API 与 UI** — **已决 D3:要做,含 UI 上传/编辑 spec** → TASKS **W3**(`/admin/bots`)。

### 需补场景 →(2026-08-20 勘误)
- ~~recordings~~ 已挂 VC-S4-04;坐席回放/越权由 TASKS T6.1 起草。
- ~~callbacks~~ 已挂 VC-S3-04(原笔误 VC-SX-CB-01)。
- contacts(来电弹屏按号码查联系人 + 编辑回写)— 仍缺,TASKS T6.2 起草。
- quality_reviews — 随 D1 defer 下一期。
