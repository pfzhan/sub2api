package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// kiroPromptCacheTimeout 限制缓存读写的耗时，缓存不可用时不拖慢请求。
const kiroPromptCacheTimeout = 2 * time.Second

// KiroPromptCache 记录 Kiro 账号已写入的 prompt 缓存前缀，用于在本地推算缓存
// 读写 token。键是 kiro.PromptCache 给出的前缀哈希，按账号隔离。
type KiroPromptCache interface {
	// Lookup 返回 keys 中仍在有效期内的键。
	Lookup(ctx context.Context, accountID int64, keys []string) (map[string]bool, error)
	// Store 写入前缀并设置有效期，已存在的键刷新有效期。
	Store(ctx context.Context, accountID int64, entries []kiro.PromptCacheEntry) error
}

// SetKiroPromptCache 注入 Kiro prompt 缓存记录。未注入时 Kiro 请求的输入
// 全部按未缓存计费。
func (s *GatewayService) SetKiroPromptCache(c KiroPromptCache) {
	s.kiroPromptCache = c
}

// kiroCachePlan 查询账号的缓存前缀，返回本次请求的缓存读写比例，以及 Kiro
// 产出内容后要写入的前缀。调用方负责让 ctx 不随客户端取消。查询失败时返回
// nil，输入按未缓存计费，也不写入，避免把整段输入当作缓存写入。
func (s *GatewayService) kiroCachePlan(ctx context.Context, account *Account, claudeBody []byte, upstreamModel string) (*kiro.CachePlan, []kiro.PromptCacheEntry) {
	if s.kiroPromptCache == nil {
		return nil, nil
	}
	pc := kiro.NewPromptCache(claudeBody, upstreamModel)
	if pc == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, kiroPromptCacheTimeout)
	defer cancel()

	hits, err := s.kiroPromptCache.Lookup(ctx, account.ID, pc.LookupKeys())
	if err != nil {
		logger.LegacyPrintf("service.gateway", "[Kiro] Prompt cache lookup failed: account=%d err=%v", account.ID, err)
		return nil, nil
	}
	entries := pc.Entries()
	if touch := pc.TouchEntries(hits); len(touch) > 0 {
		entries = append(entries, touch...)
	}
	return pc.Plan(hits), entries
}
