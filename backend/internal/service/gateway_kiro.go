package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
)

const (
	kiroCredentialUnavailableReason = GatewayFailureReason("kiro_credential_unavailable")
	kiroEmptyResponseReason         = GatewayFailureReason("kiro_empty_response")
)

// SetKiroTokenProvider 注入 Kiro 账号的 token 提供者。未注入时 Kiro 账号请求直接失败。
func (s *GatewayService) SetKiroTokenProvider(p *KiroTokenProvider) {
	s.kiroTokenProvider = p
}

// forwardKiro 把 Anthropic Messages 请求转换为 Kiro generateAssistantResponse，
// 再把 Kiro 的 event stream 转换回 Anthropic 响应。
func (s *GatewayService) forwardKiro(ctx context.Context, c *gin.Context, account *Account, parsed *ParsedRequest, startTime time.Time) (*ForwardResult, error) {
	reqModel := parsed.Model
	mappedModel := account.GetMappedModel(reqModel)

	if s.kiroTokenProvider == nil {
		return nil, errors.New("kiro token provider is not configured")
	}
	token, account, err := s.kiroTokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		return nil, s.kiroCredentialFailure(ctx, c, account, err)
	}
	creds := kiro.CredentialsFromMap(account.Credentials)
	creds.AccessToken = token

	built, err := kiro.BuildRequest(parsed.Body.Bytes(), kiro.BuildOptions{
		Model:      mappedModel,
		ProfileARN: creds.ProfileARN,
	})
	if err != nil {
		writeKiroClientError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, fmt.Errorf("build kiro request: %w", err)
	}

	req, err := kiro.NewGenerateRequest(ctx, creds, built.Body)
	if err != nil {
		return nil, fmt.Errorf("build kiro upstream request: %w", err)
	}
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, s.handleUpstreamTransportError(ctx, c, account, err, OpsUpstreamErrorEvent{
			UpstreamURL: safeUpstreamURL(req.URL.String()),
		})
	}
	defer func() { _ = resp.Body.Close() }()
	requestID := kiroRequestID(resp.Header)
	if requestID != "" && resp.Header.Get("x-request-id") == "" {
		resp.Header.Set("x-request-id", requestID)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return s.handleKiroErrorResponse(ctx, c, account, resp, mappedModel)
	}
	if parsed.OnUpstreamAccepted != nil {
		parsed.OnUpstreamAccepted()
	}

	estimatedInput, err := estimateAnthropicCountTokensLocally(parsed.Body.Bytes())
	if err != nil {
		estimatedInput = 0
	}
	conv := kiro.NewConverter(kiro.ConverterOptions{
		Model:                reqModel,
		UpstreamModel:        built.UpstreamModel,
		ThinkingRequested:    built.ThinkingRequested,
		Tools:                built.Tools,
		EstimatedInputTokens: estimatedInput,
		CountTokens:          kiroCountTokens,
	})
	outcome, err := s.relayKiroResponse(ctx, c, account, resp, conv, parsed.Stream, startTime, mappedModel)
	if err != nil {
		return nil, err
	}

	usage := conv.Usage()
	return &ForwardResult{
		RequestID:        requestID,
		UpstreamHeaders:  resp.Header,
		Usage:            ClaudeUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens},
		Model:            reqModel,
		UpstreamModel:    built.UpstreamModel,
		Stream:           parsed.Stream,
		Duration:         time.Since(startTime),
		FirstTokenMs:     outcome.firstTokenMs,
		ClientDisconnect: outcome.clientDisconnect,
	}, nil
}

// kiroCredentialFailure 处理取不到可用 token 的情况：临时停用账号并换下一个账号。
func (s *GatewayService) kiroCredentialFailure(ctx context.Context, c *gin.Context, account *Account, err error) error {
	message := sanitizeUpstreamErrorMessage(err.Error())
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:     opsUpstreamProxyID(account),
		ProxyName:   opsUpstreamProxyName(account),
		Platform:    account.Platform,
		AccountID:   account.ID,
		AccountName: account.Name,
		Kind:        "failover",
		Stage:       string(GatewayFailureStageAccountAuth),
		Scope:       string(GatewayFailureScopeAccount),
		Reason:      string(kiroCredentialUnavailableReason),
		Message:     message,
	})
	if s.rateLimitService != nil && !errors.Is(err, context.Canceled) {
		s.rateLimitService.HandleUpstreamError(ctx, account, http.StatusUnauthorized, nil, []byte(message))
	}
	return &UpstreamFailoverError{
		StatusCode:        http.StatusUnauthorized,
		ResponseBody:      []byte(message),
		Stage:             GatewayFailureStageAccountAuth,
		Scope:             GatewayFailureScopeAccount,
		Reason:            kiroCredentialUnavailableReason,
		NextAccountAction: NextAccountRetry,
		ClientStatusCode:  http.StatusBadGateway,
		ClientMessage:     "Upstream authentication failed, please contact administrator",
	}
}

