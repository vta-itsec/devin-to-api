package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vta-itsec/devin-to-api/internal/config"
)

// cloudCapture 记录最近一次到达 fake upstream 的请求。
type cloudCapture struct {
	header http.Header
	path   string
	method string
	body   []byte
}

// newCloudTestApp 返回挂在 fake upstream 前的应用与请求捕获器。
func newCloudTestApp(t *testing.T, upstreamKey, snapshot string) (*App, *cloudCapture) {
	t.Helper()
	cap := &cloudCapture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cap.header = r.Header.Clone()
		cap.path = r.URL.Path
		cap.method = r.Method
		cap.body = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"session_id":"devin-abc","status":"running"}`))
	}))
	t.Cleanup(upstream.Close)

	application := New(nil, config.ServerConfig{}, nil)
	application.SetAPIKey("proxy-key")
	application.SetDevinCloud(config.DevinCloudConfig{
		BaseURL:    upstream.URL,
		APIKey:     upstreamKey,
		SnapshotID: snapshot,
	})
	return application, cap
}

func cloudDo(t *testing.T, application *App, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer proxy-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	application.Router().ServeHTTP(rec, req)
	return rec
}

func TestDevinCloudCreateSessionPassthrough(t *testing.T) {
	application, cap := newCloudTestApp(t, "cloud-key", "")
	rec := cloudDo(t, application, http.MethodPost, "/v1/sessions", `{"prompt":"fix tests"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body)
	}
	if got := cap.header.Get("Authorization"); got != "Bearer cloud-key" {
		t.Fatalf("upstream auth = %q", got)
	}
	if !strings.Contains(string(cap.body), `"prompt":"fix tests"`) {
		t.Fatalf("body = %s", cap.body)
	}
	if cap.path != "/v1/sessions" {
		t.Fatalf("path = %q", cap.path)
	}
}

func TestDevinCloudSnapshotInjection(t *testing.T) {
	application, cap := newCloudTestApp(t, "cloud-key", "snapshot-abc123")
	cloudDo(t, application, http.MethodPost, "/v1/sessions", `{"prompt":"hi"}`)
	if !strings.Contains(string(cap.body), `"snapshot_id":"snapshot-abc123"`) {
		t.Fatalf("snapshot not injected: %s", cap.body)
	}
	// 显式 snapshot_id 不被覆盖。
	cloudDo(t, application, http.MethodPost, "/v1/sessions", `{"prompt":"hi","snapshot_id":"snapshot-mine"}`)
	if !strings.Contains(string(cap.body), `"snapshot_id":"snapshot-mine"`) ||
		strings.Contains(string(cap.body), `"snapshot-abc123"`) {
		t.Fatalf("explicit snapshot overridden: %s", cap.body)
	}
}

func TestDevinCloudGetAndMessage(t *testing.T) {
	application, cap := newCloudTestApp(t, "cloud-key", "")
	cloudDo(t, application, http.MethodGet, "/v1/sessions/devin-abc", "")
	if cap.path != "/v1/sessions/devin-abc" || cap.method != http.MethodGet {
		t.Fatalf("get = %s %s", cap.method, cap.path)
	}
	cloudDo(t, application, http.MethodPost, "/v1/sessions/devin-abc/message", `{"message":"ping"}`)
	if cap.path != "/v1/sessions/devin-abc/message" {
		t.Fatalf("message path = %q", cap.path)
	}
	if !strings.Contains(string(cap.body), `"message":"ping"`) {
		t.Fatalf("message body = %s", cap.body)
	}
}

func TestDevinCloudClientBearerFallback(t *testing.T) {
	// 未配置服务端 key：优先 X-Devin-Api-Key，其次透传 Authorization。
	application, cap := newCloudTestApp(t, "", "")
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", strings.NewReader(`{"prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer proxy-key")
	req.Header.Set("X-Devin-Api-Key", "devin-own-key")
	rec := httptest.NewRecorder()
	application.Router().ServeHTTP(rec, req)
	if got := cap.header.Get("Authorization"); got != "Bearer devin-own-key" {
		t.Fatalf("X-Devin-Api-Key auth = %q", got)
	}
	// 无自带 key 头时透传 Authorization。
	cloudDo(t, application, http.MethodPost, "/v1/sessions", `{"prompt":"x"}`)
	if got := cap.header.Get("Authorization"); got != "Bearer proxy-key" {
		t.Fatalf("fallback auth = %q", got)
	}
}

func TestDevinCloudNotConfigured(t *testing.T) {
	application := New(nil, config.ServerConfig{}, nil)
	rec := cloudDo(t, application, http.MethodPost, "/v1/sessions", `{"prompt":"x"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
