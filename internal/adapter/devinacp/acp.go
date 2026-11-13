// 本文件实现 ACP 会话后端：每个客户端对话绑定一个持久的 devin CLI 会话。
//
// 与 connect 后端的区别：connect 后端每请求一个随机 trajectory，上游无状态；
// ACP 后端用 `devin acp` 的 session/new + session/load 把同一 VS Code 聊天的
// 每轮都挂到同一个 devin CLI 会话上——devin 端保留真实上下文（还能执行自身
// 的 fs/exec 工具，workspace 内的代码对它可见）。
package devinacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vta-itsec/devin-to-api/internal/adapter"
	"github.com/vta-itsec/devin-to-api/internal/llm"
)

// Config 是 ACP 后端配置。
type Config struct {
	// Bin 是 devin 可执行文件路径（默认 "devin"，走 PATH）。
	Bin string
	// WorkDir 是 devin 会话的工作目录（workspace）：fs 工具可见范围，
	// 也作为 session/new、session/load 的 cwd。
	WorkDir string
	// Model 是默认模型；请求带 model 时按请求走。
	Model string
	// Mode 是会话权限模式（bypass/smart/ask/...）；默认 bypass。
	Mode string
	// StateFile 持久化「对话链 → sessionId」映射；默认 <WorkDir>/.devin-to-api-sessions.json。
	StateFile string
	// SessionTTL 是映射条目的空闲回收时间；默认 7 天。
	SessionTTL time.Duration
	// Models 复用 connect adapter 的模型目录实现。
	Models modelLister
}

type modelLister interface {
	ListModels(context.Context) ([]adapter.ModelInfo, error)
}

// Adapter 实现 adapter.Adapter：Stream 按对话链前缀路由到 devin 会话。
type Adapter struct {
	config Config
	store  *sessionStore
}

var _ adapter.Adapter = (*Adapter)(nil)

// New 创建 ACP adapter。
func New(config Config) (*Adapter, error) {
	if strings.TrimSpace(config.Bin) == "" {
		config.Bin = "devin"
	}
	if strings.TrimSpace(config.WorkDir) == "" {
		return nil, errors.New("devinacp: workspace directory is required")
	}
	if strings.TrimSpace(config.Mode) == "" {
		config.Mode = "bypass"
	}
	if strings.TrimSpace(config.StateFile) == "" {
		config.StateFile = defaultStateFile(config.WorkDir)
	}
	if config.SessionTTL <= 0 {
		config.SessionTTL = 7 * 24 * time.Hour
	}
	if err := os.MkdirAll(config.WorkDir, 0o755); err != nil {
		return nil, fmt.Errorf("devinacp: create workspace %s: %w", config.WorkDir, err)
	}
	store, err := newSessionStore(config.StateFile, config.SessionTTL)
	if err != nil {
		return nil, err
	}
	return &Adapter{config: config, store: store}, nil
}

// ListModels 复用 connect adapter 的目录（同一账号）。
func (a *Adapter) ListModels(ctx context.Context) ([]adapter.ModelInfo, error) {
	if a.config.Models == nil {
		return nil, errors.New("devinacp: no model lister configured")
	}
	return a.config.Models.ListModels(ctx)
}

// SessionCount 返回当前映射的会话数（健康检查展示）。
func (a *Adapter) SessionCount() int { return a.store.sessionCount() }

