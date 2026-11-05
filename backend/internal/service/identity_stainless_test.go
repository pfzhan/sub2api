package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
)

func TestCreateFingerprintIgnoresNonClaudeStainless(t *testing.T) {
	t.Parallel()
	svc := NewIdentityService(&stubIdentityCache{})
	headers := headersWithUA("python-httpx/0.27")
	headers.Set("X-Stainless-Lang", "python")
	headers.Set("X-Stainless-Package-Version", "0.40.0")
	headers.Set("X-Stainless-Runtime", "CPython")
	headers.Set("X-Stainless-Runtime-Version", "3.12.0")
	headers.Set("X-Stainless-OS", "MacOS")
	headers.Set("X-Stainless-Arch", "x64")

	fp, err := svc.GetOrCreateFingerprint(context.Background(), 11, headers)
	require.NoError(t, err)
	df := defaultFingerprint()
	require.Equal(t, df.StainlessLang, fp.StainlessLang)
	require.Equal(t, df.StainlessPackageVersion, fp.StainlessPackageVersion)
	require.Equal(t, df.StainlessRuntime, fp.StainlessRuntime)
	require.Equal(t, df.StainlessRuntimeVersion, fp.StainlessRuntimeVersion)
	require.Equal(t, "MacOS", fp.StainlessOS)
	require.Equal(t, "x64", fp.StainlessArch)
}

func TestCreateFingerprintKeepsClaudeCodeStainless(t *testing.T) {
	t.Parallel()
	svc := NewIdentityService(&stubIdentityCache{})
	headers := headersWithUA("claude-cli/" + claude.CLICurrentVersion + " (external, cli)")
	headers.Set("X-Stainless-Lang", "js")
	headers.Set("X-Stainless-Package-Version", "0.94.0")
	headers.Set("X-Stainless-Runtime", "node")
	headers.Set("X-Stainless-Runtime-Version", "v24.3.0")
	headers.Set("X-Stainless-OS", "Darwin")
	headers.Set("X-Stainless-Arch", "arm64")

	fp, err := svc.GetOrCreateFingerprint(context.Background(), 12, headers)
	require.NoError(t, err)
	require.Equal(t, "js", fp.StainlessLang)
	require.Equal(t, "0.94.0", fp.StainlessPackageVersion)
	require.Equal(t, "node", fp.StainlessRuntime)
	require.Equal(t, "v24.3.0", fp.StainlessRuntimeVersion)
	require.Equal(t, "Darwin", fp.StainlessOS)
	require.Equal(t, "arm64", fp.StainlessArch)
}

func TestApplyClaudeCodeMimicHeadersForcesJSNodeStainless(t *testing.T) {
	t.Parallel()
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	require.NoError(t, err)
	req.Header.Set("X-Stainless-Lang", "python")
	req.Header.Set("X-Stainless-Package-Version", "0.40.0")
	req.Header.Set("X-Stainless-Runtime", "CPython")
	req.Header.Set("X-Stainless-Runtime-Version", "3.12.0")
	req.Header.Set("X-Stainless-OS", "MacOS")
	req.Header.Set("X-Stainless-Arch", "x64")

	ua := claude.DefaultUserAgent()
	applyClaudeCodeMimicHeaders(req, false, ua)

	require.Equal(t, ua, getHeaderRaw(req.Header, "User-Agent"))
	require.Equal(t, "js", getHeaderRaw(req.Header, "X-Stainless-Lang"))
	require.Equal(t, "node", getHeaderRaw(req.Header, "X-Stainless-Runtime"))
	require.Equal(t, "0.94.0", getHeaderRaw(req.Header, "X-Stainless-Package-Version"))
	require.Equal(t, "v24.3.0", getHeaderRaw(req.Header, "X-Stainless-Runtime-Version"))
	require.Equal(t, "MacOS", getHeaderRaw(req.Header, "X-Stainless-OS"))
	require.Equal(t, "x64", getHeaderRaw(req.Header, "X-Stainless-Arch"))
}

func TestMergeHeadersIntoFingerprintKeepsCachedStainlessWhenVersionHeadersMissing(t *testing.T) {
	t.Parallel()
	cached := defaultFingerprint()
	cached.UserAgent = "claude-cli/2.1.200 (external, cli)"
	cached.StainlessPackageVersion = "0.90.0"
	cached.StainlessRuntimeVersion = "v24.3.0"
	svc := NewIdentityService(&stubIdentityCache{fingerprint: &cached})

	headers := headersWithUA("claude-cli/" + claude.CLICurrentVersion + " (external, cli)")
	headers.Set("X-Stainless-Lang", "js")
	headers.Set("X-Stainless-Runtime", "node")

	fp, err := svc.GetOrCreateFingerprint(context.Background(), 13, headers)
	require.NoError(t, err)
	require.Equal(t, "js", fp.StainlessLang)
	require.Equal(t, "node", fp.StainlessRuntime)
	require.Equal(t, "0.90.0", fp.StainlessPackageVersion)
	require.Equal(t, "v24.3.0", fp.StainlessRuntimeVersion)
	require.Equal(t, "claude-cli/"+claude.CLICurrentVersion+" (external, cli)", fp.UserAgent)
}
