package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestKiroPromptCacheLookupAndExpiry(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer func() { _ = client.Close() }()
	cache := NewKiroPromptCache(client)
	ctx := context.Background()

	hits, err := cache.Lookup(ctx, 1, []string{"a", "b"})
	require.NoError(t, err)
	require.Empty(t, hits)

	require.NoError(t, cache.Store(ctx, 1, []kiro.PromptCacheEntry{
		{Key: "a", TTL: kiro.PromptCacheTTL5m},
		{Key: "b", TTL: kiro.PromptCacheTTL1h},
	}))

	hits, err = cache.Lookup(ctx, 1, []string{"a", "b", "c"})
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"a": true, "b": true}, hits)

	hits, err = cache.Lookup(ctx, 2, []string{"a"})
	require.NoError(t, err)
	require.Empty(t, hits, "不同账号的缓存互不命中")

	server.FastForward(6 * time.Minute)
	hits, err = cache.Lookup(ctx, 1, []string{"a", "b"})
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"b": true}, hits)
}
