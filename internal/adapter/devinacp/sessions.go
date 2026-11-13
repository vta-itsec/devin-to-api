// 本文件维护「客户端对话」到「Devin CLI 会话」的映射。
//
// VS Code 等客户端每次请求都重放完整消息历史，proxy 用消息链匹配找出
// 该对话绑定的 devin acp 会话。匹配规则（v2）：
//   - 匹配链只含【用户/工具结果】消息的哈希：助手消息是 proxy 自己产出的
//     回声，客户端可能重排 content block 或附加 thinking，不可作为链数据。
//     devin 端 transcript 自己保有它说过的话。
//   - 会话只能「向后延伸」：incoming 的用户链必须以会话已存链为前缀才允许
//     resume；中途编辑/分叉 → 新会话 + transcript 上下文注入，保证
//     devin 端 transcript 永不被污染。
//   - 助手回复指纹（Replies）用于锚定：incoming 最后一条助手消息命中哪个
//     会话的指纹，哪个会话优先——开头相同的不同聊天在 turn-2 自动归位。
//   - turn-1（单条用户消息）永不复用/重放：开头相同的聊天各自独立建会话。
//   - 显式 conversation_id（客户端字段）直接索引，跳过启发式。
// 映射持久化到 JSON 文件，proxy 重启后仍可 resume（devin 端会话存在
// sessions.db 中，由 session/load 恢复完整上下文）；文件丢失时可按
// devin session 的 title（首条 prompt 文本）从 session/list 收养。
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

// session 记录一次对话与 devin session 的绑定及其已消费的用户消息链。
type session struct {
	// ID 是 devin acp 的 sessionId（如 "silly-measure"）。
	ID string `json:"id"`
	// Chain 是按序消费过的【用户/工具结果】消息哈希。
	Chain []string `json:"chain"`
	// Replies 是本会话产生过的助手回复文本哈希（指纹），
	// 用于开头相同的多会话消歧。
	Replies []string `json:"replies,omitempty"`
	// Conversation 是客户端显式传入的会话标识（conversation_id 等）。
	Conversation string `json:"conversation,omitempty"`
	// Model 是创建会话时使用的模型。
	Model string `json:"model"`
	// LastChain 是上一次完整请求用户链的哈希，用于幂等重放。
	LastChain string `json:"last_chain,omitempty"`
	// LastReply 是 LastChain 对应的完整回复文本。
	LastReply string `json:"last_reply,omitempty"`
	// UpdatedAt 用于空闲会话回收与平局决胜。
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *session) hasReply(hash string) bool {
	for _, r := range s.Replies {
		if r == hash {
			return true
		}
	}
	return false
}

