package service

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Anthropic 会话 Fallback 相关常量
const (
	// anthropicSessionTTLSeconds Anthropic 会话缓存 TTL（5 分钟）
	anthropicSessionTTLSeconds = 300

	// anthropicDigestSessionKeyPrefix Anthropic 摘要 fallback 会话 key 前缀
	anthropicDigestSessionKeyPrefix = "anthropic:digest:"
)

// AnthropicSessionTTL 返回 Anthropic 会话缓存 TTL
func AnthropicSessionTTL() time.Duration {
	return anthropicSessionTTLSeconds * time.Second
}

// BuildAnthropicDigestChain 根据 Anthropic 请求生成摘要链
// 格式: s:<hash>-u:<hash>-a:<hash>-u:<hash>-...
// s = system, u = user, a = assistant
func BuildAnthropicDigestChain(parsed *ParsedRequest) string {
	if parsed == nil {
		return ""
	}

	var parts []string

	if systemRaw := parsed.SystemRaw(); len(systemRaw) > 0 && string(systemRaw) != "null" {
		parts = append(parts, "s:"+shortHash(canonicalAnthropicDigestJSON(systemRaw)))
	}

	messages := parsed.MessagesRaw()
	if len(messages) > 0 {
		gjson.ParseBytes(messages).ForEach(func(_, msg gjson.Result) bool {
			prefix := rolePrefix(msg.Get("role").String())
			content := msg.Get("content")
			parts = append(parts, prefix+":"+shortHash(canonicalAnthropicDigestJSON([]byte(content.Raw))))
			return true
		})
	}

	return strings.Join(parts, "-")
}

// canonicalAnthropicDigestJSON 保持 digest 对 JSON key 顺序和空白不敏感，并去掉
// cache_control：客户端每轮把缓存断点挪到最新消息上，上一轮最后一条消息会因此
// 变化，不去掉的话新一轮的摘要链就不再以上一轮的摘要链为前缀。
func canonicalAnthropicDigestJSON(raw []byte) []byte {
	if len(raw) == 0 {
		return raw
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return raw
	}
	stripAnthropicCacheControl(value)
	canonical, err := json.Marshal(value)
	if err != nil {
		return raw
	}
	return canonical
}

func stripAnthropicCacheControl(value any) {
	switch v := value.(type) {
	case map[string]any:
		delete(v, "cache_control")
		for _, child := range v {
			stripAnthropicCacheControl(child)
		}
	case []any:
		for _, child := range v {
			stripAnthropicCacheControl(child)
		}
	}
}

// HasMetadataSessionID 报告会话 hash 是否取自 metadata.user_id 中的会话 ID
// （GenerateSessionHash 的第一级）。这类 ID 跨轮稳定，不需要摘要链兜底。
func HasMetadataSessionID(parsed *ParsedRequest) bool {
	if parsed == nil || parsed.MetadataUserID == "" {
		return false
	}
	uid := ParseMetadataUserID(parsed.MetadataUserID)
	return uid != nil && uid.SessionID != ""
}

// rolePrefix 将 Anthropic 的 role 映射为单字符前缀
func rolePrefix(role string) string {
	switch role {
	case "assistant":
		return "a"
	default:
		return "u"
	}
}

// GenerateAnthropicDigestSessionKey 生成 Anthropic 摘要 fallback 的 sessionKey
// 组合 prefixHash 前 8 位 + uuid 前 8 位，确保不同会话产生不同的 sessionKey
func GenerateAnthropicDigestSessionKey(prefixHash, uuid string) string {
	prefix := prefixHash
	if len(prefixHash) >= 8 {
		prefix = prefixHash[:8]
	}
	uuidPart := uuid
	if len(uuid) >= 8 {
		uuidPart = uuid[:8]
	}
	return anthropicDigestSessionKeyPrefix + prefix + ":" + uuidPart
}
