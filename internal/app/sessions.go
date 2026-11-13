// 本文件实现 Devin Cloud Sessions REST API (api.devin.ai) 的透传端点。
//
// 与 chat completions 走 devin CLI/Connect 不同，这里把请求原样转发到
// api.devin.ai/v1/sessions*，由服务端 Bearer <DEVIN_API_KEY> 鉴权。
// 请求体不做编解码，因此 prompt/snapshot_id/playbook_id/tags 等官方字段
// 以及未来新增字段都自动支持。
package app

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/vta-itsec/devin-to-api/internal/config"
)

// devinCloudClient 保存 api.devin.ai 透传所需的配置与 HTTP 客户端。
type devinCloudClient struct {
	// baseURL 是上游基础地址，默认 https://api.devin.ai。
	baseURL string
	// apiKey 是服务端注入的 Bearer key；为空时透传客户端 Authorization。
	apiKey string
	// snapshotID 是可选默认快照：POST /v1/sessions 且请求体未带 snapshot_id 时注入。
	snapshotID string
	// http 是上游调用客户端。
	http *http.Client
}

// SetDevinCloud 启用 /v1/sessions* 透传端点；cloud 为 nil 或 baseURL 为空则不启用。
func (application *App) SetDevinCloud(cloud config.DevinCloudConfig) {
	if strings.TrimSpace(cloud.BaseURL) == "" {
		return
	}
	application.cloud = &devinCloudClient{
		baseURL:    strings.TrimRight(cloud.BaseURL, "/"),
		apiKey:     cloud.APIKey,
		snapshotID: cloud.SnapshotID,
		http:       &http.Client{Timeout: 60 * time.Second},
	}
}

// createDevinSession 处理 POST /v1/sessions。
func (application *App) createDevinSession(writer http.ResponseWriter, request *http.Request) {
	application.proxyDevinCloud(writer, request, true)
}

// listDevinSessions 处理 GET /v1/sessions。
func (application *App) listDevinSessions(writer http.ResponseWriter, request *http.Request) {
	application.proxyDevinCloud(writer, request, false)
}

// getDevinSession 处理 GET /v1/sessions/{sessionID}。
func (application *App) getDevinSession(writer http.ResponseWriter, request *http.Request) {
	application.proxyDevinCloud(writer, request, false)
}

// messageDevinSession 处理 POST /v1/sessions/{sessionID}/message。
func (application *App) messageDevinSession(writer http.ResponseWriter, request *http.Request) {
	application.proxyDevinCloud(writer, request, false)
}

// proxyDevinCloud 把请求原样转发到 api.devin.ai 对应路径并回写响应。
// injectSnapshot 仅用于 session 创建：配置了默认 snapshot_id 且请求体缺少该字段时注入。
func (application *App) proxyDevinCloud(writer http.ResponseWriter, request *http.Request, injectSnapshot bool) {
	if application.cloud == nil {
		writeJSONError(writer, http.StatusServiceUnavailable, "devin cloud passthrough is not configured")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, 8<<20))
	if err != nil {
		writeJSONError(writer, http.StatusBadRequest, "read request body: "+err.Error())
		return
	}
	if injectSnapshot && application.cloud.snapshotID != "" {
		body = injectSnapshotID(body, application.cloud.snapshotID)
	}
	upstream, err := http.NewRequestWithContext(request.Context(), request.Method,
		application.cloud.baseURL+request.URL.RequestURI(), strings.NewReader(string(body)))
	if err != nil {
		writeJSONError(writer, http.StatusBadGateway, "build upstream request: "+err.Error())
		return
	}
	if contentType := request.Header.Get("Content-Type"); contentType != "" {
		upstream.Header.Set("Content-Type", contentType)
	} else if len(body) > 0 {
		upstream.Header.Set("Content-Type", "application/json")
	}
	upstream.Header.Set("Accept", "application/json")
	if application.cloud.apiKey != "" {
		upstream.Header.Set("Authorization", "Bearer "+application.cloud.apiKey)
	} else if key := upstreamDevinKey(request); key != "" {
		// 未配置服务端 key：优先客户端自带的 Devin key 头（与代理 auth 解耦），
		// 其次透传客户端 Authorization（代理未开鉴权时）。
		upstream.Header.Set("Authorization", "Bearer "+key)
	} else if auth := request.Header.Get("Authorization"); auth != "" {
		upstream.Header.Set("Authorization", auth)
	} else {
		writeJSONError(writer, http.StatusServiceUnavailable,
			"devin cloud api key is not configured (set DEVIN_API_KEY or pass a client Bearer token)")
		return
	}
	response, err := application.cloud.http.Do(upstream)
	if err != nil {
		writeJSONError(writer, http.StatusBadGateway, "devin cloud request: "+err.Error())
		return
	}
	defer response.Body.Close()
	for key, values := range response.Header {
		if strings.EqualFold(key, "Content-Type") || strings.EqualFold(key, "Content-Length") {
			for _, value := range values {
				writer.Header().Add(key, value)
			}
		}
	}
	if writer.Header().Get("Content-Type") == "" {
		writer.Header().Set("Content-Type", "application/json")
	}
	writer.WriteHeader(response.StatusCode)
	_, _ = io.Copy(writer, response.Body)
}

// upstreamDevinKey 提取调用方自带的 Devin Cloud key（X-Devin-Api-Key 头）。
// 不能用 X-Api-Key：PROXY_API_KEY 开启时该头已被代理鉴权占用。
func upstreamDevinKey(request *http.Request) string {
	return strings.TrimSpace(request.Header.Get("X-Devin-Api-Key"))
}

// injectSnapshotID 在请求体缺少 snapshot_id 时注入默认值；非法 JSON 原样返回。
func injectSnapshotID(body []byte, snapshotID string) []byte {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	if existing, ok := payload["snapshot_id"].(string); ok && existing != "" {
		return body
	}
	payload["snapshot_id"] = snapshotID
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return encoded
}
