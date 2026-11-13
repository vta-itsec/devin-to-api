// 本文件实现按 model 名在多个后端适配器之间路由的复用器。
//
// 例如 model="devin-cloud" 走 Devin Cloud sessions 适配器，
// 其余走主适配器（acp/connect）。
package adapter

import (
	"context"

	"github.com/vta-itsec/devin-to-api/internal/llm"
)

// Mux 把请求按模型名路由到 primary 或备用适配器。
type Mux struct {
	// Primary 是默认适配器。
	Primary Adapter
	// Alt 是备用适配器（可为 nil，此时 Route 永远走 Primary）。
	Alt Adapter
	// Route 判定某模型名是否走 Alt。
	Route func(model string) bool
}

// Stream 按模型路由。
func (m *Mux) Stream(ctx context.Context, req llm.RequestMessages) (llm.ResponseStream, error) {
	if m.Alt != nil && m.Route != nil && m.Route(req.Model) {
		return m.Alt.Stream(ctx, req)
	}
	return m.Primary.Stream(ctx, req)
}

// ListModels 合并两个适配器的模型目录（Alt 在前，便于客户端看到特殊模型）。
func (m *Mux) ListModels(ctx context.Context) ([]ModelInfo, error) {
	var models []ModelInfo
	if m.Alt != nil {
		if alt, err := m.Alt.ListModels(ctx); err == nil {
			models = append(models, alt...)
		}
	}
	primary, err := m.Primary.ListModels(ctx)
	if err != nil {
		return models, err
	}
	return append(models, primary...), nil
}
