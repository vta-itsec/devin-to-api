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
	chain := chainOf(request.Messages)
	match := a.store.match(chain)

	// 幂等：客户端重发完全相同的链 → 回放缓存回复。
	if match.consumed == len(chain) && match.session != nil && match.session.LastReply != "" {
		return replayStream(model, match.session.LastReply), nil
	}
	newMessages := request.Messages[match.consumed:]
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
		a.runTurn(ctx, stream, match.session, model, prompt, chain)
	}()
	return stream, nil
}

// runTurn 执行一轮：spawn → 会话 → prompt → 事件流 → 注册链。
func (a *Adapter) runTurn(ctx context.Context, stream *eventStream, existing *session, model string, prompt []map[string]any, chain []string) {
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
			// Chain = 请求链 + 我们生成的助手回复哈希：下轮客户端回传的
			// [.., u_n, a_n, u_{n+1}] 才能与存储链形成完整前缀，从而只把
			// u_{n+1} 作为新 prompt 发给 devin（devin 会话自己保有 a_n 上下文）。
			fullChain := append(append([]string(nil), chain...),
				messageHash(llm.AssistantMessage{Content: []llm.Content{llm.TextContent{Text: reply.String()}}}))
			a.store.register(&session{ID: sessionID}, model, fullChain, reply.String())
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
