package devinacp

import (
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
	store.register(&session{ID: id}, "m", convID, userChainOf(msgs),
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
