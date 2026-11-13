// 本文件把 ACP 一轮的文本增量转换为协议层通用的有序 ResponseEvent。
package devinacp

import (
	"context"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/vta-itsec/devin-to-api/internal/llm"
)

// eventStream 以 channel 队列实现 llm.ResponseStream；
// 事件序列与 connect 后端一致：start → text_start → text_delta* → text_end → done/error。
type eventStream struct {
	model   string
	partial llm.AssistantMessage
	text    strings.Builder
	usage   llm.Usage
	reason  llm.StopReason

	mu       sync.Mutex
	queue    []llm.ResponseEvent
	notify   chan struct{}
	finished bool
}

func newEventStream(model string) *eventStream {
	s := &eventStream{model: model, notify: make(chan struct{}, 1)}
	s.partial = llm.AssistantMessage{
		API:          "acp",
		Provider:     "devin",
		Model:        model,
		StopReason:   llm.StopReasonPending,
		TimestampMS:  time.Now().UnixMilli(),
		ResponseModel: model,
	}
	return s
}

func (s *eventStream) emit(event llm.ResponseEvent) {
	s.mu.Lock()
	s.queue = append(s.queue, event)
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *eventStream) partialCopy() *llm.AssistantMessage {
	p := s.partial
	return &p
}

// pushText 追加一段助手文本增量。
func (s *eventStream) pushText(delta string) {
	s.mu.Lock()
	if !s.started() {
		s.mu.Unlock()
		s.begin()
		s.mu.Lock()
	}
	if s.textOpen() {
		s.text.WriteString(delta)
		s.partial.Content[0] = llm.TextContent{Text: s.text.String()}
	}
	s.mu.Unlock()
	s.emit(llm.ResponseEvent{
		Type: llm.ResponseEventTextDelta, ContentIndex: 0, Delta: delta, Partial: s.partialCopy(),
	})
}

func (s *eventStream) started() bool  { return len(s.partial.Content) > 0 || s.finished }
func (s *eventStream) textOpen() bool { return len(s.partial.Content) > 0 }

// begin 发出 start + text_start。
func (s *eventStream) begin() {
	s.mu.Lock()
	s.partial.Content = []llm.Content{llm.TextContent{}}
	s.mu.Unlock()
	s.emit(llm.ResponseEvent{Type: llm.ResponseEventStart, Partial: s.partialCopy()})
	s.emit(llm.ResponseEvent{Type: llm.ResponseEventTextStart, ContentIndex: 0, Partial: s.partialCopy()})
}

// finish 记录 prompt 返回的 stopReason/usage。
func (s *eventStream) finish(stopReason string, input, output, total int64) {
	s.mu.Lock()
	s.usage = llm.Usage{Input: input, Output: output, TotalTokens: total}
	switch stopReason {
	case "max_tokens":
		s.reason = llm.StopReasonLength
	default:
		s.reason = llm.StopReasonStop
	}
	s.mu.Unlock()
}

func (s *eventStream) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.finished = true
	msg := llm.AssistantMessage{
		API: "acp", Provider: "devin", Model: s.model,
		StopReason: llm.StopReasonError, ErrorMessage: err.Error(), TimestampMS: time.Now().UnixMilli(),
	}
	s.queue = append(s.queue, llm.ResponseEvent{Type: llm.ResponseEventError, Reason: llm.StopReasonError, Error: &msg})
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// close 在 prompt 正常结束后补齐 text_end + done。
func (s *eventStream) close() {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	if len(s.partial.Content) == 0 {
		s.partial.Content = []llm.Content{llm.TextContent{}}
	}
	s.partial.StopReason = s.reason
	if s.partial.StopReason == "" {
		s.partial.StopReason = llm.StopReasonStop
	}
	s.partial.Usage = s.usage
	final := s.partial
	s.queue = append(s.queue,
		llm.ResponseEvent{Type: llm.ResponseEventTextEnd, ContentIndex: 0, Content: s.text.String(), Partial: s.partialCopy()},
		llm.ResponseEvent{Type: llm.ResponseEventDone, Reason: s.partial.StopReason, Message: &final},
	)
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// Recv 依序返回事件；队列空时等待新事件或 ctx 取消。
func (s *eventStream) Recv(ctx context.Context) (llm.ResponseEvent, error) {
	for {
		s.mu.Lock()
		if len(s.queue) > 0 {
			event := s.queue[0]
			s.queue = s.queue[1:]
			s.mu.Unlock()
			return event, nil
		}
		if s.finished {
			s.mu.Unlock()
			return llm.ResponseEvent{}, io.EOF
		}
		s.mu.Unlock()
		select {
		case <-s.notify:
		case <-ctx.Done():
			return llm.ResponseEvent{}, ctx.Err()
		}
	}
}

// replayStream 回放缓存回复（客户端重发同一消息链时的幂等响应）。
func replayStream(model, text string) llm.ResponseStream {
	s := newEventStream(model)
	if text != "" {
		s.pushText(text)
	}
	s.close()
	return s
}
