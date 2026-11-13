// 本文件负责加载配置、组装服务依赖并启动 HTTP 服务器。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/vta-itsec/devin-to-api/internal/adapter"
	"github.com/vta-itsec/devin-to-api/internal/adapter/devin"
	"github.com/vta-itsec/devin-to-api/internal/adapter/devinacp"
	"github.com/vta-itsec/devin-to-api/internal/app"
	"github.com/vta-itsec/devin-to-api/internal/config"
	"github.com/vta-itsec/devin-to-api/internal/dashboard"
	"github.com/vta-itsec/devin-to-api/internal/debuglog"
)

func main() {
	configPath := flag.String("config", "config.yaml", "YAML 配置文件路径")
	flag.Parse()

	absoluteConfigPath, err := filepath.Abs(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	serviceConfig, err := config.Load(absoluteConfigPath)
	if err != nil {
		log.Fatal(err)
	}

	devinConfig := devin.Config{
		BaseURL:         serviceConfig.Devin.BaseURL,
		Token:           serviceConfig.Devin.Token,
		CredentialsFile: serviceConfig.Devin.CredentialsFile,
		Model:           serviceConfig.Devin.Model,
		Proxy:           serviceConfig.Devin.Proxy,
		ForceHTTP1:      serviceConfig.Devin.ForceHTTP1 != nil && *serviceConfig.Devin.ForceHTTP1,
	}
	configured, createErr := devin.New(devinConfig)
	if createErr != nil {
		log.Fatal(createErr)
	}
	var providerAdapter adapter.Adapter = configured
	if serviceConfig.Devin.Backend == "acp" {
		procIdle := 15 * time.Minute
		if serviceConfig.Devin.AcpProcIdle != "" {
			if d, err := time.ParseDuration(serviceConfig.Devin.AcpProcIdle); err == nil && d > 0 {
				procIdle = d
			}
		}
		acpAdapter, acpErr := devinacp.New(devinacp.Config{
			Bin:         serviceConfig.Devin.AcpBin,
			WorkDir:     serviceConfig.Devin.Workspace,
			Model:       serviceConfig.Devin.Model,
			Mode:        serviceConfig.Devin.AcpMode,
			StateFile:   serviceConfig.Devin.AcpStateFile,
			ProcIdleTTL: procIdle,
			ProcMax:     serviceConfig.Devin.AcpProcMax,
			Models:      configured,
		})
		if acpErr != nil {
			log.Fatal(acpErr)
		}
		defer acpAdapter.Close()
		providerAdapter = acpAdapter
		procMax := serviceConfig.Devin.AcpProcMax
		if procMax <= 0 {
			procMax = 8
		}
		log.Printf("upstream backend: acp (bin=%s, workspace=%s, mode=%s, sessions=%d, proc_pool idle=%s max=%d)",
			serviceConfig.Devin.AcpBin, serviceConfig.Devin.Workspace, serviceConfig.Devin.AcpMode,
			acpAdapter.SessionCount(), procIdle, procMax)
	}
	log.Printf("upstream auth: %s (model=%s)", serviceConfig.Devin.AuthSource, serviceConfig.Devin.Model)
	if ok, detail := configured.AuthStatus(); !ok {
		log.Printf("WARNING: no Devin session token found at %s — run `devin auth login`, then requests work without a restart", detail)
	}
	var debugManager *debuglog.Manager
	if serviceConfig.Debug.Enabled {
		debugManager = debuglog.NewManager(filepath.Join(filepath.Dir(absoluteConfigPath), "logs"))
	}
	application := app.New(providerAdapter, serviceConfig.Server, debugManager)
	application.SetAPIKey(serviceConfig.Auth.APIKey)
	application.SetAuthStatus(func() (bool, string) { return configured.AuthStatus() })
	application.SetDashboard(dashboard.New(serviceConfig.Dashboard.Password, serviceConfig.Devin.BaseURL, devinConfig.TokenProvider(), serviceConfig.Devin.Proxy, serviceConfig.Devin.ForceHTTP1 != nil && *serviceConfig.Devin.ForceHTTP1))
	server := application.HTTPServer()
	log.Printf("HTTP server listening on %s", listenURL(server.Addr))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, server); err != nil {
		log.Fatal(err)
	}
}

// listenURL 生成启动日志中的监听描述：配置为通配地址时同时给出 localhost 可访问地址。
func listenURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return listen + " (http://localhost:" + port + ")"
	}
	return "http://" + host + ":" + port
}

func run(ctx context.Context, server interface {
	ListenAndServe() error
	Shutdown(context.Context) error
	Close() error
}) error {
	result := make(chan error, 1)
	go func() {
		result <- server.ListenAndServe()
	}()

	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		// 优雅关闭超时（可能有活跃 SSE 流），强制关闭不再报错。
		server.Close()
	}
	return nil
}