// messageHash 对一条消息计算稳定哈希。助手回复指纹只按纯文本：
// 客户端回传时可能重排 content block，但可见文本一致。
func messageHash(message llm.Message) string {
	var b strings.Builder
	b.WriteString(string(message.Role()))
	b.WriteByte(0)
	switch m := message.(type) {
	case llm.UserMessage:
		writeBlocks(&b, m.Content)
	case llm.ToolResultMessage:
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

// userChainOf 返回只含用户/工具结果消息的匹配链：用户消息是客户端输入，
// 逐字节可信；助手回声可能经客户端重排，不进链。
func userChainOf(messages []llm.Message) []string {
	var hashes []string
	for _, message := range messages {
		switch message.(type) {
		case llm.UserMessage, llm.ToolResultMessage:
			hashes = append(hashes, messageHash(message))
		}
	}
	return hashes
}

// lastAssistantHash 返回最后一条助手消息的文本指纹（-1 表示没有）。
func lastAssistantHash(messages []llm.Message) (int, string) {
	for i := len(messages) - 1; i >= 0; i-- {
		if m, ok := messages[i].(llm.AssistantMessage); ok {
			return i, messageHash(m)
		}
	}
	return -1, ""
}

// firstUserText 返回首条用户消息的纯文本（session 收养的 title 匹配依据）。
func firstUserText(messages []llm.Message) string {
	for _, message := range messages {
		if m, ok := message.(llm.UserMessage); ok {
			return flatten(m.Content)
		}
	}
	return ""
}

// tailFromLastAssistant 返回最后一条助手消息之后的所有消息。
// 用于「历史被客户端改写」或「客户端不重放历史」的场景：
// 回声之后的内容就是新轮次，之前的部分 devin transcript 里已有。
// 没有助手消息时返回全部。
func tailFromLastAssistant(messages []llm.Message) []llm.Message {
	idx := -1
	for i, m := range messages {
		if _, ok := m.(llm.AssistantMessage); ok {
			idx = i
		}
	}
	return messages[idx+1:]
}

// tailAfter 返回消耗了 consumed 条非助手消息之后的剩余消息；
// 开头的助手回声（我们自己产出的内容）被跳过——devin 已经知道它说过什么。
func tailAfter(messages []llm.Message, consumed int) []llm.Message {
	if consumed <= 0 {
		return messages
	}
	seen, idx := 0, -1
	for i, message := range messages {
		switch message.(type) {
		case llm.UserMessage, llm.ToolResultMessage:
			seen++
			if seen == consumed {
				idx = i
			}
		}
	}
	if idx < 0 {
		return messages
	}
	tail := messages[idx+1:]
	for len(tail) > 0 {
		if _, ok := tail[0].(llm.AssistantMessage); ok {
			tail = tail[1:]
		} else {
			break
		}
	}
	return tail
}

// matchResult 描述一次匹配。
type matchResult struct {
	session  *session
	// consumed 是已消费的非助手消息条数（非下标）。
	consumed int
	// replay 为 true 时整条链已消费过 → 直接回放缓存回复。
	replay bool
	// afterAssistant 为 true 时历史被客户端改写（压缩/截断），链不可比：
	// prompt = 最后一条助手回声之后的消息（新轮次），并把存储链重绑为 incoming。
	afterAssistant bool
}

// sessionStore 是线程安全的「消息链前缀 → devin session」注册表。
type sessionStore struct {
	mu       sync.Mutex
	path     string
	ttl      time.Duration
	sessions map[string]*session
	byConv   map[string]string // conversation_id → sessionId
	// busy 串行化同一会话的轮次（同一对话不应并发 prompt）。
	busy map[string]*sync.Mutex
}

func newSessionStore(path string, ttl time.Duration) (*sessionStore, error) {
	store := &sessionStore{path: path, ttl: ttl, sessions: map[string]*session{}, byConv: map[string]string{}, busy: map[string]*sync.Mutex{}}
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
		if s == nil || s.ID == "" {
			continue
		}
		store.sessions[s.ID] = s
		if s.Conversation != "" {
			store.byConv[s.Conversation] = s.ID
		}
	}
	return store, nil
}

// match 按「严格前缀延伸 + 回复指纹」找会话：
//   - conversation_id 命中：直接取对应会话；其链非 incoming 前缀 → 视分叉，新建。
//   - 否则：候选 = 链是 incoming 严格前缀的会话；
//     优先含 anchor 者 → 链最长 → 最近活跃。无候选 → 新建。
//   - 完全相等且 ≥2 条 → 幂等重放。
//   - turn-1（单条用户消息且无显式 id）直接返回新建。
func (store *sessionStore) match(messages []llm.Message, chain []string, conversationID string) matchResult {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.pruneLocked()
	joined := strings.Join(chain, ",")

	if conversationID != "" {
		if id, ok := store.byConv[conversationID]; ok {
			if s := store.sessions[id]; s != nil {
				if s.LastChain == joined && s.LastReply != "" {
					return matchResult{session: s, replay: true}
				}
				// 显式 id 的会话总是 resume：客户端可以不重放历史。
				// consumed=0（完全不重放/全部分叉）时由 afterAssistant
				// prompt 兜底——devin transcript 保有真实上下文。
				consumed := prefixLen(s.Chain, chain)
				return matchResult{session: s, consumed: consumed, afterAssistant: consumed == 0}
			}
		}
		// 带显式 id 的分叉不 fallback 到链匹配，避免串会话。
		return matchResult{}
	}

	// 标签锚定：回复中内嵌的 <!--d2a:<sid>--> 被客户端原样回传时，
	// 直接定位到产生该回复的会话——最精确的匹配。
	// 必须在链长检查之前：压缩/截断后的短链请求同样依赖标签路由。
	if sid := lastSessionTag(messages); sid != "" {
		if s := store.sessions[sid]; s != nil {
			if s.LastChain == joined && s.LastReply != "" {
				return matchResult{session: s, replay: true}
			}
			if isPrefix(s.Chain, chain) && len(s.Chain) < len(chain) {
				return matchResult{session: s, consumed: len(s.Chain)}
			}
			// 客户端压缩/截断/改写了历史：链在位置 0 即分叉（无公共前缀），
			// 但标签仍锚定本会话 → resume 同一会话，prompt 只发新轮次。
			// prefixLen>0 的分叉（中途编辑）不 resume——那是真分叉，开新会话。
			if prefixLen(s.Chain, chain) == 0 && len(chain) > 0 && isContinuation(chain, messages) {
				return matchResult{session: s, afterAssistant: true}
			}
		}
	}

	anchorIdx, anchorHash := lastAssistantHash(messages)
	if len(chain) <= 1 {
		// 链太短没有链匹配可言：唯一剩余信号是助手回声指纹，
		// 指纹唯一时按「历史重写」resume（只发回声之后的新轮次）。
		if anchorIdx >= 0 && isContinuation(chain, messages) {
			if s := store.uniqueReplyOwnerLocked(anchorHash); s != nil {
				return matchResult{session: s, afterAssistant: true}
			}
		}
		return matchResult{}
	}

	var best *session
	bestScore := -1
	for _, s := range store.sessions {
		if !isPrefix(s.Chain, chain) {
			continue
		}
		if len(s.Chain) == len(chain) {
			if s.LastChain == joined && s.LastReply != "" {
				return matchResult{session: s, replay: true}
			}
			continue
		}
		// 评分：anchor 命中 > 链长 > 最近活跃。完全相同时取最近活跃者——
		// 此时两条链的前缀逐字节相同，上下文等价。
		score := len(s.Chain)
		if anchorIdx >= 0 && s.hasReply(anchorHash) {
			score += 1 << 20
		}
		if best == nil || score > bestScore || (score == bestScore && s.UpdatedAt.After(best.UpdatedAt)) {
			best, bestScore = s, score
		}
	}
	if best != nil {
		return matchResult{session: best, consumed: len(best.Chain)}
	}
	// 没有任何链候选且链≥2：指纹回声只能提示「可能同源」，无法区分
	// 压缩（应 resume）与首条编辑（应分叉）——保守起见开新会话 + transcript
	// 注入。要确定性的跨压缩路由必须依赖标签（或显式 conversation_id）。
	return matchResult{}
}

// uniqueReplyOwnerLocked 返回唯一持有该回复指纹的会话；多个或零个返回 nil。
func (store *sessionStore) uniqueReplyOwnerLocked(hash string) *session {
	var owner *session
	for _, s := range store.sessions {
		if s.hasReply(hash) {
			if owner != nil {
				return nil
			}
			owner = s
		}
	}
	return owner
}

func isPrefix(prefix, full []string) bool {
	return prefixLen(prefix, full) == len(prefix)
}

func prefixLen(prefix, full []string) int {
	n := 0
	for n < len(prefix) && n < len(full) && prefix[n] == full[n] {
		n++
	}
	return n
}

// sessionTag 是嵌入回复尾部的会话指纹标记（HTML 注释，markdown 渲染不可见）。
const sessionTagPrefix = "<!--d2a:"
const sessionTagSuffix = "-->"

// sessionTagFor 生成某会话的回复指纹标记。
func sessionTagFor(sessionID string) string {
	return "\n\n" + sessionTagPrefix + sessionID + sessionTagSuffix
}

// lastSessionTag 从后往前找助手消息中的会话标记；客户端回传原文时即指纹。
func lastSessionTag(messages []llm.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		m, ok := messages[i].(llm.AssistantMessage)
		if !ok {
			continue
		}
		for _, block := range m.Content {
			text, ok := block.(llm.TextContent)
			if !ok {
				continue
			}
			if id := extractTag(text.Text); id != "" {
				return id
			}
		}
	}
	return ""
}

