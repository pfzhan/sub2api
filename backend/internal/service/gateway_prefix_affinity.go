package service

import (
	"context"
	"strconv"
	"strings"
	"sync"

	"github.com/cespare/xxhash/v2"
	"github.com/tidwall/gjson"
)

// 前缀亲和选号。
//
// 上游 prompt 缓存按前缀匹配、按 workspace 隔离。同一客户端的 tools 与固定 system
// 在所有用户之间相同，但负载感知选择把新会话平均打散到各账号，这段共享前缀要在每个
// 账号上各写一遍、各自保温，流量被摊薄后也更容易过期。前缀亲和用 rendezvous 哈希给
// 账号排序，相同前缀的新会话优先落到同一个账号，共享前缀因此只分布在少数账号上。
// 已有会话仍按粘性绑定走，不受影响。
//
// 排序按分组加盐：多个分组共用账号时，各分组的首选账号相互独立，一个分组的流量不会
// 挤占另一个分组的首选账号。首选账号的负载受 bounded-load 约束（见
// selectByPrefixAffinity），超出时按同一顺序顺延到下一个账号。

// prefixAffinityBalancePercent 是前缀亲和接受的账号负载率相对分组平均负载率的上限
// 倍数（百分比），另外再放宽一个请求的负载。
const prefixAffinityBalancePercent = 125

type prefixAffinityKeyCtxKey struct{}

// prefixAffinityRequest 保存计算前缀 key 所需的请求，key 推迟到负载感知选择真正用到
// 时才计算：命中粘性绑定的请求用不到它，不必为此规范化全部 tools。
type prefixAffinityRequest struct {
	parsed *ParsedRequest
	model  string
	once   sync.Once
	key    string
}

// WithSharedPrefixAffinity 在开启前缀亲和时，把计算共享前缀 key 所需的请求放进 ctx，
// 供负载感知选择使用。
func (s *GatewayService) WithSharedPrefixAffinity(ctx context.Context, parsed *ParsedRequest, model string) context.Context {
	if !s.schedulingConfig().PrefixAffinityEnabled || parsed == nil {
		return ctx
	}
	return context.WithValue(ctx, prefixAffinityKeyCtxKey{}, &prefixAffinityRequest{parsed: parsed, model: model})
}

func prefixAffinityKeyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	req, _ := ctx.Value(prefixAffinityKeyCtxKey{}).(*prefixAffinityRequest)
	if req == nil {
		return ""
	}
	req.once.Do(func() {
		req.key = SharedPrefixKey(req.parsed, req.model)
	})
	return req.key
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

// selectByPrefixAffinity 用按分组加盐的 rendezvous 哈希给账号排序，返回可接受账号中
// 排名最前的一个。可接受指负载率低于 maxLoadRate，且不超过 bounded-load 上限：分组
// 平均负载率的 prefixAffinityBalancePercent%，再加该账号一个请求的负载。没有可接受
// 的账号时返回 nil，由调用方回到按负载选号。
//
// 负载率只反映正在处理的请求，两轮之间空闲的已绑定会话不计入；相对平均值的上限让
// 首选账号最多比其他账号多出一两个并发，账号上的会话总数由账号的会话数上限约束。
func selectByPrefixAffinity(accounts []accountWithLoad, groupID int64, prefixKey string, maxLoadRate int) *accountWithLoad {
	if len(accounts) == 0 {
		return nil
	}
	totalLoadRate := 0
	for i := range accounts {
		totalLoadRate += accounts[i].loadInfo.LoadRate
	}
	n := len(accounts)
	salt := prefixKey + ":" + strconv.FormatInt(groupID, 10) + ":"
	var (
		best      *accountWithLoad
		bestScore uint64
	)
	for i := range accounts {
		acc := &accounts[i]
		rate := acc.loadInfo.LoadRate
		if rate >= maxLoadRate {
			continue
		}
		// rate > avg*balance% + slot，两边同乘 100*n 以免整数除法截断
		slot := 100 / acc.account.EffectiveLoadFactor()
		if rate*100*n > totalLoadRate*prefixAffinityBalancePercent+slot*100*n {
			continue
		}
		score := xxhash.Sum64String(salt + strconv.FormatInt(acc.account.ID, 10))
		if best == nil || score > bestScore {
			best, bestScore = acc, score
		}
	}
	return best
}
