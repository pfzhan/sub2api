package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type passthroughRuleRepoStub struct {
	service.ErrorPassthroughRepository
	rules []*model.ErrorPassthroughRule
}

func (r *passthroughRuleRepoStub) List(context.Context) ([]*model.ErrorPassthroughRule, error) {
	return r.rules, nil
}

// 换号耗尽后命中 passthrough_body 规则：上游消息带给调用方前要脱敏 token 类查询参数。
func TestGatewayHandleFailoverExhausted_PassthroughBodyRedactsTokenQueryParams(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	rule := &model.ErrorPassthroughRule{
		ID: 1, Name: "relay-503", Enabled: true, Priority: 1,
		ErrorCodes: []int{http.StatusServiceUnavailable}, Keywords: []string{"unavailable"},
		MatchMode: model.MatchModeAll, PassthroughCode: true, PassthroughBody: true,
	}
	h := &GatewayHandler{errorPassthroughService: service.NewErrorPassthroughService(&passthroughRuleRepoStub{rules: []*model.ErrorPassthroughRule{rule}}, nil)}
	failoverErr := &service.UpstreamFailoverError{
		StatusCode:   http.StatusServiceUnavailable,
		ResponseBody: []byte(`{"error":{"message":"unavailable: https://relay.example/v1?access_token=at-secret&refresh_token=rt-secret"}}`),
	}

	h.handleFailoverExhausted(c, failoverErr, service.PlatformAnthropic, false)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	body := rec.Body.String()
	require.NotContains(t, body, "at-secret")
	require.NotContains(t, body, "rt-secret")
	require.Contains(t, body, "access_token=***")
	require.Contains(t, body, "unavailable: https://relay.example/v1")
}
