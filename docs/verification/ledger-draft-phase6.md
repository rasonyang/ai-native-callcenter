# 阶段 6 起草:旅程缺口的新用例

> 起草于 2026-08-21,承接阶段 2–5 的执行经验。
> **每条 expect 的 file:line 均已对着当前代码核实过**(核实日期同上)。
> ~~本文件是**待批准的草案**,批准后按原样并入 `ledger.yaml`。~~
> **已于 2026-08-22 按原样并入 `ledger.yaml`**(账本 28 → 39 条;新场景 S13/S14,VC-S12-04 进既有 S12 段)。
> 本文件自此是**起草过程的留档**,不再是待办;账本以 `ledger.yaml` 为准,两处若有出入以 `ledger.yaml` 为准。
> 并入时只做了一处规格化:VC-S14-04 的一次性键 `collect_note` 折进了它的 `note`(账本的既有可选键是 `note`)。
>
> 编号:`S13` = 坐席与主管旅程缺口,`S14` = 管理员旅程缺口 + 坐席外呼,
> `S12-04` 并入既有恢复泳道。(VC-S13-04 是主管缺口 G-B1,放在 S13 里是因为它与
> VC-S13-03 的 my-day 共用同一批对账手法,分开反而割裂。)
>
> **本批覆盖 T6.1 / T6.2 / T6.3 / T6.4 / T6.5 / T6.7 / T6.8 / T6.11 / T6.12,共 11 条。**
> **未覆盖的三项,原因如下 ——**
> - **T6.6 只做了三分之二**:G-C2(分机,VC-S14-01)与 G-C5(配员,VC-S14-02)已起草,
>   **G-C1(账号/坐席全生命周期:建坐席→绑分机→签入→接听)未起草**。
>   它要串起 `POST /agents`、分机绑定、签入与一通真实来电,跨度大于其余各条,
>   且与 W9(账号清理,只保留 wei/agent/supervisor/admin)直接冲突 ——
>   建号又删号会和 W9 的名单打架。**建议 W9 落地后再起草**,届时用例可直接以
>   "新建一个 W9 之外的临时坐席并在用例末尾删除"为形态。
> - **T6.9 / T6.10 被实现阻塞**:分别等 W1/W2 与 W7 合入后才能改写既有 case 与补最小断言,
>   现在起草只会写出一批需要重写的东西。
>
> **通用约定(阶段 2–5 的教训,已写进每条 collect)**
> - SSE 抓流一律 `--max-time 1800`。阶段 4 用 120 秒,人还没拨完号就过期了(C23③)。
> - 数 SSE 事件一律 `grep -c '"type":"X"'`,**不要**裸串 —— 每个事件有 `event:` 与 `data:` 两行,
>   裸串会把一个数成两个(C23②)。
> - 会话按角色选:队列/号码/分机的写操作要 **ADMIN**;`/calls/waiting` 主管可读(C12 已修);
>   `/cdrs/mine`、`/agent/*` 要坐席身份(C23①)。
> - 等 CDR 落库要**轮询**而不是固定 sleep。阶段 4 我按 5 秒抓,抓到的是上一通(见 VC-S6-01 判定)。

---

## T6.12 · VC-S12-04 — 交换机侧状态真丢失后的重建

**缺口来源**:T5.3 执行发现 —— S12-03 通过了,但 `added=0`,mod_callcenter 自 `callcenter.db`
自行恢复,应用的 reconcile 只是**确认**而非**恢复**。真正的重建路径从未被压到。

```yaml
- id: VC-S12-04
  scenario: S12
  layer: signaling
  precondition: "无进行中呼叫;wei 与 ben 均已签入 READY;允许重启 FreeSWITCH。
    本用例与 S12-03 的差别是:重启前把交换机侧状态真正清空,逼应用去重建"
  steps:
    - who: agent
      do: "记录重建前的两侧账面:执行 collect 第 1、2 条"
    - who: human
      do: "停 FreeSWITCH(`sudo /usr/local/freeswitch/bin/freeswitch -stop`),
        停稳后执行 collect 第 3 条清空 mod_callcenter 的库,再启动 FreeSWITCH"
    - who: agent
      do: "执行其余 collect,判定应用是否把 agents / tiers / 状态推了回去"
  collect:
    - "/usr/local/freeswitch/bin/fs_cli -x 'callcenter_config agent list' | awk -F'|' 'NR>1&&NF>5{print $1\"|\"$6}'"
    - "sqlite3 /usr/local/freeswitch/db/callcenter.db 'select queue, agent from tiers;'"
    - "sqlite3 /usr/local/freeswitch/db/callcenter.db 'delete from agents; delete from tiers;' && echo cleared"
    - "sleep 45; LOG=$(ls -t logs/aicc-*.log | head -1); grep -E 'agent presence mirrored to the switch|staffing (reconciled|already matched)|registrations reconciled' \"$LOG\" | tail -8"
    - "/usr/local/freeswitch/bin/fs_cli -x 'callcenter_config agent list' | awk -F'|' 'NR>1&&NF>5{print $1\"|\"$6}'"
    - "/usr/local/freeswitch/bin/fs_cli -x 'callcenter_config tier list' | awk -F'|' 'NR>1&&NF>=5{print $2\"|\"$1}'"
  expect: |
    第 3 条输出 cleared,且清空后 agent list 为空(前置成立的证据)
    日志出现 "agent staffing reconciled" 且 **added= 为正**(catalog/service.go:286 附近的 converge
    真的添加了 tier)——这是本用例与 S12-03 的唯一分野:S12-03 只拿到 already matched/added=0
    重建后 agent list 含 agent-wei|Available 与 agent-ben|Available
    重建后 tier list 含 agent-wei|support-en@<domain>、agent-wei|support-zh@<domain>、agent-ben|support-zh@<domain>
  failure_looks_like: |
    日志照常打出 "mirrored" 与 "reconciled",但 agent list 仍为空 ——
    OnConnect 钩子跑了、converge 也跑了,可它读到的"期望"来自 DB 而"现实"来自一个空交换机,
    若 converge 只在"现实多于期望"时动作(只删不加),重建就永远不会发生:
    app 里人人 READY,switch 里谁都不存在,每个入队呼叫等到超时,页面上毫无异常。
    这正是 S12-03 的 failure_looks_like 描述、而 S12-03 无法触及的那个失败。
  evidence_dir: docs/verification/artifacts/VC-S12-04/
  status: TODO
  refs: ["internal/catalog/service.go:286", "internal/agents/service.go:595", "cmd/aicc/wiring.go:99", "internal/esl/link.go:55"]
  note: "清空前务必先跑 collect 第 2 条留底,便于失败后手工恢复。
    C1 的陈旧行 support-en@192.168.31.176 也在该库,清空会一并抹掉 ——
    C1 已判定'症状潜伏、缺陷未动'(见 VC-S3-02/verdict.md 追记),不再依赖该行复现,故可接受"
```

