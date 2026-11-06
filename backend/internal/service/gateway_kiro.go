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
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
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
	up, err := s.openKiroUpstream(ctx, c, account, parsed.Model, parsed.Body.Bytes(), parsed.SessionContext, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = up.resp.Body.Close() }()
	if parsed.OnUpstreamAccepted != nil {
		parsed.OnUpstreamAccepted()
	}

	outcome, err := s.relayKiroResponse(ctx, c, up, parsed.Stream, startTime)
	if outcome == nil {
		return nil, err
	}
	// 流中途出错时 outcome 和 err 同时返回：客户端已经收到的内容照常计费。
	return up.forwardResult(parsed.Model, parsed.Stream, startTime, outcome), err
}

// forwardResult 按 Converter 的用量生成 ForwardResult。
func (up *kiroUpstream) forwardResult(model string, stream bool, startTime time.Time, outcome *kiroRelayOutcome) *ForwardResult {
	usage := up.conv.Usage()
	return &ForwardResult{
		RequestID:       up.requestID,
		UpstreamHeaders: up.resp.Header,
		Usage: ClaudeUsage{
			InputTokens:              usage.InputTokens,
			OutputTokens:             usage.OutputTokens,
			CacheCreationInputTokens: usage.CacheCreationInputTokens,
			CacheReadInputTokens:     usage.CacheReadInputTokens,
			CacheCreation5mTokens:    usage.CacheCreation5mTokens,
			CacheCreation1hTokens:    usage.CacheCreation1hTokens,
		},
		Model:            model,
		UpstreamModel:    up.built.UpstreamModel,
		Stream:           stream,
		Duration:         time.Since(startTime),
		FirstTokenMs:     outcome.firstTokenMs,
		ClientDisconnect: outcome.clientDisconnect,
	}
}

// kiroErrorWriter 按入站协议的格式向客户端写错误。
type kiroErrorWriter func(c *gin.Context, status int, errType, message string)

// kiroUpstream 是已经返回 2xx 的 Kiro 请求。account 是取 token 后的最新账号。
type kiroUpstream struct {
	account     *Account
	resp        *http.Response
	conv        *kiro.Converter
	built       *kiro.BuiltRequest
	requestID   string
	mappedModel string
	// cacheEntries 是 Kiro 产出内容后要写入的 prompt 缓存前缀。
	cacheEntries []kiro.PromptCacheEntry

	// outputOnce 保证首次产出内容时的处理只执行一次；firstOutputAt 在其中
	// 写入，只在流式转换结束后读取。
	outputOnce    sync.Once
	firstOutputAt time.Time
}

// markKiroOutput 在 Kiro 首次产出内容时调用：记录时间，并在后台写入 prompt
// 缓存前缀。产出内容说明 Kiro 已经处理并缓存了这些前缀；此时就写入，并发的
// 同前缀请求不必等这次响应结束才能命中。在此之前空响应或出错转到别的账号
// 的请求不会写入。
func (s *GatewayService) markKiroOutput(ctx context.Context, up *kiroUpstream) {
	if up == nil || up.conv == nil || !up.conv.Started() {
		return
	}
	up.outputOnce.Do(func() {
		up.firstOutputAt = time.Now()
		if s.kiroPromptCache == nil || len(up.cacheEntries) == 0 {
			return
		}
		accountID, entries := up.account.ID, up.cacheEntries
		storeCtx := context.WithoutCancel(ctx)
		s.runKiroBackground(func() {
			ctx, cancel := context.WithTimeout(storeCtx, kiroPromptCacheTimeout)
			defer cancel()
			if err := s.kiroPromptCache.Store(ctx, accountID, entries); err != nil {
				logger.LegacyPrintf("service.gateway", "[Kiro] Prompt cache store failed: account=%d err=%v", accountID, err)
			}
		})
	})
}

// runKiroBackground 执行不阻塞请求的后台任务。测试中可替换为同步执行。
func (s *GatewayService) runKiroBackground(f func()) {
	if s.kiroBackground != nil {
		s.kiroBackground(f)
		return
	}
	go f()
}

