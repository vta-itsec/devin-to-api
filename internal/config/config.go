// 本文件定义服务启动配置及其 YAML 加载和校验逻辑。
//
// Package config 负责加载和校验服务启动配置。
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vta-itsec/devin-to-api/internal/devinauth"
	"gopkg.in/yaml.v3"
)

// DefaultDevinBaseURL 是 Devin Connect 服务的默认地址（与 devin CLI 一致）。
const DefaultDevinBaseURL = "https://server.codeium.com"

// DefaultModel 是未显式配置 devin.model 时使用的模型；adaptive 由上游自动选路。
const DefaultModel = "adaptive"

// Config 保存服务启动所需的全部配置；服务运行期间不会热更新。
type Config struct {
	// Server 保存 HTTP 服务配置。
	Server ServerConfig `yaml:"server"`
	// Devin 保存 Devin Connect 上游配置。
	Devin DevinConfig `yaml:"devin"`
	// Debug 保存仅用于本地诊断的日志配置。
	Debug DebugConfig `yaml:"debug"`
	// Dashboard 保存管理面板配置。
	Dashboard DashboardConfig `yaml:"dashboard"`
	// Auth 保存对外 OpenAI 兼容接口的访问控制配置。
	Auth AuthConfig `yaml:"auth"`
}

// ServerConfig 保存 HTTP 服务监听配置。
type ServerConfig struct {
	// Listen 是 HTTP 服务监听地址。
	Listen string `yaml:"listen"`
	// MaxConcurrency 是同时处理的 /v1/* 请求数上限；0 表示使用默认值。
	MaxConcurrency int `yaml:"max_concurrency"`
}

// DevinConfig 保存 Devin Connect 上游调用配置。
type DevinConfig struct {
	// Backend 选择上游驱动："connect"（默认，直连 GetChatMessage RPC，每请求新 trajectory）
	// 或 "acp"（`devin acp` 子进程，按对话前缀绑定持久 devin 会话）。
	Backend string `yaml:"backend"`
	// AcpBin 是 devin 可执行文件路径；默认 "devin"（走 PATH）。
	AcpBin string `yaml:"acp_bin"`
	// Workspace 是 ACP 后端的工作目录：devin fs/exec 工具可见范围，也是会话 cwd。
	Workspace string `yaml:"workspace"`
	// AcpMode 是会话权限模式（bypass/smart/ask/plan/accept-edits）；默认 bypass。
	AcpMode string `yaml:"acp_mode"`
	// AcpStateFile 持久化对话→devin 会话映射；默认 <workspace>/.devin-to-api-sessions.json。
	AcpStateFile string `yaml:"acp_state_file"`
	// AcpProcIdle 是常驻 devin acp 进程的空闲回收时长（如 "15m"）；默认 15m。
	// 常驻进程免除每轮的 spawn + session/load 重放开销。
	AcpProcIdle string `yaml:"acp_proc_idle"`
	// AcpProcMax 是常驻进程数上限；默认 8，超出时淘汰最老空闲进程。
	AcpProcMax int `yaml:"acp_proc_max"`

	// BaseURL 是 Devin Connect 服务的基础地址。
	BaseURL string `yaml:"base_url"`
	// Token 是 Devin session token；不会写入日志。
	Token string `yaml:"token"`
	// CredentialsFile 是 `devin auth login` 写入的 credentials.toml 路径；
	// Token 为空时按请求实时读取该文件，用户登录/登出无需重启。
	CredentialsFile string `yaml:"credentials_file"`
	// Model 是 Devin chat model UID。
	Model string `yaml:"model"`
	// Proxy 是可选的 HTTP/HTTPS/SOCKS5 代理地址；为空时直连或走系统环境变量。
	Proxy string `yaml:"proxy"`
	// ForceHTTP1 为 true 时强制使用 HTTP/1.1，每请求独立 TCP 连接，
	// 避免 HTTP/2 单连接多 stream 复用导致的上游并发瓶颈（首字延迟飙升/卡住）。
	// 行为对齐 Devin 客户端多窗口各自独立连接的模式。默认 true。
	ForceHTTP1 *bool `yaml:"force_http1"`

	// AuthSource 描述上游凭据来源，用于启动日志与 /healthz 展示；不参与请求。
	AuthSource string `yaml:"-"`
}

// DebugConfig 保存请求级调试日志配置。
type DebugConfig struct {
	// Enabled 表示是否在配置文件同目录的 logs 下写入请求调试日志。
	Enabled bool `yaml:"enabled"`
}

// DashboardConfig 保存管理面板配置。
type DashboardConfig struct {
	// Password 是面板访问密码；为空则不要求登录，直接进入面板。
	Password string `yaml:"password"`
}

// AuthConfig 保存对外 OpenAI 兼容接口的访问控制配置。
type AuthConfig struct {
	// APIKey 是客户端访问 /v1/* 接口所需的密钥；为空时不启用鉴权。
	APIKey string `yaml:"api_key"`
}

