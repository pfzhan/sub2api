package kiro

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// claudeCodeTurn 按 Claude Code 的写法构造第 n 轮请求：system 带 1h 断点；
// 最后一条 user 消息是带 5m 断点的文本块数组，之前的 user 消息是普通字符串。
func claudeCodeTurn(n int) []byte {
	var msgs []string
	for i := 1; i <= n; i++ {
		text := fmt.Sprintf("question %d <system-reminder>%s</system-reminder>", i, strings.Repeat("q", 400))
		if i == n {
			msgs = append(msgs, fmt.Sprintf(`{"role":"user","content":[{"type":"text","text":%q,"cache_control":{"type":"ephemeral"}}]}`, text))
		} else {
			msgs = append(msgs, fmt.Sprintf(`{"role":"user","content":%q}`, text))
		}
		if i < n {
			msgs = append(msgs, fmt.Sprintf(`{"role":"assistant","content":[{"type":"text","text":"answer %d %s"}]}`, i, strings.Repeat("a", 400)))
		}
	}
	return []byte(fmt.Sprintf(`{"model":"claude-sonnet-5-5","system":[{"type":"text","text":"%s","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[%s]}`,
		strings.Repeat("s", 4000), strings.Join(msgs, ",")))
}

// store 模拟缓存存储：把 Entries 记下来，Lookup 时返回已记下的键。
type store map[string]bool

func (s store) plan(pc *PromptCache) *CachePlan {
	hits := map[string]bool{}
	for _, k := range pc.LookupKeys() {
		if s[k] {
			hits[k] = true
		}
	}
	p := pc.Plan(hits)
	for _, e := range pc.Entries() {
		s[e.Key] = true
	}
	return p
}

func TestPromptCacheNoBreakpoint(t *testing.T) {
	t.Parallel()

	require.Nil(t, NewPromptCache([]byte(`{"system":"hi","messages":[{"role":"user","content":"hello"}]}`), "m"))
	require.Nil(t, NewPromptCache([]byte(`not json`), "m"))
}

func TestPromptCacheClaudeCodeTurns(t *testing.T) {
	t.Parallel()

	s := store{}

	first := s.plan(NewPromptCache(claudeCodeTurn(1), "m"))
	require.Zero(t, first.read)
	require.Positive(t, first.write1h)
	require.Positive(t, first.write5m)
	require.Equal(t, first.total, first.write1h+first.write5m)

	second := s.plan(NewPromptCache(claudeCodeTurn(2), "m"))
	require.Equal(t, first.total, second.read, "上一轮的整个前缀都应命中")
	require.Zero(t, second.write1h)
	require.Equal(t, second.total-second.read, second.write5m)
}

func TestPromptCacheModelsAreIsolated(t *testing.T) {
	t.Parallel()

	s := store{}
	s.plan(NewPromptCache(claudeCodeTurn(1), "a"))
	other := s.plan(NewPromptCache(claudeCodeTurn(2), "b"))
	require.Zero(t, other.read)
}

func TestPromptCacheSystemHitAfterHistoryChange(t *testing.T) {
	t.Parallel()

	s := store{}
	s.plan(NewPromptCache(claudeCodeTurn(1), "m"))
	// 同样的 system，换了一条不同的用户消息：只有 system 命中。
	body := []byte(strings.Replace(string(claudeCodeTurn(1)), "question 1", "another 1", 1))
	p := s.plan(NewPromptCache(body, "m"))
	require.Positive(t, p.read)
	require.Zero(t, p.write1h)
	require.Equal(t, p.total, p.read+p.write5m)
}

func TestPromptCacheLookbackLimit(t *testing.T) {
	t.Parallel()

	// 第一轮在第 1 个块打断点；第二轮在其后 25 个块处打断点，超出回看范围。
	block := func(i int, cc bool) string {
		mark := ""
		if cc {
			mark = `,"cache_control":{"type":"ephemeral"}`
		}
		return fmt.Sprintf(`{"type":"text","text":"block %d"%s}`, i, mark)
	}
	first := fmt.Sprintf(`{"messages":[{"role":"user","content":[%s]}]}`, block(0, true))
	var blocks []string
	for i := 0; i <= 25; i++ {
		blocks = append(blocks, block(i, i == 25))
	}
	second := fmt.Sprintf(`{"messages":[{"role":"user","content":[%s]}]}`, strings.Join(blocks, ","))

	s := store{}
	s.plan(NewPromptCache([]byte(first), "m"))
	p := s.plan(NewPromptCache([]byte(second), "m"))
	require.Zero(t, p.read)
}

func TestCanonicalCacheBlockMatchesUnmarkedBlock(t *testing.T) {
	t.Parallel()

	marked, ttl, ok := canonicalCacheBlock([]byte(`{"type":"text","text":"x","cache_control":{"type":"ephemeral","ttl":"1h"}}`))
	require.True(t, ok)
	require.Equal(t, PromptCacheTTL1h, ttl)
	plain, _, ok := canonicalCacheBlock([]byte(`{"type":"text","text":"x"}`))
	require.False(t, ok)
	require.Equal(t, string(plain), string(marked))

	// 同一段内容的字符串形式、键顺序不同、转义方式不同，都得到同样的结果。
	for _, raw := range []string{
		`"a<b>&c"`,
		`{"text":"a<b>&c","type":"text"}`,
		`{"type":"text","text":"a<b>&c","cache_control":{"type":"ephemeral"}}`,
	} {
		got, _, _ := canonicalCacheBlock([]byte(raw))
		require.Equal(t, `{"text":"a<b>&c","type":"text"}`, string(got), raw)
	}
}

func TestCachePlanSplitSumsToInput(t *testing.T) {
	t.Parallel()

	p := &CachePlan{total: 1000, read: 700, write5m: 200, write1h: 50}
	u := p.split(4999)
	require.Equal(t, 4999, u.InputTokens+u.CacheReadInputTokens+u.CacheCreationInputTokens)
	require.Equal(t, u.CacheCreation5mTokens+u.CacheCreation1hTokens, u.CacheCreationInputTokens)
	require.Equal(t, 3499, u.CacheReadInputTokens)

	var nilPlan *CachePlan
	require.Equal(t, Usage{InputTokens: 10}, nilPlan.split(10))
}

func TestConverterReportsCacheUsage(t *testing.T) {
	t.Parallel()

	conv := NewConverter(ConverterOptions{
		Model:         "claude-sonnet-5-5",
		UpstreamModel: "claude-sonnet-5.5",
		CachePlan:     &CachePlan{total: 100, read: 80, write5m: 20},
	})
	conv.Handle(Event{Kind: EventText, Text: "ok"})
	conv.Handle(Event{Kind: EventContextUsage, ContextUsagePercentage: 1})
	events, err := conv.Finish()
	require.NoError(t, err)

	u := conv.Usage()
	require.Positive(t, u.CacheReadInputTokens)
	require.Positive(t, u.CacheCreation5mTokens)
	input := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	require.InDelta(t, 0.8, float64(u.CacheReadInputTokens)/float64(input), 0.001)
	require.InDelta(t, 0.2, float64(u.CacheCreation5mTokens)/float64(input), 0.001)

	var delta string
	for _, ev := range events {
		if ev.Event == "message_delta" {
			delta = string(ev.Data)
		}
	}
	require.Contains(t, delta, fmt.Sprintf(`"cache_read_input_tokens":%d`, u.CacheReadInputTokens))
	require.Contains(t, delta, fmt.Sprintf(`"ephemeral_5m_input_tokens":%d`, u.CacheCreation5mTokens))
}