// Stream 路由请求到对应 devin 会话并返回响应事件流。
func (a *Adapter) Stream(ctx context.Context, request llm.RequestMessages) (llm.ResponseStream, error) {
	if err := request.Validate(); err != nil {
		return nil, fmt.Errorf("validate request: %w", err)
	}
	if len(request.Messages) == 0 {
		return nil, errors.New("devinacp: at least one message is required")
	}
	model := strings.TrimSpace(request.Model)
	if model == "" {
		model = a.config.Model
	}
	chain := userChainOf(request.Messages)
	conversationID := strings.TrimSpace(request.ConversationID)
	match := a.store.match(request.Messages, chain, conversationID)

	// 幂等：客户端重发完全相同的链 → 回放缓存回复。
	if match.replay {
		return replayStream(model, match.session.LastReply), nil
	}
	newMessages := tailAfter(request.Messages, match.consumed)
	// 显式 conversation_id 的会话允许客户端只发新轮次：前缀匹配为空时，
	// 兜底发送最后一条用户消息（devin 会话自己保有上下文）。
	if len(newMessages) == 0 && match.session != nil {
		newMessages = lastUserMessages(request.Messages)
	}
	prompt, err := buildPrompt(request.SystemPrompt, newMessages, match.consumed == 0)
	if err != nil {
		return nil, err
	}

	var lock *sync.Mutex
	if match.session != nil {
		lock = a.store.lock(match.session.ID)
		lock.Lock()
	}

	stream := newEventStream(model)
	go func() {
		if lock != nil {
			defer lock.Unlock()
		}
		a.runTurn(ctx, stream, match.session, model, prompt, chain, conversationID, request.Messages)
	}()
	return stream, nil
}

// runTurn 执行一轮：spawn → 会话 → prompt → 事件流 → 注册链。
// existing 为 nil 时先试 session/list 收养（state 文件丢失后按首条
// prompt 的 title 找回 devin 会话），收不到才 session/new。
func (a *Adapter) runTurn(ctx context.Context, stream *eventStream, existing *session, model string, prompt []map[string]any, chain []string, conversationID string, messages []llm.Message) {
	proc, err := spawnAcp(ctx, a.config.Bin, model, a.config.WorkDir)
	if err != nil {
		stream.fail(err)
		return
	}
	defer proc.kill()

	setupCtx, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()
	if _, err := proc.call(setupCtx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": true, "writeTextFile": true},
			"terminal": false,
		},
	}); err != nil {
		stream.fail(fmt.Errorf("acp initialize: %w", err))
		return
	}

	cwd, _ := filepath.Abs(a.config.WorkDir)
	sessParams := func() map[string]any {
		p := map[string]any{"cwd": cwd, "mcpServers": []any{}}
		return p
	}
	var sessionID string
	if existing != nil {
		// session/load 重放历史 update：读响应前先置 loading，拿到结果后清除。
		proc.loading.Store(true)
		params := sessParams()
		params["sessionId"] = existing.ID
		_, loadErr := proc.call(setupCtx, "session/load", params)
		proc.loading.Store(false)
		// load 期间可能还有 update 在管道里，清空。
		drainUpdates(proc)
		if loadErr == nil {
			sessionID = existing.ID
		}
	}
	if sessionID == "" && isContinuation(chain, messages) {
		// 收养：state 丢失/换机时，按首条用户消息文本找回 devin 会话。
		// 只对「延续中的对话」收养——裸 turn-1 与现存会话开场相同属于
		// 新聊天，收养会把别人的 session 抢过来污染上下文。
		// 收养的会话 transcript 已含全部历史 → prompt 只发最后一条用户消息。
		if adopted := a.adoptByTitle(setupCtx, proc, cwd, firstUserText(messages)); adopted != "" {
			sessionID = adopted
			if lastUser := lastUserPrompt(messages); len(lastUser) > 0 {
				prompt = lastUser
			}
		}
	}
	if sessionID == "" {
		result, err := proc.call(setupCtx, "session/new", sessParams())
		if err != nil {
			stream.fail(fmt.Errorf("acp session/new: %w", err))
			return
		}
		var created struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(result, &created); err != nil || created.SessionID == "" {
			stream.fail(errors.New("devin opened no session"))
			return
		}
		sessionID = created.SessionID
	}
	_, _ = proc.call(setupCtx, "session/set_mode", map[string]any{
		"sessionId": sessionID, "modeId": a.config.Mode,
	})
	cancel() // setup 完成，提示轮由请求 ctx 控制时长

	// 流式收集：agent_message_chunk → 文本增量。
	var reply strings.Builder
	done := make(chan error, 1)
	go func() {
		result, err := proc.call(ctx, "session/prompt", map[string]any{
			"sessionId": sessionID,
			"prompt":    prompt,
		})
		if err != nil {
			done <- err
			return
		}
		var turn struct {
			StopReason string `json:"stopReason"`
			Usage      struct {
				InputTokens  int64 `json:"inputTokens"`
				OutputTokens int64 `json:"outputTokens"`
				TotalTokens  int64 `json:"totalTokens"`
			} `json:"usage"`
		}
		_ = json.Unmarshal(result, &turn)
		stream.finish(turn.StopReason, turn.Usage.InputTokens, turn.Usage.OutputTokens, turn.Usage.TotalTokens)
		done <- nil
	}()

	for {
		select {
		case msg := <-proc.updates:
			text := agentChunkText(msg.Params)
			if text != "" {
				reply.WriteString(text)
				stream.pushText(text)
			}
		case err := <-done:
			if err != nil {
				stream.fail(err)
				return
			}
			// prompt 返回后管道里可能还有最后的 chunk。
			for {
				select {
				case msg := <-proc.updates:
					if t := agentChunkText(msg.Params); t != "" {
						reply.WriteString(t)
						stream.pushText(t)
					}
				default:
					goto flushed
				}
			}
		flushed:
			// 回复尾部嵌入会话标签：客户端原样回传后，下一轮可直接定位会话。
			tag := sessionTagFor(sessionID)
			reply.WriteString(tag)
			stream.pushText(tag)
			replyHash := messageHash(llm.AssistantMessage{Content: []llm.Content{llm.TextContent{Text: reply.String()}}})
			a.store.register(&session{ID: sessionID}, model, conversationID, chain, replyHash, reply.String())
			stream.close()
			return
		case <-ctx.Done():
			stream.fail(ctx.Err())
			return
		case <-proc.done:
			stream.fail(errors.New("devin process exited mid-turn"))
			return
		}
	}
}

