package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// KiroOAuthHandler 处理 Kiro 账号的凭据导入与 Builder ID 登录。两种方式都只
// 返回 credentials，账号由前端通过通用的创建账号接口创建。
type KiroOAuthHandler struct {
	kiroOAuthService *service.KiroOAuthService
}

func NewKiroOAuthHandler(kiroOAuthService *service.KiroOAuthService) *KiroOAuthHandler {
	return &KiroOAuthHandler{kiroOAuthService: kiroOAuthService}
}

type KiroImportCredentialsRequest struct {
	Credentials string `json:"credentials" binding:"required"`
	Region      string `json:"region"`
	ProxyID     *int64 `json:"proxy_id"`
}

// ImportCredentials 校验导入的凭据并返回可写入账号的 credentials。
// POST /api/v1/admin/kiro/oauth/import
func (h *KiroOAuthHandler) ImportCredentials(c *gin.Context) {
	var req KiroImportCredentialsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	credentials, err := h.kiroOAuthService.ImportCredentials(c.Request.Context(), service.KiroImportInput{
		Raw:     req.Credentials,
		ProxyID: req.ProxyID,
		Region:  req.Region,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"credentials": credentials})
}

type KiroDeviceAuthRequest struct {
	Region  string `json:"region"`
	ProxyID *int64 `json:"proxy_id"`
}

// StartDeviceAuth 发起 Builder ID 设备码登录。
// POST /api/v1/admin/kiro/oauth/device-auth
func (h *KiroOAuthHandler) StartDeviceAuth(c *gin.Context) {
	var req KiroDeviceAuthRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	result, err := h.kiroOAuthService.StartBuilderIDLogin(c.Request.Context(), req.ProxyID, req.Region)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

type KiroDevicePollRequest struct {
	SessionID string `json:"session_id" binding:"required"`
}

// PollDeviceAuth 查询 Builder ID 登录结果。
// POST /api/v1/admin/kiro/oauth/device-poll
func (h *KiroOAuthHandler) PollDeviceAuth(c *gin.Context) {
	var req KiroDevicePollRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	result, err := h.kiroOAuthService.PollBuilderIDLogin(c.Request.Context(), req.SessionID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}
