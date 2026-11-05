package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func customBaseURLAdminAccount(id int64, extra map[string]any) *Account {
	return &Account{
		ID:          id,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Credentials: map[string]any{"access_token": "token"},
		Extra:       extra,
	}
}

func TestCreateAccountRejectsInvalidCustomBaseURL(t *testing.T) {
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken} {
		t.Run(accountType, func(t *testing.T) {
			svc := &adminServiceImpl{accountRepo: &upstreamBillingProbeAccountRepo{}}
			_, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
				Name:                 "claude",
				Platform:             PlatformAnthropic,
				Type:                 accountType,
				Credentials:          map[string]any{"access_token": "token"},
				SkipDefaultGroupBind: true,
				Extra:                map[string]any{"custom_base_url_enabled": true, "custom_base_url": "relay.example.com"},
			})
			require.Error(t, err)
			require.Equal(t, "INVALID_CUSTOM_BASE_URL", infraerrors.Reason(err))
		})
	}
}

func TestCreateAccountAcceptsCustomBaseURLThatRuntimeAccepts(t *testing.T) {
	svc := &adminServiceImpl{accountRepo: &upstreamBillingProbeAccountRepo{}}
	created, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "claude",
		Platform:             PlatformAnthropic,
		Type:                 AccountTypeOAuth,
		Credentials:          map[string]any{"access_token": "token"},
		SkipDefaultGroupBind: true,
		Extra:                map[string]any{"custom_base_url_enabled": true, "custom_base_url": "https://relay.example.com"},
	})
	require.NoError(t, err)
	require.Equal(t, "https://relay.example.com", created.GetEffectiveCustomBaseURL())
}

// 保存校验与网关转发用同一套规则：开启白名单后不在白名单里的中继不能保存。
func TestUpdateAccountCustomBaseURLFollowsRuntimeAllowlist(t *testing.T) {
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = true
	cfg.Security.URLAllowlist.UpstreamHosts = []string{"relay.example.com"}
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		301: customBaseURLAdminAccount(301, map[string]any{}),
	}}
	svc := &adminServiceImpl{accountRepo: repo, cfg: cfg}

	_, err := svc.UpdateAccount(context.Background(), 301, &UpdateAccountInput{
		Extra: map[string]any{"custom_base_url_enabled": true, "custom_base_url": "https://other.example.com"},
	})
	require.Error(t, err)
	require.Equal(t, "INVALID_CUSTOM_BASE_URL", infraerrors.Reason(err))
	require.Empty(t, repo.accounts[301].Extra, "a rejected update must not be persisted")

	updated, err := svc.UpdateAccount(context.Background(), 301, &UpdateAccountInput{
		Extra: map[string]any{"custom_base_url_enabled": true, "custom_base_url": "https://relay.example.com"},
	})
	require.NoError(t, err)
	require.Equal(t, "https://relay.example.com", updated.GetEffectiveCustomBaseURL())
}

// 中继不生效的组合（开关关闭、地址为空）不校验，保存后原样保留，运行时按未启用处理。
func TestUpdateAccountSkipsCustomBaseURLValidationWhenRelayInactive(t *testing.T) {
	for name, extra := range map[string]map[string]any{
		"switch off":    {"custom_base_url_enabled": false, "custom_base_url": "relay.example.com"},
		"empty url":     {"custom_base_url_enabled": true, "custom_base_url": " "},
		"url absent":    {"custom_base_url_enabled": true},
		"switch absent": {"custom_base_url": "not a url"},
	} {
		t.Run(name, func(t *testing.T) {
			repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
				302: customBaseURLAdminAccount(302, map[string]any{}),
			}}
			svc := &adminServiceImpl{accountRepo: repo}
			updated, err := svc.UpdateAccount(context.Background(), 302, &UpdateAccountInput{Extra: extra})
			require.NoError(t, err)
			require.Empty(t, updated.GetEffectiveCustomBaseURL())
		})
	}
}
