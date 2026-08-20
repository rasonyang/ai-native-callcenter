# VC-S3-01 — PASS(2026-08-20 18:10–18:12)

## 实测(output.txt)
- collect#1 分机=1008(以查询为准 ✓);#2 login=AGENT_ALREADY_LOGGED_IN(UI 已签入,expect 允许);#3 READY ✓;switch Available/Waiting ✓
- members:恰好 1 行,cid_number=18688886669,state=**Trying**,serving_agent=agent-wei ✓
  【F1 已闭】实测表头:queue|instance_id|uuid|session_uuid|cid_number|cid_name|system_epoch|joined_epoch|rejoined_epoch|bridge_epoch|abandoned_epoch|base_score|skill_score|serving_agent|serving_system|state|score
- /calls/waiting:恰好 1 项,queueName=support-en,fromNumber=18688886669,slaThresholdSec=20 ✓(经 wei jar,见发现)
- SSE 顺序:QUEUE_JOINED(payload.queueName=support-en)→ QUEUE_COUNT(waiting=1)→ QUEUE_AGENT_OFFERED
  (信封 agentId=wei、payload.queue=support-en 裸名)✓ seq 连续(6900142-144)

## 发现(新缺陷立案 → TASKS C12)
- **GET /calls/waiting 拒绝 supervisor**:403 "this account is not an agent"(ListWaitingCalls 走
  agentIDFor+QueuesForAgent,call_handlers.go:52-66)。账本多个 case(S3-01/S6-01/S12-01/02)与旅程 B3
  都假设 sup 可读——要么 handler 补 sup 分支(全队列),要么明确 agent-only 并改 UI/账本口径。
  本次执行以 wei jar 代替,断言语义不变;collect 修订建议:该行改用 /tmp/vc-wei.jar。
