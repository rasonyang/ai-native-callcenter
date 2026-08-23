# VC-S9-01 — FAIL(2026-08-20 18:37;链路全通,质量条款不达,C14 立案)

## 通过的条款
- 日志链:transcription tap attached → tapped stream connected → transcription live(同 call,18:37:34-35)✓
- SSE:CALL_TRANSCRIPT isFinal=false partial ≥2 ✓
- DB(source=ASR):HUMAN_AGENT 行 has_agent=t、CUSTOMER 行 has_agent=f、provider=openai,
  seq 5-11 与 MODEL 行共序无冲突(uq 不炸)✓

## 不达的条款(→ FAIL)
- isFinal=true 含 "quick brown fox":未命中——fox 句被识别为 "Butro focus jobs owing the lazy workin"
  (seq 11),CUSTOMER 侧同样破碎("Brew forcement"/"Joss Are the 多")。
- 无 "transcribe: audio was dropped":**双侧告警**——HUMAN_AGENT sent=1146 dropped=46(4.0%),
  CUSTOMER sent=1084 dropped=78(7.2%)。
- 因果判读:pump 计数器正是为分开"引擎听错"与"我们没送到"而设(pump.go:88-90 注释)——本次是**没送到**,
  丢帧率足以解释识别质量崩坏。

## 立案 → TASKS C14
ASR tap 摄取路径丢帧(pump 背压/缓冲不足候选),修复后重跑本用例。

---

## 重跑 —— 2026-08-23 11:47–12:14(四通,provider 已切 qwen)

### 第一通就撞上门口的回归:转写从头到尾没开始(→ C40)

通话正常、坐席通了 28 秒、CDR 正确,**日志里连一行 `transcription tap attached` 都没有**。
不是丢帧,是根本没录。根因在 `Coordinator.join`:tap 挂在**合并分支里面**,
而入口第一行是 `if !ok || !otherOK || callID == otherID { return }`。
`dec47ad`(2026-08-21)让派单腿在 `CHANNEL_CREATE` 时就绑进主叫那通电话 —— 那个改动是对的,
它正是阻止"派单读成一通外呼"的东西 —— 但从此桥接时两条腿已在同一通电话里,
`callID == otherID` 直接 return,`tapAgentLeg` 再也够不着。
**VC-S9-01 上次执行是 2026-08-20,绑定次日落地**,此后没人重跑过这条:
每一通经队列派单的电话都没有转写,而没有任何东西报错。
既有单测一直绿着,因为它造的派单腿**只带 `variable_dialed_user`、不带 `cc_member_session_uuid`** ——
那是 8-21 之前的形态,**测试模型停在旧世界**。已修并立案 C40。

### 第二通:tap 回来了,丢帧是零,但文本被测试环境污染

```
12:01:35.242  transcription tap attached  rateHz=16000
12:01:35.243  tapped stream connected
12:01:35.435  transcription live
（整通电话没有一行 "audio was dropped"）
```

```
seq 11  HUMAN_AGENT  "The quick brown fox. "
seq 12  CUSTOMER     "The quick brown fox jumps over the lazy dog. "
seq 13  HUMAN_AGENT  "Jobs always lazy dog. "
```

**一帧没丢,坐席那一路照样被切碎** —— 但这**不能判成产品缺陷**:执行者同时是主叫(手机)
与坐席(1008)且在**同一个房间**,坐席侧麦克风同时收到人声与手机扬声器,两路重叠。
下一通把手机拿开即验证了这一点。
不过它已经证伪了立案时的一个推断 —— 原文写"计数器证明是'没送到'而非'听错'",
而这一通说明**不丢帧一样会崩**:崩坏与丢帧本来就不是同一件事的两面。
另注:qwen 的 `OwnsEndpointing: false`,这个切分是引擎自己做的,不是我们的 600ms 静音规则。

### 第三通:把手机拿开,坐席那一路一字不差

```
seq 6  HUMAN_AGENT  ASR  has_agent=t  "The quick brown fox jumps over the lazy dog. "
seq 7  CUSTOMER     ASR  has_agent=f  "对后标。"          ← 手机侧串进来的模糊人声
seq 8  CUSTOMER     ASR  has_agent=f  "Hello, can you hear me? "
```

零丢帧。串音假设成立。

### 第三通同时暴露 C40 的另一半:写进了库,发给了没有人

抓 wei 的 SSE 整通:`QUEUE_JOINED` / `PARTY_RINGING` / `PARTY_ESTABLISHED` /
`PARTY_RELEASED` / `CALL_CDR` 都在,**一条 `CALL_TRANSCRIPT` 都没有**,而库里 ASR 行好好地躺着。
**坐席面板全程空白且不报错。**
根因是 C40 第一版只修了一半:tap 挪出去了,`announceAudience` 还留在合并分支里,
理由写的是"没有合并就没有 party 迁移,不必重播受众" —— **错的**。
转写 actor 按 `agentIDs` 定 scope,受众没宣告过就是空。
受众与 tap 是同一个理由、同一个时刻:派单腿在创建时就绑进来了,没有东西迁移,
但这通电话上确实多了一个人。已一并修掉。

### 第四通:四条断言全绿

```
SSE:  CALL_TRANSCRIPT isFinal=false ×27（HUMAN_AGENT 14 / CUSTOMER 13）
      isFinal=true 且含 fox 句 —— speaker=HUMAN_AGENT, agentId=807b2164…(wei)
      text: "The quick brown fox jumps over the lazy dog. The quick brown fox jumps over the lazy dog. "
DB:   seq 10 HUMAN_AGENT ASR has_agent=t  fox 句
      seq 8/9/12/13 CUSTOMER ASR has_agent=f
      seq 8–13 与该呼叫 MODEL 行共用同一序列,无冲突
丢帧: 0（坐席腿 36 秒,每侧约 1800 帧）
```

证据留档:`sse-2026-08-23.log`。

### 判定

**PASS。**

### C14 的结论:在 qwen 这条路上不复现

三通 tap 在线的电话(19s / 41s / 36s,每侧合计约 4800 帧),**丢帧全部为 0**。
立案那次是 openai 路径,每侧约 1100 帧丢 46(4.0%)与 78(7.2%)。
两条协议的每帧成本相差很大:openai 每帧 base64 + JSON 封装、24 kHz;
qwen 是裸二进制帧、16 kHz —— 约三分之一的字节量,外加每秒每路 50 次 JSON 编码的省去。

**不写成"已修"**:丢弃策略、队列深度、写超时一行没改。
准确的说法是 **"在当前部署的 provider 上不复现"** —— 与 C32 那种"同配置下不再复现"不同,
**这里是配置本身变了**。openai 路径是否仍然丢帧**未测**(测它要花 OpenAI 的钱,由 owner 定)。
真要再遇上,现在 pump 会说出是三种里的哪一种:
`dropRuns`(丢帧开始了几次)、`slowSends`(单帧发送 ≥20ms,即一帧的时长)、`maxSendMs`(最慢那次)。
