package devincloud

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vta-itsec/devin-to-api/internal/llm"
)

// fakeCloud 模拟 api.devin.ai：create → working×N → finished；记录收到的请求。
type fakeCloud struct {
	t           *testing.T
	sessions    int32
	messages    map[string][]string
	pollCount   int32
	lastCreate  map[string]any
	finishAfter int32
	answer      string
}

func newFakeCloud(t *testing.T, answer string, finishAfter int32) (*fakeCloud, *httptest.Server) {
	f := &fakeCloud{t: t, messages: map[string][]string{}, finishAfter: finishAfter, answer: answer}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	return f, server
}

func (f *fakeCloud) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions":
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		f.lastCreate = payload
		id := atomic.AddInt32(&f.sessions, 1)
		sid := "devin-" + string(rune('a'+id-1))
		f.messages[sid] = []string{payload["prompt"].(string)}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session_id": sid, "url": "https://app.devin.ai/sessions/" + sid,
			"status_enum": "working",
		})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/message"):
		sid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/sessions/"), "/message")
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		f.messages[sid] = append(f.messages[sid], payload["message"].(string))
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/sessions/"):
		sid := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
		n := atomic.AddInt32(&f.pollCount, 1)
		status := "working"
		if n > f.finishAfter {
			status = "finished"
		}
		resp := map[string]any{"session_id": sid, "status_enum": status, "url": "https://app.devin.ai/sessions/" + sid}
		if status == "finished" {
			resp["messages"] = []any{map[string]any{"message": f.answer}}
		}
		_ = json.NewEncoder(w).Encode(resp)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newAdapter(t *testing.T, baseURL string) *Adapter {
	t.Helper()
	a, err := New(Config{BaseURL: baseURL, APIKey: "k", Workspace: t.TempDir(), PollInterval: 5 * time.Millisecond, ProgressEvery: 1})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func collect(stream llm.ResponseStream) (text string, err error) {
	for {
		event, e := stream.Recv(context.Background())
		if e == io.EOF {
			return text, nil
		}
		if e != nil {
			return text, e
		}
		if event.Type == llm.ResponseEventDone {
			for _, c := range event.Message.Content {
				if tc, ok := c.(llm.TextContent); ok {
					text = tc.Text
				}
			}
		}
		if event.Type == llm.ResponseEventError {
			return text, errFromMsg(event.Error.ErrorMessage)
		}
	}
}

type msgErr string

func (e msgErr) Error() string { return string(e) }
func errFromMsg(s string) error {
	return msgErr(s)
}

func userMsg(text string) llm.UserMessage {
	return llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: text}}}
}

func TestCloudCreateAndPoll(t *testing.T) {
	f, server := newFakeCloud(t, "DONE-ANSWER", 1)
	a := newAdapter(t, server.URL)
	stream, err := a.Stream(context.Background(), llm.RequestMessages{Model: CloudModel, Messages: []llm.Message{userMsg("do task X")}})
	if err != nil {
		t.Fatal(err)
	}
	text, err := collect(stream)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "DONE-ANSWER") {
		t.Fatalf("answer = %q", text)
	}
	if f.lastCreate["prompt"] == nil || !strings.Contains(f.lastCreate["prompt"].(string), "do task X") {
		t.Fatalf("create prompt = %v", f.lastCreate["prompt"])
	}
	if atomic.LoadInt32(&f.sessions) != 1 {
		t.Fatalf("sessions = %d", f.sessions)
	}
}

func TestCloudResumeViaMessage(t *testing.T) {
	f, server := newFakeCloud(t, "R2", 0)
	a := newAdapter(t, server.URL)

	msgs := []llm.Message{userMsg("task A")}
	stream, _ := a.Stream(context.Background(), llm.RequestMessages{Model: CloudModel, Messages: msgs})
	collect(stream)

	msgs = append(msgs, llm.AssistantMessage{Content: []llm.Content{llm.TextContent{Text: "R1"}}}, userMsg("follow up"))
	stream, err := a.Stream(context.Background(), llm.RequestMessages{Model: CloudModel, Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collect(stream); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&f.sessions) != 1 {
		t.Fatalf("follow-up created new session: %d", f.sessions)
	}
	got := f.messages["devin-a"]
	if len(got) != 2 || got[1] != "follow up" {
		t.Fatalf("messages = %v", got)
	}
}

func TestCloudIdenticalOpeningsIsolated(t *testing.T) {
	f, server := newFakeCloud(t, "X", 0)
	a := newAdapter(t, server.URL)
	for i := 0; i < 2; i++ {
		stream, _ := a.Stream(context.Background(), llm.RequestMessages{Model: CloudModel, Messages: []llm.Message{userMsg("same opening")}})
		collect(stream)
	}
	if atomic.LoadInt32(&f.sessions) != 2 {
		t.Fatalf("identical openings shared a session: %d", f.sessions)
	}
}

func TestCloudReplaySameChain(t *testing.T) {
	f, server := newFakeCloud(t, "REPLY-1", 0)
	a := newAdapter(t, server.URL)
	msgs := []llm.Message{userMsg("u1")}
	stream, _ := a.Stream(context.Background(), llm.RequestMessages{Model: CloudModel, Messages: msgs})
	collect(stream)

	// 追加新轮次 → 走 /message，不新建 session。
	msgs = append(msgs, llm.AssistantMessage{Content: []llm.Content{llm.TextContent{Text: "REPLY-1"}}}, userMsg("u2"))
	stream, _ = a.Stream(context.Background(), llm.RequestMessages{Model: CloudModel, Messages: msgs})
	collect(stream)
	got := f.messages["devin-a"]
	if len(got) != 2 || got[1] != "u2" {
		t.Fatalf("expected /message u2 once, got %v", got)
	}
}

func TestCloudSnapshotFromModel(t *testing.T) {
	f, server := newFakeCloud(t, "S", 0)
	a := newAdapter(t, server.URL)
	stream, _ := a.Stream(context.Background(), llm.RequestMessages{
		Model:    CloudModel + "@snapshot-b23ef738",
		Messages: []llm.Message{userMsg("x")},
	})
	collect(stream)
	if f.lastCreate["snapshot_id"] != "snapshot-b23ef738" {
		t.Fatalf("snapshot_id = %v", f.lastCreate["snapshot_id"])
	}
}

func TestCloudDefaultSnapshot(t *testing.T) {
	f, server := newFakeCloud(t, "S", 0)
	a, err := New(Config{BaseURL: server.URL, APIKey: "k", SnapshotID: "snapshot-default", Workspace: t.TempDir(), PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	stream, _ := a.Stream(context.Background(), llm.RequestMessages{Model: CloudModel, Messages: []llm.Message{userMsg("x")}})
	collect(stream)
	if f.lastCreate["snapshot_id"] != "snapshot-default" {
		t.Fatalf("snapshot_id = %v", f.lastCreate["snapshot_id"])
	}
}

func TestIsCloudModel(t *testing.T) {
	if !IsCloudModel("devin-cloud") || !IsCloudModel("devin-cloud@snapshot-x") {
		t.Fatal("devin-cloud should route")
	}
	if IsCloudModel("swe-2-max") || IsCloudModel("devin-cloudx") {
		t.Fatal("non-cloud model misrouted")
	}
	if ParseModel("devin-cloud@snapshot-a") != "snapshot-a" || ParseModel("devin-cloud") != "" {
		t.Fatal("ParseModel")
	}
}
