package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const maxDimensionRunes = 160

type normalizedUsage struct {
	Dimensions  Dimensions
	RequestedAt time.Time
	LatencyNS   uint64
	TTFTNS      uint64
	Counters    Counters
}

func decodeUsage(raw []byte, now time.Time) (normalizedUsage, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return normalizedUsage{}, fmt.Errorf("decode usage record: %w", err)
	}

	requestedAt := firstTime(root, "RequestedAt", "requested_at")
	now = now.UTC()
	if requestedAt.IsZero() || requestedAt.After(now.Add(24*time.Hour)) {
		requestedAt = now
	} else {
		requestedAt = requestedAt.UTC()
	}

	failure := firstObject(root, "Failure", "failure")
	detail := firstObject(root, "Detail", "detail")
	inputTokens := firstInt64(detail, "InputTokens", "input_tokens")
	outputTokens := firstInt64(detail, "OutputTokens", "output_tokens")
	reasoningTokens := firstInt64(detail, "ReasoningTokens", "reasoning_tokens")
	cachedTokens := firstInt64(detail, "CachedTokens", "cached_tokens")
	cacheReadTokens := firstInt64(detail, "CacheReadTokens", "cache_read_tokens")
	cacheCreationTokens := firstInt64(detail, "CacheCreationTokens", "cache_creation_tokens")
	total := firstInt64(detail, "TotalTokens", "total_tokens")
	if total <= 0 {
		// The SDK always serializes TotalTokens, so providers that leave it at
		// zero still produce a present JSON field. Match the raw-record fallback
		// used by the companion statistics implementation: input + output +
		// reasoning, then cached tokens only when that sum is still zero.
		total = saturatingInt64Sum(saturatingInt64Sum(inputTokens, outputTokens), reasoningTokens)
		if total == 0 {
			total = cachedTokens
		}
	}

	failed := firstBool(root, "Failed", "failed")
	provider := normalizeDimension(firstString(root, "Provider", "provider"))
	return normalizedUsage{
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
		RequestedAt: requestedAt,
		LatencyNS:   positiveDurationNS(root, "Latency", "latency", "latency_ns"),
		TTFTNS:      positiveDurationNS(root, "TTFT", "ttft", "ttft_ns"),
		Counters: Counters{
			Requests:            1,
			FailedRequests:      boolCount(failed),
			InputTokens:         positiveUint(inputTokens),
			OutputTokens:        positiveUint(outputTokens),
			ReasoningTokens:     positiveUint(reasoningTokens),
			CachedTokens:        positiveUint(cachedTokens),
			CacheReadTokens:     positiveUint(cacheReadTokens),
			CacheCreationTokens: positiveUint(cacheCreationTokens),
			TotalTokens:         positiveUint(total),
		},
	}, nil
}

func firstObject(root map[string]json.RawMessage, keys ...string) map[string]json.RawMessage {
	for _, key := range keys {
		if value, ok := root[key]; ok {
			var result map[string]json.RawMessage
			if json.Unmarshal(value, &result) == nil {
				return result
			}
		}
	}
	return map[string]json.RawMessage{}
}

func firstString(root map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		if value, ok := root[key]; ok {
			var result string
			if json.Unmarshal(value, &result) == nil {
				return result
			}
		}
	}
	return ""
}

func firstBool(root map[string]json.RawMessage, keys ...string) bool {
	for _, key := range keys {
		if value, ok := root[key]; ok {
			var result bool
			if json.Unmarshal(value, &result) == nil {
				return result
			}
		}
	}
	return false
}

func firstInt64(root map[string]json.RawMessage, keys ...string) int64 {
	value, _ := firstInt64Present(root, keys...)
	return value
}

func firstInt64Present(root map[string]json.RawMessage, keys ...string) (int64, bool) {
	for _, key := range keys {
		value, ok := root[key]
		if !ok {
			continue
		}
		var result int64
		if json.Unmarshal(value, &result) == nil {
			return result, true
		}
	}
	return 0, false
}

func firstTime(root map[string]json.RawMessage, keys ...string) time.Time {
	for _, key := range keys {
		if value, ok := root[key]; ok {
			var result time.Time
			if json.Unmarshal(value, &result) == nil {
				return result
			}
		}
	}
	return time.Time{}
}

func positiveDurationNS(root map[string]json.RawMessage, keys ...string) uint64 {
	for _, key := range keys {
		value, ok := root[key]
		if !ok {
			continue
		}
		var numeric int64
		if json.Unmarshal(value, &numeric) == nil {
			return positiveUint(numeric)
		}
		var text string
		if json.Unmarshal(value, &text) == nil {
			if duration, err := time.ParseDuration(text); err == nil && duration > 0 {
				return uint64(duration)
			}
		}
	}
	return 0
}

func normalizeDimension(value string) string {
	value = strings.TrimSpace(value)
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "�")
	}
	runes := []rune(value)
	if len(runes) > maxDimensionRunes {
		value = string(runes[:maxDimensionRunes])
	}
	return value
}

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

func positiveUint(value int64) uint64 {
	if value <= 0 {
		return 0
	}
	return uint64(value)
}

func boolCount(value bool) uint64 {
	if value {
		return 1
	}
	return 0
}

func clampStatus(value int64) int {
	if value < 0 || value > 999 {
		return 0
	}
	return int(value)
}

func saturatingInt64Sum(left, right int64) int64 {
	if left <= 0 {
		left = 0
	}
	if right <= 0 {
		right = 0
	}
	if left > int64(^uint64(0)>>1)-right {
		return int64(^uint64(0) >> 1)
	}
	return left + right
}
