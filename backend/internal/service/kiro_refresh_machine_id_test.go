//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/stretchr/testify/require"
)

func TestKiroRefreshKeepsConcurrentlyMergedMachineID(t *testing.T) {
	dbAccount := &Account{
		ID:       7,
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Status:   StatusActive,
		Credentials: map[string]any{
			kiro.CredAuthMethod:   kiro.AuthMethodSocial,
			kiro.CredAccessToken:  "old-at",
			kiro.CredRefreshToken: "old-rt",
			"expires_at":          time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		},
	}
	repo := &kiroMachineIDUpdateRepo{
		mockAccountRepoForGemini: mockAccountRepoForGemini{
			accountsByID: map[int64]*Account{7: dbAccount},
		},
	}
	upstream := &kiroFakeUpstream{responses: map[string][]kiroFakeResponse{
		"prod.us-east-1.auth.desktop.kiro.dev/refreshToken": {{200, `{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":3600}`}},
	}}
	svc := newKiroOAuthServiceForTest(upstream, time.Now())
	// 上游刷新期间，请求路径的 persistKiroMachineID 已用 MergeCredentialIfAbsent 补写 machine_id。
	svc.newDoer = func(string) (kiro.Doer, error) {
		return kiro.DoerFunc(func(req *http.Request) (*http.Response, error) {
			merged := *repo.accountsByID[7]
			merged.Credentials = shallowCopyMap(merged.Credentials)
			merged.Credentials[kiro.CredMachineID] = "merged-machine-id"
			repo.accountsByID[7] = &merged
			return upstream.do(req)
		}), nil
	}

	api := NewOAuthRefreshAPI(repo, nil)
	result, err := api.RefreshIfNeeded(context.Background(), dbAccount, NewKiroTokenRefresher(svc), kiroTokenRefreshSkew)
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Len(t, repo.updateCalls, 1)
	persisted := repo.updateCalls[0]
	require.Equal(t, "new-at", persisted[kiro.CredAccessToken])
	require.Equal(t, "new-rt", persisted[kiro.CredRefreshToken])
	require.Equal(t, "merged-machine-id", persisted[kiro.CredMachineID], "refresh must not replace a machine_id merged during the refresh")
}

func TestKiroRefreshAccountTokenDoesNotMintMachineID(t *testing.T) {
	upstream := &kiroFakeUpstream{responses: map[string][]kiroFakeResponse{
		"prod.us-east-1.auth.desktop.kiro.dev/refreshToken": {{200, `{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":3600}`}},
	}}
	svc := newKiroOAuthServiceForTest(upstream, time.Now())
	account := &Account{
		ID:       8,
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			kiro.CredAuthMethod:   kiro.AuthMethodSocial,
			kiro.CredRefreshToken: "old-rt",
		},
	}

	creds, err := svc.RefreshAccountToken(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "new-at", creds[kiro.CredAccessToken])
	require.Equal(t, "new-rt", creds[kiro.CredRefreshToken])
	_, has := creds[kiro.CredMachineID]
	require.False(t, has, "machine_id is merged by persistKiroMachineID, not minted by refresh")
}

func TestKiroImportCredentialsKeepsMachineID(t *testing.T) {
	upstream := &kiroFakeUpstream{responses: map[string][]kiroFakeResponse{
		"prod.us-east-1.auth.desktop.kiro.dev/refreshToken": {{200, `{"accessToken":"at","refreshToken":"rt2","expiresIn":3600}`}},
	}}
	svc := newKiroOAuthServiceForTest(upstream, time.Now())

	creds, err := svc.ImportCredentials(context.Background(), KiroImportInput{Raw: "aorAAAAAGexample-refresh-token"})
	require.NoError(t, err)
	require.NotEmpty(t, creds[kiro.CredMachineID], "a new social account is created with a machine_id")
}
