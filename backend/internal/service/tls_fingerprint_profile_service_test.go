package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/stretchr/testify/require"
)

func TestResolveTLSProfileRandomIsStickyPerAccount(t *testing.T) {
	t.Parallel()
	svc := &TLSFingerprintProfileService{}
	svc.setLocalCache([]*model.TLSFingerprintProfile{
		{ID: 1, Name: "alpha", CipherSuites: []uint16{0x1301}},
		{ID: 2, Name: "beta", CipherSuites: []uint16{0x1302}},
	})
	account := tlsFingerprintTestAccount(11, -1)

	first := svc.ResolveTLSProfile(account)
	require.NotNil(t, first)
	for i := 0; i < 20; i++ {
		got := svc.ResolveTLSProfile(account)
		require.NotNil(t, got)
		require.Equal(t, first.Name, got.Name, "random mode must stick to one profile per account")
		require.Equal(t, first.CipherSuites, got.CipherSuites)
	}

	other := svc.ResolveTLSProfile(tlsFingerprintTestAccount(12, -1))
	require.NotNil(t, other)
}

func TestResolveTLSProfileRandomRepicksAfterDelete(t *testing.T) {
	t.Parallel()
	svc := &TLSFingerprintProfileService{}
	svc.setLocalCache([]*model.TLSFingerprintProfile{
		{ID: 1, Name: "alpha", CipherSuites: []uint16{0x1301}},
		{ID: 2, Name: "beta", CipherSuites: []uint16{0x1302}},
	})
	account := tlsFingerprintTestAccount(21, -1)
	first := svc.ResolveTLSProfile(account)
	require.NotNil(t, first)

	remaining := &model.TLSFingerprintProfile{ID: 2, Name: "beta", CipherSuites: []uint16{0x1302}}
	if first.Name == "beta" {
		remaining = &model.TLSFingerprintProfile{ID: 1, Name: "alpha", CipherSuites: []uint16{0x1301}}
	}
	svc.setLocalCache([]*model.TLSFingerprintProfile{remaining})

	got := svc.ResolveTLSProfile(account)
	require.NotNil(t, got)
	require.Equal(t, remaining.Name, got.Name)
}

func TestResolveTLSProfileRandomSeesLiveSettings(t *testing.T) {
	t.Parallel()
	svc := &TLSFingerprintProfileService{}
	svc.setLocalCache([]*model.TLSFingerprintProfile{
		{ID: 7, Name: "Node24", CipherSuites: []uint16{0x1301, 0x1302}},
	})
	account := tlsFingerprintTestAccount(31, -1)
	first := svc.ResolveTLSProfile(account)
	require.Equal(t, []uint16{0x1301, 0x1302}, first.CipherSuites)

	svc.setLocalCache([]*model.TLSFingerprintProfile{
		{ID: 7, Name: "Node24", CipherSuites: []uint16{0x1301}},
	})
	got := svc.ResolveTLSProfile(account)
	require.Equal(t, []uint16{0x1301}, got.CipherSuites)
}

func tlsFingerprintTestAccount(id, profileID int64) *Account {
	return &Account{
		ID:       id,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"enable_tls_fingerprint":     true,
			"tls_fingerprint_profile_id": profileID,
		},
	}
}
