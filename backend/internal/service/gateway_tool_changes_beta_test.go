//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBuildUpstreamRequestToolChangeBetas(t *testing.T) {
	const mid = "mid-conversation-tool-changes-2026-07-01"
	const inline = "inline-tools-2026-09-15"
	// CLI 2.1.318 puts both tokens in the default mimic portrait. A client
	// header is not required. A drop set can still remove one. Unknown tokens
	// stay out, and a repeated client token collapses to one.
	for _, tc := range []struct {
		name, header        string
		drop                map[string]struct{}
		wantMid, wantInline bool
	}{
		{"portrait includes both", "", nil, true, true},
		{"client token is not required", mid, nil, true, true},
		{"unknown client token stays out", inline + "-other", nil, true, true},
		{"duplicate collapses", inline + ", " + inline + "," + mid, nil, true, true},
		{"filter inline", mid + "," + inline, map[string]struct{}{inline: {}}, true, false},
		{"filter mid", mid + "," + inline, map[string]struct{}{mid: {}}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			c.Request.Header.Set("anthropic-beta", tc.header+",unrelated-beta")
			if tc.drop != nil {
				c.Set(betaPolicyFilterSetKey, tc.drop)
			}
			account := &Account{ID: 701, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
				Credentials: map[string]any{"access_token": "test-token"}, Status: StatusActive, Schedulable: true}
			body := []byte(`{"model":"claude-opus-5-5","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"tool_addition","tool":{"type":"tool_reference","tool_name":"lookup"}},{"type":"text","text":"Use lookup"}]}]}`)
			svc := &GatewayService{cfg: &config.Config{}}
			req, _, err := svc.buildUpstreamRequest(context.Background(), c, account, body,
				"test-token", "oauth", "claude-opus-5-5", false, true)
			require.NoError(t, err)
			out := readUpstreamBodyForTest(t, req)
			require.JSONEq(t, gjson.GetBytes(body, "messages").Raw, gjson.GetBytes(out, "messages").Raw)
			header := getHeaderRaw(req.Header, "anthropic-beta")
			require.Equal(t, tc.wantMid, containsBetaToken(header, mid))
			require.Equal(t, tc.wantInline, containsBetaToken(header, inline))
			require.False(t, containsBetaToken(header, "unrelated-beta"))
			require.False(t, containsBetaToken(header, inline+"-other"))
			require.True(t, containsBetaToken(header, "oauth-2025-04-20"))
			if tc.wantInline {
				require.Equal(t, 1, strings.Count(header, inline))
			}
		})
	}
}
