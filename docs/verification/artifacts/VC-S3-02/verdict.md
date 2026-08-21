# VC-S3-02 — FAIL(发现真实镜像漂移 + 收敛记账缺陷)(2026-08-20)

## 实测
- DB 期望(已签入坐席):agent-wei|support-en, agent-wei|support-zh
- switch 现实(tier list):support-en@192.168.31.176(**陈旧,旧 IP 域**)、support-en@192.168.31.55、support-zh@192.168.31.55
- 另:switch 存在 DB 没有的静态队列 support@default(宿主 callcenter.conf.xml 遗留)

## 根因(比 failure_looks_like 预言的更糟:漂移不是"未被发现",而是每次都被"成功修复")
1. CallcenterTiers() 只剥配置域后缀(internal/telephony/tiers.go:64-66),异域名 support-en@192.168.31.176 原样保留 —— 这一步是对的(防伪装)。
2. converge 视其为多余 tier,调 DeleteCallcenterTier("support-en@192.168.31.176", agent-wei)(internal/catalog/service.go:286-296)。
3. adapter.DeleteCallcenterTier 无条件再限定域(internal/telephony/adapter.go:145-147 → QueueName adapter.go:42),实际下发
   `callcenter_config tier del support-en@192.168.31.176@192.168.31.55 agent-wei` —— 目标不存在。
4. mod_callcenter 对 miss 的 tier del 返回 +OK(本次探针实证,见 output.txt),converge 记 removed=1 failed=0。
   日志证据(logs/aicc-20260820-110836.log):11:08 两次、11:32、11:39 各一次
   "agent staffing reconciled agent=agent-wei ... removed=1 failed=0",而 tier 至今仍在。

## 影响
- 旧 IP 域的陈旧 tier 永生;每次签入/重连的收敛日志谎报成功(账面 removed=1)。
- 若旧域队列真实存在(再次换 IP 后),坐席会继续被不该派的队列派单 —— 正是本用例 failure_looks_like 预言的静默形态。

## 建议修复方向(仅记录,不实施)
- adapter.DeleteCallcenterTier 不应对已含 "@" 的名字再做 QueueName 限定;或 converge 对异域条目原样下发删除。
- converge 的 removed 计数应以删除后复查(或解析 mod_callcenter 实际删除行数)为准,而非 +OK 即成功。

## 判定依据(expect 对照)
- expect 要求两侧逐行一致 → 实际 switch 多出 support-en@192.168.31.176 一行 → 不一致 → FAIL。
- agent list:agent-wei status=Available、contact=user/1008@192.168.31.55,与 DB(READY、分机 1008)一致 → 该子项通过。

---

## 追记 2026-08-21:症状潜伏,缺陷未动 —— 重跑会得到假通过

阶段 5 开跑前复查,现场变了:

```
callcenter_config tier list   →  只有 3 行(support-en@…55 / support-zh@…55 ×2),陈旧行不在其中
sqlite3 /usr/local/freeswitch/db/callcenter.db "select queue, agent from tiers"
                              →  4 行,含 support-en@192.168.31.176|agent-wei   ← 还在
今天的 converge 日志           →  "already matched … added=0 removed=0"          ← 不再谎报
```

**陈旧行仍在 mod_callcenter 自己的数据库里,只是从 `tier list` 里消失了** ——
mod_callcenter 只列出**已加载队列**的 tier,而叫 `support-en@192.168.31.176` 的队列早已不存在
(本机 IP 已固定为 …55)。于是 `CallcenterTiers()` 读不到它,converge 也就不再对它下发那条
注定落空的删除,`removed=1` 的谎报随之消失。

**代码缺陷一个字没动**:`adapter.go` 的 `DeleteCallcenterTier` 仍然无条件
`a.QueueName(queue)`,任何已含 `@` 的名字都会被二次限定成
`support-en@192.168.31.176@192.168.31.55`。只要那个域的队列再次出现(主机 IP 换回去、
或多机部署里另一台的域进入视野),行会重新浮现,谎报也会一并回来。

**因此:今天重跑 VC-S3-02 大概率 PASS,而那是假通过。** 判定维持 FAIL,
直到 `DeleteCallcenterTier` 的二次限定与 converge 的 `removed` 计数口径被真正修掉。
复现方法(需要时):往 `callcenter.db` 的 tiers 插一行异域条目,或临时建一个该名字的队列。

**对阶段 5 的影响:无。** S12-03 的 expect 是"**正确的** tier 存在",多一行陈旧的不会让它失败。
反过来,S12-03 重启 FreeSWITCH 会让 mod_callcenter 从 `callcenter.db` 重新加载,
那一行有可能重新进入 `tier list` —— 若真如此,C1 就重新有了活的复现场景,对修它是好事。
