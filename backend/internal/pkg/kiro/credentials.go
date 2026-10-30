package kiro

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 认证方式。Builder ID 与 IAM Identity Center 都是 AWS SSO OIDC 签发的凭据，
// 刷新方式相同，统一记为 idc；social 是 Kiro 自己的 Google/GitHub 登录。
const (
	AuthMethodSocial = "social"
	AuthMethodIdC    = "idc"
)

// 账号 credentials 中使用的键。
const (
	CredAccessToken           = "access_token"
	CredRefreshToken          = "refresh_token"
	CredExpiresAt             = "expires_at"
	CredAuthMethod            = "auth_method"
	CredProvider              = "provider"
	CredClientID              = "client_id"
	CredClientSecret          = "client_secret"
	CredClientSecretExpiresAt = "client_secret_expires_at"
	CredRegion                = "region"
	CredSSORegion             = "sso_region"
	CredProfileARN            = "profile_arn"
	CredStartURL              = "start_url"
)

// ProviderBuilderID 是 Builder ID 登录在 provider 字段里的取值，与 Kiro IDE 一致。
const ProviderBuilderID = "BuilderId"

// Credentials 是一个 Kiro 账号的认证信息。
type Credentials struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time

	AuthMethod string
	// Provider 记录登录来源（Google、Github、BuilderId、Enterprise），仅用于展示。
	Provider string

	// ClientID / ClientSecret 是 OIDC 客户端注册信息，idc 刷新时需要。
	ClientID              string
	ClientSecret          string
	ClientSecretExpiresAt time.Time

	// Region 是 q / codewhisperer API 所在区域；SSORegion 是 OIDC 刷新所在区域。
	Region     string
	SSORegion  string
	ProfileARN string
	StartURL   string
}

// IsSocial 报告凭据是否走 Kiro social 刷新。
func (c Credentials) IsSocial() bool {
	return c.AuthMethod == AuthMethodSocial
}

// APIRegion 返回调用生成接口使用的区域。
func (c Credentials) APIRegion() string {
	if r := strings.TrimSpace(c.Region); r != "" {
		return r
	}
	if r := strings.TrimSpace(c.SSORegion); r != "" {
		return r
	}
	return DefaultRegion
}

// OIDCRegion 返回 idc 刷新使用的区域。
func (c Credentials) OIDCRegion() string {
	if r := strings.TrimSpace(c.SSORegion); r != "" {
		return r
	}
	return c.APIRegion()
}

// IsBuilderID 报告凭据是否来自 Builder ID 登录。
func (c Credentials) IsBuilderID() bool {
	if c.AuthMethod != AuthMethodIdC {
		return false
	}
	if strings.EqualFold(c.Provider, ProviderBuilderID) {
		return true
	}
	return strings.TrimSpace(c.StartURL) == BuilderIDStartURL
}

// CredentialsFromMap 从账号 credentials 读取 Kiro 凭据。
func CredentialsFromMap(m map[string]any) Credentials {
	c := Credentials{
		AccessToken:  stringValue(m[CredAccessToken]),
		RefreshToken: stringValue(m[CredRefreshToken]),
		ExpiresAt:    timeValue(m[CredExpiresAt]),
		Provider:     stringValue(m[CredProvider]),
		ClientID:     stringValue(m[CredClientID]),
		ClientSecret: stringValue(m[CredClientSecret]),

		ClientSecretExpiresAt: timeValue(m[CredClientSecretExpiresAt]),

		Region:     stringValue(m[CredRegion]),
		SSORegion:  stringValue(m[CredSSORegion]),
		ProfileARN: stringValue(m[CredProfileARN]),
		StartURL:   stringValue(m[CredStartURL]),
	}
	c.AuthMethod = NormalizeAuthMethod(stringValue(m[CredAuthMethod]), c.ClientID != "" && c.ClientSecret != "")
	return c
}

// ToMap 输出写入账号 credentials 的字段，空值不输出。
func (c Credentials) ToMap() map[string]any {
	out := make(map[string]any, 12)
	put := func(key, value string) {
		if v := strings.TrimSpace(value); v != "" {
			out[key] = v
		}
	}
	put(CredAccessToken, c.AccessToken)
	put(CredRefreshToken, c.RefreshToken)
	if !c.ExpiresAt.IsZero() {
		out[CredExpiresAt] = c.ExpiresAt.UTC().Format(time.RFC3339)
	}
	put(CredAuthMethod, c.AuthMethod)
	put(CredProvider, c.Provider)
	put(CredClientID, c.ClientID)
	put(CredClientSecret, c.ClientSecret)
	if !c.ClientSecretExpiresAt.IsZero() {
		out[CredClientSecretExpiresAt] = c.ClientSecretExpiresAt.UTC().Format(time.RFC3339)
	}
	put(CredRegion, c.Region)
	put(CredSSORegion, c.SSORegion)
	put(CredProfileARN, c.ProfileARN)
	put(CredStartURL, c.StartURL)
	return out
}

// NormalizeAuthMethod 把各来源的写法归一为 social 或 idc。
// Kiro IDE 写 "social"/"IdC"，AIClient2API 写 "builder-id"；缺失时按有没有
// OIDC 客户端信息推断。
func NormalizeAuthMethod(method string, hasClientCredentials bool) string {
	switch normalizeMethodSpelling(method) {
	case AuthMethodSocial:
		return AuthMethodSocial
	case "idc", "builder-id", "builderid", "identity-center", "iam-identity-center", "enterprise":
		return AuthMethodIdC
	}
	if hasClientCredentials {
		return AuthMethodIdC
	}
	return AuthMethodSocial
}

