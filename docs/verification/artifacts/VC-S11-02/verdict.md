# VC-S11-02 — PASS(2026-08-20)

## 实测(output.txt)
1. wei 重复签入:第一次 409(已签入,expect 明示可接受)、第二次 "AGENT_ALREADY_LOGGED_IN" → 符合。
2. amy 抢占 wei 分机(WEIEXT=1008,从 DB 实读):"EXTENSION_IN_USE" → 符合。
3. amy 未签入即 ready:"AGENT_NOT_LOGGED_IN" → 符合。

## 附带事实
- wei 的 default_extension 在本机 DB 中确为 1008(任务书第 0 部分正确;seed 默认 1001 已被本机改绑)。
- 三个守卫分别命中 state.go:104(ErrAlreadyLoggedIn)、service.go:204-207(extensionHolderLocked)、
  state.go:129(ErrNotLoggedIn)的映射,错误码与 openapi ErrorCode 契约一致。
