# 请求明细「来源」显示提供商名称 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 新写入的用量记录把 `Dimensions.Source` 清洗为提供商名称（可选附带安全账号摘要），请求明细/维度明细/CSV 不再显示完整 API Key。

**Architecture:** 仅在 `decodeUsage` 写入路径清洗 host 下发的 `Source`。新增 `safeSourceSummary` 识别并丢弃密钥样值，`sanitizeSource` 合成 `provider` 或 `provider · 摘要`。读路径、仪表盘、API 字段名与表头不变，新数据自动受益；历史记录不回填。

**Tech Stack:** Go 1.26、`encoding/json`、现有 `pluginapi.UsageRecord`、`go test`、内嵌仪表盘（无前端框架）

## Global Constraints

- 只影响新请求；不迁移/脱敏历史 `source`
- 不改表头「来源」、不改 JSON 字段名 `source`
- 不新增 Client IP / User-Agent / base_url
- 继续忽略 `APIKey`、`AuthID`、`AuthIndex`、失败 body
- 合成格式固定为 `provider · 摘要`（空格 + 中点 U+00B7 + 空格）
- 密钥判定偏保守：宁可丢弃可疑摘要，也不落库完整密钥
- 遵循现有代码风格：无多余注释、函数小而专一、TDD

**Spec:** `docs/superpowers/specs/2026-07-27-request-source-display-design.md`

---

## File Structure

| 文件 | 职责 |
|---|---|
| `usage.go` | `decodeUsage`；新增 `sanitizeSource` / `looksLikeSecretSource` / `safeSourceSummary` |
| `usage_test.go` | Source 清洗与不泄漏用例 |
| `README.md` | 中英文隐私说明：`source` 为提供商或「提供商 · 安全摘要」 |

仪表盘 `dashboard.go` **不改**（已绑定 `item.source` / `g.source`）。

---

### Task 1: Source 清洗逻辑（TDD）

**Files:**
- Modify: `usage.go`（`decodeUsage` 中 Source 赋值；文件末尾附近新增辅助函数）
- Test: `usage_test.go`

**Interfaces:**
- Consumes: 现有 `normalizeDimension`、`firstString`、`maxDimensionRunes`
- Produces:
  - `func sanitizeSource(provider, raw string) string`
  - `func safeSourceSummary(raw string) string`
  - `func looksLikeSecretSource(raw string) bool`
  - `decodeUsage` 写入：`Source: sanitizeSource(provider, firstString(root, "Source", "source"))`  
    其中 `provider` 为已 `normalizeDimension` 的 Provider 值

- [ ] **Step 1: 写失败测试（表驱动）**

在 `usage_test.go` 追加：

```go
func TestSanitizeSource(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		raw      string
		want     string
	}{
		{
			name:     "api key becomes provider",
			provider: "anthropic",
			raw:      "sk-ant-api03-abcdefghijklmnopqrstuvwxyz012345",
			want:     "anthropic",
		},
		{
			name:     "email becomes provider with summary",
			provider: "openai",
			raw:      "user@example.com",
			want:     "openai · user@example.com",
		},
		{
			name:     "empty source uses provider",
			provider: "gemini",
			raw:      "",
			want:     "gemini",
		},
		{
			name:     "same provider and source no duplicate",
			provider: "anthropic",
			raw:      "Anthropic",
			want:     "anthropic",
		},
		{
			name:     "long high-entropy token discarded",
			provider: "openai",
			raw:      "AbCdEfGhIjKlMnOpQrStUvWxYz0123",
			want:     "openai",
		},
		{
			name:     "secret only yields empty",
			provider: "",
			raw:      "sk-proj-abcdefghijklmnopqrstuvwxyz",
			want:     "",
		},
		{
			name:     "short label kept with provider",
			provider: "vertex",
			raw:      "my-project-id",
			want:     "vertex · my-project-id",
		},
		{
			name:     "provider trimmed",
			provider: " anthropic ",
			raw:      "sk-ant-secret-value-long-enough",
			want:     "anthropic",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeSource(tt.provider, tt.raw)
			if got != tt.want {
				t.Fatalf("sanitizeSource(%q, %q) = %q, want %q", tt.provider, tt.raw, got, tt.want)
			}
		})
	}
}

func TestDecodeUsageSanitizesSecretSource(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	secret := "sk-ant-api03-abcdefghijklmnopqrstuvwxyz012345"
	record := pluginapi.UsageRecord{
		Provider:    "anthropic",
		Model:       "claude-opus-4",
		Source:      secret,
		APIKey:      "must-not-survive",
		AuthID:      "secret-auth",
		RequestedAt: now,
		Detail:      pluginapi.UsageDetail{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	usage, err := decodeUsage(raw, now)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Dimensions.Source != "anthropic" {
		t.Fatalf("Source = %q, want anthropic", usage.Dimensions.Source)
	}
	encoded, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{secret, "must-not-survive", "secret-auth"} {
		if strings.Contains(string(encoded), leak) {
			t.Fatalf("sensitive value leaked: %s", leak)
		}
	}
}

func TestDecodeUsageKeepsEmailSourceSummary(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		Provider:    "openai",
		Model:       "gpt-4o",
		Source:      "user@example.com",
		RequestedAt: now,
		Detail:      pluginapi.UsageDetail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	usage, err := decodeUsage(raw, now)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Dimensions.Source != "openai · user@example.com" {
		t.Fatalf("Source = %q, want openai · user@example.com", usage.Dimensions.Source)
	}
}
```

