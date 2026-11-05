package kiro

import "testing"

func TestIsSupportedIDEVersion(t *testing.T) {
	cases := []struct {
		name    string
		version string
		want    bool
	}{
		{"内置基线本身", ideVersion, true},
		{"高于基线的补丁位", "0.11.64", true},
		{"高于基线的次版本", "0.12.0", true},
		{"高于基线的主版本", "1.0.0", true},
		{"低于基线", "0.11.62", false},
		{"远低于基线", "0.1.0", false},
		{"空值", "", false},
		{"只有两段", "0.11", false},
		{"带 v 前缀", "v0.12.0", false},
		{"预发布后缀", "0.12.0-nightly", false},
		{"构建元数据", "0.12.0+build1", false},
		{"换行注入", "0.12.0\r\nX", false},
		{"空格", "0.12.0 extra", false},
		{"非数字", "latest", false},
		{"多余的段", "0.11.63.1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSupportedIDEVersion(tc.version); got != tc.want {
				t.Fatalf("isSupportedIDEVersion(%q) = %v, want %v", tc.version, got, tc.want)
			}
		})
	}
}

func TestResolveIDEVersion(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"未配置时用内置基线", "", ideVersion},
		{"只有空白等同未配置", "   ", ideVersion},
		{"合法覆盖生效", "0.12.0", "0.12.0"},
		{"两侧空白被裁掉", "  0.12.0  ", "0.12.0"},
		{"非法值回落基线", "not-a-version", ideVersion},
		{"向下覆盖被拒", "0.10.0", ideVersion},
		{"预发布后缀被拒", "0.12.0-nightly", ideVersion},
		{"换行注入被拒", "0.12.0\nX", ideVersion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveIDEVersion(tc.raw); got != tc.want {
				t.Fatalf("resolveIDEVersion(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestIDEVersionDefaultsToBuiltinPin(t *testing.T) {
	if got := IDEVersion(); got != ideVersion {
		t.Fatalf("IDEVersion() = %q, want built-in pin %q (测试进程未设置 %s)", got, ideVersion, IDEVersionEnv)
	}
}
