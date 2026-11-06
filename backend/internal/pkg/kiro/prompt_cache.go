package kiro

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Kiro 上游有前缀缓存，但响应里只有本次消耗的 credit，没有缓存 token 数。
// 这里按 Anthropic prompt caching 的规则在本地推算缓存读写：请求按 tools、
// system、messages 的顺序展开成内容块，带 cache_control 的块是断点；断点处
// 的前缀写入缓存，之后的请求在断点往前 promptCacheLookback 个块内找已写入
// 的最长前缀，命中部分记为缓存读取。

const (
	// promptCacheLookback 是每个断点往前查找命中的块数，与 Anthropic 一致。
	promptCacheLookback = 20

	// PromptCacheTTL5m 和 PromptCacheTTL1h 是 cache_control 支持的两种有效期。
	PromptCacheTTL5m = 5 * time.Minute
	PromptCacheTTL1h = time.Hour
)

// PromptCacheEntry 是需要写入的缓存前缀。
type PromptCacheEntry struct {
	Key string
	TTL time.Duration
}

// PromptCache 是一次请求按内容块展开后的前缀信息。
type PromptCache struct {
	// keys[i] 是前 i+1 个块组成的前缀的哈希。
	keys []string
	// weights[i] 是前 i+1 个块的累计字节数，用于按比例拆分 token。
	weights []int
	// breakpoints 是带 cache_control 的块下标，按出现顺序排列。
	breakpoints []promptCacheBreakpoint
}

type promptCacheBreakpoint struct {
	index int
	ttl   time.Duration
}

