// 本文件把 cloud session 的进度/产出转换为有序 ResponseEvent。
package devincloud

import (
	"context"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/vta-itsec/devin-to-api/internal/llm"
)

// eventStream 是 cloud 适配器的 ResponseStream：
// thinking delta（进度）→ text delta（答案）→ done/error。
type eventStream struct {
	model    string
	provider string
	partial  llm.AssistantMessage
	text     strings.Builder
	thinking strings.Builder

	mu       sync.Mutex
	queue    []llm.ResponseEvent
	notify   chan struct{}
	finished bool
}

func newEventStream(model, provider string) *eventStream {
	s := &eventStream{model: model, provider: provider, notify: make(chan struct{}, 1)}
	s.partial = llm.AssistantMessage{
		API:           "devin-cloud",
		Provider:      provider,
		Model:         model,
		StopReason:    llm.StopReasonPending,
		TimestampMS:   time.Now().UnixMilli(),
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

// ensureStarted 补发 start 事件；调用方须持有 s.mu（emit 也取锁，不能嵌套）。
func (s *eventStream) ensureStarted() {
	if len(s.partial.Content) == 0 {
		s.partial.Content = []llm.Content{llm.ThinkingContent{}, llm.TextContent{}}
		s.queue = append(s.queue, llm.ResponseEvent{Type: llm.ResponseEventStart, Partial: s.partialCopy()})
	}
}

// pushThinking 追加一段思考/进度增量（轮询状态、路由信息）。
func (s *eventStream) pushThinking(delta string) {
	s.mu.Lock()
	s.ensureStarted()
	s.thinking.WriteString(delta)
	s.partial.Content[0] = llm.ThinkingContent{Thinking: s.thinking.String()}
	s.mu.Unlock()
	s.emit(llm.ResponseEvent{
		Type: llm.ResponseEventThinkingDelta, ContentIndex: 0, Delta: delta, Partial: s.partialCopy(),
	})
}

// pushText 追加一段最终答案增量。
func (s *eventStream) pushText(delta string) {
	s.mu.Lock()
	s.ensureStarted()
	s.text.WriteString(delta)
	s.partial.Content[1] = llm.TextContent{Text: s.text.String()}
	s.mu.Unlock()
	s.emit(llm.ResponseEvent{
		Type: llm.ResponseEventTextDelta, ContentIndex: 1, Delta: delta, Partial: s.partialCopy(),
	})
}

func (s *eventStream) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.finished = true
	msg := llm.AssistantMessage{
		API: "devin-cloud", Provider: s.provider, Model: s.model,
		StopReason: llm.StopReasonError, ErrorMessage: err.Error(), TimestampMS: time.Now().UnixMilli(),
	}
	s.queue = append(s.queue, llm.ResponseEvent{Type: llm.ResponseEventError, Reason: llm.StopReasonError, Error: &msg})
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

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
	s.partial.StopReason = llm.StopReasonStop
	final := s.partial
	s.queue = append(s.queue,
		llm.ResponseEvent{Type: llm.ResponseEventTextEnd, ContentIndex: 1, Content: s.text.String(), Partial: s.partialCopy()},
		llm.ResponseEvent{Type: llm.ResponseEventDone, Reason: llm.StopReasonStop, Message: &final},
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