// Load 从 YAML 文件读取配置，叠加环境变量后校验。
// 配置文件不存在时按空配置处理，允许纯环境变量运行（容器场景）。
func Load(path string) (Config, error) {
	var config Config
	file, err := os.Open(path)
	switch {
	case err == nil:
		defer file.Close()
		decoder := yaml.NewDecoder(file)
		decoder.KnownFields(true)
		if err := decoder.Decode(&config); err != nil {
			return Config{}, fmt.Errorf("decode config %q: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
		// 无配置文件：走 env-only 路径，listen/model 等由默认值兜底。
	default:
		return Config{}, fmt.Errorf("open config %q: %w", path, err)
	}
	config.applyEnv()
	if err := config.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate config %q: %w", path, err)
	}
	return config, nil
}

// applyEnv 用环境变量覆盖对应配置项；YAML 显式值优先于默认值，环境变量优先于 YAML。
func (config *Config) applyEnv() {
	if v := os.Getenv("DEVIN_TO_API_LISTEN"); v != "" {
		config.Server.Listen = v
	}
	if v := os.Getenv("DEVIN_BASE_URL"); v != "" {
		config.Devin.BaseURL = v
	}
	if v := os.Getenv("DEVIN_SESSION_TOKEN"); v != "" {
		config.Devin.Token = v
		config.Devin.AuthSource = "DEVIN_SESSION_TOKEN"
	}
	if v := os.Getenv("DEVIN_CREDENTIALS_FILE"); v != "" {
		config.Devin.CredentialsFile = v
	}
	if v := os.Getenv("DEVIN_MODEL"); v != "" {
		config.Devin.Model = v
	}
	if v := os.Getenv("DEVIN_PROXY"); v != "" {
		config.Devin.Proxy = v
	}
	if v := os.Getenv("DEVIN_BACKEND"); v != "" {
		config.Devin.Backend = v
	}
	if v := os.Getenv("DEVIN_ACP_BIN"); v != "" {
		config.Devin.AcpBin = v
	}
	if v := os.Getenv("DEVIN_WORKSPACE"); v != "" {
		config.Devin.Workspace = v
	}
	if v := os.Getenv("DEVIN_ACP_MODE"); v != "" {
		config.Devin.AcpMode = v
	}
	if v := os.Getenv("DEVIN_ACP_PROC_IDLE"); v != "" {
		config.Devin.AcpProcIdle = v
	}
	if v := os.Getenv("DEVIN_ACP_PROC_MAX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			config.Devin.AcpProcMax = n
		}
	}
	if v := os.Getenv("PROXY_API_KEY"); v != "" {
		config.Auth.APIKey = v
	}
	if v := os.Getenv("DASHBOARD_PASSWORD"); v != "" {
		config.Dashboard.Password = v
	}
	if v := os.Getenv("DEBUG"); v == "true" || v == "1" {
		config.Debug.Enabled = true
	}
}

// Validate 检查配置中的必填项，并设置默认值。
func (config *Config) Validate() error {
	if config.Server.Listen == "" {
		config.Server.Listen = ":8080"
	}
	if config.Server.MaxConcurrency <= 0 {
		config.Server.MaxConcurrency = 1024
	}
	// ForceHTTP1 默认开启：HTTP/2 单连接多 stream 复用是并发首字延迟飙升的根因。
	if config.Devin.ForceHTTP1 == nil {
		force := true
		config.Devin.ForceHTTP1 = &force
	}
	if config.Devin.CredentialsFile == "" {
		config.Devin.CredentialsFile = devinauth.DefaultPath()
	}
	if config.Devin.Token == "" {
		// 无显式 token：走 credentials.toml 实时读取（devin auth login 来源），
		// base_url 缺省时取文件中的 api_server_url。
		config.Devin.AuthSource = fmt.Sprintf("devin auth login (%s)", config.Devin.CredentialsFile)
		if credentials, err := devinauth.Load(config.Devin.CredentialsFile); err == nil {
			if config.Devin.BaseURL == "" {
				config.Devin.BaseURL = credentials.APIServerURL
			}
		}
	} else if config.Devin.AuthSource == "" {
		config.Devin.AuthSource = "config.yaml token"
	}
	if config.Devin.BaseURL == "" {
		config.Devin.BaseURL = DefaultDevinBaseURL
	}
	if config.Devin.Model == "" {
		config.Devin.Model = DefaultModel
	}
	switch strings.ToLower(strings.TrimSpace(config.Devin.Backend)) {
	case "":
		config.Devin.Backend = "connect"
	case "connect", "acp":
		config.Devin.Backend = strings.ToLower(config.Devin.Backend)
	default:
		return fmt.Errorf("devin.backend must be \"connect\" or \"acp\", got %q", config.Devin.Backend)
	}
	if config.Devin.Backend == "acp" {
		if config.Devin.AcpBin == "" {
			config.Devin.AcpBin = "devin"
		}
		if config.Devin.Workspace == "" {
			config.Devin.Workspace = filepath.Join(os.Getenv("HOME"), ".devin-to-api", "workspace")
		}
		if config.Devin.AcpMode == "" {
			config.Devin.AcpMode = "bypass"
		}
	}
	return nil
}