// handleKiroErrorResponse 处理非 2xx 响应。Kiro 的错误体先转换为 Anthropic
// 错误格式，再交给通用的错误处理流程，账号状态按原始错误体判断。
func (s *GatewayService) handleKiroErrorResponse(ctx context.Context, c *gin.Context, account *Account, resp *http.Response, modelKey string) (*ForwardResult, error) {
	raw, _ := s.readUpstreamErrorBody(resp)
	status := kiro.NormalizeStatus(resp.StatusCode, raw)
	upstreamErr := kiro.ParseUpstreamError(raw)
	message := strings.TrimSpace(upstreamErr.Message)
	if message == "" {
		message = http.StatusText(status)
	}
	if kiro.IsContextLimit(resp.StatusCode, raw) {
		// Claude Code 依据 "prompt is too long" 触发上下文压缩。
		message = "prompt is too long: " + message
	}
	converted := anthropicErrorBody(status, message)
	resp.StatusCode = status
	resp.Body = io.NopCloser(bytes.NewReader(converted))

	// 402 是 Kiro 的额度用尽，与 401/403/429/5xx 一样需要换账号。
	if status == http.StatusPaymentRequired || s.shouldFailoverUpstreamError(status) {
		logger.LegacyPrintf("service.gateway", "[Kiro] Upstream error (failover): Account=%d(%s) Status=%d Body=%s",
			account.ID, account.Name, status, truncateString(string(raw), 1000))
		if s.rateLimitService != nil {
			s.rateLimitService.HandleUpstreamError(ctx, account, status, resp.Header, raw, modelKey)
		}
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			ProxyID:            opsUpstreamProxyID(account),
			ProxyName:          opsUpstreamProxyName(account),
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: status,
			UpstreamRequestID:  resp.Header.Get("x-request-id"),
			Kind:               "failover",
			Message:            sanitizeUpstreamErrorMessage(message),
		})
		return nil, &UpstreamFailoverError{
			StatusCode:      status,
			ResponseBody:    converted,
			ResponseHeaders: resp.Header.Clone(),
		}
	}
	return s.handleErrorResponse(ctx, resp, c, account, modelKey)
}

type kiroRelayOutcome struct {
	firstTokenMs     *int
	clientDisconnect bool
}

type kiroFrameResult struct {
	frame *kiro.Frame
	err   error
}

