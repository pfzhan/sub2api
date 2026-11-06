package repository

import (
	"context"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// kiroPromptCachePrefix 键格式: kiro_prompt_cache:{accountID}:{prefixHash}
const kiroPromptCachePrefix = "kiro_prompt_cache:"

type kiroPromptCache struct {
	rdb *redis.Client
}

// NewKiroPromptCache 创建基于 Redis 的 Kiro prompt 缓存记录。
func NewKiroPromptCache(rdb *redis.Client) service.KiroPromptCache {
	return &kiroPromptCache{rdb: rdb}
}

func kiroPromptCacheKey(accountID int64, hash string) string {
	return fmt.Sprintf("%s%d:%s", kiroPromptCachePrefix, accountID, hash)
}

func (c *kiroPromptCache) Lookup(ctx context.Context, accountID int64, keys []string) (map[string]bool, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	redisKeys := make([]string, len(keys))
	for i, k := range keys {
		redisKeys[i] = kiroPromptCacheKey(accountID, k)
	}
	values, err := c.rdb.MGet(ctx, redisKeys...).Result()
	if err != nil {
		return nil, err
	}
	hits := make(map[string]bool)
	for i, v := range values {
		if v != nil {
			hits[keys[i]] = true
		}
	}
	return hits, nil
}

func (c *kiroPromptCache) Store(ctx context.Context, accountID int64, entries []kiro.PromptCacheEntry) error {
	if len(entries) == 0 {
		return nil
	}
	pipe := c.rdb.Pipeline()
	for _, e := range entries {
		pipe.Set(ctx, kiroPromptCacheKey(accountID, e.Key), 1, e.TTL)
	}
	_, err := pipe.Exec(ctx)
	return err
}
