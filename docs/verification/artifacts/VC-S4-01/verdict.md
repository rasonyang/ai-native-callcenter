# VC-S4-01 — PASS(2026-08-20 18:18–18:19;附 C13 立案)

## 实测(output.txt)
- SSE(经 ring 回放自 seq 6900144 补采——原 150s 抓流窗在拨号前 5s 到期,S10 的 replay 机制救场):
  本呼叫 PARTY_RINGING=1 → PARTY_ESTABLISHED → AGENT_AVAILABILITY,QUEUE_AGENT_OFFERED=1,顺序正确,
  事件 scope 命中 wei(信封 agentId=807b2164)。
- /calls/mine:同一呼叫三腿——ORIGINATOR TALKING、bot 腿 RELEASED(isBotLeg,NORMAL_CLEARING)、
  wei 腿(1008,agentId)TALKING ✓。
- roster:availability=ON_CALL,isOnCall=true ✓。
- failure_looks_like(无 PARTY_RINGING、腿未归户)未出现。

## 偏差(立案 → TASKS C13)
- expect "payload.extensionNumber=wei 分机" 未满足:两次实测(本呼叫与 T3.1 呼叫)payload 的
  extensionNumber/toNumber 均为浏览器话机的 WS 注册标识("g7bih4lv"),非 1008。腿归属正确
  (agentForLeg 命中,agentId/scope 无误),但供屏显消费的字段装了 destination 原始值
  (coordinator.go:412-416 直取 ev.DestinationNumber;:829 注释早已预告这种 destination)。
  产品侧应在归户成功后用坐席绑定分机回填该字段。判定:主链全过,单条款立案不改判。