// sessionContextFromGin 在调用方没有 SessionContext 时，从当前请求补上调用方区分因子。
func sessionContextFromGin(c *gin.Context) *SessionContext {
	if c == nil || c.Request == nil {
		return nil
	}
	sc := &SessionContext{
		ClientIP:  ip.GetClientIP(c),
		UserAgent: c.GetHeader("User-Agent"),
	}
	if v, ok := c.Get("api_key"); ok {
		if key, ok := v.(*APIKey); ok && key != nil {
			sc.APIKeyID = key.ID
		}
	}
	return sc
}

// kiroConversationID 用账号、调用方和首条 user 文本派生稳定的 conversationId。
// 对话往后追加消息时这三项不变，所以同一段聊天跨轮使用同一个 id。
func kiroConversationID(accountID int64, body []byte, sc *SessionContext) string {
	return generateSessionUUID(buildStableSessionSeed(accountID, sessionContextDiscriminator(sc), extractFirstUserText(body)))
}

// openKiroUpstream 取 token、把 Anthropic Messages 请求体转换为 Kiro 请求并发送。
// 需要换账号的错误返回 UpstreamFailoverError；其余错误先写给客户端再返回
// error。writeErr 为 nil 时按 Anthropic 格式处理，并走通用的错误透传规则。
// sc 为空时从 gin 请求补调用方区分因子。conversationId 按账号和这段对话固定。
func (s *GatewayService) openKiroUpstream(ctx context.Context, c *gin.Context, account *Account, reqModel string, claudeBody []byte, sc *SessionContext, writeErr kiroErrorWriter) (*kiroUpstream, error) {
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
	if sc == nil {
		sc = sessionContextFromGin(c)
	}

	built, err := kiro.BuildRequest(claudeBody, kiro.BuildOptions{
		Model:          mappedModel,
		ProfileARN:     creds.ProfileARN,
		ConversationID: kiroConversationID(account.ID, claudeBody, sc),
	})
	if err != nil {
		if writeErr == nil {
			writeErr = writeKiroClientError
		}
		writeErr(c, http.StatusBadRequest, "invalid_request_error", err.Error())
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
	// 输入 token 估算和缓存查询与上游请求并行，不占用首字节时间。
	prep, cancelPrep := s.prepareKiroUsage(ctx, account, claudeBody, built.UpstreamModel)
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		cancelPrep()
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, s.handleUpstreamTransportError(ctx, c, account, err, OpsUpstreamErrorEvent{
			UpstreamURL: safeUpstreamURL(req.URL.String()),
		})
	}
	requestID := kiroRequestID(resp.Header)
	if requestID != "" && resp.Header.Get("x-request-id") == "" {
		resp.Header.Set("x-request-id", requestID)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		cancelPrep()
		defer func() { _ = resp.Body.Close() }()
		return nil, s.handleKiroErrorResponse(ctx, c, account, resp, mappedModel, writeErr)
	}

	usage := <-prep
	cancelPrep()
	conv := kiro.NewConverter(kiro.ConverterOptions{
		Model:                reqModel,
		UpstreamModel:        built.UpstreamModel,
		ThinkingRequested:    built.ThinkingRequested,
		Tools:                built.Tools,
		EstimatedInputTokens: usage.estimatedInput,
		CountTokens:          kiroCountTokens,
		CachePlan:            usage.cachePlan,
	})
	return &kiroUpstream{
		account:      account,
		resp:         resp,
		conv:         conv,
		built:        built,
		requestID:    requestID,
		mappedModel:  mappedModel,
		cacheEntries: usage.cacheEntries,
	}, nil
}

// kiroUsagePrep 是计算用量所需、与上游请求无关的准备结果。
type kiroUsagePrep struct {
	estimatedInput int
	cachePlan      *kiro.CachePlan
	cacheEntries   []kiro.PromptCacheEntry
}

