package dto

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 账号 extra 从数据库 JSON 解出来时数字是 float64，部分路径用 UseNumber 得到 json.Number，
// 代码内写入的是 int。三种形态在 DTO 上必须得到同样的值，-1 随机模板不得变成 0。
func TestAccountFromServiceShallow_ExtraNumbersAcrossDecodedTypes(t *testing.T) {
	t.Parallel()

	const raw = `{
		"window_cost_limit": 50,
		"window_cost_sticky_reserve": 12,
		"max_sessions": 4,
		"session_idle_timeout_minutes": 9,
		"base_rpm": 30,
		"rpm_sticky_buffer": 6,
		"enable_tls_fingerprint": true,
		"tls_fingerprint_profile_id": -1
	}`

	decodeFloat := func(t *testing.T) map[string]any {
		var out map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &out))
		return out
	}
	decodeNumber := func(t *testing.T) map[string]any {
		dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
		dec.UseNumber()
		var out map[string]any
		require.NoError(t, dec.Decode(&out))
		return out
	}
	goInts := func(*testing.T) map[string]any {
		return map[string]any{
			"window_cost_limit":            50,
			"window_cost_sticky_reserve":   12,
			"max_sessions":                 4,
			"session_idle_timeout_minutes": 9,
			"base_rpm":                     int64(30),
			"rpm_sticky_buffer":            6,
			"enable_tls_fingerprint":       true,
			"tls_fingerprint_profile_id":   int64(-1),
		}
	}

	for _, decoded := range []struct {
		name  string
		build func(*testing.T) map[string]any
	}{
		{"float64", decodeFloat},
		{"json.Number", decodeNumber},
		{"go ints", goInts},
	} {
		for _, accountType := range []string{service.AccountTypeOAuth, service.AccountTypeSetupToken} {
			t.Run(decoded.name+"/"+accountType, func(t *testing.T) {
				out := AccountFromServiceShallow(&service.Account{
					ID:          1,
					Platform:    service.PlatformAnthropic,
					Type:        accountType,
					Concurrency: 3,
					Extra:       decoded.build(t),
				})

				require.NotNil(t, out.WindowCostLimit)
				require.Equal(t, 50.0, *out.WindowCostLimit)
				require.NotNil(t, out.WindowCostStickyReserve)
				require.Equal(t, 12.0, *out.WindowCostStickyReserve)
				require.NotNil(t, out.MaxSessions)
				require.Equal(t, 4, *out.MaxSessions)
				require.NotNil(t, out.SessionIdleTimeoutMin)
				require.Equal(t, 9, *out.SessionIdleTimeoutMin)
				require.NotNil(t, out.BaseRPM)
				require.Equal(t, 30, *out.BaseRPM)
				require.NotNil(t, out.RPMStickyBuffer)
				require.Equal(t, 6, *out.RPMStickyBuffer)
				require.NotNil(t, out.EnableTLSFingerprint)
				require.True(t, *out.EnableTLSFingerprint)
				require.NotNil(t, out.TLSFingerprintProfileID)
				require.Equal(t, int64(-1), *out.TLSFingerprintProfileID)
			})
		}
	}
}