// NewPromptCache 解析 Anthropic Messages 请求体。model 参与前缀哈希，不同模型
// 的缓存互不命中。请求体里没有断点时返回 nil。
func NewPromptCache(claudeBody []byte, model string) *PromptCache {
	var req struct {
		Tools    []json.RawMessage `json:"tools"`
		System   json.RawMessage   `json:"system"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(claudeBody, &req); err != nil {
		return nil
	}

	pc := &PromptCache{}
	h := sha256.Sum256([]byte("kiro-prompt-cache\x00" + model))
	total := 0
	add := func(section string, raw json.RawMessage) {
		block, weight, ttl, marked := canonicalCacheBlock(raw)
		sum := sha256.New()
		sum.Write(h[:])
		sum.Write([]byte(section))
		sum.Write([]byte{0})
		sum.Write(block)
		copy(h[:], sum.Sum(nil))
		total += weight
		pc.keys = append(pc.keys, hex.EncodeToString(h[:]))
		pc.weights = append(pc.weights, total)
		if marked {
			pc.breakpoints = append(pc.breakpoints, promptCacheBreakpoint{index: len(pc.keys) - 1, ttl: ttl})
		}
	}

	for _, tool := range req.Tools {
		add("tool", tool)
	}
	for _, block := range rawContentBlocks(req.System) {
		add("system", block)
	}
	for _, msg := range req.Messages {
		for _, block := range rawContentBlocks(msg.Content) {
			add(msg.Role, block)
		}
	}
	if len(pc.breakpoints) == 0 {
		return nil
	}
	return pc
}

// LookupKeys 返回可能命中的前缀哈希，即每个断点及其往前 promptCacheLookback
// 个块的前缀。
func (pc *PromptCache) LookupKeys() []string {
	if pc == nil {
		return nil
	}
	seen := make(map[int]struct{})
	var keys []string
	for _, bp := range pc.breakpoints {
		for i := bp.index; i >= 0 && i > bp.index-promptCacheLookback; i-- {
			if _, ok := seen[i]; ok {
				continue
			}
			seen[i] = struct{}{}
			keys = append(keys, pc.keys[i])
		}
	}
	return keys
}

// Entries 返回本次请求要写入的前缀，即每个断点处的前缀。
func (pc *PromptCache) Entries() []PromptCacheEntry {
	if pc == nil {
		return nil
	}
	entries := make([]PromptCacheEntry, 0, len(pc.breakpoints))
	for _, bp := range pc.breakpoints {
		entries = append(entries, PromptCacheEntry{Key: pc.keys[bp.index], TTL: bp.ttl})
	}
	return entries
}

// renewTTLAt 返回第 i 个块所在前缀的续期时长：本轮位于它之后（含它本身）的
// 第一个断点的有效期，即这一段内容在本轮被哪个断点缓存。Claude Code 的 system
// 断点是 1 小时、消息断点是 5 分钟，消息部分的前缀不能按 system 的 1 小时续期。
// 之后没有断点时用 5 分钟。5 分钟和 1 小时这两个常量本身不在这里改。
func (pc *PromptCache) renewTTLAt(i int) time.Duration {
	for _, bp := range pc.breakpoints {
		if bp.index >= i {
			return bp.ttl
		}
	}
	return PromptCacheTTL5m
}

// TouchEntries 返回已经命中、本轮却不再作为断点写入的前缀。
// 这些键要重新 Store，查找本身不会刷新 Redis TTL。
func (pc *PromptCache) TouchEntries(hits map[string]bool) []PromptCacheEntry {
	if pc == nil || len(hits) == 0 {
		return nil
	}
	written := make(map[string]struct{}, len(pc.breakpoints))
	for _, bp := range pc.breakpoints {
		written[pc.keys[bp.index]] = struct{}{}
	}
	var out []PromptCacheEntry
	for i, key := range pc.keys {
		if !hits[key] {
			continue
		}
		if _, ok := written[key]; ok {
			continue
		}
		out = append(out, PromptCacheEntry{Key: key, TTL: pc.renewTTLAt(i)})
	}
	return out
}

// Plan 根据已存在的前缀哈希计算缓存读写比例。
func (pc *PromptCache) Plan(hits map[string]bool) *CachePlan {
	if pc == nil || len(pc.weights) == 0 {
		return nil
	}
	hit := -1
	for i, key := range pc.keys {
		if i > pc.breakpoints[len(pc.breakpoints)-1].index {
			break
		}
		if hits[key] {
			hit = i
		}
	}

	plan := &CachePlan{total: pc.weights[len(pc.weights)-1]}
	covered := 0
	if hit >= 0 {
		covered = pc.weights[hit]
		plan.read = covered
	}
	for _, bp := range pc.breakpoints {
		end := pc.weights[bp.index]
		if end <= covered {
			continue
		}
		if bp.ttl == PromptCacheTTL1h {
			plan.write1h += end - covered
		} else {
			plan.write5m += end - covered
		}
		covered = end
	}
	return plan
}

// CachePlan 是缓存读取、写入和未缓存部分在请求体中的字节比例。
type CachePlan struct {
	total   int
	read    int
	write5m int
	write1h int
}

// split 按比例把输入 token 拆成缓存读取、写入和未缓存三部分，四项之和等于
// input。Kiro 追加的上下文没有出现在请求体里，按同样的比例分摊。
func (p *CachePlan) split(input int) Usage {
	if p == nil || p.total <= 0 || input <= 0 {
		return Usage{InputTokens: input}
	}
	scale := func(w int) int { return int(int64(input) * int64(w) / int64(p.total)) }
	u := Usage{
		CacheReadInputTokens:  scale(p.read),
		CacheCreation5mTokens: scale(p.write5m),
		CacheCreation1hTokens: scale(p.write1h),
	}
	u.CacheCreationInputTokens = u.CacheCreation5mTokens + u.CacheCreation1hTokens
	u.InputTokens = input - u.CacheReadInputTokens - u.CacheCreationInputTokens
	return u
}

// rawContentBlocks 把 system 或 content 字段展开成内容块。字符串作为单个块
// 返回，由 canonicalCacheBlock 转成文本块。
func rawContentBlocks(raw json.RawMessage) []json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return blocks
	}
	return []json.RawMessage{raw}
}

// canonicalCacheBlock 把块解码后去掉 cache_control 再按固定格式编码，返回编码
// 结果、拆分 token 用的权重、块是否带断点及其有效期。cache_control 为 null 时
// 不算断点，与 BuildRequest 一致。
//
// 权重是编码后的字节数减去其中 base64 数据的长度。输入 token 的本地估算不按
// base64 长度计算图片和文档；按字节加权时一张截图就会占满整个前缀，把其后
// 未缓存的文本也算成缓存读取。
//
// Claude Code 只给最后一条消息加断点：这条消息在本轮是带 cache_control 的文本块
// 数组，下一轮作为历史重发时是普通字符串。字符串在这里转成同样的文本块；重新编码
// 时键按字母排序且不转义 HTML 字符，与客户端的键顺序和转义方式无关。这样同一段
// 内容不论以哪种形式出现，都得到相同的哈希。
func canonicalCacheBlock(raw json.RawMessage) ([]byte, int, time.Duration, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return raw, len(raw), 0, false
	}
	if s, ok := v.(string); ok {
		v = map[string]any{"type": "text", "text": s}
	}

	ttl, marked := time.Duration(0), false
	if obj, ok := v.(map[string]any); ok {
		if cc, ok := obj["cache_control"]; ok {
			if cc != nil {
				marked, ttl = true, PromptCacheTTL5m
				if m, ok := cc.(map[string]any); ok && m["ttl"] == "1h" {
					ttl = PromptCacheTTL1h
				}
			}
			delete(obj, "cache_control")
		}
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return raw, len(raw), ttl, marked
	}
	block := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	return block, max(len(block)-base64DataLen(v), 0), ttl, marked
}

// base64DataLen 返回 v 中所有 base64 source（{"type":"base64","data":...}）的
// data 长度之和，包括 tool_result 里嵌套的图片。
func base64DataLen(v any) int {
	n := 0
	switch t := v.(type) {
	case map[string]any:
		if t["type"] == "base64" {
			if data, ok := t["data"].(string); ok {
				n += len(data)
			}
		}
		for _, child := range t {
			n += base64DataLen(child)
		}
	case []any:
		for _, child := range t {
			n += base64DataLen(child)
		}
	}
	return n
}