// prepareKiroUsage 在后台查询 prompt 缓存并估算输入 token。长上下文的分词
// 和 Redis 往返可能要几十到几百毫秒，与等待上游首包并行。上游失败时调用返回
// 的 cancel：缓存查询随之中止，还没开始的分词跳过。通道带缓冲，没人读取时
// goroutine 也能退出。
func (s *GatewayService) prepareKiroUsage(ctx context.Context, account *Account, claudeBody []byte, upstreamModel string) (<-chan kiroUsagePrep, context.CancelFunc) {
	prepCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	out := make(chan kiroUsagePrep, 1)
	go func() {
		var prep kiroUsagePrep
		prep.cachePlan, prep.cacheEntries = s.kiroCachePlan(prepCtx, account, claudeBody, upstreamModel)
		if prepCtx.Err() == nil {
			if estimated, err := estimateAnthropicCountTokensLocally(claudeBody); err == nil {
				prep.estimatedInput = estimated
			}
		}
		out <- prep
	}()
	return out, cancel
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
// 错误格式，再交给通用的错误处理流程，账号状态按原始错误体判断。writeErr
// 不为 nil 时，不需要换账号的错误按调用方的格式直接写给客户端。
func (s *GatewayService) handleKiroErrorResponse(ctx context.Context, c *gin.Context, account *Account, resp *http.Response, modelKey string, writeErr kiroErrorWriter) error {
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
		return &UpstreamFailoverError{
			StatusCode:      status,
			ResponseBody:    converted,
			ResponseHeaders: resp.Header.Clone(),
		}
	}
	if writeErr != nil {
		writeErr(c, status, anthropicErrorType(status), message)
		return fmt.Errorf("upstream error: %d message=%s", status, sanitizeUpstreamErrorMessage(message))
	}
	_, err := s.handleErrorResponse(ctx, resp, c, account, modelKey)
	return err
}

type kiroRelayOutcome struct {
	firstTokenMs     *int
	clientDisconnect bool
}

type kiroFrameResult struct {
	frame *kiro.Frame
	err   error
}

// kiroStreamError 是读取 Kiro 响应时出现的错误：上游异常事件、读取失败、
// 读超时或空回复。
type kiroStreamError struct {
	status    int
	message   string
	transient bool
	throttled bool
	timeout   bool
	empty     bool
}

func (e *kiroStreamError) Error() string { return "kiro stream error: " + e.message }

