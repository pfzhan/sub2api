//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/stretchr/testify/require"
)

// kiroRefreshFailingExecutor 用真实的 KiroTokenRefresher 判断是否需要刷新，刷新调用固定返回 err。
type kiroRefreshFailingExecutor struct {
	*KiroTokenRefresher
	err error
}

func (e *kiroRefreshFailingExecutor) Refresh(context.Context, *Account) (map[string]any, error) {
	return nil, e.err
}

// 请求路径上 token 已过期且刷新失败：瞬时故障只冷却并换号，不把凭据标成失效；
// 返回给调用方的是泛化文案，不带刷新端点或上游错误体。
func TestForwardKiro_TransientRefreshFailureCoolsDownWithoutMarkingCredentialsBroken(t *testing.T) {
	account := newKiroAccountForTest()
	account.Credentials["expires_at"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	account.Credentials[kiro.CredMachineID] = "machine-1"

	repo := &rateLimitAccountRepoStub{}
	repo.accountsByID = map[int64]*Account{account.ID: account}
	upstream := &kiroHTTPUpstreamStub{resp: kiroHelloResponse()}
	svc := newKiroGatewayForTest(upstream, repo)
	executor := &kiroRefreshFailingExecutor{
		KiroTokenRefresher: &KiroTokenRefresher{},
		err:                &kiro.HTTPError{Op: "refresh", StatusCode: http.StatusServiceUnavailable, Body: []byte(`{"message":"try later"}`)},
	}
	svc.kiroTokenProvider = NewKiroTokenProvider(repo, nil, NewOAuthRefreshAPI(repo, nil), executor)

	gatewayCtx, rec := newKiroCompatContext("/v1/messages")
	parsed, err := ParseGatewayRequest(NewRequestBodyRef([]byte(
		`{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)), PlatformAnthropic)
	require.NoError(t, err)
	_, err = svc.Forward(context.Background(), gatewayCtx, account, parsed)

	var failover *UpstreamFailoverError
	require.True(t, errors.As(err, &failover), "credential failure must switch accounts")
	require.Equal(t, kiroCredentialUnavailableReason, failover.Reason)
	require.Equal(t, http.StatusBadGateway, failover.ClientStatusCode)
	require.NotContains(t, failover.ClientMessage, "try later")
	require.Nil(t, upstream.lastReq, "no generate request is sent without a usable token")
	require.Zero(t, rec.Body.Len())

	require.Equal(t, 1, repo.tempCalls, "a transient refresh failure only cools the account down")
	require.Zero(t, repo.setErrorCalls, "only invalid_grant may mark the credentials broken")
}
