//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/stretchr/testify/require"
)

type kiroFakeResponse struct {
	status int
	body   string
}

// kiroFakeUpstream 按 URL 路径返回预设响应，同一路径的多个响应按顺序消费，
// 最后一个会被重复使用。
type kiroFakeUpstream struct {
	mu        sync.Mutex
	responses map[string][]kiroFakeResponse
	calls     []string
	proxyURLs []string
}

func (f *kiroFakeUpstream) newDoer(proxyURL string) (kiro.Doer, error) {
	f.mu.Lock()
	f.proxyURLs = append(f.proxyURLs, proxyURL)
	f.mu.Unlock()
	return kiro.DoerFunc(f.do), nil
}

func (f *kiroFakeUpstream) do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := req.URL.Host + req.URL.Path
	f.calls = append(f.calls, path)
	queue := f.responses[path]
	resp := kiroFakeResponse{status: http.StatusNotFound, body: `{"message":"unexpected request"}`}
	if len(queue) > 0 {
		resp = queue[0]
		if len(queue) > 1 {
			f.responses[path] = queue[1:]
		}
	}
	return &http.Response{
		StatusCode: resp.status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(resp.body)),
	}, nil
}

func newKiroOAuthServiceForTest(upstream *kiroFakeUpstream, now time.Time) *KiroOAuthService {
	svc := NewKiroOAuthService(nil)
	svc.newDoer = upstream.newDoer
	svc.now = func() time.Time { return now }
	return svc
}

func TestKiroBuilderIDLoginFlow(t *testing.T) {
	upstream := &kiroFakeUpstream{responses: map[string][]kiroFakeResponse{
		"oidc.us-east-1.amazonaws.com/client/register": {{200, `{"clientId":"cid","clientSecret":"csecret","clientSecretExpiresAt":1893456000}`}},
		"oidc.us-east-1.amazonaws.com/device_authorization": {{200, `{"deviceCode":"dc","userCode":"ABCD-EFGH",` +
			`"verificationUri":"https://device.sso.us-east-1.amazonaws.com/","verificationUriComplete":"https://device.sso.us-east-1.amazonaws.com/?user_code=ABCD-EFGH","expiresIn":600,"interval":5}`}},
		"oidc.us-east-1.amazonaws.com/token": {
			{400, `{"error":"authorization_pending"}`},
			{400, `{"error":"slow_down"}`},
			{200, `{"accessToken":"at","refreshToken":"rt","expiresIn":3600}`},
		},
		"codewhisperer.us-east-1.amazonaws.com/ListAvailableProfiles": {{200, `{"profiles":[{"arn":"arn:aws:codewhisperer:us-east-1:1:profile/p"}]}`}},
	}}
	svc := newKiroOAuthServiceForTest(upstream, time.Now())
	ctx := context.Background()

	started, err := svc.StartBuilderIDLogin(ctx, nil, "")
	require.NoError(t, err)
	require.NotEmpty(t, started.SessionID)
	require.Equal(t, "ABCD-EFGH", started.UserCode)
	require.Equal(t, 600, started.ExpiresIn)
	require.Equal(t, 5, started.Interval)

	pending, err := svc.PollBuilderIDLogin(ctx, started.SessionID)
	require.NoError(t, err)
	require.Equal(t, &KiroDevicePollResult{Status: KiroDeviceStatusPending, Interval: 5}, pending)

	slowed, err := svc.PollBuilderIDLogin(ctx, started.SessionID)
	require.NoError(t, err)
	require.Equal(t, &KiroDevicePollResult{Status: KiroDeviceStatusPending, Interval: 10}, slowed)

	done, err := svc.PollBuilderIDLogin(ctx, started.SessionID)
	require.NoError(t, err)
	require.Equal(t, KiroDeviceStatusSuccess, done.Status)
	creds := kiro.CredentialsFromMap(done.Credentials)
	require.Equal(t, "at", creds.AccessToken)
	require.Equal(t, "rt", creds.RefreshToken)
	require.Equal(t, kiro.AuthMethodIdC, creds.AuthMethod)
	require.Equal(t, kiro.ProviderBuilderID, creds.Provider)
	require.Equal(t, kiro.BuilderIDStartURL, creds.StartURL)
	require.Equal(t, "cid", creds.ClientID)
	require.Equal(t, "arn:aws:codewhisperer:us-east-1:1:profile/p", creds.ProfileARN)

	_, err = svc.PollBuilderIDLogin(ctx, started.SessionID)
	require.Equal(t, http.StatusNotFound, infraerrors.Code(err), "session is removed after success")
}

