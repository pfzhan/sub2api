//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 上游刷新期间其他路径写入了凭据（另一个 worker 写了 refresh_token、管理员改了
// model_mapping）：落库时只应用本次刷新改动的键，不能用调用前的快照整份写回。
func TestRefreshIfNeeded_PersistKeepsCredentialsWrittenDuringRefresh(t *testing.T) {
	account := &Account{
		ID: 21, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Status: StatusActive,
		Credentials: map[string]any{
			"access_token":  "at-old",
			"refresh_token": "rt-1",
			"token_type":    "Bearer",
			"model_mapping": map[string]any{"a": "a1"},
			"legacy_field":  "x",
		},
	}
	repo := &refreshAPIAccountRepo{account: account}
	executor := &refreshAPIExecutorStub{
		needsRefresh: true,
		// 与 ClaudeTokenRefresher 一样：刷新结果合并自调用前的快照。
		// refresh_token 未轮换，legacy_field 被刷新逻辑移除。
		credentials: map[string]any{
			"access_token":  "at-new",
			"refresh_token": "rt-1",
			"token_type":    "Bearer",
			"expires_at":    "1900000000",
			"model_mapping": map[string]any{"a": "a1"},
		},
		onRefresh: func() {
			concurrent := *repo.account
			concurrent.Credentials = map[string]any{
				"access_token":  "at-old",
				"refresh_token": "rt-2",
				"token_type":    "Bearer",
				"model_mapping": map[string]any{"a": "a2"},
				"legacy_field":  "x",
			}
			repo.account = &concurrent
		},
	}

	result, err := NewOAuthRefreshAPI(repo, nil).RefreshIfNeeded(context.Background(), account, executor, 3*time.Minute)

	require.NoError(t, err)
	require.True(t, result.Refreshed)
	stored := repo.account.Credentials
	require.Equal(t, "at-new", stored["access_token"])
	require.Equal(t, "1900000000", stored["expires_at"])
	require.Equal(t, "rt-2", stored["refresh_token"], "a refresh_token written during the refresh must not be reverted to the snapshot")
	require.Equal(t, map[string]any{"a": "a2"}, stored["model_mapping"])
	require.NotContains(t, stored, "legacy_field", "keys the refresh removed stay removed")
	require.NotNil(t, stored["_token_version"])
	require.Equal(t, stored, result.Account.Credentials, "the returned account matches what was persisted")
}

func TestRefreshIfNeeded_PersistRereadFailureStillWritesRefreshedCredentials(t *testing.T) {
	account := &Account{
		ID: 22, Platform: PlatformAnthropic, Type: AccountTypeSetupToken, Status: StatusActive,
		Credentials: map[string]any{"access_token": "at-old", "refresh_token": "rt-1"},
	}
	repo := &refreshAPIAccountRepo{account: account, getByIDErrAfterCall: 2, getByIDErrAfterCallErr: errors.New("db read timeout")}
	executor := &refreshAPIExecutorStub{
		needsRefresh: true,
		credentials:  map[string]any{"access_token": "at-new", "refresh_token": "rt-2"},
	}

	result, err := NewOAuthRefreshAPI(repo, nil).RefreshIfNeeded(context.Background(), account, executor, 3*time.Minute)

	require.NoError(t, err)
	require.True(t, result.Refreshed)
	require.Equal(t, 1, repo.updateCredentialsCalls, "rotated tokens are persisted even when the pre-write reread fails")
	require.Equal(t, "rt-2", repo.account.Credentials["refresh_token"])
	require.Equal(t, "at-new", repo.account.Credentials["access_token"])
}
