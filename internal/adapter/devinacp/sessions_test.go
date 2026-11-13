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

func TestMatchNewConversation(t *testing.T) {
	store := newTestStore(t)
	chain := chainOf([]llm.Message{userMsg("hello")})
	match := store.match(chain)
	if match.session != nil || match.consumed != 0 {
		t.Fatalf("expected no match, got %+v", match)
	}
}

func TestMatchResumeAndReplay(t *testing.T) {
	store := newTestStore(t)
	u1 := chainOf([]llm.Message{userMsg("hi")})
	s := &session{ID: "sess-a"}
	store.register(s, "m", append(append([]string(nil), u1...), messageHash(assistantMsg("hello"))), "hello")

	// 第二轮：[u1, a1, u2] 前缀完全命中 → consumed=2，只剩 u2 待发送。
	turn2 := chainOf([]llm.Message{userMsg("hi"), assistantMsg("hello"), userMsg("how are you")})
	match := store.match(turn2)
	if match.session == nil || match.session.ID != "sess-a" {
		t.Fatalf("expected sess-a, got %+v", match)
	}
	if match.consumed != 2 {
		t.Fatalf("expected consumed=2, got %d", match.consumed)
	}

	// 客户端重发同一请求链 → 幂等重放。
	retry := chainOf([]llm.Message{userMsg("hi"), assistantMsg("hello")})
	match = store.match(retry)
	if match.session == nil || match.consumed != 2 {
		t.Fatalf("expected replay hit consumed=2, got %+v", match)
	}
}

func TestAmbiguousPrefixStartsFresh(t *testing.T) {
	store := newTestStore(t)
	u1 := chainOf([]llm.Message{userMsg("hi")})
	// 两个会话共享相同的 user 首轮（不同后续）→ 前缀并列 → 新建。
	a := &session{ID: "sess-a"}
	store.register(a, "m", append(append([]string(nil), u1...), messageHash(assistantMsg("reply-a"))), "reply-a")
	b := &session{ID: "sess-b"}
	store.register(b, "m", append(append([]string(nil), u1...), messageHash(assistantMsg("reply-b"))), "reply-b")

	ambiguous := chainOf([]llm.Message{userMsg("hi"), assistantMsg("something-else"), userMsg("next")})
	match := store.match(ambiguous)
	if match.session != nil {
		t.Fatalf("expected fresh session on ambiguity, got %s", match.session.ID)
	}
}

func TestLongestPrefixWins(t *testing.T) {
	store := newTestStore(t)
	u1, u2 := userMsg("a"), userMsg("b")
	// 模拟已完成的两轮：链 = [u1, a1, u2, a2]（register 会追加 assistant hash）。
	full := chainOf([]llm.Message{u1, assistantMsg("r1"), u2, assistantMsg("r2")})
	s := &session{ID: "sess-long"}
	store.register(s, "m", full, "r2")

	incoming := chainOf([]llm.Message{u1, assistantMsg("r1"), u2, assistantMsg("r2"), userMsg("c")})
	match := store.match(incoming)
	if match.session == nil || match.session.ID != "sess-long" || match.consumed != 4 {
		t.Fatalf("expected sess-long consumed=4, got %+v", match)
	}
}
