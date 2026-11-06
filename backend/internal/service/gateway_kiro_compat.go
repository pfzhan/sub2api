package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

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
	startTime := time.Now()
	up, err := s.openKiroUpstream(ctx, c, account, reqModel, anthropicBody, nil, writeErr)
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
		outputStarted := c.Writer.Size() != writerSizeBefore
		failoverErr := s.sseStreamErrorEventFailover(ctx, c, up.account, resp, sseErr.RawData, up.mappedModel, outputStarted)
		if outputStarted && up.conv.Started() {
			// 客户端已经收到部分内容，不能 failover；按已收到的内容计费。
			partial := up.forwardResult(reqModel, clientStream, startTime, kiroCompatOutcome(up, startTime))
			return partial, fmt.Errorf("kiro stream failed after output started: %w", sseErr)
		}
		return nil, failoverErr
	}
	if pb, ok := resp.Body.(*kiroPipeBody); ok && up.conv.Started() {
		// Close 等转换协程退出，之后才能读它的结果和 Converter。Kiro 流没有正常
		// 结束（请求被取消、客户端断开后管道关闭）时，handler 只从 message_start
		// 拿到输入用量；已经发出的输出按 Converter 结算。
		_ = pb.Close()
		if pb.pumpErr != nil {
			up.conv.SettleUsage()
			partial := up.forwardResult(reqModel, clientStream, startTime, kiroCompatOutcome(up, startTime))
			if result == nil {
				result = partial
			} else {
				result.Usage = partial.Usage
				if result.FirstTokenMs == nil {
					result.FirstTokenMs = partial.FirstTokenMs
				}
			}
		}
	}
	if result != nil {
		result.RequestID = up.requestID
		result.UpstreamModel = up.built.UpstreamModel
	}
	return result, err
}

// kiroCompatOutcome 按首次产出内容的时间生成首字耗时。
func kiroCompatOutcome(up *kiroUpstream, startTime time.Time) *kiroRelayOutcome {
	outcome := &kiroRelayOutcome{}
	if !up.firstOutputAt.IsZero() {
		ms := int(up.firstOutputAt.Sub(startTime).Milliseconds())
		outcome.firstTokenMs = &ms
	}
	return outcome
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
			if len(events) > 0 {
				s.markKiroOutput(ctx, up)
			}
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
			s.markKiroOutput(ctx, up)
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
			// 先结算再写错误事件：读到错误事件的一方能看到结算后的用量。
			up.conv.SettleUsage()
			errEvent := kiro.SSEEvent{Event: "error", Data: anthropicErrorBody(se.status, se.message)}
			_, _ = pw.Write(errEvent.AppendTo(nil))
		}
		body.pumpErr = err
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
	// pumpErr 是转换协程结束时 pumpKiroEvents 的返回值，done 关闭后才能读。
	pumpErr error
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