---

## T6.5 · VC-S13-01 — 坐席看到的等待名单只是自己配员的队列

**缺口 G-A1**:`/calls/waiting` 的坐席视角(按 `QueuesForAgent` 过滤)从未被验证 ——
既有的 S3-01/S6-01/S12-01 全是主管视角(主管现在看全部队列,C12 已修)。

```yaml
- id: VC-S13-01
  scenario: S13
  layer: api
  precondition: "wei 配员 support-en 与 support-zh;ben **只**配员 support-zh(实测 tier 现状即如此);
    两人均 READY;两条队列各有一名等待者"
  steps:
    - who: agent
      do: "确认两人的配员差异:执行 collect 第 1 条"
    - who: human
      do: "拨 95001(→support-en)听到保持音后【不要挂断】;
        用第二路主叫拨 95002(→support-zh)同样保持等待。
        若只有一路主叫,则先后拨两次、第一次不挂断"
    - who: agent
      do: "执行其余 collect,判定三种视角各自看到什么"
  collect:
    - "/usr/local/freeswitch/bin/fs_cli -x 'callcenter_config tier list' | awk -F'|' 'NR>1&&NF>=5{print $2\"|\"$1}' | sort"
    - "curl -s -b /tmp/vc-wei.jar http://127.0.0.1:8080/api/v1/calls/waiting | jq -c '[.items[]|{queueName, fromNumber}] | sort_by(.queueName)'"
    - "curl -s -b /tmp/vc-ben.jar http://127.0.0.1:8080/api/v1/calls/waiting | jq -c '[.items[]|{queueName, fromNumber}] | sort_by(.queueName)'"
    - "curl -s -b /tmp/vc-sup.jar http://127.0.0.1:8080/api/v1/calls/waiting | jq -c '[.items[]|{queueName, fromNumber}] | sort_by(.queueName)'"
  expect: |
    wei(配员两条队列):两条等待者都在 —— support-en 与 support-zh 各 1
    ben(只配员 support-zh):**只有 support-zh 那一条**,support-en 的等待者对他不可见
      (call_handlers.go:72 以 QueuesForAgent 取队列,再交给 WaitingCalls 过滤)
    supervisor:两条都在(C12 修复后走 AllWaitingCalls,call_handlers.go 的 RoleSupervisor 分支)
    三者对 support-zh 那一条的 fromNumber 一致 —— 同一个等待者,三种视角
  failure_looks_like: |
    ben 也看到了 support-en 的等待者 —— 过滤失效,坐席被展示了自己接不到的电话:
    他会去点、去等,而队列永远不会把那通派给他。反向的失败同样静默:
    wei 只看到一条,于是他配员的另一条队列在他屏幕上永远是空的,
    电话在那里排队而唯一能接的人不知道。
  evidence_dir: docs/verification/artifacts/VC-S13-01/
  status: TODO
  refs: ["internal/httpapi/call_handlers.go:52", "internal/httpapi/call_handlers.go:72", "internal/telephony/waiting.go:168", "internal/httpapi/agent_handlers.go:27"]
  note: "本用例是 C12 修复后的配套:主管看全部、坐席看自己的,两条线要同时成立才算对。
    ben 现只在 support-zh 的 tier 上(2026-08-21 实测),不必额外配置"
```

---

## T6.1 · VC-S13-02 — 坐席回放自己的录音,且回放不了别人的

**缺口 G-A4**:①列表只见自己 ②`hasRecording` 可播 ③**越权拒绝**。
靶子已备好:T3.6 产出的 `primary_agent=ben` 录音 `01a02276-2a62-710d-ad0e-b9273d2887da`
(call `01a02275-8884-7617-95ca-e9dbd63ae5e0`,zh,39 秒)。