// pumpKiroEvents 读取 Kiro event stream，把转换后的事件依次交给 emit，直到
// 上游结束。emit 返回错误时停止读取并原样返回。请求被取消时返回
// context.Canceled，其余读取问题返回 *kiroStreamError。
func (s *GatewayService) pumpKiroEvents(resp *http.Response, conv *kiro.Converter, emit func([]kiro.SSEEvent) error) error {
	frames := make(chan kiroFrameResult, 16)
	done := make(chan struct{})
	defer close(done)
	// readingSince 是解码协程开始等待上游下一帧的时间，没在等上游时为 0。
	// 间隔超时只统计等上游的时间：客户端读得慢时解码协程阻塞在发送上，
	// 那段时间不能算作上游没有数据。
	var readingSince atomic.Int64
	go func() {
		defer close(frames)
		decoder := kiro.NewDecoder(resp.Body)
		for {
			readingSince.Store(time.Now().UnixNano())
			frame, err := decoder.Next()
			readingSince.Store(0)
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

	for {
		select {
		case result, ok := <-frames:
			if !ok {
				tail, err := conv.Finish()
				if errors.Is(err, kiro.ErrEmptyResponse) {
					return &kiroStreamError{status: http.StatusBadGateway, message: "Upstream returned an empty response", transient: true, empty: true}
				}
				return emit(tail)
			}
			if result.err != nil {
				if errors.Is(result.err, context.Canceled) {
					return result.err
				}
				return &kiroStreamError{status: http.StatusBadGateway, message: "upstream stream read error: " + result.err.Error(), transient: true}
			}
			ev := kiro.ParseEvent(result.frame)
			if ev.Kind == kiro.EventException {
				throttled := strings.Contains(strings.ToLower(ev.ExceptionType), "throttling")
				status := http.StatusBadGateway
				if throttled {
					status = http.StatusTooManyRequests
				}
				return &kiroStreamError{
					status:    status,
					message:   strings.TrimSpace(ev.ExceptionType + ": " + ev.Text),
					transient: !throttled,
					throttled: throttled,
				}
			}
			if err := emit(conv.Handle(ev)); err != nil {
				return err
			}

		case <-intervalCh:
			since := readingSince.Load()
			if since == 0 || time.Since(time.Unix(0, since)) < streamInterval {
				continue
			}
			return &kiroStreamError{status: http.StatusGatewayTimeout, message: "stream data interval timeout", timeout: true}
		}
	}
}

// handleKiroStreamError 处理读取 Kiro 响应时的错误。committed 表示客户端已经
// 收到内容：没收到时返回 failover，交给 handler 换账号重试；已收到时由调用方
// 在流中写入错误事件。
func (s *GatewayService) handleKiroStreamError(ctx context.Context, c *gin.Context, account *Account, resp *http.Response, se *kiroStreamError, committed bool, modelKey string) error {
	if s.rateLimitService != nil {
		switch {
		case se.throttled && !committed:
			s.rateLimitService.HandleUpstreamError(ctx, account, se.status, resp.Header, []byte(se.message), modelKey)
		case se.timeout:
			logger.LegacyPrintf("service.gateway", "[Kiro] Stream data interval timeout: account=%d model=%s", account.ID, modelKey)
			s.rateLimitService.HandleStreamTimeout(ctx, account, modelKey)
		}
	}
	if committed {
		return se
	}
	if se.empty {
		logger.LegacyPrintf("service.gateway", "[Kiro] Empty response: account=%d model=%s", account.ID, modelKey)
		return &UpstreamFailoverError{
			StatusCode:             se.status,
			ResponseBody:           anthropicErrorBody(se.status, se.message),
			Reason:                 kiroEmptyResponseReason,
			RetryableOnSameAccount: true,
			RequestScopedTransient: true,
		}
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:            opsUpstreamProxyID(account),
		ProxyName:          opsUpstreamProxyName(account),
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: se.status,
		UpstreamRequestID:  resp.Header.Get("x-request-id"),
		Kind:               "failover",
		Message:            sanitizeUpstreamErrorMessage(se.message),
	})
	return &UpstreamFailoverError{
		StatusCode:             se.status,
		ResponseBody:           anthropicErrorBody(se.status, se.message),
		RetryableOnSameAccount: se.transient,
		RequestScopedTransient: se.transient,
	}
}

// relayKiroResponse 读取 Kiro event stream 并写回客户端。流式请求逐事件转发；
// 非流式请求在结束后一次性返回。客户端收到数据之前出现的错误都转为
// failover，交给 handler 换账号重试。
func (s *GatewayService) relayKiroResponse(
	ctx context.Context,
	c *gin.Context,
	up *kiroUpstream,
	stream bool,
	startTime time.Time,
) (*kiroRelayOutcome, error) {
	account, resp, conv, modelKey := up.account, up.resp, up.conv, up.mappedModel
	outcome := &kiroRelayOutcome{}
	var flusher http.Flusher
	if stream {
		f, ok := c.Writer.(http.Flusher)
		if !ok {
			return nil, errors.New("streaming not supported")
		}
		flusher = f
	}

	headersWritten := false
	emit := func(events []kiro.SSEEvent) error {
		if len(events) == 0 {
			return nil
		}
		s.markKiroOutput(ctx, up)
		if outcome.firstTokenMs == nil {
			ms := int(time.Since(startTime).Milliseconds())
			outcome.firstTokenMs = &ms
		}
		if !stream || outcome.clientDisconnect {
			return nil
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
			// 客户端断开后继续读完上游，用于统计用量。
			outcome.clientDisconnect = true
			logger.LegacyPrintf("service.gateway", "[Kiro] Client disconnected during streaming, continue draining for usage: account=%d", account.ID)
			return nil
		}
		flusher.Flush()
		return nil
	}

	err := s.pumpKiroEvents(resp, conv, emit)
	if err == nil {
		if !stream {
			c.Data(http.StatusOK, "application/json", conv.Message())
		}
		return outcome, nil
	}
	if outcome.clientDisconnect || errors.Is(err, context.Canceled) {
		// 上游已经产出的内容仍按用量计费。
		outcome.clientDisconnect = true
		conv.SettleUsage()
		return outcome, nil
	}
	var se *kiroStreamError
	if !errors.As(err, &se) {
		return nil, err
	}
	committed := stream && conv.Started()
	if err := s.handleKiroStreamError(ctx, c, account, resp, se, committed, modelKey); !committed {
		return nil, err
	}
	errEvent := kiro.SSEEvent{Event: "error", Data: anthropicErrorBody(se.status, se.message)}
	if _, werr := c.Writer.Write(errEvent.AppendTo(nil)); werr == nil {
		flusher.Flush()
	}
	// 客户端已经收到部分内容，按已收到的内容计费。
	conv.SettleUsage()
	return outcome, se
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
