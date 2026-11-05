//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/stretchr/testify/require"
)

func TestKiroTokenRefresherNeedsRefresh(t *testing.T) {
	r := NewKiroTokenRefresher(nil)
	window := 15 * time.Minute
	account := func(creds map[string]any) *Account {
		return &Account{ID: 1, Platform: PlatformKiro, Type: AccountTypeOAuth, Credentials: creds}
	}

	require.True(t, r.NeedsRefresh(account(map[string]any{"refresh_token": "rt"}), window), "imported refresh token only")
	require.True(t, r.NeedsRefresh(account(map[string]any{"access_token": "at"}), window), "unknown expiry")
	require.True(t, r.NeedsRefresh(account(map[string]any{
		"access_token": "at",
		"expires_at":   time.Now().Add(5 * time.Minute).Format(time.RFC3339),
	}), window))
	require.False(t, r.NeedsRefresh(account(map[string]any{
		"access_token": "at",
		"expires_at":   time.Now().Add(time.Hour).Format(time.RFC3339),
	}), window))

	other := &Account{ID: 2, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Credentials: map[string]any{}}
	require.False(t, r.CanRefresh(other))
	require.False(t, r.NeedsRefresh(other, window))
	require.Equal(t, "kiro:account:1", r.CacheKey(account(nil)))
}

type kiroMachineIDUpdateRepo struct {
	mockAccountRepoForGemini
	updateCalls []map[string]any
}

func (r *kiroMachineIDUpdateRepo) UpdateCredentials(_ context.Context, id int64, credentials map[string]any) error {
	copied := shallowCopyMap(credentials)
	r.updateCalls = append(r.updateCalls, copied)
	if acc, ok := r.accountsByID[id]; ok {
		acc.Credentials = copied
	}
	return nil
}

type kiroMachineIDMergeRepo struct {
	kiroMachineIDUpdateRepo
	mergeCalls int
	mergeKey   string
	mergeValue string
}

func (r *kiroMachineIDMergeRepo) MergeCredentialIfAbsent(_ context.Context, id int64, key, value string) (string, error) {
	r.mergeCalls++
	r.mergeKey = key
	r.mergeValue = value
	acc := r.accountsByID[id]
	if acc != nil {
		if existing := acc.GetCredential(key); existing != "" {
			return existing, nil
		}
		if acc.Credentials == nil {
			acc.Credentials = map[string]any{}
		}
		acc.Credentials[key] = value
	}
	return value, nil
}

func TestPersistKiroMachineIDMergesOnlyMachineID(t *testing.T) {
	dbAccount := &Account{
		ID:       7,
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":  "new-at",
			"refresh_token": "new-rt",
		},
	}
	repo := &kiroMachineIDMergeRepo{
		kiroMachineIDUpdateRepo: kiroMachineIDUpdateRepo{
			mockAccountRepoForGemini: mockAccountRepoForGemini{
				accountsByID: map[int64]*Account{7: dbAccount},
			},
		},
	}
	snapshot := &Account{
		ID:       7,
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":  "old-at",
			"refresh_token": "old-rt",
		},
	}
	provider := NewKiroTokenProvider(repo, nil, nil, nil)
	provider.persistKiroMachineID(context.Background(), snapshot)

	require.Equal(t, 1, repo.mergeCalls)
	require.Equal(t, kiro.CredMachineID, repo.mergeKey)
	require.NotEmpty(t, repo.mergeValue)
	require.Empty(t, repo.updateCalls, "must not rewrite the whole credential map")
	require.Equal(t, "new-rt", dbAccount.GetCredential("refresh_token"))
	require.Equal(t, "old-rt", snapshot.GetCredential("refresh_token"))
	require.Equal(t, repo.mergeValue, snapshot.GetCredential(kiro.CredMachineID))
	require.Equal(t, repo.mergeValue, dbAccount.GetCredential(kiro.CredMachineID))
}

func TestPersistKiroMachineIDFallbackWritesFreshCredentials(t *testing.T) {
	dbAccount := &Account{
		ID:       8,
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":  "new-at",
			"refresh_token": "new-rt",
		},
	}
	repo := &kiroMachineIDUpdateRepo{
		mockAccountRepoForGemini: mockAccountRepoForGemini{
			accountsByID: map[int64]*Account{8: dbAccount},
		},
	}
	snapshot := &Account{
		ID:       8,
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":  "old-at",
			"refresh_token": "old-rt",
		},
	}
	provider := NewKiroTokenProvider(repo, nil, nil, nil)
	provider.persistKiroMachineID(context.Background(), snapshot)

	require.Len(t, repo.updateCalls, 1)
	require.Equal(t, "new-rt", repo.updateCalls[0]["refresh_token"])
	require.Equal(t, "new-at", repo.updateCalls[0]["access_token"])
	require.NotEmpty(t, repo.updateCalls[0][kiro.CredMachineID])
	require.Equal(t, repo.updateCalls[0][kiro.CredMachineID], snapshot.GetCredential(kiro.CredMachineID))
}

func TestPersistKiroMachineIDAdoptsExistingDBValue(t *testing.T) {
	dbAccount := &Account{
		ID:       9,
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"refresh_token": "new-rt",
			"machine_id":    "already-there",
		},
	}
	repo := &kiroMachineIDMergeRepo{
		kiroMachineIDUpdateRepo: kiroMachineIDUpdateRepo{
			mockAccountRepoForGemini: mockAccountRepoForGemini{
				accountsByID: map[int64]*Account{9: dbAccount},
			},
		},
	}
	snapshot := &Account{
		ID:       9,
		Platform: PlatformKiro,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"refresh_token": "old-rt",
		},
	}
	provider := NewKiroTokenProvider(repo, nil, nil, nil)
	provider.persistKiroMachineID(context.Background(), snapshot)

	require.Equal(t, 0, repo.mergeCalls)
	require.Empty(t, repo.updateCalls)
	require.Equal(t, "already-there", snapshot.GetCredential(kiro.CredMachineID))
	require.Equal(t, "old-rt", snapshot.GetCredential("refresh_token"))
}
