# VC-S2-01 — PASS(2026-08-20 17:37)

## 实测(output.txt)
- "caller leg ended, closing the model session"(17:36:50,call 01a01e87-…)✓;无 "ai call failed" ✓
- show channels count = 0 ✓
- cdrs 最新 95001 行:ANSWERED | is_contained=f | bot_sec=4 | NORMAL_CLEARING ✓(说话中挂断,bot_sec≥1)
- mailbox 差分 = 0(baseline 0 → now 0)✓
- 收尾链完整:caller leg ended(17:36:50)→ recording booked(17:36:52,142KB)

## 发现(账本叙述修订建议,下轮批)
- failure_looks_like 与 collect#2 的 grep 模式把「无 "model session closed"」当作 provider WS 泄漏指标——
  **在主叫先挂路径上该行不出现是健康常态**:它只在 provider.EventTypeClosed(模型侧主动关闭)时打
  (session.go:461);我方主动关闭走 Close()→s.model.Close(ctx)(session.go:288-292),同步关断传输,
  不产生该事件。建议 failure_looks_like 改指真实泄漏证据(如 Close 未达/传输错误日志),
  collect#2 的 grep 模式相应缩窄。
