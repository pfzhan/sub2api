//go:build integration

package repository

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

type IdentityCacheSuite struct {
	IntegrationRedisSuite
	cache *identityCache
}

func (s *IdentityCacheSuite) SetupTest() {
	s.IntegrationRedisSuite.SetupTest()
	s.cache = NewIdentityCache(s.rdb).(*identityCache)
}

func (s *IdentityCacheSuite) TestGetFingerprint_Missing() {
	_, err := s.cache.GetFingerprint(s.ctx, 1)
	require.True(s.T(), errors.Is(err, redis.Nil), "expected redis.Nil for missing fingerprint")
}

func (s *IdentityCacheSuite) TestSetAndGetFingerprint() {
	fp := &service.Fingerprint{ClientID: "c1", UserAgent: "ua"}
	require.NoError(s.T(), s.cache.SetFingerprint(s.ctx, 1, fp), "SetFingerprint")
	gotFP, err := s.cache.GetFingerprint(s.ctx, 1)
	require.NoError(s.T(), err, "GetFingerprint")
	require.Equal(s.T(), "c1", gotFP.ClientID)
	require.Equal(s.T(), "ua", gotFP.UserAgent)
}

func (s *IdentityCacheSuite) TestFingerprint_TTL() {
	fp := &service.Fingerprint{ClientID: "c1", UserAgent: "ua"}
	require.NoError(s.T(), s.cache.SetFingerprint(s.ctx, 2, fp))

	fpKey := fmt.Sprintf("%s%d", fingerprintKeyPrefix, 2)
	ttl, err := s.rdb.TTL(s.ctx, fpKey).Result()
	require.NoError(s.T(), err, "TTL fpKey")
	s.AssertTTLWithin(ttl, 1*time.Second, fingerprintTTL)
}

func (s *IdentityCacheSuite) TestGetFingerprint_JSONCorruption() {
	fpKey := fmt.Sprintf("%s%d", fingerprintKeyPrefix, 999)
	require.NoError(s.T(), s.rdb.Set(s.ctx, fpKey, "invalid-json-data", 1*time.Minute).Err(), "Set invalid JSON")

	_, err := s.cache.GetFingerprint(s.ctx, 999)
	require.Error(s.T(), err, "expected error for corrupted JSON")
	require.False(s.T(), errors.Is(err, redis.Nil), "expected decoding error, not redis.Nil")
}

func (s *IdentityCacheSuite) TestSetFingerprint_Nil() {
	err := s.cache.SetFingerprint(s.ctx, 100, nil)
	require.NoError(s.T(), err, "SetFingerprint(nil) should succeed")
}

func (s *IdentityCacheSuite) TestTouchMaskedSessionID_SkipsWhenTTLHigh() {
	require.NoError(s.T(), s.cache.SetMaskedSessionID(s.ctx, 7, "sess-high"))
	key := maskedSessionKey(7)
	require.NoError(s.T(), s.rdb.Expire(s.ctx, key, maskedSessionTTL).Err())

	require.NoError(s.T(), s.cache.TouchMaskedSessionID(s.ctx, 7))
	ttl, err := s.rdb.TTL(s.ctx, key).Result()
	require.NoError(s.T(), err)
	s.AssertTTLWithin(ttl, 1*time.Second, maskedSessionTTL)
}

func (s *IdentityCacheSuite) TestTouchMaskedSessionID_RefreshesWhenTTLLow() {
	require.NoError(s.T(), s.cache.SetMaskedSessionID(s.ctx, 8, "sess-low"))
	key := maskedSessionKey(8)
	require.NoError(s.T(), s.rdb.Expire(s.ctx, key, maskedSessionRefreshTTL-time.Minute).Err())

	require.NoError(s.T(), s.cache.TouchMaskedSessionID(s.ctx, 8))
	ttl, err := s.rdb.TTL(s.ctx, key).Result()
	require.NoError(s.T(), err)
	s.AssertTTLWithin(ttl, 1*time.Second, maskedSessionTTL)
	got, err := s.cache.GetMaskedSessionID(s.ctx, 8)
	require.NoError(s.T(), err)
	require.Equal(s.T(), "sess-low", got)
}

func TestIdentityCacheSuite(t *testing.T) {
	suite.Run(t, new(IdentityCacheSuite))
}
