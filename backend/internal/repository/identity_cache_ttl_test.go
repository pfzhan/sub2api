package repository

import (
	"testing"
	"time"
)

func TestShouldRefreshMaskedSession(t *testing.T) {
	cases := []struct {
		name      string
		remaining time.Duration
		want      bool
	}{
		{"刚好阈值", maskedSessionRefreshTTL, true},
		{"低于阈值", maskedSessionRefreshTTL - time.Minute, true},
		{"1 秒剩余", time.Second, true},
		{"高于阈值", maskedSessionRefreshTTL + time.Second, false},
		{"满 TTL", maskedSessionTTL, false},
		{"已过期或无 key", 0, false},
		{"无过期", -1 * time.Second, false},
		{"key 不存在", -2 * time.Second, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRefreshMaskedSession(tc.remaining); got != tc.want {
				t.Fatalf("shouldRefreshMaskedSession(%v) = %v, want %v", tc.remaining, got, tc.want)
			}
		})
	}
}
