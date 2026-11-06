package kiro

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const usageLimitsFixture = `{
  "subscriptionInfo": {"subscriptionTitle": "KIRO PRO"},
  "userInfo": {"email": "dev@example.com"},
  "nextDateReset": 1798761600,
  "usageBreakdownList": [{
    "resourceType": "AGENTIC_REQUEST",
    "displayName": "Credits",
    "currentUsage": 120,
    "currentUsageWithPrecision": 120.5,
    "usageLimit": 1000,
    "usageLimitWithPrecision": 1000.0,
    "nextDateReset": 1798761600,
    "bonuses": [
      {"bonusCode": "WELCOME", "displayName": "Welcome bonus", "currentUsage": 10, "usageLimit": 500, "expiresAt": 4102444800, "status": "ACTIVE"},
      {"bonusCode": "OLD", "displayName": "Old bonus", "currentUsage": 5, "usageLimit": 100, "expiresAt": 1600000000, "status": "EXPIRED"}
    ],
    "freeTrialInfo": {"currentUsage": 0, "usageLimit": 50, "freeTrialExpiry": 1600000000, "freeTrialStatus": "EXPIRED"}
  }]
}`

func TestGetUsageLimits(t *testing.T) {
	t.Parallel()
	var got *http.Request
	doer := DoerFunc(func(req *http.Request) (*http.Response, error) {
		got = req
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(usageLimitsFixture)),
		}, nil
	})

	limits, err := GetUsageLimits(context.Background(), doer, Credentials{
		AccessToken: "at",
		AuthMethod:  AuthMethodSocial,
		Region:      "us-east-1",
		ProfileARN:  "arn:aws:codewhisperer:us-east-1:1:profile/p",
	})
	require.NoError(t, err)

	require.Equal(t, http.MethodGet, got.Method)
	require.Equal(t, "q.us-east-1.amazonaws.com", got.URL.Host)
	require.Equal(t, "/getUsageLimits", got.URL.Path)
	require.Equal(t, url.Values{
		"isEmailRequired": {"true"},
		"origin":          {"AI_EDITOR"},
		"resourceType":    {"AGENTIC_REQUEST"},
		"profileArn":      {"arn:aws:codewhisperer:us-east-1:1:profile/p"},
	}, got.URL.Query())
	require.Equal(t, "Bearer at", got.Header.Get("Authorization"))
	require.Contains(t, got.Header.Get("User-Agent"), "KiroIDE-")

	require.Equal(t, "KIRO PRO", limits.SubscriptionTitle)
	require.Equal(t, "dev@example.com", limits.Email)
	require.Equal(t, time.Unix(1798761600, 0), limits.NextReset)
	require.Len(t, limits.Items, 4)
	require.Equal(t, UsageItem{Kind: UsageKindBase, Label: "Credits", Used: 120.5, Limit: 1000, ResetsAt: time.Unix(1798761600, 0)}, limits.Items[0])
	require.False(t, limits.Items[1].Expired)
	require.True(t, limits.Items[2].Expired)
	require.Equal(t, UsageKindFreeTrial, limits.Items[3].Kind)
	require.True(t, limits.Items[3].Expired)

	used, limit := limits.Totals()
	require.InDelta(t, 130.5, used, 1e-9)
	require.InDelta(t, 1500, limit, 1e-9)
}

func TestGetUsageLimitsBuilderIDWithoutProfile(t *testing.T) {
	t.Parallel()
	var got *http.Request
	doer := DoerFunc(func(req *http.Request) (*http.Response, error) {
		got = req
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	limits, err := GetUsageLimits(context.Background(), doer, Credentials{AccessToken: "at", AuthMethod: AuthMethodIdC, Region: "us-east-1"})
	require.NoError(t, err)
	require.Equal(t, "codewhisperer.us-east-1.amazonaws.com", got.URL.Host)
	require.Empty(t, got.URL.Query().Get("profileArn"))
	require.Empty(t, limits.Items)
}

func TestGetUsageLimitsHTTPError(t *testing.T) {
	t.Parallel()
	doer := DoerFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader(`{"message":"suspended"}`))}, nil
	})
	_, err := GetUsageLimits(context.Background(), doer, Credentials{AccessToken: "at", AuthMethod: AuthMethodSocial})
	var httpErr *HTTPError
	require.ErrorAs(t, err, &httpErr)
	require.Equal(t, http.StatusForbidden, httpErr.StatusCode)
}
