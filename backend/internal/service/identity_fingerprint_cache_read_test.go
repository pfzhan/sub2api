package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
)

// erroringIdentityCache 读指纹时返回指定错误，用于区分"缓存没有"与"读失败"。
type erroringIdentityCache struct {
	stubIdentityCache
	getErr error
}

func (c *erroringIdentityCache) GetFingerprint(_ context.Context, _ int64) (*Fingerprint, error) {
	return nil, c.getErr
}

func TestGetOrCreateFingerprintTransientReadErrorDoesNotOverwriteCache(t *testing.T) {
	cache := &erroringIdentityCache{getErr: errors.New("redis: i/o timeout")}
	svc := NewIdentityService(cache)

	fp, err := svc.GetOrCreateFingerprint(context.Background(), 1, headersWithUA("claude-cli/"+claude.CLICurrentVersion+" (external, cli)"))

	require.NoError(t, err)
	require.NotNil(t, fp)
	require.NotEmpty(t, fp.ClientID)
	require.Zero(t, cache.setCalls, "a failed read must not overwrite the fingerprint that may still be cached")
}

func TestGetOrCreateFingerprintCreatesWhenCacheMissingOrCorrupt(t *testing.T) {
	corrupt := json.Unmarshal([]byte("invalid-json"), &Fingerprint{})
	require.Error(t, corrupt)
	for name, getErr := range map[string]error{"missing": redis.Nil, "corrupt": corrupt} {
		t.Run(name, func(t *testing.T) {
			cache := &erroringIdentityCache{getErr: getErr}
			svc := NewIdentityService(cache)

			_, err := svc.GetOrCreateFingerprint(context.Background(), 1, headersWithUA("claude-cli/"+claude.CLICurrentVersion+" (external, cli)"))

			require.NoError(t, err)
			require.Equal(t, 1, cache.setCalls)
		})
	}
}