```yaml
- id: VC-S13-02
  scenario: S13
  layer: api
  precondition: "库中存在两通有录音的已收官呼叫,主责坐席分别是 ben 与 wei,且互不参与对方那通。
    2026-08-21 实测可用靶子:ben 的 01a02275-…(recordingId 01a02276-2a62-…)。
    wei 的靶子取其最近一通 has_recording=t 的 CDR"
  steps:
    - who: agent
      do: "取两个靶子并确认归属互斥:执行 collect 第 1 条"
    - who: agent
      do: "执行其余 collect,判定列表归属、可播、越权拒绝三件事"
  collect:
    - "docker exec -i aicc-postgres psql -U aicc -d aicc -Atc \"select c.call_id, u.username, c.has_recording, r.id from cdrs c join agents a on a.id=c.primary_agent_id join users u on u.id=a.user_id join recordings r on r.call_id=c.call_id where c.has_recording order by c.started_at desc limit 6\""
    - "curl -s -b /tmp/vc-ben.jar 'http://127.0.0.1:8080/api/v1/cdrs/mine?limit=50' | jq -c '{total, mine_only: ([.items[].callId] | length)}'"
    - "BEN_CALL=<ben 的 call_id>; curl -s -b /tmp/vc-ben.jar 'http://127.0.0.1:8080/api/v1/cdrs/mine?limit=50' | jq -c --arg c \"$BEN_CALL\" '[.items[]|select(.callId==$c)|{callId,hasRecording}]'"
    - "WEI_CALL=<wei 的 call_id>; curl -s -b /tmp/vc-ben.jar 'http://127.0.0.1:8080/api/v1/cdrs/mine?limit=50' | jq -c --arg c \"$WEI_CALL\" '[.items[]|select(.callId==$c)] | length'"
    - "BEN_REC=<ben 的 recording_id>; curl -s -o /tmp/vc-ben-audio.wav -w 'ben→自己 http=%{http_code} bytes=%{size_download} type=%{content_type}\\n' -b /tmp/vc-ben.jar http://127.0.0.1:8080/api/v1/recordings/$BEN_REC/audio"
    - "WEI_REC=<wei 的 recording_id>; curl -s -b /tmp/vc-ben.jar http://127.0.0.1:8080/api/v1/recordings/$WEI_REC/audio -w '\\nben→别人 http=%{http_code}\\n' | head -c 200"
    - "WEI_REC=<wei 的 recording_id>; curl -s -o /dev/null -w 'sup→任意 http=%{http_code}\\n' -b /tmp/vc-sup.jar http://127.0.0.1:8080/api/v1/recordings/$WEI_REC/audio"
  expect: |
    /cdrs/mine(ben):**含**他自己那通,**不含** wei 那通(第 4 条长度=0)
      —— 归属线是 agentWasOnCall:primary 或在 agent_ids 里(recording_handlers.go:55-61)
    ben 取自己那通的音频:http=200,bytes>0,content_type 为 audio/*
    ben 取 wei 那通的音频:**http=403**,body 含 "not one of your calls"
      (recording_handlers.go:46-50 与 :91 的 mayHearCall 分支)
    supervisor 取同一条:http=200 —— 主管审阅任何人的通话
      (recording_handlers.go:33 的 Role.AtLeast(RoleSupervisor) 早退)
  failure_looks_like: |
    ben 取到了 wei 那通的音频(200 + 字节流)—— 越权成立,任何坐席可以听遍全公司的通话录音,
    而访问日志里它和一次正常回放毫无区别。反向的失败同样要命:
    ben 取自己那通得到 403,坐席无法回听自己刚接的电话,而唯一能自查的手段就此关闭。
  evidence_dir: docs/verification/artifacts/VC-S13-02/
  status: TODO
  refs: ["internal/httpapi/recording_handlers.go:27", "internal/httpapi/recording_handlers.go:55", "internal/httpapi/recording_handlers.go:81", "internal/httpapi/ledger_handlers.go:19"]
  note: "collect 里的 <…> 占位符执行时由第 1 条的输出填入。
    音频取回用 -o 落盘再看 size,不要直接管道到 jq —— 那是二进制流"
```

---

## T6.3 · VC-S13-03 — 我的一天:汇总数字与明细对得上

**缺口 G-A5**:`ReportAgentToday` 的 CTE(ledger.sql:215-245)从无 case,
坐席首页那几个数字没有任何东西保证它们和 CDR 明细一致。

```yaml
- id: VC-S13-03
  scenario: S13
  layer: api
  precondition: "wei 今天至少接过 2 通并各自完成 ACW(阶段 3–5 的执行已自然满足);
    本用例只读,不产生新呼叫"
  steps:
    - who: agent
      do: "取汇总与明细两侧,逐项对账:执行全部 collect"
  collect:
    - "curl -s -b /tmp/vc-wei.jar http://127.0.0.1:8080/api/v1/reports/me | jq -c ."
    - "docker exec -i aicc-postgres psql -U aicc -d aicc -Atc \"select count(*), coalesce(sum(talk_sec),0) from cdrs c join agents a on a.id=c.primary_agent_id join users u on u.id=a.user_id where u.username='wei' and c.started_at >= date_trunc('day', now())\""
    - "docker exec -i aicc-postgres psql -U aicc -d aicc -Atc \"select count(*), count(*) filter (where is_confirmed) from wrap_ups w join agents a on a.id=w.agent_id join users u on u.id=a.user_id where u.username='wei' and w.created_at >= date_trunc('day', now())\""
  expect: |
    my-day 的 callsHandled 与 talkSec 等于第 2 条的两个数
      —— 同一口径:primary_agent_id 且 started_at 落在当天(ledger.sql:220-226 的 handled CTE)
    my-day 的 wrapUpsOpened 与 wrapUpsConfirmed 等于第 3 条的两个数
      —— filings CTE 按 created_at 计,且 confirmed 用 FILTER (WHERE is_confirmed)(ledger.sql:235-241)
    三个数都 ≥ 阶段 3–5 实际执行过的通话数(不做上界断言:环境里还有 seed 与他人的行)
  failure_looks_like: |
    汇总比明细多或少,而两边都不报错 —— 坐席首页的数字与他自己的通话列表对不上,
    却没有任何提示说明哪个是对的。最隐蔽的一种:talkSec 用了 sum(total_sec) 而不是 sum(talk_sec),
    数字始终偏大且始终"看起来合理",直到有人拿它算工时。
  evidence_dir: docs/verification/artifacts/VC-S13-03/
  status: TODO
  refs: ["internal/store/sql/ledger.sql:215", "internal/store/sql/ledger.sql:220", "internal/store/sql/ledger.sql:235", "internal/httpapi/agent_handlers.go:252"]
  note: "路径与字段已对契约核实(2026-08-21):operationId getMyDay = **GET /reports/me**,
    响应 schema AgentToday 含 callsHandled / talkSec / wrapUpsOpened / wrapUpsConfirmed
    及 avgHandleSec / avgWrapUpSec / confirmedPct / occupancyPct / signedInSec / wrapUpSec。
    后六个是派生量,本用例不对它们断言——它们该由 T6.7(报表对账)一并覆盖"
```

---

## T6.7 · VC-S13-04 — 报表数字与 CDR 明细逐项对账

**缺口 G-B1**(并入 G-C8):`ReportOverview` / `ReportByQueue` / `ReportDaily`
三条查询从无 case,墙板上的数字没有任何东西保证它们和明细一致。

