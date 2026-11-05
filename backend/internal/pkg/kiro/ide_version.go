package kiro

import (
	"log/slog"
	"os"
	"strings"

	"golang.org/x/mod/semver"
)

// IDEVersionEnv 是 ideVersion 的可选运维覆盖。
//
// 伪装身份必须在进程生命周期内恒定：User-Agent 与 x-amz-user-agent 都嵌入
// 这个版本号。每次请求读环境变量会让运行中改 env 的进程自相矛盾。
const IDEVersionEnv = "SUB2API_KIRO_IDE_VERSION"

var resolvedIDEVersion = resolveIDEVersion(os.Getenv(IDEVersionEnv))

// IDEVersion 返回对外声明的 Kiro IDE 版本号（三段 semver）。
func IDEVersion() string {
	return resolvedIDEVersion
}

// isSupportedIDEVersion 要求严格三段纯数字，且不低于内置基线 ideVersion。
// 非法字符会破坏 Header.Set；预发布后缀没有官方对照，一律拒绝。
func isSupportedIDEVersion(version string) bool {
	version = strings.TrimSpace(version)
	if version == "" {
		return false
	}
	canonical := "v" + version
	if !semver.IsValid(canonical) || semver.Canonical(canonical) != canonical {
		return false
	}
	if semver.Prerelease(canonical) != "" || semver.Build(canonical) != "" {
		return false
	}
	return semver.Compare(canonical, "v"+ideVersion) >= 0
}

func resolveIDEVersion(raw string) string {
	version := strings.TrimSpace(raw)
	if version == "" {
		return ideVersion
	}
	if !isSupportedIDEVersion(version) {
		slog.Warn("ignoring invalid Kiro IDE version override; falling back to the built-in pin",
			"env", IDEVersionEnv,
			"value", version,
			"builtin", ideVersion,
			"requirement", "strict three-part semver, not older than the built-in pin")
		return ideVersion
	}
	return version
}
