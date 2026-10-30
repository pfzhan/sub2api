package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
)

const (
	kiroAuthHTTPTimeout = 30 * time.Second
	// kiroSessionGrace 是登录会话过期后继续保留的时间，便于返回明确的过期错误。
	kiroSessionGrace = time.Minute
	// kiroSocialSessionTTL 是 social 登录从发起到粘贴回调地址的时限。
	kiroSocialSessionTTL = 10 * time.Minute
	kiroSlowDownStep     = 5
)

// KiroOAuthService 处理 Kiro 账号的凭据导入、登录（Builder ID、IAM Identity
// Center、Google / GitHub）与 token 刷新。
type KiroOAuthService struct {
	proxyRepo ProxyRepository

	mu       sync.Mutex
	sessions map[string]*kiroLoginSession

	// newDoer 创建上游 HTTP 客户端，测试中可替换。
	newDoer func(proxyURL string) (kiro.Doer, error)
	now     func() time.Time
}

// kiroLoginSession 是进行中的登录。设备码登录使用 client 及之后的字段，
// social 登录只使用 social。
type kiroLoginSession struct {
	proxyURL  string
	expiresAt time.Time

	client     *kiro.ClientRegistration
	deviceCode string
	region     string
	ssoRegion  string
	startURL   string
	interval   int

	social *kiro.SocialLogin
}

func NewKiroOAuthService(proxyRepo ProxyRepository) *KiroOAuthService {
	return &KiroOAuthService{
		proxyRepo: proxyRepo,
		sessions:  make(map[string]*kiroLoginSession),
		newDoer:   newKiroHTTPDoer,
		now:       time.Now,
	}
}

func newKiroHTTPDoer(proxyURL string) (kiro.Doer, error) {
	client, err := httpclient.GetClient(httpclient.Options{
		ProxyURL:           proxyURL,
		Timeout:            kiroAuthHTTPTimeout,
		ValidateResolvedIP: true,
	})
	if err != nil {
		return nil, err
	}
	return client, nil
}

func (s *KiroOAuthService) resolveProxyURL(ctx context.Context, proxyID *int64) string {
	if proxyID == nil || s.proxyRepo == nil {
		return ""
	}
	proxy, err := s.proxyRepo.GetByID(ctx, *proxyID)
	if err != nil || proxy == nil {
		return ""
	}
	return proxy.URL()
}

func (s *KiroOAuthService) doer(ctx context.Context, proxyID *int64) (kiro.Doer, error) {
	return s.newDoer(s.resolveProxyURL(ctx, proxyID))
}

// KiroImportInput 是导入凭据的参数。
type KiroImportInput struct {
	// Raw 是 refresh token、kiro-auth-token.json 内容或导出的 JSON。
	Raw     string
	ProxyID *int64
	// Region 在凭据未注明区域时使用。
	Region string
}

// ImportCredentials 解析并验证导入的凭据：执行一次刷新取得 access token，
// idc 凭据没有 profile 时尝试查询。返回可直接写入账号的 credentials。
func (s *KiroOAuthService) ImportCredentials(ctx context.Context, input KiroImportInput) (map[string]any, error) {
	creds, err := kiro.ParseImport(input.Raw)
	if err != nil {
		return nil, infraerrors.BadRequest("KIRO_IMPORT_INVALID", "invalid kiro credentials: "+err.Error())
	}
	if region := strings.TrimSpace(input.Region); region != "" && creds.Region == "" {
		creds.Region = region
		if creds.AuthMethod == kiro.AuthMethodIdC && creds.SSORegion == "" {
			creds.SSORegion = region
		}
	}
	doer, err := s.doer(ctx, input.ProxyID)
	if err != nil {
		return nil, fmt.Errorf("create kiro http client: %w", err)
	}
	refreshed, err := s.refresh(ctx, doer, creds)
	if err != nil {
		return nil, infraerrors.BadRequest("KIRO_IMPORT_REFRESH_FAILED", "kiro token refresh failed: "+err.Error())
	}
	return refreshed.ToMap(), nil
}

// RefreshAccountToken 刷新账号 token，返回合并后的 credentials。
func (s *KiroOAuthService) RefreshAccountToken(ctx context.Context, account *Account) (map[string]any, error) {
	if account == nil || account.Platform != PlatformKiro || account.Type != AccountTypeOAuth {
		return nil, errors.New("not a kiro oauth account")
	}
	doer, err := s.doer(ctx, account.ProxyID)
	if err != nil {
		return nil, fmt.Errorf("create kiro http client: %w", err)
	}
	refreshed, err := s.refresh(ctx, doer, kiro.CredentialsFromMap(account.Credentials))
	if err != nil {
		return nil, err
	}
	return MergeCredentials(account.Credentials, refreshed.ToMap()), nil
}

// refresh 刷新 token，并为没有 profile 的 idc 凭据查询 profile。查询失败不影响
// 刷新结果，没有 profile 的 Builder ID 凭据仍可走 codewhisperer 端点。
func (s *KiroOAuthService) refresh(ctx context.Context, doer kiro.Doer, creds kiro.Credentials) (kiro.Credentials, error) {
	refreshed, err := kiro.Refresh(ctx, doer, creds)
	if err != nil {
		return creds, err
	}
	s.fillProfileARN(ctx, doer, &refreshed)
	return refreshed, nil
}