```yaml
- id: VC-S13-04
  scenario: S13
  layer: api
  precondition: "当天已有若干已收官呼叫(阶段 3–5 的执行已自然满足);只读,不产生新呼叫"
  steps:
    - who: agent
      do: "取报表与明细两侧,按同一时间窗逐项对账:执行全部 collect"
  collect:
    - "FROM=$(date -u +%Y-%m-%dT00:00:00Z); TO=$(date -u -v+1d +%Y-%m-%dT00:00:00Z 2>/dev/null || date -u -d tomorrow +%Y-%m-%dT00:00:00Z); echo \"$FROM $TO\" | tee /tmp/vc-window.txt"
    - "read FROM TO < /tmp/vc-window.txt; curl -s -b /tmp/vc-sup.jar \"http://127.0.0.1:8080/api/v1/reports/overview?from=$FROM&to=$TO\" | jq -c ."
    - "read FROM TO < /tmp/vc-window.txt; docker exec -i aicc-postgres psql -U aicc -d aicc -Atc \"select count(*), count(*) filter (where status='ANSWERED'), count(*) filter (where missed_reason in ('SHORT_ABANDONED','ABANDONED_RINGING','ABANDONED_WAITING')), count(*) filter (where is_contained), count(*) filter (where queue_id is not null) from cdrs where started_at >= '$FROM' and started_at < '$TO'\""
    - "read FROM TO < /tmp/vc-window.txt; curl -s -b /tmp/vc-sup.jar \"http://127.0.0.1:8080/api/v1/reports/queues?from=$FROM&to=$TO\" | jq -c '[.items[]|{queueId,totalCalls}]'"
    - "read FROM TO < /tmp/vc-window.txt; docker exec -i aicc-postgres psql -U aicc -d aicc -Atc \"select queue_id, count(*) from cdrs where started_at >= '$FROM' and started_at < '$TO' and queue_id is not null group by queue_id\""
  expect: |
    overview 的 totalCalls / answeredCalls / abandonedCalls / containedCalls / queueCalls
      五项逐一等于第 3 条的五个数 —— 同一口径,ledger.sql:120-136 的 FILTER 子句即定义
    **abandonedCalls 的口径要特别核**:它数的是 missed_reason 落在三个 ABANDONED* 值里的行
      (ledger.sql:124-125)。ABANDONED_RINGING 目前**不可达**(条件互斥,归 W2),
      故实测该项应只由 SHORT_ABANDONED 与 ABANDONED_WAITING 贡献
    queues 报表:每个 queueId 的 totalCalls 等于第 5 条同 queueId 的计数
    answeredWithinSla 的分母是 queueCalls 而非 totalCalls(ledger.sql:127-129)——
      若前端把它除以 totalCalls,SLA 会被系统性低估;本用例只断言后端数字,呈现层归 G-B2
  failure_looks_like: |
    报表数字自洽、页面也正常,但和明细差几行 —— 差在时间窗的边界(报表用 started_at,
    若某处误用 ended_at,跨零点的通话会两天各算一次或一次都不算),
    或差在 abandoned 的口径(把所有 NO_ANSWER 都算成放弃,把 NO_AVAILABLE_AGENT
    和直拨未接也计了进去)。两种都不会报错,只会让日报常年偏若干个百分点。
  evidence_dir: docs/verification/artifacts/VC-S13-04/
  status: TODO
  refs: ["internal/store/sql/ledger.sql:120", "internal/store/sql/ledger.sql:137", "internal/store/sql/ledger.sql:153", "internal/httpapi/ledger_handlers.go:19"]
  note: "端点已对契约核实(2026-08-21):getReportOverview=GET /reports/overview、
    getReportQueues=GET /reports/queues、getReportDaily=GET /reports/daily,守卫均为 SUPERVISOR。
    第 1 条的 date 命令写了 BSD(-v+1d)与 GNU(-d tomorrow)两种形态,本机是 macOS 走前者"
```

---

## T6.4 · VC-S13-05 — 话机失联 → 坐席被摘除 → 恢复

**缺口 G-A6**:`AvailDeviceUnreachable`(state.go:207-210)只在事件层被 S11/S12 碰到,
"失联→摘除→恢复"这条旅程从无 case。这是坐席侧头号故障形态:
**崩掉的浏览器标签页和正常的看起来一模一样**。

```yaml
- id: VC-S13-05
  scenario: S13
  layer: app-state
  precondition: "wei 已签入 READY 且话机 1008 已注册;support-en 队列空闲;
    需要能关掉 wei 的浏览器话机标签页(或断其网络)"
  steps:
    - who: agent
      do: "记录基线并开启抓流:执行 collect 第 1、2 条"
    - who: human
      do: "**直接关掉 wei 的软电话标签页**(不要先签出——签出是另一条路径),等 30 秒"
    - who: agent
      do: "执行 collect 第 3–5 条,判定失联是否被识别"
    - who: human
      do: "重新打开标签页并重新注册 1008"
    - who: agent
      do: "执行其余 collect,判定恢复"
  collect:
    - "curl -s -b /tmp/vc-sup.jar http://127.0.0.1:8080/api/v1/agents | jq -c '.items[]|select(.username==\"wei\")|{state,availability,isRegistered}'"
    - "curl -s -N --max-time 1800 -b /tmp/vc-sup.jar http://127.0.0.1:8080/api/v1/events > /tmp/vc-s13dev-sse.log 2>&1 &"
    - "sleep 30; /usr/local/freeswitch/bin/fs_cli -x 'show registrations' | awk -F',' 'NR>1&&NF>3{print $1}'"
    - "curl -s -b /tmp/vc-sup.jar http://127.0.0.1:8080/api/v1/agents | jq -c '.items[]|select(.username==\"wei\")|{state,availability,isRegistered}'"
    - "grep -c '\"type\":\"DEVICE_UNREGISTERED\"' /tmp/vc-s13dev-sse.log; grep -c '\"type\":\"AGENT_NOT_READY\"' /tmp/vc-s13dev-sse.log"
    - "/usr/local/freeswitch/bin/fs_cli -x 'callcenter_config agent list' | grep agent-wei | awk -F'|' '{print $6}'"
    - "sleep 20; curl -s -b /tmp/vc-sup.jar http://127.0.0.1:8080/api/v1/agents | jq -c '.items[]|select(.username==\"wei\")|{state,availability,isRegistered}'"
    - "grep -c '\"type\":\"DEVICE_IN_SERVICE\"' /tmp/vc-s13dev-sse.log"
  expect: |
    失联后:
      registrations 中 1008 消失
      roster:wei 的 **availability=DEVICE_UNREACHABLE**、isRegistered=false
        (state.go:207-210:!IsRegistered || !IsDeviceInService → AvailDeviceUnreachable,
         该分支排在 StateNotReady 之后、default READY 之前)
      **state 仍为 READY** —— 失联不改坐席自己的意愿,只改可达性;两者是不同的列
      SSE:DEVICE_UNREGISTERED ≥1
      switch 侧 agent-wei status **不再是 Available**(否则队列仍会往一部死话机派单)
    恢复后:
      roster 回到 availability=READY、isRegistered=true;SSE 出现 DEVICE_IN_SERVICE
        (service.go:394 的 publish 点)
  failure_looks_like: |
    标签页关了、注册没了,而 roster 里 wei 依旧 READY/Available、switch 里也依旧 Available ——
    队列把每一通电话都派给一部不存在的话机,每通都振铃到超时再重派,
    主管墙上看到的是"有人在线却没人接",而坐席本人早已下班。
    这正是 state.go:207 注释里写的那句"崩掉的浏览器标签页和正常的看起来一模一样"。
  evidence_dir: docs/verification/artifacts/VC-S13-05/
  status: TODO
  refs: ["internal/agents/state.go:207", "internal/agents/service.go:366", "internal/agents/service.go:394", "internal/events/event.go:57"]
  note: "【执行前须落实】switch 侧那条断言(agent-wei 不再 Available)对应哪次镜像调用尚未静态确认——
    ObserveDevice 是否会驱动 mirrorStatus 需在执行时以日志佐证。若实测 switch 侧仍 Available
    而应用侧已 DEVICE_UNREACHABLE,则为**新缺陷**(队列仍向死话机派单),按实测立案而非改 expect"
```

