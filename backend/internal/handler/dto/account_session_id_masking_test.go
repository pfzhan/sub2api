package dto

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestAccountFromServiceShallow_SessionIDMaskingAlwaysEmitted(t *testing.T) {
	t.Parallel()

	t.Run("missing extra defaults on", func(t *testing.T) {
		out := AccountFromServiceShallow(&service.Account{
			ID:       1,
			Platform: service.PlatformAnthropic,
			Type:     service.AccountTypeOAuth,
		})
		require.NotNil(t, out.EnableSessionIDMasking)
		require.True(t, *out.EnableSessionIDMasking)

		payload, err := json.Marshal(out)
		require.NoError(t, err)
		var decoded map[string]any
		require.NoError(t, json.Unmarshal(payload, &decoded))
		require.Equal(t, true, decoded["session_id_masking_enabled"])
	})

	t.Run("explicit false is serialized", func(t *testing.T) {
		out := AccountFromServiceShallow(&service.Account{
			ID:       2,
			Platform: service.PlatformAnthropic,
			Type:     service.AccountTypeSetupToken,
			Extra:    map[string]any{"session_id_masking_enabled": false},
		})
		require.NotNil(t, out.EnableSessionIDMasking)
		require.False(t, *out.EnableSessionIDMasking)

		payload, err := json.Marshal(out)
		require.NoError(t, err)
		var decoded map[string]any
		require.NoError(t, json.Unmarshal(payload, &decoded))
		require.Equal(t, false, decoded["session_id_masking_enabled"])
	})
}
