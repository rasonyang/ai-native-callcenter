# VC-S3-03 — FAIL(2026-08-20 18:12;F6 漂移复现并钉死方位)

## 实测(output.txt)
- 【探针,通话中】主叫通道 01a01ea6-76af-… 上四戳齐全:aicc_bot_sec=10、aicc_flow_id=019ffd60-…、
  aicc_language=en、aicc_did=95001 —— **stamp 侧无罪**。
- 【挂断后】CDR:ANSWERED|bot_sec=0|queue_wait_sec=120|talk_sec=63|has_flow=f|agents=1 ——
  **bot 份额与 flow 全丢** → 漂移钉死在挂断读回/快照侧(switchevent.go:81 botShare / registry.go:346 /
  合并路径三者之一),非 stamp 侧。expect 的 bot_sec>0、has_flow=t 不满足 → FAIL。
- queue_events:JOINED→OFFERED(agent)→BRIDGED(wait_ms=58151 ✓ 正确)→LEFT —— 事件路径无损。
- **次生缺陷(疑似同根)**:cdrs.queue_wait_sec=120 = joined→left(把 63s 通话计入等待);真实等待
  58s(BRIDGED wait_ms)。快照里 Queue.BridgedAt 疑似与 Bot 一起丢失。
- legs(DB 与 list API)完好:[QUEUE(support-en,120s), AGENT(1008,63s)],无 BOT leg(与 Bot 丢失一致)。

## collect 缺陷(回修建议)
- collect#4 jq 路径应为 `.cdr.legs`(GET /cdrs/{id} 响应包 {"cdr":{…}} 信封);本次 `.legs` 返回 null
  一度误判为第三缺陷。

## 立案
- 主缺陷 + queue_wait_sec 计法 → TASKS **C11**(修复后重跑本 case;上午 95002 三行为同型在野样本)。
