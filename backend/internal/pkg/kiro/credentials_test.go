package kiro

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseImportBareRefreshToken(t *testing.T) {
	t.Parallel()
	c, err := ParseImport("  aorAAAAAG-token  \n")
	require.NoError(t, err)
	require.Equal(t, "aorAAAAAG-token", c.RefreshToken)
	require.Equal(t, AuthMethodSocial, c.AuthMethod)
}

func TestParseImportRejectsMultiWordText(t *testing.T) {
	t.Parallel()
	_, err := ParseImport("not a token")
	require.Error(t, err)
}

func TestParseImportKiroIDESocial(t *testing.T) {
	t.Parallel()
	c, err := ParseImport(`{
		"accessToken": "at",
		"refreshToken": "rt",
		"expiresAt": "2026-10-30T10:00:00.000Z",
		"authMethod": "social",
		"provider": "Github",
		"profileArn": "arn:aws:codewhisperer:us-east-1:111:profile/ABC"
	}`)
	require.NoError(t, err)
	require.Equal(t, AuthMethodSocial, c.AuthMethod)
	require.Equal(t, "Github", c.Provider)
	require.Equal(t, "arn:aws:codewhisperer:us-east-1:111:profile/ABC", c.ProfileARN)
	require.Equal(t, time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC), c.ExpiresAt.UTC())
}

func TestParseImportKiroIDEBuilderIDWithClientFile(t *testing.T) {
	t.Parallel()
	c, err := ParseImport(`{
		"accessToken": "at",
		"refreshToken": "rt",
		"expiresAt": "2026-10-30T10:00:00Z",
		"authMethod": "IdC",
		"provider": "BuilderId",
		"region": "us-east-1",
		"clientIdHash": "abc"
	}
	{
		"clientId": "cid",
		"clientSecret": "secret",
		"expiresAt": "2027-01-28T10:00:00Z"
	}`)
	require.NoError(t, err)
	require.Equal(t, AuthMethodIdC, c.AuthMethod)
	require.True(t, c.IsBuilderID())
	require.Equal(t, "cid", c.ClientID)
	require.Equal(t, "secret", c.ClientSecret)
	require.Equal(t, time.Date(2027, 1, 28, 10, 0, 0, 0, time.UTC), c.ClientSecretExpiresAt.UTC())
	require.Equal(t, time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC), c.ExpiresAt.UTC())
	require.Equal(t, "us-east-1", c.SSORegion)
}

func TestParseImportArrayAndBuilderIDLabel(t *testing.T) {
	t.Parallel()
	c, err := ParseImport(`[{"refresh_token":"rt","client_id":"cid","client_secret":"cs","auth_method":"builder-id","idcRegion":"eu-west-1","expiresAt":1793354400000}]`)
	require.NoError(t, err)
	require.Equal(t, AuthMethodIdC, c.AuthMethod)
	require.Equal(t, ProviderBuilderID, c.Provider)
	require.Equal(t, "eu-west-1", c.SSORegion)
	require.Equal(t, "eu-west-1", c.OIDCRegion())
	require.Equal(t, "eu-west-1", c.APIRegion())
	require.Equal(t, int64(1793354400), c.ExpiresAt.Unix())
}

func TestParseImportIdCRequiresClientCredentials(t *testing.T) {
	t.Parallel()
	_, err := ParseImport(`{"refreshToken":"rt","authMethod":"IdC"}`)
	require.Error(t, err)
}

func TestCredentialsMapRoundTrip(t *testing.T) {
	t.Parallel()
	in := Credentials{
		AccessToken:  "at",
		RefreshToken: "rt",
		ExpiresAt:    time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC),
		AuthMethod:   AuthMethodIdC,
		Provider:     ProviderBuilderID,
		ClientID:     "cid",
		ClientSecret: "cs",
		Region:       "us-east-1",
		SSORegion:    "us-east-1",
		StartURL:     BuilderIDStartURL,
	}
	m := in.ToMap()
	require.Equal(t, "2026-10-30T10:00:00Z", m[CredExpiresAt])
	require.NotContains(t, m, CredProfileARN)
	out := CredentialsFromMap(m)
	require.Equal(t, in, Credentials{
		AccessToken:  out.AccessToken,
		RefreshToken: out.RefreshToken,
		ExpiresAt:    out.ExpiresAt.UTC(),
		AuthMethod:   out.AuthMethod,
		Provider:     out.Provider,
		ClientID:     out.ClientID,
		ClientSecret: out.ClientSecret,
		Region:       out.Region,
		SSORegion:    out.SSORegion,
		StartURL:     out.StartURL,
	})
}

func TestNormalizeAuthMethod(t *testing.T) {
	t.Parallel()
	require.Equal(t, AuthMethodSocial, NormalizeAuthMethod("social", true))
	require.Equal(t, AuthMethodIdC, NormalizeAuthMethod("IdC", false))
	require.Equal(t, AuthMethodIdC, NormalizeAuthMethod("builder_id", false))
	require.Equal(t, AuthMethodIdC, NormalizeAuthMethod("", true))
	require.Equal(t, AuthMethodSocial, NormalizeAuthMethod("", false))
}