func (s *KiroOAuthService) fillProfileARN(ctx context.Context, doer kiro.Doer, creds *kiro.Credentials) {
	if creds.IsSocial() || strings.TrimSpace(creds.ProfileARN) != "" {
		return
	}
	arn, err := kiro.DiscoverProfileARN(ctx, doer, *creds)
	if err != nil {
		slog.Warn("kiro_profile_discovery_failed", "error", err)
		return
	}
	creds.ProfileARN = arn
}

// KiroDeviceLoginInput 是发起设备码登录的参数。
type KiroDeviceLoginInput struct {
	ProxyID *int64
	// Region 是调用 Kiro 接口的区域，默认 us-east-1。
	Region string
	// SSORegion 是 IAM Identity Center 所在区域，默认与 Region 相同。
	SSORegion string
	// StartURL 为空时使用 Builder ID；填写组织的 start URL 时走 IAM Identity Center。
	StartURL string
}

// KiroDeviceAuthResult 是发起设备码登录的结果。
type KiroDeviceAuthResult struct {
	SessionID               string `json:"session_id"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// StartDeviceLogin 注册 OIDC 客户端并发起设备码授权，Builder ID 与 IAM
// Identity Center 共用这一流程，区别只在 start URL 和 SSO 区域。
func (s *KiroOAuthService) StartDeviceLogin(ctx context.Context, input KiroDeviceLoginInput) (*KiroDeviceAuthResult, error) {
	region := firstNonEmpty(input.Region, kiro.DefaultRegion)
	ssoRegion := firstNonEmpty(input.SSORegion, region)
	if !kiro.ValidRegion(region) || !kiro.ValidRegion(ssoRegion) {
		return nil, infraerrors.BadRequest("KIRO_INVALID_REGION", "invalid AWS region")
	}
	startURL := firstNonEmpty(input.StartURL, kiro.BuilderIDStartURL)
	if err := validateKiroStartURL(startURL); err != nil {
		return nil, err
	}

	proxyURL := s.resolveProxyURL(ctx, input.ProxyID)
	doer, err := s.newDoer(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("create kiro http client: %w", err)
	}
	client, err := kiro.RegisterClient(ctx, doer, ssoRegion)
	if err != nil {
		return nil, infraerrors.ServiceUnavailable("KIRO_REGISTER_CLIENT_FAILED", "register kiro oidc client failed: "+err.Error())
	}
	auth, err := kiro.StartDeviceAuthorization(ctx, doer, ssoRegion, client, startURL)
	if err != nil {
		return nil, infraerrors.ServiceUnavailable("KIRO_DEVICE_AUTH_FAILED", "start kiro device authorization failed: "+err.Error())
	}
	sessionID, err := newKiroSessionID()
	if err != nil {
		return nil, err
	}
	expiresIn := auth.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 600
	}
	s.putSession(sessionID, &kiroLoginSession{
		proxyURL:   proxyURL,
		expiresAt:  s.now().Add(time.Duration(expiresIn) * time.Second),
		client:     client,
		deviceCode: auth.DeviceCode,
		region:     region,
		ssoRegion:  ssoRegion,
		startURL:   startURL,
		interval:   auth.Interval,
	})
	return &KiroDeviceAuthResult{
		SessionID:               sessionID,
		UserCode:                auth.UserCode,
		VerificationURI:         auth.VerificationURI,
		VerificationURIComplete: auth.VerificationURIComplete,
		ExpiresIn:               expiresIn,
		Interval:                auth.Interval,
	}, nil
}

func validateKiroStartURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return infraerrors.BadRequest("KIRO_INVALID_START_URL", "start URL must be an https URL")
	}
	return nil
}

// 设备码登录轮询状态。
const (
	KiroDeviceStatusPending = "pending"
	KiroDeviceStatusSuccess = "success"
)

// KiroDevicePollResult 是一次轮询的结果。Status 为 success 时 Credentials
// 可直接用于创建账号。
type KiroDevicePollResult struct {
	Status      string         `json:"status"`
	Interval    int            `json:"interval,omitempty"`
	Credentials map[string]any `json:"credentials,omitempty"`
}

// PollDeviceLogin 查询一次设备码授权结果。
func (s *KiroOAuthService) PollDeviceLogin(ctx context.Context, sessionID string) (*KiroDevicePollResult, error) {
	session, err := s.activeSession(sessionID)
	if err != nil {
		return nil, err
	}
	if session.client == nil {
		return nil, infraerrors.BadRequest("KIRO_SESSION_TYPE_MISMATCH", "kiro login session is not a device login")
	}
	doer, err := s.newDoer(session.proxyURL)
	if err != nil {
		return nil, fmt.Errorf("create kiro http client: %w", err)
	}
	creds, err := kiro.PollDeviceToken(ctx, doer, session.ssoRegion, session.client, session.deviceCode)
	switch {
	case errors.Is(err, kiro.ErrAuthorizationPending):
		return &KiroDevicePollResult{Status: KiroDeviceStatusPending, Interval: session.interval}, nil
	case errors.Is(err, kiro.ErrSlowDown):
		interval := s.slowDown(sessionID)
		return &KiroDevicePollResult{Status: KiroDeviceStatusPending, Interval: interval}, nil
	case err != nil:
		s.deleteSession(sessionID)
		return nil, infraerrors.BadRequest("KIRO_DEVICE_TOKEN_FAILED", "kiro device authorization failed: "+err.Error())
	}
	s.deleteSession(sessionID)

	creds.Region = session.region
	creds.StartURL = session.startURL
	creds.Provider = kiro.ProviderEnterprise
	if session.startURL == kiro.BuilderIDStartURL {
		creds.Provider = kiro.ProviderBuilderID
	}
	s.fillProfileARN(ctx, doer, creds)
	return &KiroDevicePollResult{Status: KiroDeviceStatusSuccess, Credentials: creds.ToMap()}, nil
}

// KiroSocialAuthResult 是发起 social 登录的结果。
type KiroSocialAuthResult struct {
	SessionID   string `json:"session_id"`
	AuthURL     string `json:"auth_url"`
	RedirectURI string `json:"redirect_uri"`
	ExpiresIn   int    `json:"expires_in"`
}

// StartSocialLogin 生成 Google / GitHub 登录地址。
func (s *KiroOAuthService) StartSocialLogin(ctx context.Context, proxyID *int64, provider string) (*KiroSocialAuthResult, error) {
	login, err := kiro.NewSocialLogin(provider)
	if err != nil {
		return nil, infraerrors.BadRequest("KIRO_INVALID_SOCIAL_PROVIDER", err.Error())
	}
	sessionID, err := newKiroSessionID()
	if err != nil {
		return nil, err
	}
	s.putSession(sessionID, &kiroLoginSession{
		proxyURL:  s.resolveProxyURL(ctx, proxyID),
		expiresAt: s.now().Add(kiroSocialSessionTTL),
		social:    login,
	})
	return &KiroSocialAuthResult{
		SessionID:   sessionID,
		AuthURL:     login.AuthURL,
		RedirectURI: kiro.SocialRedirectURI,
		ExpiresIn:   int(kiroSocialSessionTTL / time.Second),
	}, nil
}

// ExchangeSocialLogin 用管理员粘贴的回调地址换取凭据。会话在换取成功后
// 删除；回调地址填错时保留会话，便于重新粘贴。
func (s *KiroOAuthService) ExchangeSocialLogin(ctx context.Context, sessionID, callback string) (map[string]any, error) {
	session, err := s.activeSession(sessionID)
	if err != nil {
		return nil, err
	}
	if session.social == nil {
		return nil, infraerrors.BadRequest("KIRO_SESSION_TYPE_MISMATCH", "kiro login session is not a social login")
	}
	code, err := kiro.ParseSocialCallback(callback, session.social.State)
	if err != nil {
		return nil, infraerrors.BadRequest("KIRO_INVALID_CALLBACK", "invalid kiro callback: "+err.Error())
	}
	doer, err := s.newDoer(session.proxyURL)
	if err != nil {
		return nil, fmt.Errorf("create kiro http client: %w", err)
	}
	creds, err := kiro.ExchangeSocialCode(ctx, doer, session.social, code)
	if err != nil {
		// 授权码只能使用一次，换取失败后需要重新登录。
		s.deleteSession(sessionID)
		return nil, infraerrors.BadRequest("KIRO_SOCIAL_TOKEN_FAILED", "kiro social login failed: "+err.Error())
	}
	s.deleteSession(sessionID)
	return creds.ToMap(), nil
}

// activeSession 返回未过期的登录会话。
func (s *KiroOAuthService) activeSession(sessionID string) (kiroLoginSession, error) {
	session, ok := s.getSession(sessionID)
	if !ok {
		return session, infraerrors.NotFound("KIRO_SESSION_NOT_FOUND", "kiro login session not found or expired")
	}
	if s.now().After(session.expiresAt) {
		s.deleteSession(sessionID)
		return session, infraerrors.BadRequest("KIRO_SESSION_EXPIRED", "kiro login session expired, please start again")
	}
	return session, nil
}

func (s *KiroOAuthService) putSession(id string, session *kiroLoginSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for key, existing := range s.sessions {
		if now.After(existing.expiresAt.Add(kiroSessionGrace)) {
			delete(s.sessions, key)
		}
	}
	s.sessions[id] = session
}

func (s *KiroOAuthService) getSession(id string) (kiroLoginSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok {
		return kiroLoginSession{}, false
	}
	return *session, true
}

func (s *KiroOAuthService) deleteSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

func (s *KiroOAuthService) slowDown(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok {
		return kiroSlowDownStep
	}
	session.interval += kiroSlowDownStep
	return session.interval
}

func newKiroSessionID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
