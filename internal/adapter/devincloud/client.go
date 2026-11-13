// 本文件是 api.devin.ai Sessions REST API 的精简客户端。
package devincloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// client 调 api.devin.ai 的 /v1/sessions* REST。
type client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func newClient(baseURL, apiKey string) *client {
	return &client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 60 * time.Second},
	}
}

// sessionStatus 是 GET /v1/sessions/{id} 需要的最小字段集；
// 其余字段保留在 Extra 中供答案提取兜底。
type sessionStatus struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	// StatusEnum 是较新版本的状态机字段（working/blocked/finished/...）。
	StatusEnum string         `json:"status_enum"`
	URL        string         `json:"url"`
	Title      string         `json:"title"`
	Extra      map[string]any `json:"-"`
}

func (c *client) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read %s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode %s %s: %w", method, path, err)
		}
	}
	return nil
}

// createSession POST /v1/sessions；返回 session_id 与 url。
// extra 允许调用方透传 playbook_id/tags/unlisted 等官方字段。
func (c *client) createSession(ctx context.Context, prompt, snapshotID string, extra map[string]any) (sessionStatus, error) {
	payload := map[string]any{"prompt": prompt}
	for key, value := range extra {
		if value != nil && value != "" {
			payload[key] = value
		}
	}
	if snapshotID != "" {
		payload["snapshot_id"] = snapshotID
	}
	var raw map[string]any
	if err := c.do(ctx, http.MethodPost, "/v1/sessions", payload, &raw); err != nil {
		return sessionStatus{}, err
	}
	return decodeSession(raw), nil
}

// sendMessage POST /v1/sessions/{id}/message。
func (c *client) sendMessage(ctx context.Context, sessionID, message string) error {
	return c.do(ctx, http.MethodPost, "/v1/sessions/"+sessionID+"/message",
		map[string]any{"message": message}, nil)
}

// getSession GET /v1/sessions/{id}；宽松解码，未知字段留在 Extra。
func (c *client) getSession(ctx context.Context, sessionID string) (sessionStatus, error) {
	var raw map[string]any
	if err := c.do(ctx, http.MethodGet, "/v1/sessions/"+sessionID, nil, &raw); err != nil {
		return sessionStatus{}, err
	}
	return decodeSession(raw), nil
}

func decodeSession(raw map[string]any) sessionStatus {
	s := sessionStatus{Extra: raw}
	for _, key := range []string{"session_id", "id"} {
		if v, ok := raw[key].(string); ok && v != "" {
			s.SessionID = v
			break
		}
	}
	s.Status, _ = raw["status"].(string)
	s.StatusEnum, _ = raw["status_enum"].(string)
	s.URL, _ = raw["url"].(string)
	s.Title, _ = raw["title"].(string)
	return s
}

// terminal 报告状态是否意味着可以取答案了。
// blocked 也算终止态：Devin 停下来等人回复/输入，当前轮次应返回。
func (s sessionStatus) terminal() bool {
	switch strings.ToLower(firstNonEmpty(s.StatusEnum, s.Status)) {
	case "finished", "done", "completed", "blocked", "stopped":
		return true
	default:
		return false
	}
}

// dead 报告状态是否意味着 session 已死，后续 turn 需要新建。
func (s sessionStatus) dead() bool {
	switch strings.ToLower(firstNonEmpty(s.StatusEnum, s.Status)) {
	case "expired", "failed", "error", "suspended":
		return true
	default:
		return false
	}
}

func (s sessionStatus) statusText() string {
	return firstNonEmpty(s.StatusEnum, s.Status)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
