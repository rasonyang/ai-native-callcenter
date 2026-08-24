# 决策记录(产品/契约取舍)

> 来源:ledger-audit.md(SYS-6/SYS-8、coverage 缺口汇总)+ TASKS 三角色旅程走查。
> **2026-08-20 两轮拍板后:D1–D7 全部已决**(见各项【决议】行),实现项落 TASKS.md 阶段 7 的 W 系列。
> 第二轮补充决议见文末 §补充。**~~唯一残留:settings 死表的处置~~ 已于 2026-08-24 决完:删表,配置只有一个家(`AICC_*` + `.env.example` 登记册);它唯一有设计读者的那一项 `retentionDays` 同日建成 `AICC_RECORDING_RETENTION_DAYS`(默认 0=永久保留)。trunks 同日撤回并删表(见 S3)。**
> **本文件已无残留决策。**

## D1 质检评审 UI(G-B3)
**【决议 2026-08-20】放下一期,本期不做。** quality_reviews API 保留;tables.md 缺口标注 defer;
T6 不为它起草 case。
- 现状:API 全套在(POST /recordings/{id}/reviews、ListCallReviews,recording_handlers.go:118/:152),
  **web 零引用**(tables.md 实证:写得进、看不见)。
- 选项:A. 补主管侧 UI(CDR 详情页挂评审面板);B. 记录为"API-first,UI 后续里程碑";C. 降级出契约。
- 影响:不决则 quality_reviews 表永远只进不出。

## D2 主管实时转写旁听(G-B4)
**【决议 2026-08-20】B —— by design。** 实时转写是坐席工具,主管看事后 CDR 详情。
S9 不长主管视角 case;TASKS 旅程 B8 行记为设计决定。

## D3 流程(flow)管理面(G-C6)
**【决议 2026-08-20】要做。** 交互参考 `~/workspaces/github/ui-test`(admin/bots:列表 + $flowId 详情——
spec JSON 查看器、节点可达性分析、transitions 摘要、publish 对话框);**路由定为 `/admin/bots`**(owner 指定)。
落 TASKS W3(spec-first:契约先行)。CLI flowadd 保留为自动化入口。

## D4 处置词(dispositions)管理(G-C7)
**【决议 2026-08-20】固定词表。** 不做 CRUD;在设计文档记录"固定词表是产品决策"(TASKS W5)。
VC-S4-03 的"恰好 4 行"断言由此转为正式契约级断言。

## D5 audit_logs 检索(G-C8)
**【决议 2026-08-20】要做。** 参考 `~/workspaces/github/ui-test`(admin/audit.tsx:分类过滤由
action 前缀派生);**路由定为 `/admin/audit`**(owner 指定)。落 TASKS W4(spec-first)。
~~settings/trunks 两张死表的处置不在本决议内(仍开放)~~ —— **2026-08-24 两张都已决:双双删表**(`00021` trunks、`00022` settings)。共同的理由是同一条:**它们各自代表的东西不在这个应用手里** ——网关归交换机的 sofia profile XML,配置归 `AICC_*` 与 `.env.example` 登记册。

## D6 契约内 10 个零生产者 SSE 类型(events.md 需实现节)
**【决议 2026-08-20】全部实现。** 落 TASKS **W7**,按实现难度分四组推进:
①CALL_RECORDING_STARTED/STOPPED(RECORD_START/STOP 已归一化,补 coordinator→Hub 一跳);
②DEVICE_REGISTERED/UNREGISTERED(信号已达 ObserveDevice,补区分发布);
③PARTY_DIALING、CALL_USER_DATA、SYSTEM_LINK(全新 publish 点;SYSTEM_LINK 挂 esl.Link 断连/重连);
④BOT_SESSION_STARTED/INTERRUPTED/ENDED(需给 aicall 引入 Hub 依赖——架构面,W7 内单独评审)。

## D7 死代码/死配置的处置方向(fsm-edges 需删除节 + 审计 SYS-8)
- ① Call 状态 `ENDING`(call.go:23,无赋值方)——**【决议 2026-08-20】删除**(有 ENDED 即可)。
  契约 CallState 枚举同步删,breaking,走 `make api-breaking`;落 TASKS C4 执行。
- ② `Presence.RingNoAnswer` 全链(state.go:189-194、service.go:342-346)——
  **【决议 2026-08-20】实现,含 missed_reason 条件互斥修复**(cdr.go:255-258 的 ABANDONED_RINGING
  需 BridgedAt≠0 与语义矛盾)。落 TASKS W2;S5-01 在实现合入前先按旧行为执行留证,合入后改写重跑。
- ③ 转写状态 `ENDED`——**【决议 2026-08-20:先删除】→【2026-08-20 晚 T3.4 执行推翻前提,待重议】**:
  "无发布方"只对 SSE 成立;GetCallTranscript 对已收官呼叫**合成 state=ENDED**(transcript_handlers.go:69),
  是在用的收官快照语义(VC-S9-02 实测)。重议选项:A. 保留 ENDED 为收官快照值(撤销删除,契约留值,
  文档注明"快照专用、不经 SSE");B. 仍删除,handler 收官改返回 STOPPED(行为变化,UI 判断需核)。
  C4 中的此项挂起,等裁决。
- ④ `Profile.TranscribeModel` 恒空(profile.go:37,realtime.go:367 分支不可达——SYS-8 的根)——
  **【决议 2026-08-20】实现:端对端模型本身自带 transcript,stock profile 默认开。** 落 TASKS W1;
  合入后 S1-02/S8-01 的留证条款转正式断言(CUSTOMER|MODEL ≥1)并重跑。

---

## §补充(2026-08-20 第二轮小项决议)

| # | 事项 | 决议 | 落点 |
|---|---|---|---|
| S1 | W3 flows 管理面范围 | **UI 也要上传/编辑 spec**(不止查看+publish) | TASKS W3:契约含 POST /flows、PUT /flows/{id}(草稿)、POST /flows/{id}/publish |
| S2 | qwen realtime 是否支持模型自带转写 | **支持**(owner 确认:response.text.delta 流式返回文本片段) | TASKS W1:两个 stock profile 都默认开;实现期以 AICC_LIVE_PROVIDER_TEST 复核 |
| S3 | trunks 死表处置 | ~~需要:中继号要管理~~ → **2026-08-24 撤回:删表,只做只读状态**(owner 直裁) | **W8 撤销**;`00021` 删表,中继状态经 `sofia status` 只读呈现在 Overview。理由:网关定义在交换机自己的 sofia profile XML 里、profile 加载时读取,应用不写那个文件 —— 要让一行变成网关,得再加 `luacc.trunks` 视图、让 `aicc_xml.lua` 接管 configuration 的 sofia 部分、每次改动 rescan,为一个单机部署只有一个、部署时配一次的东西。而 **D6 已经定了 gateway 是系统级配置**,一个编辑它的界面正说反了,且兑现不了承诺(W11.1 改名时仓内只能改一半)。settings 表仍未决(唯一残留) |
| S4 | 非 seed 账号清理 | **清理;只保留 wei、agent、supervisor、admin;seed 名单(demoPeople)一起改**(2026-08-20 补充:删 amy/ben 两行,seed 不再建回) | TASKS **W9**;ben 是第 2 坐席——清理排在阶段 3–6 之后;S11-02 的 amy 依赖随 W9 改写 |
| S5 | C10(202/204 契约核对) | **放第二期,第二期需要** | TASKS C10 标记 defer;本期账本 expect 维持 202 |
