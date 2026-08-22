# VC-S13-06 — 联系人全链(建/查/改/删)· 判定:**PASS**

执行 2026-08-22 10:32。自建自清,未产生呼叫。app `logs/aicc-20260822-084508.log`。
基线 contacts=1,收尾 contacts=**1** —— 环境无残留。

## 执行前:草案 note 点名的三处,以契约为准核过

note 写着"【起草时未核三处,执行前须查】…三处以契约为准,**不符则改 collect 而非改 expect**"。
实查 `docs/openapi.json`,**三中有两处草案猜错了**:

| # | 草案的猜测 | 契约实际 | 处置 |
|---|---|---|---|
| ① 请求体字段 | `displayName` / 响应 `contactId` | **`name`** / 响应 **`id`**(`ContactWrite` 仅 `phoneNumber` 必填;另有 `email`、`tags`) | **改 collect** |
| ② 查询参数 | `q` | `q` ✓,另有更精确的 **`phoneNumber`** | collect 保留 `q`,**加测 `phoneNumber`** |
| ③ DELETE 成功码 | "204 还是 200" | **204**(契约只列 204) | expect 收紧为 204 |

按 note 的规矩执行:**改的是 collect 的字段拼写,expect 的实质一条没动**
(建返回 id、名字能往返、按号码查得到、删干净)。

## 逐条对照

| expect | 实测 | |
|---|---|---|
| 建:201,响应含 id | `http=201`,`id=01a02750-4ff7-…` | ✓ |
| 按号码查得到,name 与写入一致 | `q=18600000001` → `[{id:01a02750-…, name:"VC Probe", phoneNumber:"18600000001"}]` | ✓ |
| (加测)`phoneNumber=` 精确查 | 同一条 | ✓ |
| 改:200,name 变为 "VC Probe Renamed" | `{"name":"VC Probe Renamed","company":"NovaNet","notes":"renamed"}` | ✓ |
| 按新名字能查到 | `["VC Probe Renamed"]` | ✓ |
| 删:204 | `delete http=204` | ✓ |
| 删后按号码查 items 长度=0 | `0` | ✓ |
| 全程**坐席**会话,不需管理员 | wei 的 jar 全程 200/201/204 | ✓ |

## 两条 `failure_looks_like` 都被正面证伪

- **"建返回 201 而按号码查不到 —— 查询只匹配 name"**:未发生。
  `q=18600000001` 拿的是**号码**去查,查到了 —— 也就是说 `q` 确实跨字段匹配,
  而不是只认名字。这正是这张表存在的理由(坐席在通话中拿来电号码找人),
  用例的这一条**有分辨力**:若查询只匹配 name,第 2 条会返回空表。
- **"删返回 204 但行仍在,只是被标记(软删)"**:未发生,且是**直接查库证伪**的 ——
  `select count(*) from contacts where phone_number='18600000001'` → **0**,
  且 contacts 总数由 1 → 2 → **1**。不是列表过滤掉了,是行真的没了。
