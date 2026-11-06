package kiro

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewSocialLogin(t *testing.T) {
	t.Parallel()
	login, err := NewSocialLogin("github")
	require.NoError(t, err)
	require.Equal(t, SocialProviderGitHub, login.Provider)

	u, err := url.Parse(login.AuthURL)
	require.NoError(t, err)
	require.Equal(t, "prod.us-east-1.auth.desktop.kiro.dev", u.Host)
	require.Equal(t, "/login", u.Path)
	q := u.Query()
	require.Equal(t, "Github", q.Get("idp"))
	require.Equal(t, SocialRedirectURI, q.Get("redirect_uri"))
	require.Equal(t, "S256", q.Get("code_challenge_method"))
	require.Equal(t, login.State, q.Get("state"))
	sum := sha256.Sum256([]byte(login.CodeVerifier))
	require.Equal(t, base64.RawURLEncoding.EncodeToString(sum[:]), q.Get("code_challenge"))

	_, err = NewSocialLogin("twitter")
	require.Error(t, err)
}

func TestParseSocialCallback(t *testing.T) {
	t.Parallel()
	code, err := ParseSocialCallback("kiro://kiro.kiroAgent/authenticate-success?code=abc&state=s1", "s1")
	require.NoError(t, err)
	require.Equal(t, "abc", code)

	code, err = ParseSocialCallback("  raw-code  ", "s1")
	require.NoError(t, err)
	require.Equal(t, "raw-code", code)

	code, err = ParseSocialCallback("code=abc", "s1")
	require.NoError(t, err)
	require.Equal(t, "abc", code)

	_, err = ParseSocialCallback("kiro://kiro.kiroAgent/authenticate-success?code=abc&state=other", "s1")
	require.True(t, errors.Is(err, ErrSocialStateMismatch))

	_, err = ParseSocialCallback("kiro://kiro.kiroAgent/authenticate-success?error=access_denied&state=s1", "s1")
	require.ErrorContains(t, err, "access_denied")

	_, err = ParseSocialCallback("kiro://kiro.kiroAgent/authenticate-success?state=s1", "s1")
	require.ErrorContains(t, err, "no code")
}

func TestExchangeSocialCode(t *testing.T) {
	t.Parallel()
	var calls []recordedRequest
	doer := fakeDoer(t, 200, `{"accessToken":"at","refreshToken":"rt","profileArn":"arn:p","expiresIn":3600}`, &calls)
	login := &SocialLogin{Provider: SocialProviderGoogle, CodeVerifier: "verifier"}

	creds, err := ExchangeSocialCode(context.Background(), doer, login, "the-code")
	require.NoError(t, err)
	require.Equal(t, "https://prod.us-east-1.auth.desktop.kiro.dev/oauth/token", calls[0].URL)
	require.Equal(t, map[string]any{
		"code":          "the-code",
		"code_verifier": "verifier",
		"redirect_uri":  SocialRedirectURI,
	}, calls[0].Body)
	require.Equal(t, AuthMethodSocial, creds.AuthMethod)
	require.Equal(t, SocialProviderGoogle, creds.Provider)
	require.Equal(t, "arn:p", creds.ProfileARN)
	require.Equal(t, "rt", creds.RefreshToken)
	require.NoError(t, creds.Validate())
}

func TestExchangeSocialCodeRejectsEmptyTokens(t *testing.T) {
	t.Parallel()
	var calls []recordedRequest
	doer := fakeDoer(t, 200, `{"accessToken":"at"}`, &calls)
	_, err := ExchangeSocialCode(context.Background(), doer, &SocialLogin{CodeVerifier: "v"}, "c")
	require.ErrorContains(t, err, "no tokens")
}