// isContinuation 报告请求是否是一次「对话延续」（≥2 条用户消息或带助手
// 回声）。只有延续请求才允许收养 devin 会话。
func isContinuation(chain []string, messages []llm.Message) bool {
	if len(chain) > 1 {
		return true
	}
	for _, m := range messages {
		if _, ok := m.(llm.AssistantMessage); ok {
			return true
		}
	}
	return false
}

// lastUserMessages 返回只含最后一条用户消息的切片（显式会话兜底用）。
func lastUserMessages(messages []llm.Message) []llm.Message {
	for i := len(messages) - 1; i >= 0; i-- {
		if _, ok := messages[i].(llm.UserMessage); ok {
			return messages[i : i+1]
		}
	}
	return nil
}

// lastUserPrompt 提取最后一条用户消息为 ACP prompt blocks（收养会话用）。
func lastUserPrompt(messages []llm.Message) []map[string]any {
	for i := len(messages) - 1; i >= 0; i-- {
		if user, ok := messages[i].(llm.UserMessage); ok {
			var blocks []map[string]any
			for _, block := range user.Content {
				switch block := block.(type) {
				case llm.TextContent:
					blocks = append(blocks, map[string]any{"type": "text", "text": block.Text})
				case llm.ImageContent:
					data := block.Data
					if _, encoded, ok := strings.Cut(data, ","); ok && strings.HasPrefix(data, "data:") {
						data = encoded
					}
					mime := block.MIMEType
					if mime == "" {
						mime = "image/png"
					}
					blocks = append(blocks, map[string]any{"type": "image", "data": data, "mimeType": mime})
				}
			}
			return blocks
		}
	}
	return nil
}

