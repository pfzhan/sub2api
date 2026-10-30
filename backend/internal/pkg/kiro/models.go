package kiro

import (
	"sort"
	"strings"
)

// modelSpec 描述一个 Kiro 上游模型。
type modelSpec struct {
	// UpstreamID 是 conversationState 里 modelId 字段使用的 Kiro 模型名。
	UpstreamID string
	// ContextWindow 用于把 contextUsagePercentage 换算成 token 数。
	ContextWindow int
	// EffortLevels 是 additionalModelRequestFields 允许的 effort 档位；为空表示
	// 该模型不接受这个字段，请求里必须整个省略。
	EffortLevels []string
	// EffortField 区分 effort 的写法：Claude 写在 output_config，GPT 写在 reasoning。
	EffortField effortField
}

type effortField int

const (
	effortFieldNone effortField = iota
	effortFieldOutputConfig
	effortFieldReasoning
)

const defaultContextWindow = 200_000

var (
	claudeEffortFull    = []string{"low", "medium", "high", "xhigh", "max"}
	claudeEffortNoXHigh = []string{"low", "medium", "high", "max"}
	gptEffortLevels     = []string{"none", "low", "medium", "high", "xhigh", "max"}
)

// models 以客户端模型名为键。键使用 Anthropic 的连字符写法，
// 查找前会把点号写法和日期后缀归一化。
var models = map[string]modelSpec{
	"claude-opus-5-5":   {UpstreamID: "claude-opus-5.5", ContextWindow: 1_000_000, EffortLevels: claudeEffortFull, EffortField: effortFieldOutputConfig},
	"claude-sonnet-5-5": {UpstreamID: "claude-sonnet-5.5", ContextWindow: 1_000_000, EffortLevels: claudeEffortFull, EffortField: effortFieldOutputConfig},
	"claude-opus-5":     {UpstreamID: "claude-opus-5", ContextWindow: 1_000_000, EffortLevels: claudeEffortFull, EffortField: effortFieldOutputConfig},
	"claude-sonnet-5":   {UpstreamID: "claude-sonnet-5", ContextWindow: 1_000_000, EffortLevels: claudeEffortFull, EffortField: effortFieldOutputConfig},
	"claude-opus-4-8":   {UpstreamID: "claude-opus-4.8", ContextWindow: 1_000_000, EffortLevels: claudeEffortFull, EffortField: effortFieldOutputConfig},
	"claude-opus-4-7":   {UpstreamID: "claude-opus-4.7", ContextWindow: 1_000_000, EffortLevels: claudeEffortFull, EffortField: effortFieldOutputConfig},
	"claude-opus-4-6":   {UpstreamID: "claude-opus-4.6", ContextWindow: 1_000_000, EffortLevels: claudeEffortNoXHigh, EffortField: effortFieldOutputConfig},
	"claude-opus-4-5":   {UpstreamID: "claude-opus-4.5", ContextWindow: 1_000_000},
	"claude-sonnet-4-6": {UpstreamID: "claude-sonnet-4.6", ContextWindow: 200_000, EffortLevels: claudeEffortNoXHigh, EffortField: effortFieldOutputConfig},
	"claude-sonnet-4-5": {UpstreamID: "claude-sonnet-4.5", ContextWindow: 200_000},
	"claude-haiku-4-5":  {UpstreamID: "claude-haiku-4.5", ContextWindow: 200_000},
	"gpt-5.6-sol":       {UpstreamID: "gpt-5.6-sol", ContextWindow: 1_000_000, EffortLevels: gptEffortLevels, EffortField: effortFieldReasoning},
	"gpt-5.6-terra":     {UpstreamID: "gpt-5.6-terra", ContextWindow: 1_000_000, EffortLevels: gptEffortLevels, EffortField: effortFieldReasoning},
	"gpt-5.6-luna":      {UpstreamID: "gpt-5.6-luna", ContextWindow: 1_000_000, EffortLevels: gptEffortLevels, EffortField: effortFieldReasoning},
}

// upstreamIndex 让已经是 Kiro 写法的模型名（如 claude-opus-4.7）也能查到。
var upstreamIndex = func() map[string]string {
	index := make(map[string]string, len(models))
	for id, spec := range models {
		index[spec.UpstreamID] = id
	}
	return index
}()

// datedAliases 是 Anthropic 带日期的旧模型名。
var datedAliases = map[string]string{
	"claude-opus-4-5-20251101":   "claude-opus-4-5",
	"claude-sonnet-4-5-20250929": "claude-sonnet-4-5",
	"claude-haiku-4-5-20251001":  "claude-haiku-4-5",
}

// canonicalModel 返回 models 表中的键；未知模型返回空串。
func canonicalModel(model string) string {
	id := strings.ToLower(strings.TrimSpace(model))
	if id == "" {
		return ""
	}
	if _, ok := models[id]; ok {
		return id
	}
	if alias, ok := upstreamIndex[id]; ok {
		return alias
	}
	if alias, ok := datedAliases[id]; ok {
		return alias
	}
	if strings.HasPrefix(id, "claude-") {
		hyphenated := strings.ReplaceAll(id, ".", "-")
		if _, ok := models[hyphenated]; ok {
			return hyphenated
		}
		if alias, ok := datedAliases[hyphenated]; ok {
			return alias
		}
	}
	return ""
}

// IsKnownModel 报告模型名能否映射到 Kiro 上游模型。
func IsKnownModel(model string) bool {
	return canonicalModel(model) != ""
}

// UpstreamModelID 返回请求体里 modelId 应填的值。未知模型原样返回，
// 便于管理员通过账号模型映射直接指定上游模型名。
func UpstreamModelID(model string) string {
	if id := canonicalModel(model); id != "" {
		return models[id].UpstreamID
	}
	return strings.TrimSpace(model)
}

// ContextWindow 返回模型的上下文窗口（token），未知模型按 200K 计。
func ContextWindow(model string) int {
	if id := canonicalModel(model); id != "" {
		return models[id].ContextWindow
	}
	return defaultContextWindow
}

// DefaultModelIDs 返回对客户端展示的模型列表（按名称排序）。
func DefaultModelIDs() []string {
	ids := make([]string, 0, len(models))
	for id := range models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// DefaultTestModel 是连接测试使用的模型。
const DefaultTestModel = "claude-haiku-4-5"
