// 本文件维护「客户端对话」到「Devin Cloud session」的映射。
//
// 与 acp 后端同一套思路：VS Code 每次请求重放完整历史，用用户消息哈希链
// 的最长严格前缀定位 cloud session；命中则只把新轮次 /message 进去，
// 未命中创建新 session（首轮 prompt 带完整 transcript）。
// Cloud session 是昂贵异步任务，映射一旦建立不轻易分叉：
// 链在位置 0 分叉（客户端改写历史）时按「重写」处理——重绑链、继续同一
// session，因为 cloud session 的上下文在服务端，不在客户端历史里。
package devincloud

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

// cloudSession 记录对话与 cloud session 的绑定。
type cloudSession struct {
	ID      string    `json:"id"`
	URL     string    `json:"url,omitempty"`
	Chain   []string  `json:"chain"`
	Model   string    `json:"model,omitempty"`
	Conv    string    `json:"conv,omitempty"`
	Reply   string    `json:"reply,omitempty"`   // 上一轮回复（幂等重放）
	LastIn  string    `json:"last_in,omitempty"` // 上一轮完整 incoming 链 join
	Updated time.Time `json:"updated"`
}

type cloudStore struct {
	mu       sync.Mutex
	path     string
	sessions map[string]*cloudSession
	byConv   map[string]string
	busy     map[string]*sync.Mutex
}

func newCloudStore(path string) (*cloudStore, error) {
	store := &cloudStore{path: path, sessions: map[string]*cloudSession{}, byConv: map[string]string{}, busy: map[string]*sync.Mutex{}}
	if path == "" {
		return store, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return store, nil
		}
		return nil, fmt.Errorf("read cloud session store %s: %w", path, err)
	}
	var list []*cloudSession
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("decode cloud session store %s: %w", path, err)
	}
	for _, s := range list {
		if s == nil || s.ID == "" {
			continue
		}
		store.sessions[s.ID] = s
		if s.Conv != "" {
			store.byConv[s.Conv] = s.ID
		}
	}
	return store, nil
}

type cloudMatch struct {
	session   *cloudSession
	consumed  int  // 已消费的用户链长度
	replay    bool // 同链重发 → 直接回放上轮回复
	rewritten bool // 客户端改写历史（位置 0 分叉）→ 只发回声之后的新轮次
}

// match 定位会话：显式 conv > 最长严格前缀 > 新建。
// chain 只含用户/工具结果消息哈希。
func (store *cloudStore) match(chain []string, convID string) cloudMatch {
	store.mu.Lock()
	defer store.mu.Unlock()
	joined := strings.Join(chain, ",")
	if convID != "" {
		if id, ok := store.byConv[convID]; ok {
			if s := store.sessions[id]; s != nil {
				if s.LastIn == joined && s.Reply != "" {
					return cloudMatch{session: s, replay: true}
				}
				return cloudMatch{session: s, consumed: prefixOverlap(s.Chain, chain), rewritten: prefixOverlap(s.Chain, chain) == 0}
			}
		}
		return cloudMatch{}
	}
	if len(chain) <= 1 {
		// turn-1 永不复用（开头相同的聊天各自建 session）。
		return cloudMatch{}
	}
	var best *cloudSession
	for _, s := range store.sessions {
		if isPrefixOf(s.Chain, chain) && len(s.Chain) < len(chain) {
			if s.LastIn == joined && s.Reply != "" {
				return cloudMatch{session: s, replay: true}
			}
			if best == nil || len(s.Chain) > len(best.Chain) {
				best = s
			}
		}
	}
	if best != nil {
		return cloudMatch{session: best, consumed: len(best.Chain)}
	}
	// 完全相等 → 幂等重放。
	for _, s := range store.sessions {
		if s.LastIn == joined && s.Reply != "" && len(s.Chain) == len(chain) {
			return cloudMatch{session: s, replay: true}
		}
	}
	return cloudMatch{}
}

