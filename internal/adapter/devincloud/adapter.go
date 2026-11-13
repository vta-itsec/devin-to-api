// 本文件实现把 chat 协议请求映射为 Devin Cloud session 的适配器。
//
// 用法：VS Code 里把 model 设为 "devin-cloud"（或 "devin-cloud@<snapshot_id>"），
// 每个 VS Code 对话对应一个 cloud Devin session：
//   - 首轮：POST /v1/sessions {prompt: 完整 transcript, snapshot_id?}
//   - 后续：POST /v1/sessions/{id}/message {message: 新轮次}
//   - 轮询 GET /v1/sessions/{id} 直到 finished/blocked，把产出文本作为回复。
//
// Cloud session 是异步自治任务，单轮可能数分钟——流式请求期间以
// thinking delta 回报进度，最终答案走 text。
package devincloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vta-itsec/devin-to-api/internal/adapter"
	"github.com/vta-itsec/devin-to-api/internal/llm"
)

// CloudModel 是路由到本适配器的模型名；@snapshot-id 后缀覆盖默认快照。
const CloudModel = "devin-cloud"

// IsCloudModel 报告模型名是否路由到 Devin Cloud。
func IsCloudModel(model string) bool {
	return model == CloudModel || strings.HasPrefix(model, CloudModel+"@")
}

// ParseModel 拆出 snapshot 覆盖（devin-cloud@snapshot-xxx）。
func ParseModel(model string) (snapshotID string) {
	if i := strings.Index(model, "@"); i >= 0 {
		return model[i+1:]
	}
	return ""
}

// Config 是 cloud 适配器配置。
type Config struct {
	BaseURL    string // 默认 https://api.devin.ai
	APIKey     string // 服务端 Bearer（DEVIN_API_KEY），必填
	SnapshotID string // 默认 snapshot（模型名 @ 后缀可覆盖）
	Workspace  string // 会话映射文件所在目录
	// PollInterval 轮询间隔，默认 5s。
	PollInterval time.Duration
	// ProgressEvery 每多少次轮询回报一次 thinking 进度，默认每 ~30s 一条。
	ProgressEvery int
}

// Adapter 实现 adapter.Adapter。
type Adapter struct {
	cfg    Config
	client *client
	store  *cloudStore
}

// New 创建 cloud 适配器；APIKey 为空也能建（Stream 时报错，避免拉垮整个 proxy）。
func New(cfg Config) (*Adapter, error) {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.devin.ai"
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.ProgressEvery <= 0 {
		cfg.ProgressEvery = 6
	}
	if cfg.Workspace == "" {
		cfg.Workspace = filepath.Join(os.Getenv("HOME"), ".devin-to-api", "workspace")
	}
	if err := os.MkdirAll(cfg.Workspace, 0o755); err != nil {
		return nil, fmt.Errorf("create cloud workspace %s: %w", cfg.Workspace, err)
	}
	store, err := newCloudStore(defaultCloudStateFile(cfg.Workspace))
	if err != nil {
		return nil, err
	}
	return &Adapter{cfg: cfg, client: newClient(cfg.BaseURL, cfg.APIKey), store: store}, nil
}

// ListModels 暴露 devin-cloud 模型条目。
func (a *Adapter) ListModels(context.Context) ([]adapter.ModelInfo, error) {
	return []adapter.ModelInfo{{ID: CloudModel, OwnedBy: "devin-cloud"}}, nil
}

// Stream 开始一轮 cloud session 调用。
func (a *Adapter) Stream(ctx context.Context, req llm.RequestMessages) (llm.ResponseStream, error) {
	if a.cfg.APIKey == "" {
		return nil, errors.New("devin cloud api key is not configured (set DEVIN_API_KEY)")
	}
	stream := newEventStream(req.Model, "devin-cloud")
	go a.run(ctx, req, stream)
	return stream, nil
}

// run 完成 路由→create/message→轮询→产出 的整轮。
func (a *Adapter) run(ctx context.Context, req llm.RequestMessages, stream *eventStream) {
	chain := hashChain(req.Messages)
	match := a.store.match(chain, req.ConversationID)
	if match.replay && match.session != nil {
		stream.pushText(match.session.Reply)
		stream.close()
		return
	}

	var s *cloudSession
	var prompt string
	switch {
	case match.session != nil:
		s = match.session
		tail := tailAfterUserCount(req.Messages, match.consumed)
		if match.rewritten {
			tail = tailFromLastAssistant(req.Messages)
		}
		prompt = joinUserTexts(tail)
	default:
		prompt = joinTexts(req.Messages)
	}
	if strings.TrimSpace(prompt) == "" {
		stream.pushText(match.session.Reply)
		stream.close()
		return
	}

	stream.pushThinking(fmt.Sprintf("routing: %s\n", describeRoute(match, s)))

	if s == nil {
		snapshot := ParseModel(req.Model)
		if snapshot == "" {
			snapshot = a.cfg.SnapshotID
		}
		created, err := a.client.createSession(ctx, prompt, snapshot, map[string]any{"unlisted": true})
		if err != nil {
			stream.fail(fmt.Errorf("create cloud session: %w", err))
			return
		}
		if created.SessionID == "" {
			stream.fail(errors.New("create cloud session: response has no session_id"))
			return
		}
		s = &cloudSession{ID: created.SessionID, URL: created.URL}
		stream.pushThinking(fmt.Sprintf("session created: %s %s\n", s.ID, s.URL))
	} else {
		if err := a.client.sendMessage(ctx, s.ID, prompt); err != nil {
			stream.fail(fmt.Errorf("message cloud session %s: %w", s.ID, err))
			return
		}
	}

	// 同一会话串行化轮询。
	a.store.lock(s.ID).Lock()
	defer a.store.lock(s.ID).Unlock()

	final := a.poll(ctx, stream, s)
	if final.err != nil {
		stream.fail(final.err)
		return
	}
	reply := final.text
	if s.URL != "" && !strings.Contains(reply, s.URL) {
		reply += "\n\n---\nsession: " + s.URL
	}
	a.store.register(s, req.Model, req.ConversationID, chain, match.rewritten, reply)
	stream.pushText(reply)
	stream.close()
}

