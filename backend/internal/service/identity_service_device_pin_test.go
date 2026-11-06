package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetOrCreateFingerprintPinnedReusesDeviceWhenCacheMissing(t *testing.T) {
	pinned := strings.Repeat("ab", 32)
	cache := &stubIdentityCache{}
	svc := NewIdentityService(cache)

	fp, err := svc.GetOrCreateFingerprintPinned(context.Background(), 9, http.Header{}, strings.ToUpper(pinned))
	require.NoError(t, err)
	require.Equal(t, pinned, fp.ClientID)

	again, err := svc.GetOrCreateFingerprintPinned(context.Background(), 9, http.Header{}, "")
	require.NoError(t, err)
	require.Equal(t, pinned, again.ClientID)
}

func TestGetOrCreateFingerprintPinnedRemembersDeviceWhenCacheDown(t *testing.T) {
	cache := &downIdentityCache{}
	svc := NewIdentityService(cache)

	first, err := svc.GetOrCreateFingerprintPinned(context.Background(), 11, http.Header{}, "")
	require.NoError(t, err)
	require.Len(t, first.ClientID, 64)

	second, err := svc.GetOrCreateFingerprintPinned(context.Background(), 11, http.Header{}, "")
	require.NoError(t, err)
	require.Equal(t, first.ClientID, second.ClientID)
}

func TestNormalizePinnedClientID(t *testing.T) {
	require.Equal(t, strings.Repeat("ab", 32), normalizePinnedClientID("  "+strings.Repeat("AB", 32)+"  "))
	require.Empty(t, normalizePinnedClientID("deadbeef"))
	require.Empty(t, normalizePinnedClientID(strings.Repeat("zz", 32)))
}

type downIdentityCache struct {
	stubIdentityCache
}

func (d *downIdentityCache) GetFingerprint(context.Context, int64) (*Fingerprint, error) {
	return nil, errors.New("redis down")
}

func (d *downIdentityCache) SetFingerprint(context.Context, int64, *Fingerprint) error {
	return errors.New("redis down")
}
