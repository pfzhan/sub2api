package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/stretchr/testify/require"
)

func TestResolveTLSProfilePinnedPerAccount(t *testing.T) {
	svc := &TLSFingerprintProfileService{
		localCache: map[int64]*model.TLSFingerprintProfile{
			3: {ID: 3, Name: "c"},
			1: {ID: 1, Name: "a"},
			2: {ID: 2, Name: "b"},
		},
	}
	account := func(id int64) *Account {
		return &Account{
			ID:       id,
			Platform: PlatformAnthropic,
			Type:     AccountTypeOAuth,
			Extra: map[string]any{
				"enable_tls_fingerprint":     true,
				"tls_fingerprint_profile_id": float64(-1),
			},
		}
	}

	require.Equal(t, "a", svc.ResolveTLSProfile(account(0)).Name)
	require.Equal(t, "b", svc.ResolveTLSProfile(account(1)).Name)
	require.Equal(t, "c", svc.ResolveTLSProfile(account(2)).Name)
	require.Equal(t, "c", svc.ResolveTLSProfile(account(5)).Name)
	require.Equal(t, svc.ResolveTLSProfile(account(5)).Name, svc.ResolveTLSProfile(account(5)).Name)

	empty := &TLSFingerprintProfileService{localCache: map[int64]*model.TLSFingerprintProfile{}}
	require.Equal(t, "Built-in Default (Node.js 24.x)", empty.ResolveTLSProfile(account(5)).Name)
}
