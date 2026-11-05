package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsTLSFingerprintEnabled(t *testing.T) {
	t.Parallel()

	t.Run("anthropic oauth extra true", func(t *testing.T) {
		a := &Account{
			Platform: PlatformAnthropic,
			Type:     AccountTypeOAuth,
			Extra:    map[string]any{"enable_tls_fingerprint": true},
		}
		require.True(t, a.IsTLSFingerprintEnabled())
	})

	t.Run("anthropic oauth missing extra stays off", func(t *testing.T) {
		a := &Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth}
		require.False(t, a.IsTLSFingerprintEnabled())
	})

	t.Run("kiro oauth extra true", func(t *testing.T) {
		a := &Account{
			Platform: PlatformKiro,
			Type:     AccountTypeOAuth,
			Extra:    map[string]any{"enable_tls_fingerprint": true},
		}
		require.True(t, a.IsTLSFingerprintEnabled())
	})

	t.Run("kiro oauth missing extra stays off", func(t *testing.T) {
		a := &Account{Platform: PlatformKiro, Type: AccountTypeOAuth}
		require.False(t, a.IsTLSFingerprintEnabled())
	})

	t.Run("anthropic apikey extra true is ignored", func(t *testing.T) {
		a := &Account{
			Platform: PlatformAnthropic,
			Type:     AccountTypeAPIKey,
			Extra:    map[string]any{"enable_tls_fingerprint": true},
		}
		require.False(t, a.IsTLSFingerprintEnabled())
	})
}
