// 本文件维护「客户端对话」到「Devin CLI 会话」的映射。
//
// VS Code 等客户端每次请求都重放完整消息历史，proxy 用逐消息 chain-hash
// 最长前缀匹配找出该对话已绑定的 devin acp 会话；匹配不到就开新会话。
// 映射持久化到 JSON 文件，proxy 重启后仍可 resume（devin 端会话存在
// sessions.db 中，由 `session/load` 恢复完整上下文）。
package devinacp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vta-itsec/devin-to-api/internal/llm"
)

// session 记录一次对话与 devin session 的绑定及其已消费的消息链。
type session struct {
	// ID 是 devin acp 的 sessionId（如 "silly-measure"）。
	ID string `json:"id"`
	// Chain 是按序消费过的消息哈希：用户消息 + 我们生成的助手回复。
	Chain []string `json:"chain"`
	// Model 是创建会话时使用的模型。
	Model string `json:"model"`
	// LastChain 是上一次完整请求链的哈希，用于幂等重放。
	LastChain string `json:"last_chain,omitempty"`
	// LastReply 是 LastChain 对应的完整回复文本；客户端重试同一链时直接回放缓存，
	// 避免重复消耗上游轮次。
	LastReply string `json:"last_reply,omitempty"`
	// UpdatedAt 用于空闲会话回收。
	UpdatedAt time.Time `json:"updated_at"`
}

// messageHash 对一条消息计算稳定哈希。助手消息只按纯文本哈希——
// 客户端回传的我们生成的回复可能重排 content block，但文本一致。
func messageHash(message llm.Message) string {
	var b strings.Builder
	b.WriteString(string(message.Role()))
	b.WriteByte(0)
	switch m := message.(type) {
	case llm.UserMessage:
		writeBlocks(&b, m.Content)
	case llm.ToolResultMessage:
		b.WriteString(m.ToolName)
		b.WriteByte(0)
		writeBlocks(&b, m.Content)
	case llm.AssistantMessage:
		writeBlocks(&b, m.Content)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:16])
}

func writeBlocks(b *strings.Builder, content []llm.Content) {
	for _, block := range content {
		switch block := block.(type) {
		case llm.TextContent:
			b.WriteString(block.Text)
		case llm.ImageContent:
			b.WriteString("[image]")
		case llm.ThinkingContent:
			b.WriteString(block.Thinking)
		case llm.ToolCall:
			b.WriteString(block.Name)
		}
		b.WriteByte(0)
	}
}

// chainOf 返回整个请求链的逐消息哈希。
func chainOf(messages []llm.Message) []string {
	hashes := make([]string, len(messages))
	for i, message := range messages {
		hashes[i] = messageHash(message)
	}
	return hashes
}

// sessionStore 是线程安全的「消息链前缀 → devin session」注册表。
type sessionStore struct {
	mu       sync.Mutex
	path     string
	ttl      time.Duration
	sessions map[string]*session
	// busy 串行化同一会话的轮次（同一对话不应并发 prompt）。
	busy map[string]*sync.Mutex
}

func newSessionStore(path string, ttl time.Duration) (*sessionStore, error) {
	store := &sessionStore{path: path, ttl: ttl, sessions: map[string]*session{}, busy: map[string]*sync.Mutex{}}
	if path == "" {
		return store, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return store, nil
		}
		return nil, fmt.Errorf("read acp session store %s: %w", path, err)
	}
	var list []*session
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("decode acp session store %s: %w", path, err)
	}
	for _, s := range list {
		if s != nil && s.ID != "" {
			store.sessions[s.ID] = s
		}
	}
	return store, nil
}

// matchResult 描述一次匹配：选中的会话（可能为 nil = 新建）及应新消费的尾部消息下标。
type matchResult struct {
	session  *session
	consumed int
}

// match 按最长共同前缀找会话。规则：
//   - 与 LastChain 完全相等 → 命中幂等缓存（返回 lastReply 即可）。
//   - 最长唯一前缀的会话胜出；多个会话并列同一长度（对话分叉且前缀相同）
//     → 返回 nil 新建会话，避免把新聊天挂到别人的上下文上。
func (store *sessionStore) match(chain []string) matchResult {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.pruneLocked()
	joined := strings.Join(chain, ",")
	var best *session
	tie := false
	for _, s := range store.sessions {
		n := 0
		for n < len(s.Chain) && n < len(chain) && s.Chain[n] == chain[n] {
			n++
		}
		if n == 0 {
			continue
		}
		if best == nil || n > matchLen(best, chain) {
			best, tie = s, false
		} else if n == matchLen(best, chain) && s != best {
			tie = true
		}
	}
	if best != nil && best.LastChain == joined && best.LastReply != "" {
		return matchResult{session: best, consumed: len(chain)}
	}
	if tie || best == nil {
		return matchResult{}
	}
	return matchResult{session: best, consumed: matchLen(best, chain)}
}

func matchLen(s *session, chain []string) int {
	n := 0
	for n < len(s.Chain) && n < len(chain) && s.Chain[n] == chain[n] {
		n++
	}
	return n
}

// lock 返回会话级互斥锁，按 devin sessionId 串行化轮次。
func (store *sessionStore) lock(sessionID string) *sync.Mutex {
	store.mu.Lock()
	defer store.mu.Unlock()
	m, ok := store.busy[sessionID]
	if !ok {
		m = &sync.Mutex{}
		store.busy[sessionID] = m
	}
	return m
}

// register 写入/更新会话的消费链并落盘；replyChain 是本轮完成后的完整请求链，
// reply 是本轮回复文本（用于幂等重放）。
func (store *sessionStore) register(s *session, model string, chain []string, reply string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	s.Model = model
	s.Chain = append([]string(nil), chain...)
	s.LastChain = strings.Join(chain, ",")
	s.LastReply = reply
	s.UpdatedAt = time.Now()
	store.sessions[s.ID] = s
	store.saveLocked()
}

func (store *sessionStore) pruneLocked() {
	if store.ttl <= 0 {
		return
	}
	cutoff := time.Now().Add(-store.ttl)
	for id, s := range store.sessions {
		if s.UpdatedAt.Before(cutoff) {
			delete(store.sessions, id)
		}
	}
}

func (store *sessionStore) saveLocked() {
	if store.path == "" {
		return
	}
	list := make([]*session, 0, len(store.sessions))
	for _, s := range store.sessions {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return
	}
	tmp := store.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, store.path)
}

// sessionCount 返回当前注册会话数（healthz/调试用）。
func (store *sessionStore) sessionCount() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return len(store.sessions)
}

// defaultStateFile 返回映射文件的默认位置（workspace 内，随卷持久化）。
func defaultStateFile(workspace string) string {
	return filepath.Join(workspace, ".devin-to-api-sessions.json")
}
