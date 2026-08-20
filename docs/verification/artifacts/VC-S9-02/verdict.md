# VC-S9-02 — PASS(2026-08-20 18:38;附两处 expect 勘误与 D7③ 前提推翻)

## 实测
- 流(sup 视角,ring 回放核对):CONNECTING → LIVE → STOPPED,顺序正确,无 DEGRADED/ERROR ✓
- wei 视角只见 LIVE → STOPPED:CONNECTING 发布时坐席尚未进 actor 的 agentIDs(零值 scope=仅 sup)——
  scope 时序使然,非丢失;账本该断言应以 sup jar 抓流(勘误)。
- REST 快照:state=**ENDED**,items=11=DB 行数 ✓。

## 关键发现(推翻审计/覆盖表的一个 [FACT])
- "转写 ENDED 从不产生"只对 SSE 成立:**GetCallTranscript 对已收官呼叫合成 state=ENDED**
  (transcript_handlers.go:69 默认值,actor 已退休、call 不再 live 时)——它是收官快照语义,在用。
- 由此:expect 的"快照与最后一次事件一致"在收官后本就不成立(流末=STOPPED,快照=ENDED,双源并不分歧,
  是同一状态机的两个时刻);**D7③("转写 ENDED 先删除")决议前提被推翻,需 owner 重议**
  (保留为收官快照值 or 删除并让 handler 收官返回 STOPPED)。
- 判定:系统行为自洽,案面断言的语义(时间线连贯、无真双源分歧、items=DB)满足 → PASS,expect 待修订。
