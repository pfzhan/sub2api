package dto

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestAccountFromServiceShallow_TLSFingerprint(t *testing.T) {
	t.Parallel()

	t.Run("kiro oauth enabled emits true", func(t *testing.T) {
		out := AccountFromServiceShallow(&service.Account{
			ID:       1,
			Platform: service.PlatformKiro,
			Type:     service.AccountTypeOAuth,
			Extra:    map[string]any{"enable_tls_fingerprint": true},
		})
		require.NotNil(t, out.EnableTLSFingerprint)
		require.True(t, *out.EnableTLSFingerprint)
		require.Nil(t, out.TLSFingerprintProfileID)
	})

	t.Run("kiro random profile id is serialized", func(t *testing.T) {
		out := AccountFromServiceShallow(&service.Account{
			ID:       2,
			Platform: service.PlatformKiro,
			Type:     service.AccountTypeOAuth,
			Extra: map[string]any{
				"enable_tls_fingerprint":     true,
				"tls_fingerprint_profile_id": float64(-1),
			},
		})
		require.NotNil(t, out.TLSFingerprintProfileID)
		require.Equal(t, int64(-1), *out.TLSFingerprintProfileID)

		payload, err := json.Marshal(out)
		require.NoError(t, err)
		var decoded map[string]any
		require.NoError(t, json.Unmarshal(payload, &decoded))
		require.Equal(t, true, decoded["enable_tls_fingerprint"])
		require.Equal(t, float64(-1), decoded["tls_fingerprint_profile_id"])
	})

	t.Run("anthropic random profile id is serialized", func(t *testing.T) {
		out := AccountFromServiceShallow(&service.Account{
			ID:       3,
			Platform: service.PlatformAnthropic,
			Type:     service.AccountTypeOAuth,
			Extra: map[string]any{
				"enable_tls_fingerprint":     true,
				"tls_fingerprint_profile_id": float64(-1),
			},
		})
		require.NotNil(t, out.TLSFingerprintProfileID)
		require.Equal(t, int64(-1), *out.TLSFingerprintProfileID)
	})

	t.Run("kiro missing extra does not emit tls fields", func(t *testing.T) {
		out := AccountFromServiceShallow(&service.Account{
			ID:       4,
			Platform: service.PlatformKiro,
			Type:     service.AccountTypeOAuth,
		})
		require.Nil(t, out.EnableTLSFingerprint)
		require.Nil(t, out.TLSFingerprintProfileID)

		payload, err := json.Marshal(out)
		require.NoError(t, err)
		var decoded map[string]any
		require.NoError(t, json.Unmarshal(payload, &decoded))
		_, hasEnabled := decoded["enable_tls_fingerprint"]
		_, hasProfile := decoded["tls_fingerprint_profile_id"]
		require.False(t, hasEnabled)
		require.False(t, hasProfile)
	})
}
