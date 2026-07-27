# 请求明细「来源」显示提供商名称（非密钥）

日期：2026-07-27  
状态：草案（待用户审阅）

## 背景

仪表盘「请求明细」与「维度明细」中的「来源」列绑定字段 `source`（`Dimensions.Source`）。该值来自 CLIProxyAPI 的 `UsageRecord.Source`。

host 侧 `resolveUsageSource` 常把完整 API Key、账号标识等写入 `Source`，因此明细里会出现 `sk-…` 一类密钥串。插件 README 的隐私承诺是不保存 API Key，但当前实现把 host 的 `Source` 原样落库，间接违反了该意图。

用户希望：

1. 「来源」显示**提供商名称**，而不是密钥  
2. 在有安全可用信息时，可附带**账号摘要**（如邮箱、项目 ID）  
3. 客户端 IP、User-Agent **本轮不做**（`UsageRecord` 无对应字段）  
4. 历史密钥样数据**不回填**；只影响新请求

## 目标

对新写入的用量记录：`Dimensions.Source` 存为可读、非密钥的来源展示串，优先包含提供商名称。

### 成功标准

- 新请求在请求明细 / 维度明细 / CSV 导出中，`source` 不再为完整密钥串  
- 典型显示：`anthropic` 或 `anthropic · user@example.com`  
- 继续忽略 `APIKey`、`AuthID`、`AuthIndex`、失败 body  
- 既有测试通过，并新增 Source 清洗用例

### 非目标

- 不新增客户端 IP / User-Agent 列  
- 不显示 provider 服务地址（`base_url` 不下发到 `UsageRecord`）  
- 不迁移或脱敏历史 `source`  
- 不改表头文案（仍为「来源」）  
- 不改 JSON 字段名（仍为 `source`）  
- 不改 CLIProxyAPI host

## 可用数据（约束）

插件经 `pluginapi.UsageRecord` 实际可依赖的相关字段：

| 字段 | 用途 |
|---|---|
| `Provider` | 提供商标识（可靠） |
| `Source` | host 填的来源；可能是邮箱 / 项目 ID / **密钥** |

不可用：

| 期望 | 原因 |
|---|---|
| Client IP | UsageRecord 无字段 |
| User-Agent | UsageRecord 无字段 |
| base_url / 服务地址 | 仅在 host Auth 属性中，不下发 |

## 方案选择

采用 **写入时清洗（方案 A）**：

- 在 `decodeUsage` 中对 `Source` 做清洗后再写入维度  
- 旧记录保持原样  
- 读路径、仪表盘、API 无需特殊分支即可受益（针对新数据）

未选方案 B（仅前端改显示）：API/CSV 仍可能泄漏密钥。  
未选方案 C（拆 `source_display`）：改动面过大，收益与 A 相近。

## 设计

### 数据流

```
UsageRecord JSON
  → decodeUsage()
  → provider = normalizeDimension(Provider)
  → rawSource = firstString(Source)
  → Dimensions.Source = sanitizeSource(provider, rawSource)
  → Store.Record → 分钟聚合 + RequestDetail
  → /requests、仪表盘、CSV 原样读出
```

清洗只发生在 **decode 写入路径**；`normalizeDimension` 仍对最终字符串做 trim / UTF-8 / 长度上限（160 runes）。

### `sanitizeSource(provider, raw)`

伪代码：

```
provider = trim(provider)
raw = trim(raw)
summary = safeSourceSummary(raw)   // 密钥样则 ""

if provider != "" && summary != "" && !equalFold(provider, summary):
  return normalizeDimension(provider + " · " + summary)
if provider != "":
  return normalizeDimension(provider)
if summary != "":
  return normalizeDimension(summary)
return ""
```

合成格式固定为：`provider · 摘要`（中间为「空格 + 中点 + 空格」）。

### `safeSourceSummary(raw)`：何为安全摘要

返回可展示的摘要，或空串（表示丢弃）。

**判定为密钥样并丢弃**（满足任一即可）：

1. 包含常见密钥子串（大小写不敏感）：`sk-`、`sk_`、`api-key`、`apikey`、`bearer `  
2. 匹配常见密钥前缀模式（大小写不敏感）：以 `sk-`、`sk_`、`rk-`、`key-` 开头  
3. **高熵 token 启发式**：  
   - 不含 `@`  
   - 不含空白  
   - 长度 ≥ 20  
   - 仅由 `[A-Za-z0-9._-]` 组成  

**保留为摘要**（通过上述过滤后）：

- 含 `@` 的邮箱类字符串  
- 较短可读 label / 项目 ID / 非密钥账号名  
- 与 provider 同名的安全字符串（如 `anthropic`）——合成时去重，只保留 provider

边界：

- 空字符串 → 无摘要  
- 仅密钥、无 provider → `Source` 为空  
- 摘要与 provider 相同（忽略大小写）→ 只存 provider，避免 `anthropic · anthropic`

### UI / API / 导出

| 表面 | 行为 |
|---|---|
| 请求明细「来源」列 | 继续 `item.source`；新数据为清洗后值 |
| 维度明细「来源」列 | 继续 `g.source`；新数据为清洗后值 |
| CSV「来源」列 | 同源字段 |
| 表头 | 不变：「来源」 |
| 历史行 | 可能仍显示密钥；本轮不处理 |

### 错误与隐私

- 清洗失败模式：宁可丢弃可疑值，也不落库完整密钥  
- 不因 Source 清洗返回 400；decode 其余逻辑不变  
- 测试继续断言 `APIKey` / `AuthID` / failure body 不出现在持久化结构中  
- 新增断言：密钥样 `Source` 不得出现在 `Dimensions.Source`

### 测试

在 `usage_test.go`（及必要时 `request_log` / persistence 相关测试）覆盖：

1. `Source = sk-ant-…` + `Provider = anthropic` → `Source == "anthropic"`  
2. `Source = user@example.com` + `Provider = openai` → `Source == "openai · user@example.com"`  
3. `Source` 为空 + 有 Provider → `Source == provider`  
4. `Source` 与 Provider 相同 → 不重复合成  
5. 长高熵串（无 `@`）→ 丢弃，仅保留 provider  
6. 原有敏感字段不泄漏用例仍通过  

### 文件改动（实现阶段）

| 文件 | 改动 |
|---|---|
| `usage.go` | 新增 `sanitizeSource` / `safeSourceSummary`；`decodeUsage` 使用清洗结果 |
| `usage_test.go` | 新增上述用例 |
| `README.md` | 隐私/字段说明：`source` 为提供商名或「提供商 · 安全摘要」，不存密钥 |

仪表盘 HTML/JS **无需改列绑定**（字段名与表头不变）。

## 后续（本规格外）

- 客户端 IP / User-Agent：需 host 在 `UsageRecord` 增加字段后再做  
- 服务地址：需 host 下发 `base_url` 或等价字段  
- 历史密钥脱敏：可选读路径过滤或一次性迁移  

## 风险与取舍

| 风险 | 缓解 |
|---|---|
| 启发式误杀合法长 label | 阈值：误杀只丢摘要，仍保留 provider；可后续加白名单 |
| 启发式漏杀密钥 | 偏保守：常见前缀 + 高熵规则；仍可能漏掉非典型密钥格式 |
| 旧数据仍含密钥 | 已接受「只影响新请求」；README 可注明 |
| 维度分组因 Source 变化而变 | 新分组键变为 provider/摘要；符合产品意图 |