同时更新现有 `TestDecodeUsageSDKJSON`：当前 `Source: "anthropic"` 且 Provider 也为 anthropic，清洗后仍为 `"anthropic"`（与「不重复合成」一致）。若该测试未断言 Source，可不改；可选加：

```go
if usage.Dimensions.Source != "anthropic" {
	t.Fatalf("Source = %q, want anthropic", usage.Dimensions.Source)
}
```

- [ ] **Step 2: 跑测试确认失败**

Run:

```powershell
$env:GOTOOLCHAIN='auto'; go test -count=1 -run "TestSanitizeSource|TestDecodeUsageSanitizesSecretSource|TestDecodeUsageKeepsEmailSourceSummary" .
```

Expected: FAIL，提示 `sanitizeSource` 未定义，或 Source 仍为密钥。

- [ ] **Step 3: 实现清洗函数并接入 decodeUsage**

在 `usage.go` 的 `decodeUsage` 中，先算出 provider，再清洗 Source。将 Dimensions 构造改为：

```go
provider := normalizeDimension(firstString(root, "Provider", "provider"))
// ...
Dimensions: Dimensions{
	Provider:        provider,
	ExecutorType:    normalizeDimension(firstString(root, "ExecutorType", "executor_type")),
	Model:           normalizeDimension(firstString(root, "Model", "model")),
	Alias:           normalizeDimension(firstString(root, "Alias", "alias")),
	Source:          sanitizeSource(provider, firstString(root, "Source", "source")),
	AuthType:        normalizeDimension(firstString(root, "AuthType", "auth_type")),
	ServiceTier:     normalizeDimension(firstString(root, "ServiceTier", "service_tier")),
	ReasoningEffort: normalizeDimension(firstString(root, "ReasoningEffort", "reasoning_effort")),
	Failed:          failed,
	FailureStatus:   clampStatus(firstInt64(failure, "StatusCode", "status_code")),
},
```

在 `usage.go` 末尾（`normalizeDimension` 附近）新增：

```go
func sanitizeSource(provider, raw string) string {
	provider = strings.TrimSpace(provider)
	summary := safeSourceSummary(raw)
	switch {
	case provider != "" && summary != "" && !strings.EqualFold(provider, summary):
		return normalizeDimension(provider + " · " + summary)
	case provider != "":
		return normalizeDimension(provider)
	case summary != "":
		return normalizeDimension(summary)
	default:
		return ""
	}
}

func safeSourceSummary(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || looksLikeSecretSource(raw) {
		return ""
	}
	return raw
}

func looksLikeSecretSource(raw string) bool {
	lower := strings.ToLower(raw)
	for _, marker := range []string{"sk-", "sk_", "api-key", "apikey", "bearer "} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	for _, prefix := range []string{"sk-", "sk_", "rk-", "key-"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	if strings.Contains(raw, "@") || strings.ContainsAny(raw, " \t\r\n") {
		return false
	}
	if len(raw) < 20 {
		return false
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}
```

说明：

- `sanitizeSource` 入参 `provider` 在 `decodeUsage` 里已是 normalize 后的值；测试里也会传入带空格的 provider，函数内再 `TrimSpace` 一次无害。
- 表驱动用例 `"provider trimmed"` 的 want 是 `"anthropic"`：`normalizeDimension` 会 trim，正确。
- `"same provider and source no duplicate"`：raw `Anthropic` 经 `EqualFold` 与 `anthropic` 相同，只返回 normalize 后的 provider。注意：`sanitizeSource` 返回的是 **normalize 后的 provider**（小写/trim 后的入参），不是 raw 的大小写。测试 want 为 `"anthropic"` 与 decode 路径一致（provider 先 normalize）。
- 短 label `my-project-id`：长度 < 20 或含 `-` 但长度不足 20 → 不触发高熵规则，保留。

