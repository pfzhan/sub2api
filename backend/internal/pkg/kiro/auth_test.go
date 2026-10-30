package kiro

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type recordedRequest struct {
	URL    string
	Header http.Header
	Body   map[string]any
}

func fakeDoer(t *testing.T, status int, response string, calls *[]recordedRequest) Doer {
	t.Helper()
	return DoerFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if req.Body != nil {
			data, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(data, &body))
		}
		*calls = append(*calls, recordedRequest{URL: req.URL.String(), Header: req.Header.Clone(), Body: body})
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(response)),
		}, nil
	})
}

func TestRefreshSocial(t *testing.T) {
	t.Parallel()
	var calls []recordedRequest
	doer := fakeDoer(t, 200, `{"accessToken":"new-at","refreshToken":"new-rt","profileArn":"arn:p","expiresIn":3600}`, &calls)
	before := time.Now()
	out, err := Refresh(context.Background(), doer, Credentials{RefreshToken: "rt", AuthMethod: AuthMethodSocial, Region: "us-east-1"})
	require.NoError(t, err)
	require.Len(t, calls, 1)
	require.Equal(t, "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken", calls[0].URL)
	require.Equal(t, map[string]any{"refreshToken": "rt"}, calls[0].Body)
	require.Equal(t, "new-at", out.AccessToken)
	require.Equal(t, "new-rt", out.RefreshToken)
	require.Equal(t, "arn:p", out.ProfileARN)
	require.WithinDuration(t, before.Add(time.Hour), out.ExpiresAt, 5*time.Second)
}

func TestRefreshIdCUsesSSORegion(t *testing.T) {
	t.Parallel()
	var calls []recordedRequest
	doer := fakeDoer(t, 200, `{"accessToken":"new-at","expiresIn":28800}`, &calls)
	out, err := Refresh(context.Background(), doer, Credentials{
		RefreshToken: "rt",
		AuthMethod:   AuthMethodIdC,
		ClientID:     "cid",
		ClientSecret: "cs",
		Region:       "us-east-1",
		SSORegion:    "eu-west-1",
	})
	require.NoError(t, err)
	require.Equal(t, "https://oidc.eu-west-1.amazonaws.com/token", calls[0].URL)
	require.Equal(t, "refresh_token", calls[0].Body["grantType"])
	require.Equal(t, "cid", calls[0].Body["clientId"])
	require.Equal(t, "new-at", out.AccessToken)
	require.Equal(t, "rt", out.RefreshToken, "refresh token is kept when the response omits it")
}

func TestRefreshSocialUnauthorizedIsInvalidGrant(t *testing.T) {
	t.Parallel()
	var calls []recordedRequest
	doer := fakeDoer(t, 401, `{"message":"Invalid refresh token"}`, &calls)
	_, err := Refresh(context.Background(), doer, Credentials{RefreshToken: "rt", AuthMethod: AuthMethodSocial})
	require.ErrorIs(t, err, ErrInvalidGrant)
	require.Contains(t, err.Error(), "invalid_grant")
}

func TestDiscoverProfileARNPrefersRegion(t *testing.T) {
	t.Parallel()
	var calls []recordedRequest
	doer := fakeDoer(t, 200, `{"profiles":[{"arn":"arn:aws:codewhisperer:eu-central-1:1:profile/A"},{"arn":"arn:aws:codewhisperer:us-east-1:1:profile/B"}]}`, &calls)
	arn, err := DiscoverProfileARN(context.Background(), doer, Credentials{AccessToken: "at", ClientID: "cid", Region: "us-east-1"})
	require.NoError(t, err)
	require.Equal(t, "arn:aws:codewhisperer:us-east-1:1:profile/B", arn)
	require.Equal(t, "https://codewhisperer.us-east-1.amazonaws.com/ListAvailableProfiles", calls[0].URL)
	require.Equal(t, "Bearer at", calls[0].Header.Get("Authorization"))
	require.Contains(t, calls[0].Header.Get("x-amz-user-agent"), "KiroIDE-")
	require.Equal(t, float64(10), calls[0].Body["maxResults"])
}

func TestDeviceFlow(t *testing.T) {
	t.Parallel()
	var calls []recordedRequest
	client, err := RegisterClient(context.Background(), fakeDoer(t, 200, `{"clientId":"cid","clientSecret":"cs","clientSecretExpiresAt":1793354400}`, &calls), "us-east-1")
	require.NoError(t, err)
	require.Equal(t, "https://oidc.us-east-1.amazonaws.com/client/register", calls[0].URL)
	require.Equal(t, "public", calls[0].Body["clientType"])

	auth, err := StartDeviceAuthorization(context.Background(), fakeDoer(t, 200, `{"deviceCode":"dc","userCode":"ABCD-EFGH","verificationUriComplete":"https://device.sso/?user_code=ABCD-EFGH","expiresIn":600}`, &calls), "us-east-1", client, "")
	require.NoError(t, err)
	require.Equal(t, BuilderIDStartURL, calls[1].Body["startUrl"])
	require.Equal(t, 5, auth.Interval)

	_, err = PollDeviceToken(context.Background(), fakeDoer(t, 400, `{"error":"authorization_pending"}`, &calls), "us-east-1", client, "dc")
	require.ErrorIs(t, err, ErrAuthorizationPending)
	_, err = PollDeviceToken(context.Background(), fakeDoer(t, 400, `{"error":"slow_down"}`, &calls), "us-east-1", client, "dc")
	require.ErrorIs(t, err, ErrSlowDown)

	creds, err := PollDeviceToken(context.Background(), fakeDoer(t, 200, `{"accessToken":"at","refreshToken":"rt","expiresIn":3600}`, &calls), "us-east-1", client, "dc")
	require.NoError(t, err)
	require.Equal(t, AuthMethodIdC, creds.AuthMethod)
	require.Equal(t, "cid", creds.ClientID)
	require.Equal(t, "us-east-1", creds.SSORegion)
	require.Equal(t, "urn:ietf:params:oauth:grant-type:device_code", calls[len(calls)-1].Body["grantType"])
}

func TestPollDeviceTokenOtherErrorsPassThrough(t *testing.T) {
	t.Parallel()
	var calls []recordedRequest
	client := &ClientRegistration{ClientID: "cid", ClientSecret: "cs"}
	_, err := PollDeviceToken(context.Background(), fakeDoer(t, 400, `{"error":"expired_token"}`, &calls), "us-east-1", client, "dc")
	var httpErr *HTTPError
	require.True(t, errors.As(err, &httpErr))
	require.Equal(t, 400, httpErr.StatusCode)
}
