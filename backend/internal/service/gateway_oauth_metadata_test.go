package service

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildOAuthMetadataUserID_FallbackWithoutAccountUUID(t *testing.T) {
	svc := &GatewayService{}

	parsed := &ParsedRequest{
		Model:          "claude-sonnet-4-5",
		Stream:         true,
		MetadataUserID: "",
	}

	account := &Account{
		ID:    123,
		Type:  AccountTypeOAuth,
		Extra: map[string]any{}, // intentionally missing account_uuid / claude_user_id
	}

	fp := &Fingerprint{ClientID: "deadbeef"}

	got := svc.buildOAuthMetadataUserID(parsed, account, fp)
	parsedID := ParseMetadataUserID(got)
	require.NotNil(t, parsedID)
	require.True(t, parsedID.IsNewFormat, "伪装路径的版本来自当前 CLI User-Agent，使用 JSON metadata")
	require.Equal(t, "deadbeef", parsedID.DeviceID)
	require.Empty(t, parsedID.AccountUUID)
	require.NotEmpty(t, parsedID.SessionID)
}

func TestBuildOAuthMetadataUserID_UsesAccountUUIDWhenPresent(t *testing.T) {
	svc := &GatewayService{}

	parsed := &ParsedRequest{
		Model:          "claude-sonnet-4-5",
		Stream:         true,
		MetadataUserID: "",
	}

	account := &Account{
		ID:   123,
		Type: AccountTypeOAuth,
		Extra: map[string]any{
			"account_uuid":      "acc-uuid",
			"claude_user_id":    "clientid123",
			"anthropic_user_id": "",
		},
	}

	got := svc.buildOAuthMetadataUserID(parsed, account, nil)
	parsedID := ParseMetadataUserID(got)
	require.NotNil(t, parsedID)
	require.True(t, parsedID.IsNewFormat)
	require.Equal(t, "clientid123", parsedID.DeviceID)
	require.Equal(t, "acc-uuid", parsedID.AccountUUID)
	require.NotEmpty(t, parsedID.SessionID)
}

// TestBuildOAuthMetadataUserID_SessionIDStableAcrossTurns 验证伪装路径合成的
// metadata.user_id 在同一会话多轮请求间保持不变（session_id 稳定），贴近真实 Claude Code
// 进程级稳定的 session。账号 / 指纹 / UA 版本均相同，唯一可能变化的就是 session_id，
// 因此直接比较完整 user_id 字符串即可判定 session_id 是否稳定。
func TestBuildOAuthMetadataUserID_SessionIDStableAcrossTurns(t *testing.T) {
	svc := &GatewayService{}
	account := &Account{ID: 777, Type: AccountTypeOAuth, Extra: map[string]any{"account_uuid": "acc-uuid"}}
	fp := &Fingerprint{ClientID: "clientid777", UserAgent: "claude-cli/2.1.161 (external, cli)"}

	mustParse := func(body string) *ParsedRequest {
		parsed, err := ParseGatewayRequest(NewRequestBodyRef([]byte(body)), PlatformAnthropic)
		require.NoError(t, err)
		return parsed
	}

	round1 := mustParse(`{"model":"claude-sonnet-4-5","system":"sys","messages":[` +
		`{"role":"user","content":"first question"}]}`)
	round2 := mustParse(`{"model":"claude-sonnet-4-5","system":"sys","messages":[` +
		`{"role":"user","content":"first question"},` +
		`{"role":"assistant","content":"answer 1"},` +
		`{"role":"user","content":"second question"}]}`)
	round3 := mustParse(`{"model":"claude-sonnet-4-5","system":"sys","messages":[` +
		`{"role":"user","content":"first question"},` +
		`{"role":"assistant","content":"answer 1"},` +
		`{"role":"user","content":"second question"},` +
		`{"role":"assistant","content":"answer 2"},` +
		`{"role":"user","content":"third question"}]}`)

	id1 := svc.buildOAuthMetadataUserID(round1, account, fp)
	id2 := svc.buildOAuthMetadataUserID(round2, account, fp)
	id3 := svc.buildOAuthMetadataUserID(round3, account, fp)

	require.NotEmpty(t, id1)
	require.Equal(t, id1, id2, "session_id 应随对话增长保持不变")
	require.Equal(t, id2, id3, "session_id 应跨所有轮次保持不变")

	// 不同的首条 user 消息应派生出不同的 session_id（不同会话）。
	other := mustParse(`{"model":"claude-sonnet-4-5","system":"sys","messages":[` +
		`{"role":"user","content":"a completely different opener"}]}`)
	idOther := svc.buildOAuthMetadataUserID(other, account, fp)
	require.NotEqual(t, id1, idOther, "不同首条消息应派生不同 session_id")
}

func TestBuildOAuthMetadataUserID_DifferentAPIKeysDifferentSession(t *testing.T) {
	svc := &GatewayService{}
	account := &Account{ID: 777, Type: AccountTypeOAuth, Extra: map[string]any{"account_uuid": "acc-uuid"}}
	fp := &Fingerprint{ClientID: "clientid777"}
	body := `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"same opener"}]}`
	parse := func(keyID int64) *ParsedRequest {
		parsed, err := ParseGatewayRequest(NewRequestBodyRef([]byte(body)), PlatformAnthropic)
		require.NoError(t, err)
		parsed.SessionContext = &SessionContext{ClientIP: "203.0.113.8", UserAgent: "client/1", APIKeyID: keyID}
		return parsed
	}

	a := svc.buildOAuthMetadataUserID(parse(1), account, fp)
	b := svc.buildOAuthMetadataUserID(parse(2), account, fp)
	require.NotEqual(t, a, b)
}

func TestSyncClaudeCodeSessionHeader(t *testing.T) {
	sessionID := "11111111-1111-4111-8111-111111111111"
	uid := FormatMetadataUserID("device", "acct", sessionID, "2.1.258")
	body := []byte(`{"metadata":{"user_id":` + strconv.Quote(uid) + `}}`)

	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	require.NoError(t, err)
	syncClaudeCodeSessionHeader(req, body, false)
	require.Empty(t, req.Header.Get("X-Claude-Code-Session-Id"))

	syncClaudeCodeSessionHeader(req, body, true)
	require.Equal(t, sessionID, req.Header.Get("X-Claude-Code-Session-Id"))

	req.Header.Set("X-Claude-Code-Session-Id", "old")
	syncClaudeCodeSessionHeader(req, body, false)
	require.Equal(t, sessionID, req.Header.Get("X-Claude-Code-Session-Id"))
}