---

## T6.2 · VC-S13-06 — 联系人全链

**缺口 G-A3**:`contacts` 表与 `/contacts` 全链无 case(tables.md 已记为只进不出)。

```yaml
- id: VC-S13-06
  scenario: S13
  layer: api
  precondition: "无;本用例自建自清数据,不依赖呼叫"
  steps:
    - who: agent
      do: "执行全部 collect:建 → 列 → 按号码查 → 改 → 删 → 确认已删"
  collect:
    - "curl -s -b /tmp/vc-wei.jar -H 'X-AICC-Csrf: 1' -H 'Content-Type: application/json' -X POST http://127.0.0.1:8080/api/v1/contacts -d '{\"displayName\":\"VC Probe\",\"phoneNumber\":\"18600000001\",\"company\":\"NovaNet\",\"notes\":\"created by VC-S13-06\"}' -w '\\nhttp=%{http_code}\\n'"
    - "curl -s -b /tmp/vc-wei.jar 'http://127.0.0.1:8080/api/v1/contacts?q=18600000001' | jq -c '[.items[]|{contactId,displayName,phoneNumber}]'"
    - "CID=<上一条的 contactId>; curl -s -b /tmp/vc-wei.jar -H 'X-AICC-Csrf: 1' -H 'Content-Type: application/json' -X PUT http://127.0.0.1:8080/api/v1/contacts/$CID -d '{\"displayName\":\"VC Probe Renamed\",\"phoneNumber\":\"18600000001\",\"company\":\"NovaNet\",\"notes\":\"renamed\"}' -w '\\nhttp=%{http_code}\\n' | jq -c '{displayName}'"
    - "curl -s -b /tmp/vc-wei.jar 'http://127.0.0.1:8080/api/v1/contacts?q=Renamed' | jq -c '[.items[]|.displayName]'"
    - "CID=<同上>; curl -s -o /dev/null -w 'delete http=%{http_code}\\n' -b /tmp/vc-wei.jar -H 'X-AICC-Csrf: 1' -X DELETE http://127.0.0.1:8080/api/v1/contacts/$CID"
    - "curl -s -b /tmp/vc-wei.jar 'http://127.0.0.1:8080/api/v1/contacts?q=18600000001' | jq -c '.items | length'"
  expect: |
    建:201,响应含 contactId
    列/查:按号码能查到,displayName 与写入一致
    改:200,displayName 变为 "VC Probe Renamed",且按新名字能查到
    删:204(或 200);删后按号码查 items 长度=0
    全程使用**坐席**会话——联系人是坐席通话中要用的东西,不应要求管理员权限
  failure_looks_like: |
    建返回 201 而按号码查不到 —— 查询走的是另一个字段(例如只匹配 displayName),
    坐席在通话中输入来电号码却查不到这个人,而那正是这张表存在的唯一理由。
    另一种:删返回 204 但行仍在,只是被标记;若列表不过滤已删,联系人会"删不掉"。
  evidence_dir: docs/verification/artifacts/VC-S13-06/
  status: TODO
  refs: ["internal/httpapi/contact_handlers.go:31", "docs/verification/coverage/tables.md"]
  note: "【起草时未核三处,执行前须查】①POST/PUT 的请求体字段名(本草案按 displayName/phoneNumber/
    company/notes 书写,须以 docs/openapi.json 的 Contact schema 为准);②查询参数是否为 q;
    ③DELETE 的成功码是 204 还是 200。三处以契约为准,不符则改 collect 而非改 expect"
```

---

## T6.6 · VC-S14-01 — 分机生命周期:建 → 注册鉴权 → 删除守卫

**缺口 G-C2**。