func extractTag(text string) string {
	start := strings.LastIndex(text, sessionTagPrefix)
	if start < 0 {
		return ""
	}
	rest := text[start+len(sessionTagPrefix):]
	end := strings.Index(rest, sessionTagSuffix)
	if end <= 0 {
		return ""
	}
	id := rest[:end]
	for _, r := range id {
		if !(r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return ""
		}
	}
	return id
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

// register 写入/更新会话的消费链并落盘。
// chain 是本轮完成后的【规范化用户链】（会话视角的完整消费序列）；
// incoming 是本次请求原始用户链的 join 形式，用于幂等重放判断——
// 两者在「客户端只发新轮次」时不同：规范化链继续增长，incoming 只有一条。
// replyHash 是本轮回复指纹。
func (store *sessionStore) register(s *session, model, conversationID string, chain []string, incoming string, replyHash, reply string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	s.Model = model
	s.Chain = append([]string(nil), chain...)
	if conversationID != "" {
		s.Conversation = conversationID
	}
	if s.Conversation != "" {
		store.byConv[s.Conversation] = s.ID
	}
	// 收养/重建会话时合并旧指纹，不丢历史回复锚点。
	if old := store.sessions[s.ID]; old != nil && old != s {
		for _, r := range old.Replies {
			if !s.hasReply(r) {
				s.Replies = append(s.Replies, r)
			}
		}
	}
	if replyHash != "" && !s.hasReply(replyHash) {
		s.Replies = append(s.Replies, replyHash)
	}
	s.LastChain = incoming
	s.LastReply = reply
	s.UpdatedAt = time.Now()
	store.sessions[s.ID] = s
	store.saveLocked()
}

// effectiveChain 计算本轮结束后应存储的规范化用户链。
//   - 常规：incoming 本身是消费序列 → 直接存储。
//   - 显式 conv 会话且 incoming 不是存储链的延伸（客户端只发新轮次）：
//     存储链 = 旧链 + incoming 最后一个哈希，保持会话视角的完整序列。
//   - afterAssistant（历史被改写）：incoming 即客户端当前视图 → 重绑替换。
func effectiveChain(s *session, chain []string, conversationID string, match matchResult) []string {
	if match.afterAssistant || conversationID == "" || len(chain) == 0 {
		return chain
	}
	if s != nil && len(s.Chain) > 0 && !isPrefix(s.Chain, chain) {
		merged := append([]string(nil), s.Chain...)
		return append(merged, chain[len(chain)-1])
	}
	return chain
}

func (store *sessionStore) pruneLocked() {
	if store.ttl <= 0 {
		return
	}
	cutoff := time.Now().Add(-store.ttl)
	for id, s := range store.sessions {
		if s.UpdatedAt.Before(cutoff) {
			delete(store.sessions, id)
			if s.Conversation != "" {
				delete(store.byConv, s.Conversation)
			}
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
