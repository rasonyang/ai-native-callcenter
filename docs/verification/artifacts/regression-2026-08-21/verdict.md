# 整体回归 2026-08-21 · 今日全部改动的端到端复核

覆盖当日的 CDR 三锚点重构与 C11 / C12 / C13 / C16 / C18 / C20 / C21 / C22 八条缺陷修复。
四通真实电话 + 两次 API 直测。**结论:全部生效,并新发现 4 条缺陷,其中 3 条当场修掉。**

## 逐项结果

| 项 | 验法 | 结果 |
|---|---|---|
| **C12** 主管等待名单 | 三角色直接取端点 | supervisor / admin / agent 均 **200**(修复前主管 403) |
| **C13** 响铃报分机号 | SSE `PARTY_RINGING` | `extensionNumber = 1008`(修复前是 `"g7bih4lv"` 类令牌) |
| **C16** streamin 竞争 | `go test -race ./...` ×5 | 全绿;tap 在四通电话中实跑无异常 |
| **C18** INTERNAL 能力限制 | 内部通话中直测 | hold / retrieve / transfer 均 **409 `OPERATION_NOT_ALLOWED_FOR_CALL_TYPE`**;mute **202**(对照,仍可用) |
| **C20** 派单腿无幽灵 | 幽灵计数 | 四通电话后 **48 → 48**,一条没长 |
| **C21** contained 归属 | 纯 bot 通话 | **恰好一行**,`tech` 带 `codec`/`sipCallId` ⇒ 由 bot 路径写,人工路径不再抢 |
| **C11 + bot_sec 锚点** | 转接通话 | `bot_sec=18`(bot 腿区间);印记漂移 WARN 每通触发,差值稳定 3–8 秒 |
| **`talk_sec` 并集** | wei → ben 转接 | **`talk_sec=460` = `AGENT 1008 64s` + `AGENT 1007 396s`**。旧代码只记 64,ben 那 396 秒整段丢失 |
| **计费锚点** | 同上 | `bill_sec = total_sec = tech.switchBillSec = **487**`,三方精确相等 |
| **hold 计入 talk** | 通话 A | 3 秒保持含在 `talk_sec=160` 内;F12 复核:`CHANNEL_HOLD → CHANNEL_UNHOLD`,中间无 UNBRIDGE |
| **时长自洽校验** | 全程日志 | **0 次告警** —— 修好之后不再报(修复前那通 `bill 23 > total 21` 正是它该抓的) |
| **计费对账** | 全程日志 | **0 次告警** —— 实测差 0–1 秒,均在 ±2 容差内 |

三个新告警**各司其职**:该报的报(bot 印记漂移),不该报的一次没报。这比"没有告警"更能说明问题。

## 本次回归新发现的缺陷

| | 缺陷 | 处置 |
|---|---|---|
| — | **计费锚错了腿**:坐席自己发起的呼叫按"originator"取,而那是坐席**自动应答**的腿 —— 实测 `bill_sec=23 > total_sec=21`,算术上不可能 | **当场修**:改按面向运营商的那条腿(inbound 取主叫、outbound 取被叫、internal 无人计费) |
| — | **没有一处校验五个时长自洽** —— 上面那个不可能的数因此静默落库 | **当场修**:`checkDurations()` 比对三条不变式,超限记 WARN 并带上全部六个数字。**不改数**,只让它说话 |
| — | **bot 路径不填 `bill_sec`**:一通 34 秒的 contained 呼叫记成免费 | **当场修**:`aicall` 写自己那行时同样从应答起算 |
| **C24** | 无人接听的 click-to-dial 产生 **4 行 CDR**(拨号方案跌落 voicemail,后继腿各自成呼叫) | **未修**。owner 方向:aicc 应有专属 dialplan context,从源头不产生这些腿 |
| **C25** | **转接走的电话不离开第一个坐席的屏幕** —— `CallsForAgent` 不看腿死没死 | **当场修**。这就是 owner 两度报的"1008 没有挂断",此前两次都被归咎于浏览器话机 |

C25 值得单说:交换机侧当时只有 2 条 channel、party 模型也正确(1008 `RELEASED`),
**是我们的列表在骗人**。前两次现场都把它当成话机问题,这次因为同时看了三方数据才定位到。

## 门禁

`go build` / `go vet` / `gofmt` / `go test -race ./...`(含 PostgreSQL 迁移测试)/
`make api-check` / `make api-breaking` / 前端 111 tests + `tsc` —— 全部通过。