- [ ] **Step 4: 跑测试确认通过**

Run:

```powershell
$env:GOTOOLCHAIN='auto'; go test -count=1 -run "TestSanitizeSource|TestDecodeUsage|TestNormalizeDimension" .
```

Expected: PASS（全部相关用例）。

再跑全量（若环境 Go 版本允许）：

```powershell
$env:GOTOOLCHAIN='auto'; go test -count=1 .
```

Expected: PASS。

- [ ] **Step 5: Commit**

```powershell
git add usage.go usage_test.go
git commit -m "feat(usage): sanitize request source to provider display"
```

---

### Task 2: README 隐私与字段说明

**Files:**
- Modify: `README.md`（中文「隐私」与英文对应段落）

**Interfaces:**
- Consumes: Task 1 的 Source 语义（提供商名或 `provider · 安全摘要`）
- Produces: 文档与实现一致

- [ ] **Step 1: 更新中文隐私段**

在「隐私」一节，于「不会保存 API Key」列表保持不变；把描述数据库内容的段落中关于「来源」的说明改得更明确。

将类似：

> 逐请求用量元数据（例如时间、模型、来源、Tier、…）

改为明确：

> 逐请求用量元数据（例如时间、模型、来源、Tier、…）。其中「来源」在写入时清洗为提供商名称，或「提供商 · 安全账号摘要」（如邮箱、项目 ID）；host 若把 API Key 填入 Source，插件会丢弃该密钥并仅保留提供商名。历史记录不回填。客户端 IP、User-Agent 不在 UsageRecord 中，本插件不采集。

「不会保存」列表可追加一句（可选，保持简洁）：

- 以密钥形式出现在 host `Source` 中的值（写入时丢弃）

- [ ] **Step 2: 更新英文 Privacy 段**

对 English 章节做对等修改，例如：

> Per-request metadata includes time, model, source, tier, …. The stored `source` is sanitized at write time to the provider name, or `provider · safe account summary` (e.g. email, project id). If the host places an API key in `Source`, the plugin discards that secret and keeps the provider name. Existing rows are not rewritten. Client IP and User-Agent are not present on `UsageRecord` and are not collected.

- [ ] **Step 3: 快速目视检查**

确认中英文都提到：

1. source = provider 或 provider · 安全摘要  
2. 不存密钥样 Source  
3. 历史不回填  
4. 无 IP/UA  

- [ ] **Step 4: Commit**

```powershell
git add README.md
git commit -m "docs: clarify sanitized request source privacy"
```

---

### Task 3: 全量验证

**Files:**
- 无新增代码；验证 Task 1–2

- [ ] **Step 1: 跑全部测试**

```powershell
$env:GOTOOLCHAIN='auto'; go test -count=1 .
```

Expected: 全部 PASS。

- [ ] **Step 2: 若存在 lint 脚本则运行**

本仓库无独立 `golangci-lint` 配置时，至少保证：

```powershell
$env:GOTOOLCHAIN='auto'; go test -count=1 -race . 
```

（Windows 上 `-race` 可能不可用；不可用则跳过 race，仅 `go test`。）

- [ ] **Step 3: 对照 spec 成功标准自检**

| 标准 | 验证方式 |
|---|---|
| 密钥 Source → provider | `TestSanitizeSource` / `TestDecodeUsageSanitizesSecretSource` |
| 邮箱 → provider · email | `TestDecodeUsageKeepsEmailSourceSummary` |
| 空 Source → provider | `TestSanitizeSource` empty case |
| 同名不重复 | `TestSanitizeSource` same case |
| 高熵串丢弃 | `TestSanitizeSource` long token case |
| 敏感字段不泄漏 | 既有 + 新 decode 测试 |
| README 已更新 | 文件 diff |

- [ ] **Step 4: 若有失败则修复后再提交**

仅当 Step 1–3 发现问题并修改代码时再 commit；否则无需空提交。

---

## Self-Review

1. **Spec coverage**
   - 写入清洗 → Task 1  
   - 密钥/邮箱/空/同名/高熵 → Task 1 测试  
   - UI/CSV 不改绑定 → 计划明确不改 `dashboard.go`  
   - 历史不回填 → Global Constraints + README  
   - 文档 → Task 2  
   - IP/UA/base_url 非目标 → 未列入任务  

2. **Placeholder scan:** 无 TBD/TODO；步骤含完整测试与实现代码。  

3. **Type consistency:** `sanitizeSource(provider, raw string) string` 在 Task 1 测试与实现一致；`Dimensions.Source` 仍为 `string` `json:"source"`。
