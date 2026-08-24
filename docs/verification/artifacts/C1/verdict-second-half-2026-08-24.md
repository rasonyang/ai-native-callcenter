# C1 后半闭合留证 — 2026-08-24

## 1. 缺陷前提仍成立(现场重探)

```
callcenter_config tier del does-not-exist agent-wei  → +OK
callcenter_config tier del support-en agent-nobody   → +OK
```

交换机对 miss 的删除照样回 `+OK`。这不是 08-22 的旧观察,是今天重跑的。

## 2. 修法

`converge`(`catalog/service.go`)原先只要 `DeleteCallcenterTier` 返回 nil 就 `removed++`。
现在两个循环只记**尝试次数**;若有过尝试,再读一次交换机自己的 tier 表,
按**前后差集**得出 added/removed。没有尝试就不重读 —— 常态(两侧本来一致)的
交换机读取次数不变,仍是一次。

第二次读失败时**不静默回落**到尝试数,而是照报尝试数并标 `isVerified=false` ——
静默回落等于回到原点:一个没有东西支撑的数字。

## 3. 现场:诚实路径未被改坏

给 `agent-ben` 手工插一条数据库里没有的 tier,重启应用触发对账:

```
callcenter_config tier add support-en agent-ben 1 9   → +OK
```

```
WARN agent staffing reconciled agent=agent-ben desired=1 actual=2 added=0 removed=1 failed=0
```

删后 `tier list` 确认真的没了:

```
queue|agent|state|level|position
support-zh|agent-wei|Ready|1|1
support-en|agent-wei|Ready|1|1
support-zh|agent-ben|Ready|1|5
```

全程日志中 `isVerified` 出现 0 次 —— 每一次计数都经过复读。

## 4. 谎报本身为何没有现场用例

C1 记的"症状已潜伏"仍然成立:能让 `tier del` 回 +OK 却删不掉的,是异域名字的陈旧行,
而 mod_callcenter 只列已加载队列的 tier,那行不出现在 `tier list` 里,converge 读不到,
也就不会去删它。**现场复现需要先造场景**(往 `callcenter.db` 的 tiers 插异域条目,
或临时建一个该名字的队列),不是本次修复的前置。
谎报路径由单元测试钉住,它直接构造"交换机答 +OK 但 tier 表不变"的开关。

## 5. 验收不能用 VC-S3-02(仍然成立)

S3-02 断言的是**事后两侧一致**;谎报的是**如何达成一致**,不体现在那里。
新用例 `TestConvergeCountsWhatTheSwitchDidNotWhatItAccepted` 四型:
①答 +OK 却没删 → `removed=0`;②真删了 → `removed=1`;③真加了 → `added=1`;
④复读失败 → `isVerified=false` 且仍给出尝试数。
已验证:去掉复读后 ① 与 ④ FAIL。
