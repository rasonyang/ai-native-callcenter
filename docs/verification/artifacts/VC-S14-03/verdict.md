# VC-S14-03 — 号码生命周期:建号 → 拨通 → 停用 → 拒接 · 判定:**PASS**

执行 2026-08-22 13:13–13:22(UTC 05:13–05:22)。app `logs/aicc-20260822-084508.log`。
两通真实呼叫 + 两次被拒尝试。环境已复原:95009 已删,`dids` 回到 4 条,
那通已收官呼叫的 CDR **保留**(删号不抹历史,正确)。

## note 两处已落实

**① 请求体字段名**:契约 `DIDWrite` —— 必填 `number`;另有 `language` / `flowId` /
`fallbackQueueId` / `isRecordingEnabled` / `isEnabled` / `description`。草案的写法可用。

**② 停用后走哪条分支**(起草时未静态确认):已读 `aicc_inbound.lua:47-51` ——

```lua
if route == nil then
  log("warning", "unknown number " .. tostring(did) .. " from " .. ani)
  session:hangup("UNALLOCATED_NUMBER")
  return
end
```

故期望形态可以精确写死:**warning 日志 + `UNALLOCATED_NUMBER` 挂断**,与 VC-S1-03 同族。

## 逐条对照

| expect | 实测 | |
|---|---|---|
| 建号:201 | `http=201` | ✓ |
| `luacc.dids` 立刻可查(视图,无需 reload) | `95009\|en\|t` | ✓ |
| 首拨:接通、bot 应答,CDR 一行 `did=95009` 且 `bot_sec>0` | `INBOUND\|95009\|ANSWERED\|bot_sec=6\|en`,恰好 **1 行** | ✓ |
| 停用:`isEnabled=false` | `{"number":"95009","isEnabled":false}`;底表 `95009\|f` | ✓ |
| 停用后 **`luacc.dids` 查不到** | 计数 **0**(底表行仍在——视图带 `WHERE d.is_enabled`) | ✓ |
| 再拨:明确拒接,**不是**静音或长振铃 | **owner 实测:软电话立即报 `Call to 95009 failed: Not Found`** | ✓ |
| FS 日志应有痕迹 | 见下,**恰好两条**,与两次拨号一一对应 | ✓ |

```
2026-08-22 13:20:59.582  [WARNING] aicc_inbound: unknown number 95009 from 18688886669
2026-08-22 13:21:01.452  [WARNING] aicc_inbound: unknown number 95009 from 18688886669
```

`failure_looks_like` 两种**均未发生**:没有"停用后仍接通",也没有"长振铃后静音挂断"。

## 顺带把 C29 证成了系统性缺陷

C29(建分机不写 `isEnabled` 就建出停用的)本来只在 `/extensions` 上实证。
本次在 **`/dids` 上复现**,同一形态:

```
POST /dids {number,language,flowId,fallbackQueueId,isRecordingEnabled}  → isEnabled:false,luacc.dids 计数 0
POST /dids {…同上…, isEnabled:true}                                      → isEnabled:true, 可查
```

所以 C29 **不是分机接口独有**,是"可选布尔 + 非指针字段"的通病 —— 已回写 C29 条目。

## 我的一个预测错了,错法值得记

执行前我预测:"再拨**不会**产生 CDR —— Lua 的 `hangup` 在第 51 行,而铸造呼叫身份的
`create_uuid` 在第 56 行,挂断发生在身份产生之前"。

**实测 CDR 总数 6212 → 6214,两次被拒各落一行。** 推理错在:应用侧并不依赖 Lua 铸的身份,
它自己看得见主叫腿的 channel 事件,照样组装一行。**而且这是对的** —— 打进来又被拒的呼叫
本就该留痕,`hangup_cause=UNALLOCATED_NUMBER` 也精确。

## 但那两行**记不出对方拨的是哪个号** → 立案 C31

```
call_id        | call_type | did    | status    | to_number | hangup_cause
01a027ea-4c17… | INBOUND   | (null) | NO_ANSWER | (空)      | UNALLOCATED_NUMBER
01a027ea-44af… | INBOUND   | (null) | NO_ANSWER | (空)      | UNALLOCATED_NUMBER
```

**全库 11 行 `UNALLOCATED_NUMBER` 无一例外**(2026-08-19 至今),含 VC-S1-03 当时那 6 行。
机制在 `cdr.go:199-201`:`ToNumber = originator.OtherNumber`,取不到则回落 `cdr.DID`;
被拒呼叫两者皆空,于是号码丢了 —— **而交换机日志里明明白白打着 `95009`**。

后果:运营能看到"有人被拒了",但看不出**他拨的是什么** ——
"有人一直打一个已停用的号"与"有人在扫号"在账本里长得一模一样。

**并且这说明 VC-S1-03 的覆盖比它看起来薄**:那条用例只对了行数(+6 对应 6 条日志),
从未断言号码,所以这个洞在它眼皮底下 PASS 了两天。已建议在 C31 修复后给 S1-03 补一条断言。