func TestKiroBuilderIDLoginExpired(t *testing.T) {
	upstream := &kiroFakeUpstream{responses: map[string][]kiroFakeResponse{
		"oidc.eu-west-1.amazonaws.com/client/register":      {{200, `{"clientId":"cid","clientSecret":"cs"}`}},
		"oidc.eu-west-1.amazonaws.com/device_authorization": {{200, `{"deviceCode":"dc","userCode":"U","expiresIn":60}`}},
	}}
	now := time.Now()
	svc := newKiroOAuthServiceForTest(upstream, now)

	started, err := svc.StartBuilderIDLogin(context.Background(), nil, "eu-west-1")
	require.NoError(t, err)
	require.Equal(t, 5, started.Interval, "missing interval falls back to the OIDC default")

	svc.now = func() time.Time { return now.Add(61 * time.Second) }
	_, err = svc.PollBuilderIDLogin(context.Background(), started.SessionID)
	require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
	require.Equal(t, "KIRO_DEVICE_CODE_EXPIRED", infraerrors.Reason(err))
	require.NotContains(t, upstream.calls, "oidc.eu-west-1.amazonaws.com/token")
}

func TestKiroBuilderIDLoginDeniedRemovesSession(t *testing.T) {
	upstream := &kiroFakeUpstream{responses: map[string][]kiroFakeResponse{
		"oidc.us-east-1.amazonaws.com/client/register":      {{200, `{"clientId":"cid","clientSecret":"cs"}`}},
		"oidc.us-east-1.amazonaws.com/device_authorization": {{200, `{"deviceCode":"dc","userCode":"U","expiresIn":600,"interval":1}`}},
		"oidc.us-east-1.amazonaws.com/token":                {{400, `{"error":"access_denied"}`}},
	}}
	svc := newKiroOAuthServiceForTest(upstream, time.Now())

	started, err := svc.StartBuilderIDLogin(context.Background(), nil, "")
	require.NoError(t, err)
	_, err = svc.PollBuilderIDLogin(context.Background(), started.SessionID)
	require.Equal(t, "KIRO_DEVICE_TOKEN_FAILED", infraerrors.Reason(err))
	_, ok := svc.getSession(started.SessionID)
	require.False(t, ok)
}

func TestKiroImportCredentialsRefreshesSocialToken(t *testing.T) {
	upstream := &kiroFakeUpstream{responses: map[string][]kiroFakeResponse{
		"prod.us-east-1.auth.desktop.kiro.dev/refreshToken": {{200, `{"accessToken":"at","refreshToken":"rt2","profileArn":"arn:p","expiresIn":3600}`}},
	}}
	svc := newKiroOAuthServiceForTest(upstream, time.Now())

	creds, err := svc.ImportCredentials(context.Background(), KiroImportInput{Raw: "aorAAAAAGexample-refresh-token"})
	require.NoError(t, err)
	require.Equal(t, "at", creds[kiro.CredAccessToken])
	require.Equal(t, "rt2", creds[kiro.CredRefreshToken])
	require.Equal(t, "arn:p", creds[kiro.CredProfileARN])
	require.Equal(t, []string{"prod.us-east-1.auth.desktop.kiro.dev/refreshToken"}, upstream.calls)
}

func TestKiroImportCredentialsRejectsInvalidInput(t *testing.T) {
	svc := newKiroOAuthServiceForTest(&kiroFakeUpstream{}, time.Now())
	_, err := svc.ImportCredentials(context.Background(), KiroImportInput{Raw: "  "})
	require.Equal(t, "KIRO_IMPORT_INVALID", infraerrors.Reason(err))
}

func TestKiroImportCredentialsRefreshFailure(t *testing.T) {
	upstream := &kiroFakeUpstream{responses: map[string][]kiroFakeResponse{
		"prod.us-east-1.auth.desktop.kiro.dev/refreshToken": {{401, `{"message":"invalid refresh token"}`}},
	}}
	svc := newKiroOAuthServiceForTest(upstream, time.Now())
	_, err := svc.ImportCredentials(context.Background(), KiroImportInput{Raw: "aorAAAAAGexample-refresh-token"})
	require.Equal(t, "KIRO_IMPORT_REFRESH_FAILED", infraerrors.Reason(err))
}
