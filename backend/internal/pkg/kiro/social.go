package kiro

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Kiro social 登录（Google / GitHub）由 Kiro 自己的认证服务签发凭据，走
// PKCE 授权码流程。回调地址是 Kiro IDE 注册的 kiro:// 协议，浏览器无法把
// 授权码送回服务端，需要管理员从地址栏复制回调地址粘贴回来。
const (
	socialAuthBaseURLTemplate = "https://prod.%s.auth.desktop.kiro.dev"

	// SocialRedirectURI 是 Kiro IDE 的登录回调地址。
	SocialRedirectURI = "kiro://kiro.kiroAgent/authenticate-success"
)

// social 登录的身份提供方，取值与 Kiro 认证服务的 idp 参数一致。
const (
	SocialProviderGoogle = "Google"
	SocialProviderGitHub = "Github"
)

// NormalizeSocialProvider 把 google / github 等写法转换为 idp 参数取值，
// 不支持的提供方返回空串。
func NormalizeSocialProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "google":
		return SocialProviderGoogle
	case "github":
		return SocialProviderGitHub
	}
	return ""
}

// SocialLogin 是一次 social 登录的 PKCE 参数与授权地址。
type SocialLogin struct {
	Provider     string
	AuthURL      string
	State        string
	CodeVerifier string
}

// NewSocialLogin 生成 PKCE 参数并拼出授权地址。
func NewSocialLogin(provider string) (*SocialLogin, error) {
	idp := NormalizeSocialProvider(provider)
	if idp == "" {
		return nil, fmt.Errorf("unsupported social provider %q", provider)
	}
	verifier, err := randomURLSafe(32)
	if err != nil {
		return nil, err
	}
	state, err := randomURLSafe(16)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(verifier))
	query := url.Values{}
	query.Set("idp", idp)
	query.Set("redirect_uri", SocialRedirectURI)
	query.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
	query.Set("code_challenge_method", "S256")
	query.Set("state", state)
	return &SocialLogin{
		Provider:     idp,
		AuthURL:      fmt.Sprintf(socialAuthBaseURLTemplate, DefaultRegion) + "/login?" + query.Encode(),
		State:        state,
		CodeVerifier: verifier,
	}, nil
}

// ErrSocialStateMismatch 表示回调地址里的 state 与发起登录时不一致。
var ErrSocialStateMismatch = errors.New("state mismatch")

// ParseSocialCallback 从管理员粘贴的内容中取出授权码。内容可以是完整的
// 回调地址，也可以只是授权码本身。回调地址带有 state 时必须与 expectedState
// 一致；回调携带 error 参数时返回该错误。
func ParseSocialCallback(input, expectedState string) (string, error) {
	text := strings.TrimSpace(input)
	if text == "" {
		return "", errors.New("callback URL is empty")
	}
	if !strings.Contains(text, "?") && !strings.Contains(text, "=") {
		return text, nil
	}
	rawQuery := text
	if i := strings.Index(text, "?"); i >= 0 {
		rawQuery = text[i+1:]
	}
	if i := strings.Index(rawQuery, "#"); i >= 0 {
		rawQuery = rawQuery[:i]
	}
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", fmt.Errorf("invalid callback URL: %w", err)
	}
	if e := strings.TrimSpace(query.Get("error")); e != "" {
		if desc := strings.TrimSpace(query.Get("error_description")); desc != "" {
			e += ": " + desc
		}
		return "", fmt.Errorf("authorization failed: %s", e)
	}
	if state := query.Get("state"); state != "" && state != expectedState {
		return "", ErrSocialStateMismatch
	}
	code := strings.TrimSpace(query.Get("code"))
	if code == "" {
		return "", errors.New("callback URL has no code")
	}
	return code, nil
}

// ExchangeSocialCode 用授权码换取 social 凭据。
func ExchangeSocialCode(ctx context.Context, doer Doer, login *SocialLogin, code string) (*Credentials, error) {
	if login == nil {
		return nil, errors.New("social login is required")
	}
	var resp struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ProfileARN   string `json:"profileArn"`
		ExpiresIn    int64  `json:"expiresIn"`
	}
	body := map[string]any{
		"code":          code,
		"code_verifier": login.CodeVerifier,
		"redirect_uri":  SocialRedirectURI,
	}
	tokenURL := fmt.Sprintf(socialAuthBaseURLTemplate, DefaultRegion) + "/oauth/token"
	if err := postJSON(ctx, doer, "social token", tokenURL, body, nil, &resp); err != nil {
		return nil, err
	}
	if resp.AccessToken == "" || resp.RefreshToken == "" {
		return nil, errors.New("kiro social token: response has no tokens")
	}
	creds := &Credentials{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		ExpiresAt:    expiresAt(resp.ExpiresIn),
		AuthMethod:   AuthMethodSocial,
		Provider:     login.Provider,
		Region:       DefaultRegion,
		ProfileARN:   resp.ProfileARN,
	}
	creds.EnsureMachineID()
	return creds, nil
}

func randomURLSafe(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
