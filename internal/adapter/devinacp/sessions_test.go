package devinacp

import (
	"strings"
	"testing"
	"time"

	"github.com/vta-itsec/devin-to-api/internal/llm"
)

func userMsg(text string) llm.UserMessage {
	return llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: text}}}
}

func assistantMsg(text string) llm.AssistantMessage {
	return llm.AssistantMessage{Content: []llm.Content{llm.TextContent{Text: text}}}
}

func newTestStore(t *testing.T) *sessionStore {
	t.Helper()
	store, err := newSessionStore("", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// registerTurn 模拟一轮完成：存储链 = 请求的用户链，回复指纹进 Replies。
func registerTurn(store *sessionStore, id, convID string, msgs []llm.Message, reply string) {
	chain := userChainOf(msgs)
	store.register(&session{ID: id}, "m", convID, chain, strings.Join(chain, ","),
		messageHash(assistantMsg(reply)), reply)
}

func TestMatchNewConversation(t *testing.T) {
	store := newTestStore(t)
	msgs := []llm.Message{userMsg("hello")}
	match := store.match(msgs, userChainOf(msgs), "")
	if match.session != nil || match.consumed != 0 {
		t.Fatalf("expected no match, got %+v", match)
	}
}

func TestMatchResumeAndReplay(t *testing.T) {
	store := newTestStore(t)
	registerTurn(store, "sess-a", "", []llm.Message{userMsg("hi")}, "hello")

	// 第二轮：[u1, a1, u2] → 用户链前缀延伸 → consumed=1（1 条用户消息）。
	turn2 := []llm.Message{userMsg("hi"), assistantMsg("hello"), userMsg("how are you")}
	match := store.match(turn2, userChainOf(turn2), "")
	if match.session == nil || match.session.ID != "sess-a" || match.consumed != 1 {
		t.Fatalf("expected sess-a consumed=1, got %+v", match)
	}
	if tail := tailAfter(turn2, match.consumed); len(tail) != 1 {
		t.Fatalf("expected 1 new message, got %d", len(tail))
	}

	// turn-1 的完整重发不可区分于新聊天（见 TestTurn1NeverReuses）→ 新建。
	// 但 ≥2 条用户链的完整重发是安全的幂等重放：
	registerTurn(store, "sess-a", "", turn2, "fine")
	retry := []llm.Message{userMsg("hi"), assistantMsg("hello"), userMsg("how are you"), assistantMsg("fine")}
	match = store.match(retry, userChainOf(retry), "")
	if match.session == nil || !match.replay {
		t.Fatalf("expected replay, got %+v", match)
	}
}

func TestAssistantEchoReformattedStillResumes(t *testing.T) {
	store := newTestStore(t)
	registerTurn(store, "sess-a", "", []llm.Message{userMsg("hi")}, "hello")

	// 客户端把助手回复加 thinking/重排回传 → 用户链不变 → 照常 resume。
	echo := llm.AssistantMessage{Content: []llm.Content{
		llm.ThinkingContent{Thinking: "pondering"},
		llm.TextContent{Text: "hello"},
	}}
	turn2 := []llm.Message{userMsg("hi"), echo, userMsg("next")}
	match := store.match(turn2, userChainOf(turn2), "")
	if match.session == nil || match.session.ID != "sess-a" || match.consumed != 1 {
		t.Fatalf("reformatted echo must still resume sess-a, got %+v", match)
	}
}

func TestTurn1NeverReuses(t *testing.T) {
	store := newTestStore(t)
	registerTurn(store, "sess-a", "", []llm.Message{userMsg("hi")}, "hello")

	// 第二个聊天以完全相同的开场消息开始 → 开新会话，不复用也不重放。
	msgs := []llm.Message{userMsg("hi")}
	match := store.match(msgs, userChainOf(msgs), "")
	if match.session != nil {
		t.Fatalf("turn-1 must start fresh, got %s", match.session.ID)
	}
}

func TestAnchorPicksReplyOwner(t *testing.T) {
	store := newTestStore(t)
	// 两个会话用户链完全相同（同开场），但 reply 指纹不同 → turn-2 各自归位。
	registerTurn(store, "sess-a", "", []llm.Message{userMsg("hi")}, "reply-a")
	registerTurn(store, "sess-b", "", []llm.Message{userMsg("hi")}, "reply-b")

	msgsB := []llm.Message{userMsg("hi"), assistantMsg("reply-b"), userMsg("next")}
	match := store.match(msgsB, userChainOf(msgsB), "")
	if match.session == nil || match.session.ID != "sess-b" {
		t.Fatalf("anchor should pick sess-b, got %+v", match)
	}
}

func TestDivergenceForksFresh(t *testing.T) {
	store := newTestStore(t)
	registerTurn(store, "sess-a", "", []llm.Message{userMsg("hi")}, "hello")

	// 编辑了第一条用户消息 → 用户链分叉 → 新建（原 session 不被污染）。
	edited := []llm.Message{userMsg("hi EDITED"), assistantMsg("hello"), userMsg("next")}
	match := store.match(edited, userChainOf(edited), "")
	if match.session != nil {
		t.Fatalf("edited prefix must fork a fresh session, got %s", match.session.ID)
	}
}

func TestExplicitConversationID(t *testing.T) {
	store := newTestStore(t)
	registerTurn(store, "sess-a", "conv-1", []llm.Message{userMsg("hi")}, "hello")

	msgs := []llm.Message{userMsg("hi"), assistantMsg("hello"), userMsg("next")}
	match := store.match(msgs, userChainOf(msgs), "conv-1")
	if match.session == nil || match.session.ID != "sess-a" || match.consumed != 1 {
		t.Fatalf("conversation id should pin sess-a, got %+v", match)
	}

	// 别的 conversation_id → 不 fallback 到链匹配（避免串会话）。
	match = store.match(msgs, userChainOf(msgs), "conv-other")
	if match.session != nil {
		t.Fatalf("unknown conversation id must start fresh, got %s", match.session.ID)
	}
}

func TestLongestPrefixWins(t *testing.T) {
	store := newTestStore(t)
	u1, u2 := userMsg("a"), userMsg("b")
	registerTurn(store, "sess-short", "", []llm.Message{u1}, "r1")
	registerTurn(store, "sess-long", "", []llm.Message{u1, assistantMsg("r1"), u2}, "r2")

	incoming := []llm.Message{u1, assistantMsg("r1"), u2, assistantMsg("r2"), userMsg("c")}
	match := store.match(incoming, userChainOf(incoming), "")
	if match.session == nil || match.session.ID != "sess-long" || match.consumed != 2 {
		t.Fatalf("expected sess-long consumed=2, got %+v", match)
	}
}

// taggedReply 生成带会话标签的助手回声（模拟客户端原样回传）。
func taggedReply(store *sessionStore, id, text string) llm.AssistantMessage {
	return assistantMsg(text + "\n\n" + store.sessionTagFor(id))
}

// === 零宽标签编解码 =======================================================

func TestZWTagRoundTrip(t *testing.T) {
	store := newTestStore(t)
	tag := store.sessionTagFor("sess-x")
	if strings.Contains(tag, "d2a") || strings.Contains(tag, "sess-x") {
		t.Fatalf("zw tag must not contain readable text, got %q", tag)
	}
	// 标签应完全由哨兵+编码字母表构成 → 渲染后不可见。
	for _, r := range tag {
		if _, ok := zwDecodeMap[r]; !ok && r != zwSentinel {
			t.Fatalf("tag contains visible char %q", r)
		}
	}
	if got := store.extractZW("reply text" + tag); got != "sess-x" {
		t.Fatalf("extractZW round-trip failed, got %q", got)
	}
	// 再次生成同一会话 → 别名稳定。
	if tag2 := store.sessionTagFor("sess-x"); tag2 != tag {
		t.Fatalf("tag alias must be stable, got %q vs %q", tag, tag2)
	}
}

func TestZWTagLegacyCommentStillParsed(t *testing.T) {
	store := newTestStore(t)
	registerTurn(store, "sess-old", "", []llm.Message{userMsg("u1")}, "r1")

	// 旧格式 HTML 注释回声仍须能定位会话。
	msgs := []llm.Message{
		userMsg("u1"),
		assistantMsg("r1\n\n<!--d2a:sess-old-->"),
		userMsg("u2"),
	}
	match := store.match(msgs, userChainOf(msgs), "")
	if match.session == nil || match.session.ID != "sess-old" {
		t.Fatalf("legacy comment tag must still route, got %+v", match)
	}
}

func TestExtractZWRejectsGarbage(t *testing.T) {
	store := newTestStore(t)
	if id := store.extractZW("normal text ⁠no alphabet here⁠ end"); id != "" {
		t.Fatalf("must reject non-encoded sentinels, got %q", id)
	}
	if id := store.extractZW("plain reply with no tag at all"); id != "" {
		t.Fatalf("must not extract from tagless text, got %q", id)
	}
	// 哨兵包裹但 body 是乱码/无别名 → "" 。
	if id := store.extractZW("x" + string(zwSentinel) + encodeZW("zzz999") + string(zwSentinel)); id != "" {
		t.Fatalf("unknown alias must not resolve, got %q", id)
	}
}

// === 压缩/截断（客户端改写历史）===========================================

// 客户端把历史压缩成摘要：首条用户消息已变，但最后一条助手回声仍带标签 →
// 必须 resume 原会话（devin transcript 保有真实上下文），prompt 只发新轮次。
func TestCompactedHistoryResumesViaTag(t *testing.T) {
	store := newTestStore(t)
	registerTurn(store, "sess-a", "", []llm.Message{userMsg("u1")}, "r1")
	registerTurn(store, "sess-a", "", []llm.Message{userMsg("u1"), assistantMsg("r1"), userMsg("u2")}, "r2")

	// VS Code 压缩后：[summary_u, a_echo(tag), u_new] — 用户链从位置 0 分叉。
	compacted := []llm.Message{
		userMsg("summary: we discussed things earlier"),
		taggedReply(store, "sess-a", "r2"),
		userMsg("new question"),
	}
	match := store.match(compacted, userChainOf(compacted), "")
	if match.session == nil || match.session.ID != "sess-a" {
		t.Fatalf("compacted history must resume tagged session, got %+v", match)
	}
	if !match.afterAssistant {
		t.Fatalf("expected afterAssistant prompt mode, got %+v", match)
	}
	if tail := tailFromLastAssistant(compacted); len(tail) != 1 {
		t.Fatalf("expected only the new user turn as prompt, got %d msgs", len(tail))
	}
}

// 截断（drop 最早的消息）同样是位置 0 分叉 → 标签 resume。
func TestTruncatedHistoryResumesViaTag(t *testing.T) {
	store := newTestStore(t)
	registerTurn(store, "sess-a", "", []llm.Message{userMsg("u1")}, "r1")
	registerTurn(store, "sess-a", "", []llm.Message{userMsg("u1"), assistantMsg("r1"), userMsg("u2")}, "r2")

	// 客户端丢掉 u1，保留 [u2, a2(tag), u3]。
	truncated := []llm.Message{
		userMsg("u2"),
		taggedReply(store, "sess-a", "r2"),
		userMsg("u3"),
	}
	match := store.match(truncated, userChainOf(truncated), "")
	if match.session == nil || match.session.ID != "sess-a" || !match.afterAssistant {
		t.Fatalf("truncated history must resume via tag, got %+v", match)
	}
}

// 客户端剥掉标签只留回复原文：指纹无法区分「压缩」与「首条编辑」——
// 保守 fork + transcript 注入，绝不冒污染原会话的风险。
// 跨压缩的确定性路由必须依赖标签或显式 conversation_id。
func TestCompactedFingerprintForks(t *testing.T) {
	store := newTestStore(t)
	registerTurn(store, "sess-a", "", []llm.Message{userMsg("u1")}, "unique-reply-text")
	registerTurn(store, "sess-b", "", []llm.Message{userMsg("v1")}, "other-reply")

	compacted := []llm.Message{
		userMsg("summary of earlier discussion"),
		assistantMsg("unique-reply-text"), // 无标签，指纹唯一也不 resume
		userMsg("follow-up"),
	}
	match := store.match(compacted, userChainOf(compacted), "")
	if match.session != nil {
		t.Fatalf("tagless divergent history must fork, got %s", match.session.ID)
	}
}

// 指纹不唯一（两个会话回过同样的文本）→ 不乱猜 → 新会话。
func TestCompactedFingerprintAmbiguousForks(t *testing.T) {
	store := newTestStore(t)
	registerTurn(store, "sess-a", "", []llm.Message{userMsg("u1")}, "same")
	registerTurn(store, "sess-b", "", []llm.Message{userMsg("v1")}, "same")

	compacted := []llm.Message{userMsg("summary"), assistantMsg("same"), userMsg("next")}
	match := store.match(compacted, userChainOf(compacted), "")
	if match.session != nil {
		t.Fatalf("ambiguous fingerprint must fork fresh, got %s", match.session.ID)
	}
}

// 编辑分叉（prefixLen>0）即使带标签也绝不 resume → 防止污染原会话。
func TestMidEditWithTagStillForks(t *testing.T) {
	store := newTestStore(t)
	registerTurn(store, "sess-a", "", []llm.Message{userMsg("u1")}, "r1")
	registerTurn(store, "sess-a", "", []llm.Message{userMsg("u1"), assistantMsg("r1"), userMsg("u2")}, "r2")

	edited := []llm.Message{
		userMsg("u1"),
		assistantMsg("r1"),
		userMsg("u2 EDITED"),
		taggedReply(store, "sess-a", "r2"),
		userMsg("u3"),
	}
	match := store.match(edited, userChainOf(edited), "")
	if match.session != nil {
		t.Fatalf("mid-chain edit must fork even with tag, got %s", match.session.ID)
	}
}

// 链=1 但带标签回声（客户端只发最后一轮）→ 也应 resume。
func TestShortChainWithTagEcho(t *testing.T) {
	store := newTestStore(t)
	registerTurn(store, "sess-a", "", []llm.Message{userMsg("u1")}, "r1")

	msgs := []llm.Message{
		taggedReply(store, "sess-a", "r1"),
		userMsg("u2"),
	}
	match := store.match(msgs, userChainOf(msgs), "")
	if match.session == nil || match.session.ID != "sess-a" || !match.afterAssistant {
		t.Fatalf("tag echo with short chain must resume, got %+v", match)
	}
}

// === 标签安全 ==============================================================

// 用户消息里引用的标签绝不能被用来劫持会话（只扫描助手消息）。
// 构造一个全新对话，其用户消息内嵌受害会话的标签 → 必须开新会话。
func TestTagInUserMessageCannotHijack(t *testing.T) {
	store := newTestStore(t)
	registerTurn(store, "sess-victim", "", []llm.Message{userMsg("u1")}, "r1")

	msgs := []llm.Message{
		userMsg("totally different opener"),
		userMsg("you said noted <!--d2a:sess-victim--> right?"),
	}
	match := store.match(msgs, userChainOf(msgs), "")
	if match.session != nil {
		t.Fatalf("user-embedded tag must not route, got %s", match.session.ID)
	}
}

// 格式合法但指向不存在会话的标签 → 走启发式，不误导。
func TestUnknownTagIgnored(t *testing.T) {
	store := newTestStore(t)
	registerTurn(store, "sess-a", "", []llm.Message{userMsg("u1")}, "r1")

	msgs := []llm.Message{
		userMsg("u1"),
		assistantMsg("r1<!--d2a:ghost-session-->"),
		userMsg("u2"),
	}
	match := store.match(msgs, userChainOf(msgs), "")
	if match.session == nil || match.session.ID != "sess-a" {
		t.Fatalf("unknown tag should fall back to chain matching, got %+v", match)
	}
}

// 未知标签 + 链不前缀 → 新会话（不 panic、不误配）。
func TestUnknownTagDivergentForks(t *testing.T) {
	store := newTestStore(t)
	registerTurn(store, "sess-a", "", []llm.Message{userMsg("u1")}, "r1")

	msgs := []llm.Message{
		userMsg("different"),
		assistantMsg("x<!--d2a:ghost-->"),
		userMsg("next"),
	}
	match := store.match(msgs, userChainOf(msgs), "")
	if match.session != nil {
		t.Fatalf("unknown tag + divergent chain must fork, got %s", match.session.ID)
	}
}

// === conversation_id 部分历史 ==============================================

// conv 客户端只发新轮次（不重放）：链不进前缀也能 pin 住会话；
// prompt 必须是最后一条用户消息而非整段历史。
func TestConvSendOnlyNewTurns(t *testing.T) {
	store := newTestStore(t)
	registerTurn(store, "sess-a", "conv-1", []llm.Message{userMsg("u1")}, "r1")

	turn := []llm.Message{userMsg("u2")}
	match := store.match(turn, userChainOf(turn), "conv-1")
	if match.session == nil || match.session.ID != "sess-a" {
		t.Fatalf("conv id must pin session, got %+v", match)
	}
	if tail := tailFromLastAssistant(turn); len(tail) != 1 {
		t.Fatalf("expected last user message as prompt, got %d", len(tail))
	}

	// register 后重发同一轮 → 幂等重放。
	registerTurn(store, "sess-a", "conv-1", turn, "r2")
	match = store.match(turn, userChainOf(turn), "conv-1")
	if !match.replay {
		t.Fatalf("resending the same turn must replay, got %+v", match)
	}
}

// conv 会话之前只发新轮次，某次忽然重放全历史 → prompt 仍是最后一个用户
// 轮次，不能把整段历史注入 devin transcript。
func TestConvFullReplayAfterSendOnly(t *testing.T) {
	store := newTestStore(t)
	registerTurn(store, "sess-a", "conv-1", []llm.Message{userMsg("u1")}, "r1")
	// 模拟 send-only 轮次后存储链的增长（append-last 规则）。
	store.register(store.sessions["sess-a"], "m", "conv-1",
		[]string{userChainOf([]llm.Message{userMsg("u1")})[0], userChainOf([]llm.Message{userMsg("u2")})[0]},
		userChainOf([]llm.Message{userMsg("u2")})[0], messageHash(assistantMsg("r2")), "r2")

	full := []llm.Message{
		userMsg("u1"), assistantMsg("r1"), userMsg("u2"), assistantMsg("r2"), userMsg("u3"),
	}
	match := store.match(full, userChainOf(full), "conv-1")
	if match.session == nil || match.session.ID != "sess-a" {
		t.Fatalf("conv full replay must pin session, got %+v", match)
	}
}

// === 交错会话 ==============================================================

func TestInterleavedConversationsStayApart(t *testing.T) {
	store := newTestStore(t)
	registerTurn(store, "sess-a", "", []llm.Message{userMsg("hi")}, "ra")
	registerTurn(store, "sess-b", "", []llm.Message{userMsg("hi")}, "rb")

	// B 的第二轮带 B 的标签。
	bTurn2 := []llm.Message{userMsg("hi"), taggedReply(store, "sess-b", "rb"), userMsg("b2")}
	if m := store.match(bTurn2, userChainOf(bTurn2), ""); m.session == nil || m.session.ID != "sess-b" {
		t.Fatalf("B turn2 must route to sess-b, got %+v", m)
	}
	// A 的第二轮带 A 的标签。
	aTurn2 := []llm.Message{userMsg("hi"), taggedReply(store, "sess-a", "ra"), userMsg("a2")}
	if m := store.match(aTurn2, userChainOf(aTurn2), ""); m.session == nil || m.session.ID != "sess-a" {
		t.Fatalf("A turn2 must route to sess-a, got %+v", m)
	}
	// 交错继续仍不串。
	registerTurn(store, "sess-b", "", bTurn2, "rb2")
	aTurn3 := []llm.Message{userMsg("hi"), taggedReply(store, "sess-a", "ra"), userMsg("a2"), taggedReply(store, "sess-a", "ra2"), userMsg("a3")}
	registerTurn(store, "sess-a", "", []llm.Message{userMsg("hi"), assistantMsg("ra"), userMsg("a2")}, "ra2")
	if m := store.match(aTurn3, userChainOf(aTurn3), ""); m.session == nil || m.session.ID != "sess-a" {
		t.Fatalf("A turn3 must route to sess-a, got %+v", m)
	}
}

// === 重放/回滚边界 ==========================================================

// 客户端回滚到会话的祖先状态（链是会话链的前缀）→ 回放缓存的回声而不是重复 prompt。
func TestRollbackToAncestorReplaysEcho(t *testing.T) {
	store := newTestStore(t)
	registerTurn(store, "sess-a", "conv-1", []llm.Message{userMsg("u1")}, "r1")
	registerTurn(store, "sess-a", "conv-1", []llm.Message{userMsg("u1"), assistantMsg("r1"), userMsg("u2")}, "r2")

	// 客户端只发到 u1（回滚）：链是 stored 的真前缀。
	rollback := []llm.Message{userMsg("u1"), assistantMsg("r1")}
	match := store.match(rollback, userChainOf(rollback), "conv-1")
	if match.session == nil || match.session.ID != "sess-a" {
		t.Fatalf("rollback must stay on session, got %+v", match)
	}
	// prompt 为空时上层应回放缓存/回声，而不是把 u1 重新发给 devin。
	if tail := tailAfter(rollback, match.consumed); len(tail) != 0 {
		t.Fatalf("ancestor prefix must yield empty tail, got %d", len(tail))
	}
}
