package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// kiroCompatHandler 把 Anthropic SSE 响应转换成入站协议并写回客户端，即
// Chat Completions / Responses 流程里处理 Anthropic 上游响应的那一步。
type kiroCompatHandler func(resp *http.Response, mappedModel string, reasoningEffort *string) (*ForwardResult, error)

// forwardKiroCompat 处理 Kiro 账号的 Chat Completions / Responses 请求。调用方
// 已把请求转换为 Anthropic Messages；Kiro 响应先转换成 Anthropic SSE，再交给
// handle 按 Anthropic 上游响应输出。
func (s *GatewayService) forwardKiroCompat(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	anthropicBody []byte,
	reqModel string,
	clientStream bool,
	writeErr kiroErrorWriter,
	handle kiroCompatHandler,
) (*ForwardResult, error) {
	up, err := s.openKiroUpstream(ctx, c, account, reqModel, anthropicBody, writeErr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = up.resp.Body.Close() }()

	resp, err := s.kiroAnthropicResponse(ctx, c, up, clientStream)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	effort := NormalizeClaudeOutputEffort(gjson.GetBytes(anthropicBody, "output_config.effort").String())
	effort = ApplyThinkingEnabledFallback(effort, anthropicBody, up.mappedModel)
	writerSizeBefore := c.Writer.Size()
	result, err := handle(resp, up.mappedModel, effort)
	var sseErr *sseStreamErrorEventError
	if errors.As(err, &sseErr) {
		return nil, s.sseStreamErrorEventFailover(ctx, c, up.account, resp, sseErr.RawData, up.mappedModel, c.Writer.Size() != writerSizeBefore)
	}
	if result != nil {
		result.RequestID = up.requestID
		result.UpstreamModel = up.built.UpstreamModel
	}
	return result, err
}

// kiroAnthropicResponse 把 Kiro 响应包装成 Anthropic 上游的 SSE 响应。
//
// 非流式请求先读完整个响应，任何错误都按 failover 返回。流式请求通过管道
// 边读边转：Chat Completions / Responses 的输出流程一开始就会写响应头，
// 所以要等到第一段内容出来再返回，此前的错误（包括空回复）按 failover
// 返回，之后的错误以 Anthropic error 事件写进流里。
func (s *GatewayService) kiroAnthropicResponse(ctx context.Context, c *gin.Context, up *kiroUpstream, clientStream bool) (*http.Response, error) {
	header := up.resp.Header.Clone()
	header.Set("Content-Type", "text/event-stream")
	wrap := func(body io.ReadCloser) *http.Response {
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: body}
	}

	if !clientStream {
		var buf bytes.Buffer
		err := s.pumpKiroEvents(up.resp, up.conv, func(events []kiro.SSEEvent) error {
			for _, ev := range events {
				buf.Write(ev.AppendTo(nil))
			}
			return nil
		})
		if err != nil {
			return nil, s.kiroCompatFailure(ctx, c, up, err)
		}
		return wrap(io.NopCloser(&buf)), nil
	}

	pr, pw := io.Pipe()
	body := &kiroPipeBody{PipeReader: pr, upstream: up.resp.Body, done: make(chan struct{})}
	started := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		defer close(body.done)
		var once sync.Once
		err := s.pumpKiroEvents(up.resp, up.conv, func(events []kiro.SSEEvent) error {
			if len(events) == 0 {
				return nil
			}
			once.Do(func() { close(started) })
			var data []byte
			for _, ev := range events {
				data = ev.AppendTo(data)
			}
			_, err := pw.Write(data)
			return err
		})
		var se *kiroStreamError
		if errors.As(err, &se) && up.conv.Started() && !body.closed.Load() {
			// 已经开始输出，只能在流里报告错误。committed 路径不访问 gin.Context。
			_ = s.handleKiroStreamError(ctx, c, up.account, up.resp, se, true, up.mappedModel)
			errEvent := kiro.SSEEvent{Event: "error", Data: anthropicErrorBody(se.status, se.message)}
			_, _ = pw.Write(errEvent.AppendTo(nil))
		}
		_ = pw.Close()
		finished <- err
	}()

	select {
	case <-started:
		return wrap(body), nil
	case err := <-finished:
		_ = body.Close()
		if err == nil {
			// Finish 保证正常结束时至少有一个事件，这里只是兜底。
			err = &kiroStreamError{status: http.StatusBadGateway, message: "Upstream returned an empty response", transient: true, empty: true}
		}
		return nil, s.kiroCompatFailure(ctx, c, up, err)
	}
}

// kiroPipeBody 是流式转换的响应体。Close 同时关闭 Kiro 上游响应，并等待
// 转换 goroutine 退出，保证请求结束后它不再使用请求上下文。
type kiroPipeBody struct {
	*io.PipeReader
	upstream io.Closer
	done     chan struct{}
	closed   atomic.Bool
}

func (b *kiroPipeBody) Close() error {
	b.closed.Store(true)
	err := b.PipeReader.Close()
	_ = b.upstream.Close()
	<-b.done
	return err
}

// kiroCompatFailure 处理客户端收到内容之前的读取错误。
func (s *GatewayService) kiroCompatFailure(ctx context.Context, c *gin.Context, up *kiroUpstream, err error) error {
	var se *kiroStreamError
	if errors.As(err, &se) {
		return s.handleKiroStreamError(ctx, c, up.account, up.resp, se, false, up.mappedModel)
	}
	return err
}