func (store *cloudStore) lock(sessionID string) *sync.Mutex {
	store.mu.Lock()
	defer store.mu.Unlock()
	m, ok := store.busy[sessionID]
	if !ok {
		m = &sync.Mutex{}
		store.busy[sessionID] = m
	}
	return m
}

// register 写入/更新会话链并落盘。rewritten 时用 incoming 链整体替换。
func (store *cloudStore) register(s *cloudSession, model, convID string, chain []string, rewritten bool, reply string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	s.Model = model
	if rewritten || len(chain) >= len(s.Chain) {
		s.Chain = append([]string(nil), chain...)
	} else {
		s.Chain = append(append([]string(nil), s.Chain...), chain[len(chain)-1])
	}
	if convID != "" {
		s.Conv = convID
	}
	if s.Conv != "" {
		store.byConv[s.Conv] = s.ID
	}
	s.Reply = reply
	s.LastIn = strings.Join(chain, ",")
	s.Updated = time.Now()
	store.sessions[s.ID] = s
	store.saveLocked()
}

func (store *cloudStore) saveLocked() {
	if store.path == "" {
		return
	}
	list := make([]*cloudSession, 0, len(store.sessions))
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

func hashChain(messages []llm.Message) []string {
	var hashes []string
	for _, message := range messages {
		switch m := message.(type) {
		case llm.UserMessage:
			hashes = append(hashes, hashText("u", flattenContent(m.Content)))
		case llm.ToolResultMessage:
			hashes = append(hashes, hashText("t", flattenContent(m.Content)))
		}
	}
	return hashes
}

func hashText(role, text string) string {
	sum := sha256.Sum256([]byte(role + "\x00" + text))
	return hex.EncodeToString(sum[:16])
}

func flattenContent(content []llm.Content) string {
	var b strings.Builder
	for _, block := range content {
		switch block := block.(type) {
		case llm.TextContent:
			b.WriteString(block.Text)
		case llm.ImageContent:
			b.WriteString("[image]")
		}
	}
	return b.String()
}

// messageText 把消息渲染成 prompt 文本行（首轮 transcript / 后续 message 用）。
func messageText(message llm.Message) string {
	var role string
	var content []llm.Content
	switch m := message.(type) {
	case llm.UserMessage:
		role, content = "user", m.Content
	case llm.AssistantMessage:
		role, content = "assistant", m.Content
	case llm.ToolResultMessage:
		role, content = "tool", m.Content
	default:
		return ""
	}
	return role + ": " + flattenContent(content)
}

func isPrefixOf(prefix, full []string) bool {
	return prefixOverlap(prefix, full) == len(prefix) && len(prefix) > 0
}

func prefixOverlap(prefix, full []string) int {
	n := 0
	for n < len(prefix) && n < len(full) && prefix[n] == full[n] {
		n++
	}
	return n
}

// tailAfterUserCount 返回消耗了 n 条用户/工具消息之后剩余的消息。
func tailAfterUserCount(messages []llm.Message, n int) []llm.Message {
	if n <= 0 {
		return messages
	}
	seen, idx := 0, -1
	for i, m := range messages {
		switch m.(type) {
		case llm.UserMessage, llm.ToolResultMessage:
			seen++
			if seen == n {
				idx = i
			}
		}
	}
	if idx < 0 {
		return messages
	}
	return messages[idx+1:]
}

// tailFromLastAssistant 返回最后一条助手消息之后的消息（历史被改写时的新轮次）。
func tailFromLastAssistant(messages []llm.Message) []llm.Message {
	idx := -1
	for i, m := range messages {
		if _, ok := m.(llm.AssistantMessage); ok {
			idx = i
		}
	}
	return messages[idx+1:]
}

// defaultCloudStateFile 返回 cloud 映射文件位置（workspace 内，随卷持久化）。
func defaultCloudStateFile(workspace string) string {
	return filepath.Join(workspace, ".devin-to-api-cloud-sessions.json")
}