```yaml
- id: VC-S14-01
  scenario: S14
  layer: app-state
  precondition: "ADMIN 会话可用(/tmp/vc-admin.jar);
    一个未被占用的分机号(建议 1099)与一部可改配置的话机"
  steps:
    - who: agent
      do: "建分机:执行 collect 第 1、2 条"
    - who: human
      do: "用话机以 1099 与设定的密码注册"
    - who: agent
      do: "执行 collect 第 3、4 条判定注册与 Lua 目录一致"
    - who: agent
      do: "执行第 5 条尝试删除一个**已被坐席绑定**的分机(1008),判定守卫"
    - who: agent
      do: "执行第 6、7 条删除 1099 并确认目录随之失效"
  collect:
    - "curl -s -b /tmp/vc-admin.jar -H 'X-AICC-Csrf: 1' -H 'Content-Type: application/json' -X POST http://127.0.0.1:8080/api/v1/extensions -d '{\"number\":\"1099\",\"password\":\"vc-probe-pass\",\"displayName\":\"VC Probe\"}' -w '\\nhttp=%{http_code}\\n'"
    - "docker exec -i aicc-postgres psql -U aicc -d aicc -Atc \"select number, display_name from luacc.directory where number='1099'\""
    - "/usr/local/freeswitch/bin/fs_cli -x 'show registrations' | awk -F',' 'NR>1&&NF>3{print $1}' | grep -c 1099"
    - "/usr/local/freeswitch/bin/fs_cli -x 'sofia status profile internal reg' | grep -c 1099 || true"
    - "EXT1008=<1008 的 extensionId>; curl -s -b /tmp/vc-admin.jar -H 'X-AICC-Csrf: 1' -X DELETE http://127.0.0.1:8080/api/v1/extensions/$EXT1008 -w '\\nhttp=%{http_code}\\n' | head -c 200"
    - "EXT1099=<1099 的 extensionId>; curl -s -o /dev/null -w 'delete 1099 http=%{http_code}\\n' -b /tmp/vc-admin.jar -H 'X-AICC-Csrf: 1' -X DELETE http://127.0.0.1:8080/api/v1/extensions/$EXT1099"
    - "docker exec -i aicc-postgres psql -U aicc -d aicc -Atc \"select count(*) from luacc.directory where number='1099'\""
  expect: |
    建:201;`luacc.directory` 立刻可查到 1099 —— Lua 目录是视图,不需要 reload
      (aicc_xml.lua:103 按 number 查该视图)
    话机注册成功:show registrations 含 1099
    删一个**被坐席绑定**的分机(1008):**拒绝**,4xx 且错误码可辨
      (期望是 409/CONFLICT 一类,不是 500;具体码见下方 note)
    删 1099:204;删后 `luacc.directory` 查不到 —— 目录随之失效,该分机再也注册不上
    坐席会话执行同样的建/删:403(写操作是 ADMIN 权限,与 C23① 同族)
  failure_looks_like: |
    删掉一个坐席正在用的分机而系统照单全收 —— 那名坐席的话机下次重注册就失败,
    而应用里他仍是 READY,队列继续给他派单,直到有人发现"这个人接不到电话"。
    另一种:删了行但 Lua 目录还查得到(视图未过滤或有缓存),分机被删了却还能注册,
    等于一个没有归属、鉴权仍然有效的分机长期存在。
  evidence_dir: docs/verification/artifacts/VC-S14-01/
  status: TODO
  refs: ["internal/httpapi/catalog_handlers.go:48", "internal/httpapi/catalog_handlers.go:67", "freeswitch/scripts/aicc_xml.lua:103", "internal/store/migrations/00002_telephony.sql:171"]
  note: "【执行前须落实两处】①创建请求体的字段名(number/password/displayName 为草案假设,
    以契约 Extension schema 为准);②删除已绑定分机的**期望行为本身尚未静态确认**——
    若实测直接删成功,那是**新缺陷**,按实测立案并把本条 expect 改写为留证。
    不要为了让用例通过而把 expect 降级成'删除成功'"
```

---

## T6.6 · VC-S14-02 — 队列配员 → tier 生效 → 撤销

**缺口 G-C5**。与 C1(VC-S3-02 FAIL)相邻但不同:C1 查的是**漂移**,本例查的是**正向生效**。

```yaml
- id: VC-S14-02
  scenario: S14
  layer: app-state
  precondition: "ADMIN 会话可用;ben 已签入 READY;ben 当前**不在** support-en 的 tier 上
    (2026-08-21 实测:ben 只在 support-zh)"
  steps:
    - who: agent
      do: "记录两侧基线:执行 collect 第 1、2 条"
    - who: agent
      do: "把 ben 配员到 support-en:第 3 条;随后第 4、5 条判定 tier 生效"
    - who: human
      do: "拨 95001 进入 support-en,观察是否会派给 ben(wei 置 NOT_READY 以隔离)"
    - who: agent
      do: "第 6 条撤销配员,第 7 条判定 tier 消失"
  collect:
    - "/usr/local/freeswitch/bin/fs_cli -x 'callcenter_config tier list' | awk -F'|' 'NR>1&&NF>=5{print $2\"|\"$1}' | sort"
    - "QID=$(docker exec -i aicc-postgres psql -U aicc -d aicc -Atc \"select id from queues where name='support-en'\"); curl -s -b /tmp/vc-sup.jar http://127.0.0.1:8080/api/v1/queues/$QID/agents | jq -c '[.items[].username]'"
    - "QID=<同上>; BEN=<ben 的 agentId>; curl -s -b /tmp/vc-admin.jar -H 'X-AICC-Csrf: 1' -H 'Content-Type: application/json' -X PUT http://127.0.0.1:8080/api/v1/queues/$QID/agents -d '{\"agentIds\":[\"<wei 的 agentId>\",\"'$BEN'\"]}' -w '\\nhttp=%{http_code}\\n'"
    - "sleep 2; /usr/local/freeswitch/bin/fs_cli -x 'callcenter_config tier list' | awk -F'|' 'NR>1&&NF>=5{print $2\"|\"$1}' | grep agent-ben | sort"
    - "LOG=$(ls -t logs/aicc-*.log | head -1); grep 'agent staffing' \"$LOG\" | tail -3"
    - "QID=<同上>; BEN=<同上>; curl -s -o /dev/null -w 'unstaff http=%{http_code}\\n' -b /tmp/vc-admin.jar -H 'X-AICC-Csrf: 1' -X DELETE http://127.0.0.1:8080/api/v1/queues/$QID/agents/$BEN"
    - "sleep 2; /usr/local/freeswitch/bin/fs_cli -x 'callcenter_config tier list' | awk -F'|' 'NR>1&&NF>=5{print $2\"|\"$1}' | grep -c 'agent-ben|support-en' || true"
  expect: |
    配员:2xx;2 秒内 switch 侧出现 `agent-ben|support-en@<domain>`
      —— 日志应显示 `agent staffing reconciled` 且 **added=1**(而不是 already matched)
    撤销:2xx;2 秒内该 tier 消失,计数=0
    **撤销的计数必须以复查为准**:C1 已证实 `removed=` 会谎报(mod_callcenter 对 miss 的
      tier del 返回 +OK,converge 记 removed=1 而 tier 仍在,见 VC-S3-02/verdict.md)。
      故本条断言只认 `tier list` 的复查结果,不认日志里的 removed 数
  failure_looks_like: |
    PUT 返回 200、页面上 ben 已在队列里,而 switch 的 tier 里没有他 ——
    配员只写进了 DB,派单永远不会找到他;主管以为加了人,队列该等还是等。
    撤销侧的镜像失败更隐蔽:tier 删不掉(C1 的形态),被撤下的坐席继续被派单,
    而两侧的日志都显示成功。
  evidence_dir: docs/verification/artifacts/VC-S14-02/
  status: TODO
  refs: ["internal/httpapi/catalog_handlers.go:108", "internal/httpapi/catalog_handlers.go:126", "internal/catalog/service.go:286", "internal/telephony/adapter.go:145"]
  note: "PUT /queues/{id}/agents 是**全量替换**(与 SYS-7 记的 PUT 语义一致),
    所以第 3 条必须把 wei 一起带上,否则会把 wei 撤下来。
    请求体字段名 agentIds 为草案假设,执行前以契约核实"
```

