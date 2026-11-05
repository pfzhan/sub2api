package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type maskingTTLCache struct {
	sessionID  string
	setCalls   int
	touchCalls int
}

func (c *maskingTTLCache) GetFingerprint(_ context.Context, _ int64) (*Fingerprint, error) {
	return nil, nil
}
func (c *maskingTTLCache) SetFingerprint(_ context.Context, _ int64, _ *Fingerprint) error {
	return nil
}
func (c *maskingTTLCache) GetMaskedSessionID(_ context.Context, _ int64) (string, error) {
	return c.sessionID, nil
}
func (c *maskingTTLCache) SetMaskedSessionID(_ context.Context, _ int64, sessionID string) error {
	c.setCalls++
	c.sessionID = sessionID
	return nil
}
func (c *maskingTTLCache) TouchMaskedSessionID(_ context.Context, _ int64) error {
	c.touchCalls++
	return nil
}

func maskingTestAccount() *Account {
	return &Account{
		ID:       123,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"session_id_masking_enabled": true,
		},
	}
}

func maskingTestBody(sessionID string) []byte {
	userID := FormatMetadataUserID(
		"d61f76d0730d2b920763648949bad5c79742155c27037fc77ac3f9805cb90169",
		"",
		sessionID,
		"2.1.78",
	)
	return []byte(`{"messages":[],"metadata":{"user_id":` + strconvQuote(userID) + `}}`)
}

func TestRewriteUserIDWithMasking_SetsOnlyWhenMissing(t *testing.T) {
	cache := &maskingTTLCache{}
	svc := NewIdentityService(cache)

	_, err := svc.RewriteUserIDWithMasking(context.Background(), maskingTestBody("7578cf37-aaca-46e4-a45c-71285d9dbb83"), maskingTestAccount(), 0, "acc-uuid", "client-xyz", "claude-cli/2.1.78 (external, cli)")
	require.NoError(t, err)
	require.Equal(t, 1, cache.setCalls)
	require.Equal(t, 0, cache.touchCalls)
	require.NotEmpty(t, cache.sessionID)
}

func TestRewriteUserIDWithMasking_TouchesExistingWithoutSet(t *testing.T) {
	existing := "11111111-2222-4333-8444-555555555555"
	cache := &maskingTTLCache{sessionID: existing}
	svc := NewIdentityService(cache)

	result, err := svc.RewriteUserIDWithMasking(context.Background(), maskingTestBody("7578cf37-aaca-46e4-a45c-71285d9dbb83"), maskingTestAccount(), 0, "acc-uuid", "client-xyz", "claude-cli/2.1.78 (external, cli)")
	require.NoError(t, err)
	require.Equal(t, 0, cache.setCalls)
	require.Equal(t, 1, cache.touchCalls)
	require.Equal(t, existing, cache.sessionID)
	require.Contains(t, string(result), existing)
}

func TestRewriteUserIDWithMasking_SecondRequestDoesNotSetAgain(t *testing.T) {
	cache := &maskingTTLCache{}
	svc := NewIdentityService(cache)
	body := maskingTestBody("7578cf37-aaca-46e4-a45c-71285d9dbb83")
	account := maskingTestAccount()

	_, err := svc.RewriteUserIDWithMasking(context.Background(), body, account, 0, "acc-uuid", "client-xyz", "claude-cli/2.1.78 (external, cli)")
	require.NoError(t, err)
	require.Equal(t, 1, cache.setCalls)

	_, err = svc.RewriteUserIDWithMasking(context.Background(), body, account, 0, "acc-uuid", "client-xyz", "claude-cli/2.1.78 (external, cli)")
	require.NoError(t, err)
	require.Equal(t, 1, cache.setCalls)
	require.Equal(t, 1, cache.touchCalls)
}

func TestRewriteUserIDWithMasking_SeparatesAPIKeysOnSameAccount(t *testing.T) {
	existing := "11111111-2222-4333-8444-555555555555"
	cache := &maskingTTLCache{sessionID: existing}
	svc := NewIdentityService(cache)
	rewrite := func(apiKeyID int64) string {
		result, err := svc.RewriteUserIDWithMasking(context.Background(), maskingTestBody("7578cf37-aaca-46e4-a45c-71285d9dbb83"), maskingTestAccount(), apiKeyID, "acc-uuid", "client-xyz", "claude-cli/2.1.78 (external, cli)")
		require.NoError(t, err)
		parsed := ParseMetadataUserID(gjson.GetBytes(result, "metadata.user_id").String())
		require.NotNil(t, parsed)
		return parsed.SessionID
	}

	keyA, keyB := rewrite(1), rewrite(2)
	require.NotEqual(t, keyA, keyB, "different API keys must not share one masked session")
	require.Equal(t, keyA, rewrite(1), "the same API key keeps its masked session while the account session is valid")
	require.NotEqual(t, existing, keyA)
	require.Equal(t, existing, rewrite(0), "without an API key the account-level session is used")
	require.Zero(t, cache.setCalls, "the account-level Redis key is not rewritten per API key")
}