// pollResult 是一轮轮询的产出。
type pollResult struct {
	text string
	err  error
}

// poll 轮询 session 直到终态；进度以 thinking delta 回报。
func (a *Adapter) poll(ctx context.Context, stream *eventStream, s *cloudSession) pollResult {
	started := time.Now()
	ticks := 0
	for {
		status, err := a.client.getSession(ctx, s.ID)
		if err != nil {
			return pollResult{err: fmt.Errorf("poll cloud session %s: %w", s.ID, err)}
		}
		if s.URL == "" && status.URL != "" {
			s.URL = status.URL
			stream.pushThinking("session url: " + s.URL + "\n")
		}
		ticks++
		if ticks%a.cfg.ProgressEvery == 0 {
			stream.pushThinking(fmt.Sprintf("status: %s (%s elapsed)\n",
				status.statusText(), time.Since(started).Round(time.Second)))
		}
		if status.terminal() {
			return pollResult{text: extractAnswer(status)}
		}
		if status.dead() {
			return pollResult{err: fmt.Errorf("cloud session %s is %s", s.ID, status.statusText())}
		}
		select {
		case <-ctx.Done():
			return pollResult{err: ctx.Err()}
		case <-time.After(a.cfg.PollInterval):
		}
	}
}

func describeRoute(match cloudMatch, s *cloudSession) string {
	switch {
	case s == nil:
		return "new cloud session"
	case match.rewritten:
		return fmt.Sprintf("resume %s (history rewritten)", s.ID)
	default:
		return fmt.Sprintf("resume %s (+%d msgs)", s.ID, match.consumed)
	}
}

// joinTexts 把消息渲染成 transcript 行（带角色前缀，首轮 prompt 用）。
func joinTexts(messages []llm.Message) string {
	var parts []string
	for _, m := range messages {
		if line := strings.TrimSpace(messageText(m)); line != "" {
			parts = append(parts, line)
		}
	}
	return strings.Join(parts, "\n")
}

// joinUserTexts 只取用户/工具消息正文（/message 追加轮次用）。
func joinUserTexts(messages []llm.Message) string {
	var parts []string
	for _, m := range messages {
		switch m := m.(type) {
		case llm.UserMessage:
			if text := strings.TrimSpace(flattenContent(m.Content)); text != "" {
				parts = append(parts, text)
			}
		case llm.ToolResultMessage:
			if text := strings.TrimSpace(flattenContent(m.Content)); text != "" {
				parts = append(parts, "tool result: "+text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// extractAnswer 从 session 详情里提取回复文本：
// 依次尝试 structured_output/result/output/summary、最后一条 devin 消息，
// 兜底返回精简 JSON，保证信息不丢。
func extractAnswer(s sessionStatus) string {
	raw := s.Extra
	for _, key := range []string{"structured_output", "result", "output", "summary", "final_message", "status_message"} {
		if text := anyToText(raw[key]); text != "" {
			return text
		}
	}
	if msgs, ok := raw["messages"].([]any); ok {
		for i := len(msgs) - 1; i >= 0; i-- {
			entry, ok := msgs[i].(map[string]any)
			if !ok {
				continue
			}
			for _, key := range []string{"message", "text", "content"} {
				if text, ok := entry[key].(string); ok && strings.TrimSpace(text) != "" {
					return text
				}
			}
		}
	}
	if s.URL != "" {
		return fmt.Sprintf("Cloud session %s is %s — see %s", s.SessionID, s.statusText(), s.URL)
	}
	data, _ := json.MarshalIndent(raw, "", "  ")
	return string(data)
}

func anyToText(v any) string {
	switch v := v.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		for _, key := range []string{"result", "output", "summary", "message", "text"} {
			if text, ok := v[key].(string); ok && strings.TrimSpace(text) != "" {
				return text
			}
		}
		if len(v) > 0 {
			data, _ := json.Marshal(v)
			return string(data)
		}
	}
	return ""
}
