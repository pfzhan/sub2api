package kiro

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Doer 执行 HTTP 请求。服务层用它接入账号代理与连接池。
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// DoerFunc 让普通函数实现 Doer。
type DoerFunc func(req *http.Request) (*http.Response, error)

func (f DoerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

const (
	DefaultRegion = "us-east-1"

	// BuilderIDStartURL 是 AWS Builder ID 的 start URL。
	BuilderIDStartURL = "https://view.awsapps.com/start"

	socialRefreshURLTemplate = "https://prod.%s.auth.desktop.kiro.dev/refreshToken"
	oidcURLTemplate          = "https://oidc.%s.amazonaws.com"
	listProfilesURLTemplate  = "https://codewhisperer.%s.amazonaws.com/ListAvailableProfiles"

	// defaultTokenLifetime 是上游未返回 expiresIn 时采用的有效期。
	defaultTokenLifetime = time.Hour

	maxAuthResponseBytes = 1 << 20
)

// oidcScopes 是注册 OIDC 客户端时申请的权限。
var oidcScopes = []string{
	"codewhisperer:completions",
	"codewhisperer:analysis",
	"codewhisperer:conversations",
}

// HTTPError 是上游返回的非 2xx 响应。
type HTTPError struct {
	Op         string
	StatusCode int
	Body       []byte
}

func (e *HTTPError) Error() string {
	body := strings.TrimSpace(string(e.Body))
	if len(body) > 512 {
		body = body[:512]
	}
	return fmt.Sprintf("kiro %s: status %d: %s", e.Op, e.StatusCode, body)
}

// ErrInvalidGrant 表示 refresh token 已失效，需要重新登录。错误文本包含
// invalid_grant，便于通用刷新流程按不可重试处理。
var ErrInvalidGrant = errors.New("invalid_grant")

// Refresh 用 refresh token 换取新的 access token，返回更新后的凭据。
// social 凭据的刷新响应会带 profileArn；idc 刷新响应不带，需要另行调用
// DiscoverProfileARN。
func Refresh(ctx context.Context, doer Doer, creds Credentials) (Credentials, error) {
	if err := creds.Validate(); err != nil {
		return creds, err
	}
	var (
		url  string
		body map[string]any
	)
	if creds.IsSocial() {
		url = fmt.Sprintf(socialRefreshURLTemplate, creds.APIRegion())
		body = map[string]any{"refreshToken": creds.RefreshToken}
	} else {
		url = fmt.Sprintf(oidcURLTemplate, creds.OIDCRegion()) + "/token"
		body = map[string]any{
			"clientId":     creds.ClientID,
			"clientSecret": creds.ClientSecret,
			"grantType":    "refresh_token",
			"refreshToken": creds.RefreshToken,
		}
	}

	var resp struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ProfileARN   string `json:"profileArn"`
		ExpiresIn    int64  `json:"expiresIn"`
	}
	if err := postJSON(ctx, doer, "refresh", url, body, nil, &resp); err != nil {
		var httpErr *HTTPError
		if creds.IsSocial() && errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusUnauthorized {
			return creds, fmt.Errorf("%w: %v", ErrInvalidGrant, err)
		}
		return creds, err
	}
	if strings.TrimSpace(resp.AccessToken) == "" {
		return creds, errors.New("kiro refresh: response has no accessToken")
	}

	updated := creds
	updated.AccessToken = resp.AccessToken
	if resp.RefreshToken != "" {
		updated.RefreshToken = resp.RefreshToken
	}
	if resp.ProfileARN != "" {
		updated.ProfileARN = resp.ProfileARN
	}
	updated.ExpiresAt = expiresAt(resp.ExpiresIn)
	return updated, nil
}

// DiscoverProfileARN 查询 idc 凭据可用的 CodeWhisperer profile，优先返回与
// API 区域一致的那个。没有可用 profile 时返回空串。
func DiscoverProfileARN(ctx context.Context, doer Doer, creds Credentials) (string, error) {
	url := fmt.Sprintf(listProfilesURLTemplate, creds.APIRegion())
	headers := ideHeaders(creds)
	headers.Set("Authorization", "Bearer "+creds.AccessToken)
	var resp struct {
		Profiles []struct {
			ARN string `json:"arn"`
		} `json:"profiles"`
	}
	if err := postJSON(ctx, doer, "list profiles", url, map[string]any{"maxResults": 10}, headers, &resp); err != nil {
		return "", err
	}
	regionMarker := ":" + creds.APIRegion() + ":"
	first := ""
	for _, p := range resp.Profiles {
		if p.ARN == "" {
			continue
		}
		if strings.Contains(p.ARN, regionMarker) {
			return p.ARN, nil
		}
		if first == "" {
			first = p.ARN
		}
	}
	return first, nil
}

// ClientRegistration 是 OIDC 公共客户端的注册结果。
type ClientRegistration struct {
	ClientID              string
	ClientSecret          string
	ClientSecretExpiresAt time.Time
}

