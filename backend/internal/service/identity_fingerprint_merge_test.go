package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
)

// cachedNonDefaultFingerprint 返回各 Stainless 字段都与 defaultFingerprint 不同的缓存指纹，
// 一旦被默认值覆盖就能看出来。
func cachedNonDefaultFingerprint(ua string) *Fingerprint {
	return &Fingerprint{
		ClientID:                "cid-cached",
		UserAgent:               ua,
		StainlessLang:           "js",
		StainlessPackageVersion: "0.80.1",
		StainlessOS:             "Windows",
		StainlessArch:           "x64",
		StainlessRuntime:        "node",
		StainlessRuntimeVersion: "v22.1.0",
		UpdatedAt:               time.Now().Unix(),
	}
}

func requireCachedStainless(t *testing.T, fp *Fingerprint) {
	t.Helper()
	require.Equal(t, "js", fp.StainlessLang)
	require.Equal(t, "0.80.1", fp.StainlessPackageVersion)
	require.Equal(t, "node", fp.StainlessRuntime)
	require.Equal(t, "v22.1.0", fp.StainlessRuntimeVersion)
	require.Equal(t, "Windows", fp.StainlessOS)
	require.Equal(t, "x64", fp.StainlessArch)
	require.Equal(t, "cid-cached", fp.ClientID)
}

// 升级只带 UA、不带任何 Stainless 头：lang/package/runtime/runtime-version 与 OS/Arch 全部保留缓存。
func TestGetOrCreateFingerprintUpgradeWithoutStainlessHeadersKeepsCache(t *testing.T) {
	cache := &stubIdentityCache{fingerprint: cachedNonDefaultFingerprint("claude-cli/2.1.200 (external, cli)")}
	svc := NewIdentityService(cache)
	newUA := "claude-cli/" + claude.CLICurrentVersion + " (external, cli)"

	fp, err := svc.GetOrCreateFingerprint(context.Background(), 1, headersWithUA(newUA))

	require.NoError(t, err)
	require.Equal(t, newUA, fp.UserAgent)
	requireCachedStainless(t, fp)
	require.Equal(t, 1, cache.setCalls)
	requireCachedStainless(t, cache.lastSet)
}

// 升级带非 js/node 的 Stainless 头：这些头不合并，缓存的 lang/package/runtime/runtime-version 保留。
func TestGetOrCreateFingerprintUpgradeWithNonClaudeStainlessKeepsCache(t *testing.T) {
	cache := &stubIdentityCache{fingerprint: cachedNonDefaultFingerprint("claude-cli/2.1.200 (external, cli)")}
	svc := NewIdentityService(cache)
	headers := headersWithUA("claude-cli/" + claude.CLICurrentVersion + " (external, cli)")
	headers.Set("X-Stainless-Lang", "python")
	headers.Set("X-Stainless-Package-Version", "0.40.0")
	headers.Set("X-Stainless-Runtime", "CPython")
	headers.Set("X-Stainless-Runtime-Version", "3.12.0")

	fp, err := svc.GetOrCreateFingerprint(context.Background(), 1, headers)

	require.NoError(t, err)
	requireCachedStainless(t, fp)
}

// 畸形 UA 自愈：只修 UA，Stainless 字段与 ClientID 保持缓存值（客户端 UA 合法与否两种情况）。
func TestGetOrCreateFingerprintHealKeepsOtherCachedFields(t *testing.T) {
	poisoned := "claude-cli/999.0.0-local (undefined, cli)"
	for name, clientUA := range map[string]string{
		"valid_client_ua":   "claude-cli/" + claude.CLICurrentVersion + " (external, cli)",
		"invalid_client_ua": poisoned,
	} {
		t.Run(name, func(t *testing.T) {
			cache := &stubIdentityCache{fingerprint: cachedNonDefaultFingerprint(poisoned)}
			svc := NewIdentityService(cache)

			fp, err := svc.GetOrCreateFingerprint(context.Background(), 1, headersWithUA(clientUA))

			require.NoError(t, err)
			require.NotContains(t, fp.UserAgent, "999.0.0")
			requireCachedStainless(t, fp)
			require.Equal(t, 1, cache.setCalls)
			requireCachedStainless(t, cache.lastSet)
		})
	}
}
