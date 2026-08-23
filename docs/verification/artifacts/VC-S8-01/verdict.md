# VC-S8-01 — PASS(2026-08-20 17:54)

## 实测(output.txt)
- 启动期 "voice provider selected" 恰好 1 次:provider=openai(12:51 实例)。
- 该通 "ai conversation started":language=zh、provider=openai、flowId=novanet_support——
  与启动行完全一致,**language 不选 provider**(phase1-decisions A1)在线证实。
- cdrs:did=95002,language=zh,ANSWERED。
- transcripts:BOT 中文问候(感谢致电 NovaNet…)+ 中文营业时间回答——判定条款满足。
- 【留证】CUSTOMER 行数 = 0,与 SYS-8 判定一致;W1 落地后由 T6.9 转正式断言。
- failure_looks_like(provider 随语言变 / bot 说英文)均未出现。

---

## 重跑 —— 2026-08-23 13:11(W1 落地后,留证条款转正式断言)

自本日起验收材料改用 **support-zh / 中文**(owner 指示),而本用例本来就是中文那条。

```
启动:  voice provider selected  provider=qwen   （整个日志恰好 1 次）
本通:  ai conversation started  did=95002  provider=qwen  language=zh
CDR:   95002 | zh | ANSWERED | INBOUND | bot_sec=16
```

provider 与启动行完全一致 —— **语言没有选 provider**,CLAUDE.md 明令的那条回归没有发生。

转写(全部 `source=MODEL`):

```
1 | BOT      | 感谢致电 NovaNet，请问有什么可以帮您？
2 | CUSTOMER | 我想咨询时，营业时间。            ← 本轮转正的断言
3 | BOT      | 我们的营业时间是周一至周五上午9点到下午6点。
```

**CUSTOMER 行数 = 1。** 这一条在 2026-08-20 首次执行时是"留证,不判定",实测为 0,
与审计 SYS-8 一致 —— 当时 stock profile 未设 `TranscribeModel`,
bot 阶段的"转写"只有 bot 自己的话和工具痕迹,**主叫说了什么一个字都没有**(设计 08 的 G-06)。

W1 之后:openai 侧由 profile 默认的 `gpt-live-transcribe` 请求;
**qwen 侧无需请求** —— 本轮同时证实它不问自答,故其 profile 故意不设,免得留下死配置。
转写语言跟随呼叫语言(zh),而不是部署的 provider。

### 判定

**PASS**(维持;新增断言一并成立)。