// RegisterClient 在 AWS SSO OIDC 注册一个公共客户端。
func RegisterClient(ctx context.Context, doer Doer, region string) (*ClientRegistration, error) {
	var resp struct {
		ClientID              string `json:"clientId"`
		ClientSecret          string `json:"clientSecret"`
		ClientSecretExpiresAt int64  `json:"clientSecretExpiresAt"`
	}
	body := map[string]any{
		"clientName": "Kiro IDE",
		"clientType": "public",
		"scopes":     oidcScopes,
	}
	if err := postJSON(ctx, doer, "register client", fmt.Sprintf(oidcURLTemplate, region)+"/client/register", body, nil, &resp); err != nil {
		return nil, err
	}
	if resp.ClientID == "" || resp.ClientSecret == "" {
		return nil, errors.New("kiro register client: response has no client credentials")
	}
	return &ClientRegistration{
		ClientID:              resp.ClientID,
		ClientSecret:          resp.ClientSecret,
		ClientSecretExpiresAt: unixTime(resp.ClientSecretExpiresAt),
	}, nil
}

// DeviceAuthorization 是设备码授权的开始结果。
type DeviceAuthorization struct {
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresIn               int
	Interval                int
}

// StartDeviceAuthorization 发起设备码授权。
func StartDeviceAuthorization(ctx context.Context, doer Doer, region string, client *ClientRegistration, startURL string) (*DeviceAuthorization, error) {
	if client == nil {
		return nil, errors.New("client registration is required")
	}
	if strings.TrimSpace(startURL) == "" {
		startURL = BuilderIDStartURL
	}
	var resp struct {
		DeviceCode              string `json:"deviceCode"`
		UserCode                string `json:"userCode"`
		VerificationURI         string `json:"verificationUri"`
		VerificationURIComplete string `json:"verificationUriComplete"`
		ExpiresIn               int    `json:"expiresIn"`
		Interval                int    `json:"interval"`
	}
	body := map[string]any{
		"clientId":     client.ClientID,
		"clientSecret": client.ClientSecret,
		"startUrl":     startURL,
	}
	if err := postJSON(ctx, doer, "device authorization", fmt.Sprintf(oidcURLTemplate, region)+"/device_authorization", body, nil, &resp); err != nil {
		return nil, err
	}
	if resp.DeviceCode == "" {
		return nil, errors.New("kiro device authorization: response has no deviceCode")
	}
	if resp.Interval <= 0 {
		resp.Interval = 5
	}
	return &DeviceAuthorization{
		DeviceCode:              resp.DeviceCode,
		UserCode:                resp.UserCode,
		VerificationURI:         resp.VerificationURI,
		VerificationURIComplete: resp.VerificationURIComplete,
		ExpiresIn:               resp.ExpiresIn,
		Interval:                resp.Interval,
	}, nil
}

// 设备码轮询期间的中间状态。
var (
	ErrAuthorizationPending = errors.New("authorization_pending")
	ErrSlowDown             = errors.New("slow_down")
)

// PollDeviceToken 查询一次设备码授权结果。用户尚未完成授权时返回
// ErrAuthorizationPending 或 ErrSlowDown。
func PollDeviceToken(ctx context.Context, doer Doer, region string, client *ClientRegistration, deviceCode string) (*Credentials, error) {
	if client == nil {
		return nil, errors.New("client registration is required")
	}
	var resp struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
	}
	body := map[string]any{
		"clientId":     client.ClientID,
		"clientSecret": client.ClientSecret,
		"deviceCode":   deviceCode,
		"grantType":    "urn:ietf:params:oauth:grant-type:device_code",
	}
	err := postJSON(ctx, doer, "device token", fmt.Sprintf(oidcURLTemplate, region)+"/token", body, nil, &resp)
	if err != nil {
		var httpErr *HTTPError
		if errors.As(err, &httpErr) {
			var oauthErr struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(httpErr.Body, &oauthErr)
			switch oauthErr.Error {
			case "authorization_pending":
				return nil, ErrAuthorizationPending
			case "slow_down":
				return nil, ErrSlowDown
			}
		}
		return nil, err
	}
	if resp.AccessToken == "" || resp.RefreshToken == "" {
		return nil, errors.New("kiro device token: response has no tokens")
	}
	return &Credentials{
		AccessToken:           resp.AccessToken,
		RefreshToken:          resp.RefreshToken,
		ExpiresAt:             expiresAt(resp.ExpiresIn),
		AuthMethod:            AuthMethodIdC,
		ClientID:              client.ClientID,
		ClientSecret:          client.ClientSecret,
		ClientSecretExpiresAt: client.ClientSecretExpiresAt,
		Region:                region,
		SSORegion:             region,
	}, nil
}

func expiresAt(expiresIn int64) time.Time {
	lifetime := defaultTokenLifetime
	if expiresIn > 0 {
		lifetime = time.Duration(expiresIn) * time.Second
	}
	return time.Now().Add(lifetime).UTC().Truncate(time.Second)
}

func postJSON(ctx context.Context, doer Doer, op, url string, body any, headers http.Header, out any) error {
	if doer == nil {
		return errors.New("kiro: http client is not configured")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("kiro %s: encode request: %w", op, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("kiro %s: build request: %w", op, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for key, values := range headers {
		for _, v := range values {
			req.Header.Add(key, v)
		}
	}
	resp, err := doer.Do(req)
	if err != nil {
		return fmt.Errorf("kiro %s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAuthResponseBytes))
	if err != nil {
		return fmt.Errorf("kiro %s: read response: %w", op, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{Op: op, StatusCode: resp.StatusCode, Body: data}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("kiro %s: decode response: %w", op, err)
	}
	return nil
}