---

## T6.8 · VC-S14-03 — 号码生命周期:建号 → 放号 → 拨通 → 停用 → 拒接

**缺口 G-C3**(F8 已闭:`luacc.dids` 视图带 `WHERE d.is_enabled`,
见 `migrations/00002_telephony.sql:171-180`)。

```yaml
- id: VC-S14-03
  scenario: S14
  layer: signaling
  precondition: "ADMIN 会话可用;一个未占用的 DID(建议 95009);
    已有可用 flow 与 fallback 队列(取 95001 的同款)"
  steps:
    - who: agent
      do: "建号并放号:执行 collect 第 1、2 条"
    - who: human
      do: "拨 95009,确认 bot 应答;听到 bot 说话后挂断"
    - who: agent
      do: "执行第 3 条确认接通留痕;随后第 4 条停用该号"
    - who: human
      do: "再拨 95009,记录听感(应为拒接/不可达,而不是静音或长振铃)"
    - who: agent
      do: "执行其余 collect,判定停用后的拒接形态"
  collect:
    - "FLOW=$(docker exec -i aicc-postgres psql -U aicc -d aicc -Atc \"select flow_id from dids where number='95001'\"); FQ=$(docker exec -i aicc-postgres psql -U aicc -d aicc -Atc \"select fallback_queue_id from dids where number='95001'\"); curl -s -b /tmp/vc-admin.jar -H 'X-AICC-Csrf: 1' -H 'Content-Type: application/json' -X POST http://127.0.0.1:8080/api/v1/dids -d '{\"number\":\"95009\",\"language\":\"en\",\"flowId\":\"'$FLOW'\",\"fallbackQueueId\":\"'$FQ'\",\"isRecordingEnabled\":true,\"isEnabled\":true}' -w '\\nhttp=%{http_code}\\n'"
    - "docker exec -i aicc-postgres psql -U aicc -d aicc -Atc \"select number, language, is_enabled from luacc.dids where number='95009'\""
    - "docker exec -i aicc-postgres psql -U aicc -d aicc -Atc \"select call_id, call_type, did, status, bot_sec from cdrs where did='95009' order by started_at desc limit 1\""
    - "DID=<95009 的 didId>; D=$(curl -s -b /tmp/vc-admin.jar http://127.0.0.1:8080/api/v1/dids | jq -c --arg n 95009 '.items[]|select(.number==$n)'); curl -s -b /tmp/vc-admin.jar -H 'X-AICC-Csrf: 1' -H 'Content-Type: application/json' -X PUT http://127.0.0.1:8080/api/v1/dids/$DID -d \"$(echo \"$D\" | jq -c '.isEnabled=false')\" | jq -c '{number,isEnabled}'"
    - "docker exec -i aicc-postgres psql -U aicc -d aicc -Atc \"select count(*) from luacc.dids where number='95009'\""
    - "/usr/local/freeswitch/bin/fs_cli -x 'console last 100' | grep -iE 'aicc_inbound|95009' | tail -5"
    - "DID=<同上>; curl -s -o /dev/null -w 'cleanup delete http=%{http_code}\\n' -b /tmp/vc-admin.jar -H 'X-AICC-Csrf: 1' -X DELETE http://127.0.0.1:8080/api/v1/dids/$DID"
  expect: |
    建号:201;`luacc.dids` 立刻可查到 95009(视图,无需 reload)
    首拨:接通、bot 应答,CDR 落一行 did=95009 且 bot_sec>0
    停用:PUT 后 `isEnabled=false`,且 **`luacc.dids` 查不到该号**
      —— 视图带 `WHERE d.is_enabled`(migrations/00002_telephony.sql:180),F8 即此
    再拨:Lua 查不到路由 → 走"号码不存在"的分支(aicc_inbound.lua:44 取不到 route),
      听感应为明确拒接,**不是**静音或长振铃;FS 日志应有对应痕迹
    该形态与 VC-S1-03(UNALLOCATED_NUMBER)同族,可对照其判定
  failure_looks_like: |
    停用后再拨仍然接通 —— 视图没过滤或 Lua 有缓存,一个"已停用"的号码继续对外服务,
    而管理页面上它显示为停用。反向的失败:停用后拨过去是长振铃后静音挂断,
    主叫不知道发生了什么,运营商侧也拿不到明确的拒接原因。
  evidence_dir: docs/verification/artifacts/VC-S14-03/
  status: TODO
  refs: ["internal/httpapi/catalog_handlers.go:139", "internal/httpapi/catalog_handlers.go:148", "internal/store/migrations/00002_telephony.sql:171", "freeswitch/scripts/aicc_inbound.lua:44"]
  note: "【执行前须落实】①POST /dids 的请求体字段名以契约 DID schema 为准;
    ②停用后的**听感与拒接码**尚未静态确认——Lua 在 route 为 nil 时走哪条分支需读
    aicc_inbound.lua 开头部分再定 expect;③PUT 是全量替换,故第 4 条用读-改-写。
    ④用例结束务必执行第 7 条清理,否则 95009 会污染后续的号码列表断言"
```

