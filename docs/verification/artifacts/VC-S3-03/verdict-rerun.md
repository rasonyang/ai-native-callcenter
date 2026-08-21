# VC-S3-03 — 重跑(2026-08-21 12:06,C11 修复后)· 判定:PASS

首跑 2026-08-20 判 FAIL(见 `verdict.md`),立案为 C11。C11 于 2026-08-21 修复
(根因见 `docs/verification/artifacts/C11/verdict.md`),本次按原 collect 重跑。

呼叫:18688886669 → 95001 → bot 对话后转人工 → wei(1008)接听 → 主叫挂断。
call_id `01a0227e-bba1-709a-9b58-cfeb60fec721`。

## 逐条对照

| expect | 实测 | |
|---|---|---|
| cdrs 恰好 1 行(不是 bot/人各一行) | 总数 6186 → **6187** | ✓ |
| status=ANSWERED | `ANSWERED` | ✓ |
| **bot_sec>0** | **13**(首跑 0) | ✓ |
| queue_wait_sec≥0 | `5` | ✓ |
| talk_sec>0 | `11` | ✓ |
| did=95001 | `95001` | ✓ |
| **has_flow=t** | **`t`**(首跑 `f`) | ✓ |
| agent_ids 长度=1 | `1` | ✓ |
| queue_events:JOINED → OFFERED(has_agent=t)→ BRIDGED(wait_ms>0) 各 ≥1 行且按时间序 | `JOINED\|f\|0` → `OFFERED\|t\|0` → `BRIDGED\|t\|6403` | ✓ |
| legs 依次含 BOT(durationSec>0)、QUEUE(label=support-en)、AGENT | `[BOT 13s] [QUEUE support-en 5s] [AGENT 1008 11s]` | ✓ |

`failure_looks_like`(通话顺利但 bot_sec=0、flow_id 空,报表把 bot→人呼叫当纯人工统计)
**已消除**。首跑缺失的 BOT leg 本次出现。

user_data 亦已落库(首跑为 `{}`):
```json
{"botReason": "Caller asked to be transferred to a human agent.",
 "botSummary": "Caller requested a transfer to a human agent for assistance."}
```

## 一处 <1s 的口径差(不构成缺陷)

`cdrs.queue_wait_sec=5` 与 `queue_events.BRIDGED.wait_ms=6403`(→6s)相差 1 秒。两者的起点不同:

- `queue_events` 的 wait_ms 用事件头 `CC-Member-Joined-Time`,mod_callcenter 只给到**整秒**
  (`epochSeconds`,switchevent.go),故它是向下取整后的起点,算出的等待偏大最多 1 秒。
- `cdrs` 的起点是 `Queue.JoinedAt`;`member-queue-start` 上该头若为空则回落到入队事件自身的
  时间戳(registry.go),那是**亚秒精度**的真实时刻。

即 CDR 的值反而更精确,差额来自 queue_events 一侧的整秒截断。另两通样本
(95002 `4` vs `4513`、VC-S7-03 `55` vs `55819`)两侧一致,说明该差仅在头值恰好跨秒时出现。
expect 只要求 `queue_wait_sec≥0` 与 `wait_ms>0`,均满足。

## collect 勘误(沿用首跑的回修建议)

collect#4 的 jq 路径应为 `.cdr.legs`(`GET /cdrs/{id}` 响应带 `{"cdr":{…}}` 信封);
本次按此执行,返回完整三段 leg。
