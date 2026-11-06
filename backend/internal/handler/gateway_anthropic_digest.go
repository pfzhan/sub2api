package handler

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// anthropicDigestSession 是按消息摘要链识别出的 Messages 会话。
//
// 不带 metadata 会话 ID 的客户端，会话 hash 取自缓存断点所在的消息或全部消息，
// 随对话增长逐轮变化：粘性绑定每轮都落空，多账号分组里每轮都可能换账号，上游
// prompt 缓存随之失效。按摘要链做最长前缀匹配找回同一会话后，改用按会话稳定的
// sessionKey，交给原有的粘性绑定选回同一账号。
type anthropicDigestSession struct {
	groupID      int64
	prefixHash   string
	chain        string
	uuid         string
	matchedChain string
	sessionKey   string
}

// resolveAnthropicDigestSession 查找请求所属的摘要会话，找不到时新建。请求
// 没有可用的摘要链时返回 nil。
func (h *GatewayHandler) resolveAnthropicDigestSession(c *gin.Context, groupID *int64, userID, apiKeyID int64, platform, model string, parsed *service.ParsedRequest) *anthropicDigestSession {
	chain := service.BuildAnthropicDigestChain(parsed)
	if chain == "" {
		return nil
	}
	d := &anthropicDigestSession{
		groupID:    derefGroupID(groupID),
		prefixHash: service.GenerateGeminiPrefixHash(userID, apiKeyID, ip.GetClientIP(c), c.GetHeader("User-Agent"), platform, model),
		chain:      chain,
	}
	if foundUUID, _, matched, found := h.gatewayService.FindAnthropicSession(c.Request.Context(), d.groupID, d.prefixHash, chain); found {
		d.uuid, d.matchedChain = foundUUID, matched
	} else {
		d.uuid = uuid.New().String()
	}
	d.sessionKey = service.GenerateAnthropicDigestSessionKey(d.prefixHash, d.uuid)
	return d
}

// save 在请求成功后记录本轮的摘要链，供下一轮匹配。
func (d *anthropicDigestSession) save(ctx context.Context, gatewayService *service.GatewayService, accountID int64) error {
	if d == nil {
		return nil
	}
	return gatewayService.SaveAnthropicSession(ctx, d.groupID, d.prefixHash, d.chain, d.uuid, accountID, d.matchedChain)
}
