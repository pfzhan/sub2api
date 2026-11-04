//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func mustParsePrefixAffinityRequest(t *testing.T, body string) *ParsedRequest {
	t.Helper()
	parsed, err := ParseGatewayRequest(NewRequestBodyRef([]byte(body)), "")
	require.NoError(t, err)
	return parsed
}

func TestSharedPrefixKey(t *testing.T) {
	const model = "claude-sonnet-4-5"
	key := func(body string) string {
		return SharedPrefixKey(mustParsePrefixAffinityRequest(t, body), model)
	}

	base := key(`{"tools":[{"name":"read","input_schema":{}}],"system":[{"type":"text","text":"You are a helper.","cache_control":{"type":"ephemeral"}},{"type":"text","text":"cwd: /home/alice"}],"messages":[{"role":"user","content":"hi"}]}`)
	require.NotEmpty(t, base)

	t.Run("断点之后的内容与消息不计入", func(t *testing.T) {
		other := key(`{"tools":[{"name":"read","input_schema":{}}],"system":[{"type":"text","text":"You are a helper.","cache_control":{"type":"ephemeral"}},{"type":"text","text":"cwd: /home/bob"}],"messages":[{"role":"user","content":"something else"}]}`)
		require.Equal(t, base, other)
	})

	t.Run("缓存断点位置与计费块不影响 key", func(t *testing.T) {
		other := key(`{"tools":[{"name":"read","input_schema":{}}],"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=1.0; cch=abc;"},{"type":"text","text":"You are a helper.","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[]}`)
		require.Equal(t, base, other)
	})

	t.Run("断点之前的内容不同则 key 不同", func(t *testing.T) {
		other := key(`{"tools":[{"name":"write","input_schema":{}}],"system":[{"type":"text","text":"You are a helper.","cache_control":{"type":"ephemeral"}}],"messages":[]}`)
		require.NotEqual(t, base, other)
	})

	t.Run("模型不同则 key 不同", func(t *testing.T) {
		parsed := mustParsePrefixAffinityRequest(t, `{"tools":[{"name":"read","input_schema":{}}],"system":[{"type":"text","text":"You are a helper.","cache_control":{"type":"ephemeral"}}],"messages":[]}`)
		require.NotEqual(t, base, SharedPrefixKey(parsed, "claude-opus-4-1"))
	})

	t.Run("tools 上的断点截断 system", func(t *testing.T) {
		withBreakpoint := key(`{"tools":[{"name":"read","input_schema":{},"cache_control":{"type":"ephemeral"}}],"system":"alice","messages":[]}`)
		require.Equal(t, withBreakpoint, key(`{"tools":[{"name":"read","input_schema":{},"cache_control":{"type":"ephemeral"}}],"system":"bob","messages":[]}`))
		// 没有断点时取全部 system
		withoutBreakpoint := key(`{"tools":[{"name":"read","input_schema":{}}],"system":"alice","messages":[]}`)
		require.NotEqual(t, withoutBreakpoint, key(`{"tools":[{"name":"read","input_schema":{}}],"system":"bob","messages":[]}`))
	})

	t.Run("没有 tools 和 system 时返回空", func(t *testing.T) {
		require.Empty(t, key(`{"messages":[{"role":"user","content":"hi"}]}`))
		require.Empty(t, SharedPrefixKey(nil, model))
	})
}