// relayKiroResponse 读取 Kiro event stream 并写回客户端。流式请求逐事件转发；
// 非流式请求在结束后一次性返回。客户端收到数据之前出现的错误都转为
// failover，交给 handler 换账号重试。
func (s *GatewayService) relayKiroResponse(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	resp *http.Response,
	conv *kiro.Converter,
	stream bool,
	startTime time.Time,
	modelKey string,
) (*kiroRelayOutcome, error) {
	outcome := &kiroRelayOutcome{}
	var flusher http.Flusher
	if stream {
		f, ok := c.Writer.(http.Flusher)
		if !ok {
			return nil, errors.New("streaming not supported")
		}
		flusher = f
	}

	frames := make(chan kiroFrameResult, 16)
	done := make(chan struct{})
	defer close(done)
	var lastReadAt atomic.Int64
	lastReadAt.Store(time.Now().UnixNano())
	go func() {
		defer close(frames)
		decoder := kiro.NewDecoder(resp.Body)
		for {
			frame, err := decoder.Next()
			if err == nil {
				lastReadAt.Store(time.Now().UnixNano())
			}
			if errors.Is(err, io.EOF) {
				return
			}
			select {
			case frames <- kiroFrameResult{frame: frame, err: err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	var intervalCh <-chan time.Time
	streamInterval := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.StreamDataIntervalTimeout > 0 {
		streamInterval = time.Duration(s.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
		ticker := time.NewTicker(streamInterval)
		defer ticker.Stop()
		intervalCh = ticker.C
	}

	headersWritten := false
	write := func(events []kiro.SSEEvent) {
		if len(events) == 0 {
			return
		}
		if outcome.firstTokenMs == nil {
			ms := int(time.Since(startTime).Milliseconds())
			outcome.firstTokenMs = &ms
		}
		if !stream || outcome.clientDisconnect {
			return
		}
		if !headersWritten {
			headersWritten = true
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")
			c.Header("X-Accel-Buffering", "no")
			if v := resp.Header.Get("x-request-id"); v != "" {
				c.Header("x-request-id", v)
			}
			c.Status(http.StatusOK)
			MarkResponseCommitted(c)
		}
		var buf []byte
		for _, ev := range events {
			buf = ev.AppendTo(buf)
		}
		if _, err := c.Writer.Write(buf); err != nil {
			outcome.clientDisconnect = true
			logger.LegacyPrintf("service.gateway", "[Kiro] Client disconnected during streaming, continue draining for usage: account=%d", account.ID)
			return
		}
		flusher.Flush()
	}
	// fail 处理读取过程中的错误：客户端还没收到数据时换账号，否则在流中写入错误事件。
	fail := func(status int, message string, transient bool) error {
		if !stream || !conv.Started() {
			body := anthropicErrorBody(status, message)
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				ProxyID:            opsUpstreamProxyID(account),
				ProxyName:          opsUpstreamProxyName(account),
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: status,
				UpstreamRequestID:  resp.Header.Get("x-request-id"),
				Kind:               "failover",
				Message:            sanitizeUpstreamErrorMessage(message),
			})
			return &UpstreamFailoverError{
				StatusCode:             status,
				ResponseBody:           body,
				RetryableOnSameAccount: transient,
				RequestScopedTransient: transient,
			}
		}
		if !outcome.clientDisconnect {
			errEvent := kiro.SSEEvent{Event: "error", Data: anthropicErrorBody(status, message)}
			if _, err := c.Writer.Write(errEvent.AppendTo(nil)); err == nil {
				flusher.Flush()
			}
		}
		return fmt.Errorf("kiro stream error: %s", message)
	}

	for {
		select {
		case result, ok := <-frames:
			if !ok {
				tail, err := conv.Finish()
				if errors.Is(err, kiro.ErrEmptyResponse) {
					logger.LegacyPrintf("service.gateway", "[Kiro] Empty response: account=%d model=%s", account.ID, modelKey)
					return nil, &UpstreamFailoverError{
						StatusCode:             http.StatusBadGateway,
						ResponseBody:           anthropicErrorBody(http.StatusBadGateway, "Upstream returned an empty response"),
						Reason:                 kiroEmptyResponseReason,
						RetryableOnSameAccount: true,
						RequestScopedTransient: true,
					}
				}
				write(tail)
				if !stream {
					c.Data(http.StatusOK, "application/json", conv.Message())
				}
				return outcome, nil
			}
			if result.err != nil {
				if outcome.clientDisconnect || errors.Is(result.err, context.Canceled) {
					outcome.clientDisconnect = true
					return outcome, nil
				}
				return nil, fail(http.StatusBadGateway, "upstream stream read error: "+result.err.Error(), true)
			}
			ev := kiro.ParseEvent(result.frame)
			if ev.Kind == kiro.EventException {
				status := http.StatusBadGateway
				if strings.Contains(strings.ToLower(ev.ExceptionType), "throttling") {
					status = http.StatusTooManyRequests
					if s.rateLimitService != nil && (!stream || !conv.Started()) {
						s.rateLimitService.HandleUpstreamError(ctx, account, status, resp.Header, []byte(ev.Text), modelKey)
					}
				}
				return nil, fail(status, strings.TrimSpace(ev.ExceptionType+": "+ev.Text), status != http.StatusTooManyRequests)
			}
			write(conv.Handle(ev))

		case <-intervalCh:
			if time.Since(time.Unix(0, lastReadAt.Load())) < streamInterval {
				continue
			}
			if outcome.clientDisconnect {
				return outcome, nil
			}
			logger.LegacyPrintf("service.gateway", "[Kiro] Stream data interval timeout: account=%d model=%s interval=%s", account.ID, modelKey, streamInterval)
			if s.rateLimitService != nil {
				s.rateLimitService.HandleStreamTimeout(ctx, account, modelKey)
			}
			return nil, fail(http.StatusGatewayTimeout, "stream data interval timeout", false)
		}
	}
}

// countTokensKiro 本地估算 count_tokens。Kiro 没有对应接口。
func (s *GatewayService) countTokensKiro(c *gin.Context, parsed *ParsedRequest) error {
	estimated, err := estimateAnthropicCountTokensLocally(parsed.Body.Bytes())
	if err != nil {
		s.countTokensError(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return fmt.Errorf("count_tokens: estimate kiro input tokens: %w", err)
	}
	c.JSON(http.StatusOK, gin.H{"input_tokens": estimated})
	return nil
}

func kiroRequestID(h http.Header) string {
	for _, key := range []string{"x-amzn-requestid", "x-amzn-request-id", "x-request-id"} {
		if v := strings.TrimSpace(h.Get(key)); v != "" {
			return v
		}
	}
	return ""
}

// kiroCountTokens 用 o200k 分词器估算输出 token，分词器不可用时按字符数估算。
func kiroCountTokens(text string) int {
	if text == "" {
		return 0
	}
	if codec, err := openAIInputTokensCodecForModel(""); err == nil {
		if n, err := codec.Count(text); err == nil {
			return n
		}
	}
	return (len(text) + 3) / 4
}

func anthropicErrorBody(status int, message string) []byte {
	body, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    anthropicErrorType(status),
			"message": message,
		},
	})
	return body
}

func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusPaymentRequired, http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

func writeKiroClientError(c *gin.Context, status int, errType, message string) {
	if c == nil {
		return
	}
	c.JSON(status, gin.H{"type": "error", "error": gin.H{"type": errType, "message": message}})
}
