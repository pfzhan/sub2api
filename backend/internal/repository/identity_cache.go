package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const (
	fingerprintKeyPrefix   = "fingerprint:"
	fingerprintTTL         = 7 * 24 * time.Hour // 7天，配合每24小时懒续期可保持活跃账号永不过期
	maskedSessionKeyPrefix = "masked_session:"
	// maskedSessionTTL 是伪装 session ID 的有效期。官方 CLI 的 session 是进程级、
	// 直到退出才更换，15 分钟轮换会让同一 OAuth 身份上出现大量短会话；拉到小时级
	// 后更接近"一台长时间运行的 CLI"，到期换 UUID 则等价于一次 CLI 重启。
	maskedSessionTTL = 2 * time.Hour
	// maskedSessionRefreshTTL：剩余寿命降到这个阈值才 EXPIRE 续期。
	// 热路径默认只 GET，避免每个 OAuth 请求都 SET 把 2h TTL 重写一遍。
	maskedSessionRefreshTTL = 30 * time.Minute
)

// fingerprintKey generates the Redis key for account fingerprint cache.
func fingerprintKey(accountID int64) string {
	return fmt.Sprintf("%s%d", fingerprintKeyPrefix, accountID)
}

// maskedSessionKey generates the Redis key for masked session ID cache.
func maskedSessionKey(accountID int64) string {
	return fmt.Sprintf("%s%d", maskedSessionKeyPrefix, accountID)
}

type identityCache struct {
	rdb *redis.Client
}

func NewIdentityCache(rdb *redis.Client) service.IdentityCache {
	return &identityCache{rdb: rdb}
}

func (c *identityCache) GetFingerprint(ctx context.Context, accountID int64) (*service.Fingerprint, error) {
	key := fingerprintKey(accountID)
	val, err := c.rdb.Get(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	var fp service.Fingerprint
	if err := json.Unmarshal([]byte(val), &fp); err != nil {
		return nil, err
	}
	return &fp, nil
}

func (c *identityCache) SetFingerprint(ctx context.Context, accountID int64, fp *service.Fingerprint) error {
	key := fingerprintKey(accountID)
	val, err := json.Marshal(fp)
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, key, val, fingerprintTTL).Err()
}

func (c *identityCache) GetMaskedSessionID(ctx context.Context, accountID int64) (string, error) {
	key := maskedSessionKey(accountID)
	val, err := c.rdb.Get(ctx, key).Result()
	if err != nil {
		if err == redis.Nil {
			return "", nil
		}
		return "", err
	}
	return val, nil
}

func (c *identityCache) SetMaskedSessionID(ctx context.Context, accountID int64, sessionID string) error {
	key := maskedSessionKey(accountID)
	return c.rdb.Set(ctx, key, sessionID, maskedSessionTTL).Err()
}

func (c *identityCache) TouchMaskedSessionID(ctx context.Context, accountID int64) error {
	key := maskedSessionKey(accountID)
	ttl, err := c.rdb.TTL(ctx, key).Result()
	if err != nil {
		return err
	}
	if !shouldRefreshMaskedSession(ttl) {
		return nil
	}
	return c.rdb.Expire(ctx, key, maskedSessionTTL).Err()
}

func shouldRefreshMaskedSession(remaining time.Duration) bool {
	return remaining > 0 && remaining <= maskedSessionRefreshTTL
}
