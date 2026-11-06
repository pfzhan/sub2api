package service

import (
	"context"
	"strconv"
	"strings"

	"github.com/cespare/xxhash/v2"
	"github.com/tidwall/gjson"
)

// 前缀亲和选号。
//
// 上游 prompt 缓存按前缀匹配、按 workspace 隔离。同一客户端的 tools 与固定 system
// 在所有用户之间相同，但负载感知选择把新会话平均打散到各账号，这段共享前缀要在每个
// 账号上各写一遍、各自保温，流量被摊薄后也更容易过期。前缀亲和用 rendezvous 哈希给
// 账号排序，相同前缀的新会话优先落到同一个账号，满载时按同一顺序顺延，共享前缀因此
// 只分布在少数账号上。已有会话仍按粘性绑定走，不受影响。

type prefixAffinityKeyCtxKey struct{}

// WithSharedPrefixAffinity 在开启前缀亲和时，把请求的共享前缀 key 放进 ctx，供
// 负载感知选择使用。
func (s *GatewayService) WithSharedPrefixAffinity(ctx context.Context, parsed *ParsedRequest, model string) context.Context {
	if !s.schedulingConfig().PrefixAffinityEnabled {
		return ctx
	}
	key := SharedPrefixKey(parsed, model)
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, prefixAffinityKeyCtxKey{}, key)
}

func prefixAffinityKeyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	key, _ := ctx.Value(prefixAffinityKeyCtxKey{}).(string)
	return key
}

// SharedPrefixKey 返回请求共享前缀的路由 key：模型 + 按上游渲染顺序（tools → system）
// 到第一个缓存断点为止的内容。客户端把断点放在跨请求不变的位置，断点之后（如
// Claude Code 写进 system 的环境信息）因用户而异，不计入。没有断点时取全部 tools 与
// system。cache_control 本身和 Claude Code 的计费归属块不计入，后者可能逐请求变化。
// tools 与 system 都为空时返回空串。
func SharedPrefixKey(parsed *ParsedRequest, model string) string {
	if parsed == nil || parsed.Body == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(model)
	wrote := false
	// add 写入一个块，返回该块是否带缓存断点。
	add := func(block gjson.Result) bool {
		b.WriteByte(0)
		b.Write(canonicalAnthropicDigestJSON([]byte(block.Raw)))
		wrote = true
		return block.Get("cache_control").Exists()
	}

	breakpoint := false
	if tools := gjson.GetBytes(parsed.Body.Bytes(), "tools"); tools.IsArray() {
		tools.ForEach(func(_, tool gjson.Result) bool {
			breakpoint = add(tool)
			return !breakpoint
		})
	}
	if !breakpoint {
		system := gjson.ParseBytes(parsed.SystemRaw())
		switch {
		case system.Type == gjson.String:
			if system.String() != "" {
				add(system)
			}
		case system.IsArray():
			system.ForEach(func(_, block gjson.Result) bool {
				if strings.HasPrefix(block.Get("text").String(), claudeCodeBillingHeaderPrefix) {
					return true
				}
				return !add(block)
			})
		}
	}
	if !wrote {
		return ""
	}
	return strconv.FormatUint(xxhash.Sum64String(b.String()), 36)
}

// preferPrefixAffinity 用于已按优先级排好序的候选列表（模型路由路径）：在
// 优先级最高的那一组里，把前缀亲和选中的账号移到最前，其余顺序不变。
func preferPrefixAffinity(sorted []accountWithLoad, prefixKey string, maxLoadRate int) {
	if prefixKey == "" || len(sorted) < 2 {
		return
	}
	top := sorted[0].account.Priority
	n := 1
	for n < len(sorted) && sorted[n].account.Priority == top {
		n++
	}
	selected := selectByPrefixAffinity(sorted[:n], prefixKey, maxLoadRate)
	if selected == nil {
		return
	}
	for i := range sorted[:n] {
		if &sorted[i] == selected {
			item := sorted[i]
			copy(sorted[1:i+1], sorted[:i])
			sorted[0] = item
			return
		}
	}
}

// selectByPrefixAffinity 用 rendezvous 哈希给账号排序，返回负载率低于 maxLoadRate
// 的账号中排名最前的一个；都达到上限时返回 nil，由调用方回到按负载选号。
func selectByPrefixAffinity(accounts []accountWithLoad, prefixKey string, maxLoadRate int) *accountWithLoad {
	var (
		best      *accountWithLoad
		bestScore uint64
	)
	for i := range accounts {
		acc := &accounts[i]
		if acc.loadInfo.LoadRate >= maxLoadRate {
			continue
		}
		score := xxhash.Sum64String(prefixKey + ":" + strconv.FormatInt(acc.account.ID, 10))
		if best == nil || score > bestScore {
			best, bestScore = acc, score
		}
	}
	return best
}
