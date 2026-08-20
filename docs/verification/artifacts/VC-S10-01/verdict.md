# VC-S10-01 — PASS(2026-08-20)

## 实测
- 抓流期间产生 3 个事件(wei not-ready BREAK/LUNCH/TRAINING):
  id 严格递增 N1=6800098, N2=6800099, N3=6800100,均为 AGENT_NOT_READY(capture-a.log)。
- 带 Last-Event-ID=6800098 重连:回放恰好从 id=6800099 开始,6800099/6800100 两条按原类型重放(replay-b.log)。
- SYSTEM_RESET 出现次数 = 0。

## expect 对照
- 回放从 N2 开始、含 N3、类型一致 → 符合(hub.go:140 的 `> lastEventID` 语义无 off-by-one)。
- 无 SYSTEM_RESET(resume 点在 ring 窗口内)→ 符合(hub.go:129-133)。

## 备注
- 事件 seq 已达 680 万量级:块预留(blockSize=100_000,events/seq.go:14)+ 多次重启烧块的累积表现,与设计一致(gap 无意义)。
- 执行后遗留状态:wei 处于 NOT_READY(TRAINING)(本用例的副作用,后续用例会覆盖)。