// adoptByTitle 在 state 映射缺失时通过 session/list 的 title（devin 取的
// 首条 prompt 摘要）找回旧 devin 会话并 session/load。title 可能被截断，
// 用双向前缀比较；多条命中时不收养（避免误认）。失败返回 ""。
func (a *Adapter) adoptByTitle(ctx context.Context, proc *rpcProcess, cwd, firstText string) string {
	firstText = strings.TrimSpace(firstText)
	if len(firstText) < 8 {
		return ""
	}
	result, err := proc.call(ctx, "session/list", map[string]any{})
	if err != nil {
		return ""
	}
	var list struct {
		Sessions []struct {
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
			Title     string `json:"title"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(result, &list); err != nil {
		return ""
	}
	var hit string
	for _, s := range list.Sessions {
		if s.Cwd != cwd {
			continue
		}
		title := strings.TrimSpace(s.Title)
		if title == "" {
			continue
		}
		// devin title 通常是 prompt 开头（可能被截断/加省略号）。
		if !strings.HasPrefix(firstText, title) && !strings.HasPrefix(title, firstText) {
			continue
		}
		if hit != "" {
			return "" // 多个候选 → 不收养，宁可新建。
		}
		hit = s.SessionID
	}
	if hit == "" {
		return ""
	}
	proc.loading.Store(true)
	_, err = proc.call(ctx, "session/load", map[string]any{
		"sessionId": hit, "cwd": cwd, "mcpServers": []any{},
	})
	proc.loading.Store(false)
	drainUpdates(proc)
	if err != nil {
		return ""
	}
	return hit
}

func drainUpdates(proc *rpcProcess) {
	for {
		select {
		case <-proc.updates:
		default:
			return
		}
	}
}

// agentChunkText 从 session/update 提取助手文本增量。
// update.content 可能是 {"type":"text","text":"..."} 或直接 {"text":"..."}。
func agentChunkText(params json.RawMessage) string {
	var envelope struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
			Content       struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Text string `json:"text"`
		} `json:"update"`
	}
	if err := json.Unmarshal(params, &envelope); err != nil {
		return ""
	}
	if envelope.Update.SessionUpdate != "agent_message_chunk" {
		return ""
	}
	if envelope.Update.Content.Text != "" {
		return envelope.Update.Content.Text
	}
	return envelope.Update.Text
}

// buildPrompt 把新消费的消息序列化为 ACP prompt content blocks。
// 单条用户消息直接映射（含图片）；多条消息拼接为带角色标记的一段文本；
// 会话首个 prompt 带上 system prompt。
func buildPrompt(systemPrompt string, messages []llm.Message, first bool) ([]map[string]any, error) {
	if len(messages) == 0 {
		return nil, errors.New("devinacp: nothing new to prompt")
	}
	var blocks []map[string]any
	if first && strings.TrimSpace(systemPrompt) != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": "<system>\n" + systemPrompt + "\n</system>\n"})
	}
	if len(messages) == 1 {
		if user, ok := messages[0].(llm.UserMessage); ok {
			for _, block := range user.Content {
				switch block := block.(type) {
				case llm.TextContent:
					blocks = append(blocks, map[string]any{"type": "text", "text": block.Text})
				case llm.ImageContent:
					data := block.Data
					if _, encoded, ok := strings.Cut(data, ","); ok && strings.HasPrefix(data, "data:") {
						data = encoded
					}
					mime := block.MIMEType
					if mime == "" {
						mime = "image/png"
					}
					blocks = append(blocks, map[string]any{"type": "image", "data": data, "mimeType": mime})
				}
			}
			return blocks, nil
		}
	}
	var text strings.Builder
	for _, message := range messages {
		switch m := message.(type) {
		case llm.UserMessage:
			text.WriteString("User: ")
			text.WriteString(flatten(m.Content))
		case llm.AssistantMessage:
			text.WriteString("Assistant: ")
			text.WriteString(flatten(m.Content))
		case llm.ToolResultMessage:
			fmt.Fprintf(&text, "Tool result (%s): ", m.ToolName)
			text.WriteString(flatten(m.Content))
		}
		text.WriteString("\n")
	}
	blocks = append(blocks, map[string]any{"type": "text", "text": text.String()})
	return blocks, nil
}

func flatten(content []llm.Content) string {
	var b strings.Builder
	for _, block := range content {
		switch block := block.(type) {
		case llm.TextContent:
			b.WriteString(block.Text)
		case llm.ImageContent:
			b.WriteString("[image]")
		case llm.ToolCall:
			fmt.Fprintf(&b, "[tool call %s]", block.Name)
		}
	}
	return b.String()
}