func TestSelectByPrefixAffinity(t *testing.T) {
	const group = int64(7)
	// 每个账号并发 5，一个请求的负载是 20%
	accounts := func(rates ...int) []accountWithLoad {
		out := make([]accountWithLoad, len(rates))
		for i, rate := range rates {
			out[i] = accountWithLoad{
				account:  &Account{ID: int64(i + 1), Concurrency: 5},
				loadInfo: &AccountLoadInfo{AccountID: int64(i + 1), LoadRate: rate},
			}
		}
		return out
	}

	idle := accounts(0, 0, 0, 0)
	first := selectByPrefixAffinity(idle, group, "prefix-a", 80)
	require.NotNil(t, first)

	// 与账号顺序和负载无关，同一前缀总是选到同一账号
	reversed := []accountWithLoad{idle[3], idle[2], idle[1], idle[0]}
	require.Equal(t, first.account.ID, selectByPrefixAffinity(reversed, group, "prefix-a", 80).account.ID)
	busy := accounts(70, 70, 70, 70)
	require.Equal(t, first.account.ID, selectByPrefixAffinity(busy, group, "prefix-a", 80).account.ID)

	// 排名最前的账号达到上限时顺延到其他账号
	capped := accounts(0, 0, 0, 0)
	capped[first.account.ID-1].loadInfo.LoadRate = 80
	next := selectByPrefixAffinity(capped, group, "prefix-a", 80)
	require.NotNil(t, next)
	require.NotEqual(t, first.account.ID, next.account.ID)

	// 全部达到上限时交回调用方
	require.Nil(t, selectByPrefixAffinity(accounts(80, 90, 100, 100), group, "prefix-a", 80))

	// 不同前缀分散到不同账号
	picked := map[int64]bool{}
	for _, prefix := range []string{"p0", "p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9"} {
		picked[selectByPrefixAffinity(idle, group, prefix, 80).account.ID] = true
	}
	require.Greater(t, len(picked), 1)
}

func TestSelectByPrefixAffinity_BoundedLoad(t *testing.T) {
	const group = int64(7)
	accounts := func(rates ...int) []accountWithLoad {
		out := make([]accountWithLoad, len(rates))
		for i, rate := range rates {
			out[i] = accountWithLoad{
				account:  &Account{ID: int64(i + 1), Concurrency: 5},
				loadInfo: &AccountLoadInfo{AccountID: int64(i + 1), LoadRate: rate},
			}
		}
		return out
	}
	first := selectByPrefixAffinity(accounts(0, 0, 0, 0), group, "prefix-a", 80).account.ID

	// 首选账号比其他账号多一个请求：仍在上限内
	oneAhead := accounts(0, 0, 0, 0)
	oneAhead[first-1].loadInfo.LoadRate = 20
	require.Equal(t, first, selectByPrefixAffinity(oneAhead, group, "prefix-a", 80).account.ID)

	// 首选账号比其他账号多两个请求：超过 bounded-load 上限，顺延到下一个账号
	twoAhead := accounts(0, 0, 0, 0)
	twoAhead[first-1].loadInfo.LoadRate = 40
	next := selectByPrefixAffinity(twoAhead, group, "prefix-a", 80)
	require.NotNil(t, next)
	require.NotEqual(t, first, next.account.ID)

	// 其他账号同样繁忙时，首选账号不受影响
	allBusy := accounts(40, 40, 40, 40)
	require.Equal(t, first, selectByPrefixAffinity(allBusy, group, "prefix-a", 80).account.ID)
}

func TestSelectByPrefixAffinity_GroupsRankIndependently(t *testing.T) {
	shared := make([]accountWithLoad, 8)
	for i := range shared {
		shared[i] = accountWithLoad{
			account:  &Account{ID: int64(i + 1), Concurrency: 5},
			loadInfo: &AccountLoadInfo{AccountID: int64(i + 1)},
		}
	}
	// 共用同一批账号的分组，同一前缀的首选账号各自独立
	picked := map[int64]bool{}
	for group := int64(1); group <= 10; group++ {
		picked[selectByPrefixAffinity(shared, group, "prefix-a", 80).account.ID] = true
	}
	require.Greater(t, len(picked), 1)
}

