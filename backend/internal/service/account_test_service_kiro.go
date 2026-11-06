package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/gin-gonic/gin"
)

const kiroTestDefaultPrompt = "hi"

// SetKiroTokenProvider 注入 Kiro 账号测试使用的 token 提供者。
func (s *AccountTestService) SetKiroTokenProvider(p *KiroTokenProvider) {
	s.kiroTokenProvider = p
}

// testKiroAccountConnection 发送一条最小的对话请求测试 Kiro 账号。
func (s *AccountTestService) testKiroAccountConnection(c *gin.Context, account *Account, modelID string, prompt string) error {
	ctx := c.Request.Context()
	if account.Type != AccountTypeOAuth {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Unsupported account type: %s", account.Type))
	}
	if s.kiroTokenProvider == nil {
		return s.sendErrorAndEnd(c, "Kiro token provider is not configured")
	}
	testModel := strings.TrimSpace(modelID)
	if testModel == "" {
		testModel = kiro.DefaultTestModel
	}
	text := strings.TrimSpace(prompt)
	if text == "" {
		text = kiroTestDefaultPrompt
	}

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()
	s.sendEvent(c, TestEvent{Type: "test_start", Model: testModel})

	token, account, err := s.kiroTokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Failed to get access token: %s", sanitizeUpstreamErrorMessage(err.Error())))
	}
	creds := kiro.CredentialsFromMap(account.Credentials)
	creds.AccessToken = token

	payload, err := json.Marshal(map[string]any{
		"model":      testModel,
		"max_tokens": 256,
		"messages":   []map[string]any{{"role": "user", "content": text}},
	})
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create test payload")
	}
	built, err := kiro.BuildRequest(payload, kiro.BuildOptions{Model: account.GetMappedModel(testModel), ProfileARN: creds.ProfileARN})
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Failed to build request: %s", err.Error()))
	}
	req, err := kiro.NewGenerateRequest(ctx, creds, built.Body)
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create request")
	}
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Request failed: %s", sanitizeUpstreamErrorMessage(err.Error())))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return s.sendErrorAndEnd(c, fmt.Sprintf("API returned %d: %s", resp.StatusCode, truncateString(string(body), 2000)))
	}

	decoder := kiro.NewDecoder(resp.Body)
	received := false
	for {
		frame, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return s.sendErrorAndEnd(c, fmt.Sprintf("Stream read error: %s", err.Error()))
		}
		ev := kiro.ParseEvent(frame)
		switch ev.Kind {
		case kiro.EventText:
			if ev.Text != "" {
				received = true
				s.sendEvent(c, TestEvent{Type: "content", Text: ev.Text})
			}
		case kiro.EventException:
			return s.sendErrorAndEnd(c, fmt.Sprintf("Upstream error: %s %s", ev.ExceptionType, ev.Text))
		}
	}
	if !received {
		return s.sendErrorAndEnd(c, "Upstream returned an empty response")
	}
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}
