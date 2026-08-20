# VC-S1-01 — PASS(2026-08-20 17:25)

## 实测(output.txt)
- show channels:恰好 2 通道——主叫腿(dest 95001,executing bridge→sofia/gateway/aicc_bot/95001)
  + 出向 bot 腿(sofia/external/95001,CS_EXCHANGE_MEDIA),callstate 均 ACTIVE。
- GET /calls:恰好 1 个呼叫;state=RUNNING,language=en;parties=ORIGINATOR(18688886669,TALKING)
  + TARGET(95001,TALKING,isBotLeg=true)。minted callId 01a01e7d-3d48-…(≠channelId,reidentify 生效)。
- 日志:"ai conversation started" flowId=novanet_support provider=openai language=en;
  "ai call bridged" law=PCMU toProvider=PCMU@8000 fromProvider=PCMU@8000 isPassthrough=true。
- failure_looks_like(2 呼叫未合并)未出现。

## 偏差(字面,不改判)
- expect 写"均 state=CS_EXCHANGE_MEDIA";实测桥内 A 腿为 CS_EXECUTE(bridge app 执行态)、B 腿
  CS_EXCHANGE_MEDIA——FS 的正常形态,断言语义(两腿在通、媒体互通)满足。建议下轮账本修订把该行
  放宽为"A 腿 CS_EXECUTE(bridge)/B 腿 CS_EXCHANGE_MEDIA,callstate 均 ACTIVE"。

## 环境注记
- 主叫链路:web-sip-phone(1000@ws.aicc.test)→ 192.168.31.5(pstn 链)→ external profile,
  ANI=18688886669——与断言无关,记录备查。
- 本 case 第一次尝试(17:23)因代理断线致 OpenAI dial timeout,app 走了自己的 fallback
  (invite 已接→模型拨号失败→"transferring the caller to the fallback queue"→BYE)。
  该样本已在案(log 17:23:29),是 orchestrator 侧兜底(非 lua 兜底)的在野实证。