func TestSelectAccountWithLoadAwareness_PrefixAffinity(t *testing.T) {
	newService := func(loads map[int64]int) *GatewayService {
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5},
				{ID: 2, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5},
				{ID: 3, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}
		loadMap := map[int64]*AccountLoadInfo{}
		for id, rate := range loads {
			loadMap[id] = &AccountLoadInfo{AccountID: id, LoadRate: rate}
		}
		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = true
		cfg.Gateway.Scheduling.PrefixAffinityEnabled = true
		cfg.Gateway.Scheduling.PrefixAffinityMaxLoadRate = 80
		return &GatewayService{
			accountRepo:        repo,
			cache:              &mockGatewayCacheForPlatform{},
			cfg:                cfg,
			concurrencyService: NewConcurrencyService(&mockConcurrencyCache{loadMap: loadMap}),
		}
	}
	parsed := mustParsePrefixAffinityRequest(t, `{"system":[{"type":"text","text":"shared","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`)
	const model = "claude-3-5-sonnet-20241022"
	prefixKey := SharedPrefixKey(parsed, model)

	// 期望账号：rendezvous 排名最前的账号
	all := []accountWithLoad{
		{account: &Account{ID: 1}, loadInfo: &AccountLoadInfo{}},
		{account: &Account{ID: 2}, loadInfo: &AccountLoadInfo{}},
		{account: &Account{ID: 3}, loadInfo: &AccountLoadInfo{}},
	}
	want := selectByPrefixAffinity(all, 0, prefixKey, 80).account.ID

	t.Run("新会话选到前缀对应的账号而不是负载最低的账号", func(t *testing.T) {
		loads := map[int64]int{1: 0, 2: 0, 3: 0}
		loads[want] = 20
		svc := newService(loads)
		ctx := svc.WithSharedPrefixAffinity(context.Background(), parsed, model)
		for _, session := range []string{"s1", "s2", "s3"} {
			result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, session, model, nil, "", 0)
			require.NoError(t, err)
			require.NotNil(t, result.Account)
			require.Equal(t, want, result.Account.ID)
		}
	})

	t.Run("前缀账号达到负载上限时按负载选号", func(t *testing.T) {
		loads := map[int64]int{1: 85, 2: 85, 3: 85}
		var lowest int64
		for id := range loads {
			if id != want {
				lowest = id
				break
			}
		}
		loads[lowest] = 81
		svc := newService(loads)
		ctx := svc.WithSharedPrefixAffinity(context.Background(), parsed, model)
		result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "", model, nil, "", 0)
		require.NoError(t, err)
		require.Equal(t, lowest, result.Account.ID)
	})

	t.Run("Gemini 按 OAuth 优先选号，不走前缀亲和", func(t *testing.T) {
		loads := map[int64]int{1: 0, 2: 0, 3: 0}
		loads[want] = 20
		svc := newService(loads)
		for i := range svc.accountRepo.(*mockAccountRepoForPlatform).accounts {
			svc.accountRepo.(*mockAccountRepoForPlatform).accounts[i].Platform = PlatformGemini
		}
		ctx := context.WithValue(svc.WithSharedPrefixAffinity(context.Background(), parsed, model), ctxkey.ForcePlatform, PlatformGemini)
		result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "", model, nil, "", 0)
		require.NoError(t, err)
		require.NotEqual(t, want, result.Account.ID)
	})

	t.Run("命中粘性绑定前不计算前缀 key", func(t *testing.T) {
		svc := newService(nil)
		ctx := svc.WithSharedPrefixAffinity(context.Background(), parsed, model)
		req := ctx.Value(prefixAffinityKeyCtxKey{}).(*prefixAffinityRequest)
		require.Empty(t, req.key)
		require.Equal(t, prefixKey, prefixAffinityKeyFromContext(ctx))
	})

	t.Run("开关关闭时不注入前缀 key", func(t *testing.T) {
		svc := newService(nil)
		svc.cfg.Gateway.Scheduling.PrefixAffinityEnabled = false
		ctx := svc.WithSharedPrefixAffinity(context.Background(), parsed, model)
		require.Empty(t, prefixAffinityKeyFromContext(ctx))
	})
}