func normalizeMethodSpelling(method string) string {
	normalized := strings.ToLower(strings.TrimSpace(method))
	return strings.NewReplacer("_", "-", " ", "-").Replace(normalized)
}

func isBuilderIDMethod(method string) bool {
	switch normalizeMethodSpelling(method) {
	case "builder-id", "builderid":
		return true
	}
	return false
}

// Validate 检查凭据是否足以刷新 token。
func (c Credentials) Validate() error {
	if strings.TrimSpace(c.RefreshToken) == "" {
		return errors.New("refresh token is required")
	}
	if c.AuthMethod == AuthMethodIdC && (c.ClientID == "" || c.ClientSecret == "") {
		return errors.New("client id and client secret are required for idc credentials")
	}
	return nil
}

// ParseImport 解析管理员粘贴的凭据。支持三种输入：
//   - Kiro IDE 的 kiro-auth-token.json，可与同目录 {clientIdHash}.json 的内容
//     一起粘贴（JSON 数组，或多个对象合并成一个）；
//   - AWS SSO cache / AIClient2API 导出的单个 JSON 对象；
//   - 只有一个 refresh token 的纯文本，按 social 处理。
func ParseImport(raw string) (Credentials, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return Credentials{}, errors.New("empty input")
	}
	if !strings.HasPrefix(text, "{") && !strings.HasPrefix(text, "[") {
		if strings.ContainsAny(text, " \t\r\n") {
			return Credentials{}, errors.New("input is neither JSON nor a single refresh token")
		}
		return Credentials{RefreshToken: text, AuthMethod: AuthMethodSocial}, nil
	}

	var objects []map[string]any
	if strings.HasPrefix(text, "[") {
		if err := json.Unmarshal([]byte(text), &objects); err != nil {
			return Credentials{}, fmt.Errorf("invalid JSON array: %w", err)
		}
	} else {
		decoder := json.NewDecoder(strings.NewReader(text))
		for decoder.More() {
			var obj map[string]any
			if err := decoder.Decode(&obj); err != nil {
				return Credentials{}, fmt.Errorf("invalid JSON: %w", err)
			}
			objects = append(objects, obj)
		}
	}

	var c Credentials
	var rawMethod string
	for _, obj := range objects {
		get := func(keys ...string) string {
			for _, key := range keys {
				if v := stringValue(obj[key]); v != "" {
					return v
				}
			}
			return ""
		}
		refreshToken := get("refreshToken", "refresh_token")
		clientSecret := get("clientSecret", "client_secret")
		if refreshToken == "" && clientSecret != "" {
			// OIDC 客户端注册文件：其中的 expiresAt 是 client secret 的过期时间。
			c.ClientID = firstNonEmpty(get("clientId", "client_id"), c.ClientID)
			c.ClientSecret = clientSecret
			if t := timeValue(firstValue(obj, "expiresAt", "expires_at", "clientSecretExpiresAt")); !t.IsZero() {
				c.ClientSecretExpiresAt = t
			}
			continue
		}
		c.RefreshToken = firstNonEmpty(refreshToken, c.RefreshToken)
		c.AccessToken = firstNonEmpty(get("accessToken", "access_token"), c.AccessToken)
		if t := timeValue(firstValue(obj, "expiresAt", "expires_at")); !t.IsZero() {
			c.ExpiresAt = t
		}
		rawMethod = firstNonEmpty(get("authMethod", "auth_method"), rawMethod)
		c.Provider = firstNonEmpty(get("provider", "socialProvider"), c.Provider)
		c.ClientID = firstNonEmpty(get("clientId", "client_id"), c.ClientID)
		c.ClientSecret = firstNonEmpty(clientSecret, c.ClientSecret)
		c.Region = firstNonEmpty(get("region"), c.Region)
		c.SSORegion = firstNonEmpty(get("idcRegion", "ssoRegion", "sso_region"), c.SSORegion)
		c.ProfileARN = firstNonEmpty(get("profileArn", "profile_arn"), c.ProfileARN)
		c.StartURL = firstNonEmpty(get("startUrl", "start_url"), c.StartURL)
	}
	c.AuthMethod = NormalizeAuthMethod(rawMethod, c.ClientID != "" && c.ClientSecret != "")
	if c.Provider == "" && isBuilderIDMethod(rawMethod) {
		c.Provider = ProviderBuilderID
	}
	if c.AuthMethod == AuthMethodIdC && c.SSORegion == "" {
		c.SSORegion = c.Region
	}
	if err := c.Validate(); err != nil {
		return Credentials{}, err
	}
	return c, nil
}

func firstValue(obj map[string]any, keys ...string) any {
	for _, key := range keys {
		if v, ok := obj[key]; ok && v != nil {
			return v
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func stringValue(v any) string {
	switch val := v.(type) {
	case string:
		return strings.TrimSpace(val)
	case json.Number:
		return val.String()
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64)
	case int64:
		return strconv.FormatInt(val, 10)
	case int:
		return strconv.Itoa(val)
	default:
		return ""
	}
}

// timeValue 解析 RFC3339 字符串或 Unix 时间戳（秒或毫秒）。
func timeValue(v any) time.Time {
	switch val := v.(type) {
	case string:
		s := strings.TrimSpace(val)
		if s == "" {
			return time.Time{}
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return unixTime(n)
		}
	case float64:
		return unixTime(int64(val))
	case int64:
		return unixTime(val)
	case int:
		return unixTime(int64(val))
	case json.Number:
		if n, err := val.Int64(); err == nil {
			return unixTime(n)
		}
	}
	return time.Time{}
}

func unixTime(n int64) time.Time {
	if n <= 0 {
		return time.Time{}
	}
	if n > 1e12 {
		return time.UnixMilli(n)
	}
	return time.Unix(n, 0)
}