---

## T6.11 · VC-S14-04 — 坐席外呼全链(三型:INTERNAL / OUTBOUND,含能力限制)

**缺口 G-A7**。媒体链与账面均已在 2026-08-20/21 实测通过(C15/C17/C18 修复后),
本用例把它们固化为断言。

```yaml
- id: VC-S14-04
  scenario: S14
  layer: signaling
  precondition: "wei(1008)与 ben(1007)均已签入 READY 且话机注册;
    pstn_sim 网关可用(外呼一型需要)"
  steps:
    - who: agent
      do: "记基线:执行 collect 第 1 条"
    - who: agent
      do: "发起内部外呼 1008→1007:第 2 条"
    - who: human
      do: "ben 接听,通话数秒后由 **ben** 挂断"
    - who: agent
      do: "第 3、4 条判定内部一型的账面与能力限制"
    - who: agent
      do: "发起外线外呼 1008→18688886669:第 5 条"
    - who: human
      do: "接听,通话数秒后挂断"
    - who: agent
      do: "执行其余 collect,判定外呼一型的账面"
  collect:
    - "echo cdr_baseline=$(docker exec -i aicc-postgres psql -U aicc -d aicc -Atc 'select count(*) from cdrs')"
    - "curl -s -b /tmp/vc-wei.jar -H 'X-AICC-Csrf: 1' -H 'Content-Type: application/json' -X POST http://127.0.0.1:8080/api/v1/calls/dial -d '{\"destination\":\"1007\"}' -w '\\nhttp=%{http_code}\\n'"
    - "CALL=<上一条的 callId>; for op in hold retrieve; do printf '%-9s ' $op; curl -s -b /tmp/vc-wei.jar -H 'X-AICC-Csrf: 1' -X POST http://127.0.0.1:8080/api/v1/calls/$CALL/$op -w ' http=%{http_code}\\n' | head -c 120; done; printf 'transfer  '; curl -s -b /tmp/vc-wei.jar -H 'X-AICC-Csrf: 1' -H 'Content-Type: application/json' -X POST http://127.0.0.1:8080/api/v1/calls/$CALL/transfer -d '{\"destination\":\"1009\"}' -w ' http=%{http_code}\\n' | head -c 120; printf 'mute      '; curl -s -o /dev/null -b /tmp/vc-wei.jar -H 'X-AICC-Csrf: 1' -X POST http://127.0.0.1:8080/api/v1/calls/$CALL/mute -w 'http=%{http_code}\\n'"
    - "CALL=<同上>; docker exec -i aicc-postgres psql -U aicc -d aicc -Atc \"select call_type, status, from_number, to_number, ring_sec, talk_sec, bill_sec, total_sec, array_length(agent_ids,1), legs from cdrs where call_id='$CALL'\""
    - "curl -s -b /tmp/vc-wei.jar -H 'X-AICC-Csrf: 1' -H 'Content-Type: application/json' -X POST http://127.0.0.1:8080/api/v1/calls/dial -d '{\"destination\":\"18688886669\"}' -w '\\nhttp=%{http_code}\\n'"
    - "CALL2=<上一条的 callId>; docker exec -i aicc-postgres psql -U aicc -d aicc -Atc \"select call_type, status, from_number, to_number, ring_sec, talk_sec, bill_sec, total_sec, legs, tech from cdrs where call_id='$CALL2'\""
    - "echo cdr_now=$(docker exec -i aicc-postgres psql -U aicc -d aicc -Atc 'select count(*) from cdrs')"
  collect_note: "两通电话预期各产 1 行,故 cdr_now - cdr_baseline = 2"
  expect: |
    内部一型(1008→1007):
      call_type=**INTERNAL**(4 位数走 callTypeFor,outbound.go:196 盖 aicc_call_type)
      status=ANSWERED,legs 只含被叫那一段 AGENT|1007
      **talk_sec 是两条坐席腿区间的并集,不是两倍** —— 两条腿都有 AgentID,
        并集把同一段对话收成一段(2026-08-21 实测 17 秒)
      **bill_sec=0** —— 分机互拨没有运营商计费(billedLeg 对 INTERNAL 返回 nil);
        answered_at 仍有值,记录它确实被接通
      hold / retrieve / transfer 三者均 **409 OPERATION_NOT_ALLOWED_FOR_CALL_TYPE**(C18);
      mute **202** —— 静音与挂断在任何通话上都成立
    外线一型(1008→18688886669):
      call_type=**OUTBOUND**,legs 含 **TRUNK** 段(出网关)
      **bill_sec 从被叫应答起算,不是从坐席自己那条自动应答的腿** ——
        故 bill_sec ≤ total_sec 恒成立(2026-08-21 曾出现 23>21 的不可能值,已修)
      tech.switchBillSec 与 bill_sec 相差 ≤2 秒
    两通合计 CDR 增量 = 2,**不含**任何 `from_number` 为分机、`to_number` 为空的多余行
      —— C24(未接时拨号方案跌落 voicemail 各自成行)在本用例里不该出现,因为两通都被接听
  failure_looks_like: |
    内部呼叫被记成 OUTBOUND 并计了费 —— 分机互拨进了话费账单;
    或 talk_sec 是实际时长的两倍 —— 两条坐席腿各算一遍,坐席工时凭空翻倍。
    能力限制侧:三个操作有一个放行了,坐席可以把一通分机互拨"转接"出去,
    而 Transfer 会去找一条不存在的主叫腿。
  evidence_dir: docs/verification/artifacts/VC-S14-04/
  status: TODO
  refs: ["internal/outbound/outbound.go:196", "internal/telephony/coordinator.go:848", "internal/telephony/cdr.go:152", "internal/httpapi/call_handlers.go:141"]
  note: "本用例的每一条 expect 都有 2026-08-21 的实测支撑(见 regression-2026-08-21/verdict.md),
    起草时即为'已知会通过'。它的价值不在发现新问题,而在把当日修的 C15/C17/C18/C25
    与计费锚点固化成回归断言——这些都是改一行就会静默退化的东西。
    **若执行时出现 C24 的多余行**(例如某一通没被接起),按 C24 处理,不改本用例 expect"
```
