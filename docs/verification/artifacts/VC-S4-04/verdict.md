# VC-S4-04 — PASS(2026-08-20 18:26)

- 前置:95001 is_recording_enabled=t(无需改写,SYS-7 守卫路径未触发)。
- recordings:backend=S3 | bucket=aicc-recordings | size_bytes=1882284(>44)| duration_sec=58 ✓
- cdrs.has_recording=t ✓(MarkRecorded)
- 音频下载:HTTP 200,content-type=audio/wav,size_download=1882284 == size_bytes ✓
- 日志:"recording booked"(key=2026/08/20/<call_id>.wav,UTC 日期约定 ✓);无 "no recording ingested"。
- 附注:本通为 bot 侧收线(主叫描述"bot 挂断"),顺带产生一条 is_contained 候选样本,不影响本用例断言。
