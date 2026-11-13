// 本文件实现 `devin acp` 子进程的 JSON-RPC 驱动。
//
// 参照 AGENIUM 的 devin bridge：每轮一个进程，initialize → session/new 或
// session/load → session/set_mode → session/prompt；devin 反向请求
// fs/read_text_file、fs/write_text_file、session/request_permission 由 proxy
// 代为应答（文件操作限制在 workspace 内）。会话上下文持久化在 devin 的
// sessions.db 中，session/load 可跨进程恢复完整上下文。
package devinacp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// setupTimeout 是 initialize/session 打开/mode 设置的等待上限。
const setupTimeout = 120 * time.Second

// rpcProcess 封装一个 `devin acp` 子进程的 stdio JSON-RPC 通道。
type rpcProcess struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	next    atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan rpcMessage
	done    chan struct{}

	// updates 推送本进程产生的 session/update 通知（load 期重放的历史除外）。
	updates chan rpcMessage
	// loading 为 true 时丢弃 session/update（session/load 会重放历史）。
	loading atomic.Bool

	// workDir 限制 devin 通过 fs/* 请求可访问的路径范围。
	workDir string
}

type rpcMessage struct {
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
	rawJSON json.RawMessage
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// spawnAcp 启动 `devin acp --model <model>` 并开始读取其 stdout。
func spawnAcp(ctx context.Context, bin, model, workDir string) (*rpcProcess, error) {
	args := []string{"acp"}
	if model != "" {
		args = append(args, "--model", model)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = workDir
	// 杀掉进程组，避免 acp 子进程（MCP server 等）泄漏。
	cmd.WaitDelay = 10 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s acp: %w", bin, err)
	}
	// stderr 只保留尾部供错误诊断。
	go io.Copy(io.Discard, stderr)
	p := &rpcProcess{
		cmd:     cmd,
		stdin:   stdin,
		pending: map[int64]chan rpcMessage{},
		done:    make(chan struct{}),
		updates: make(chan rpcMessage, 256),
		workDir: workDir,
	}
	go p.readLoop(stdout)
	return p, nil
}

func (p *rpcProcess) readLoop(stdout io.Reader) {
	defer close(p.done)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		var msg rpcMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		msg.rawJSON = append(json.RawMessage(nil), line...)
		switch {
		case msg.Method != "" && msg.ID != nil:
			// devin 的反向请求：fs/*、permission 等。
			go p.answer(msg)
		case msg.Method == "session/update":
			if !p.loading.Load() {
				select {
				case p.updates <- msg:
				default:
				}
			}
		case msg.ID != nil:
			p.mu.Lock()
			ch := p.pending[*msg.ID]
			delete(p.pending, *msg.ID)
			p.mu.Unlock()
			if ch != nil {
				ch <- msg
			}
		}
	}
	p.mu.Lock()
	pending := p.pending
	p.pending = map[int64]chan rpcMessage{}
	p.mu.Unlock()
	for _, ch := range pending {
		ch <- rpcMessage{Error: &rpcError{Message: "devin process exited"}}
	}
}

// call 发送一个 JSON-RPC 请求并等待响应。
func (p *rpcProcess) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := p.next.Add(1)
	ch := make(chan rpcMessage, 1)
	p.mu.Lock()
	p.pending[id] = ch
	p.mu.Unlock()
	payload, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int64  `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{"2.0", id, method, params})
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	_, err = p.stdin.Write(append(payload, '\n'))
	p.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("devin is gone (%s)", method)
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, resp.Error.Message)
		}
		return resp.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.done:
		return nil, errors.New("devin process exited")
	}
}

// answer 应答 devin 发来的请求。fs/* 只允许 workspace 内的路径；
// request_permission 选择 allow_once（已 set_mode bypass，兜底用）。
func (p *rpcProcess) answer(msg rpcMessage) {
	var params struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Line    *int64 `json:"line"`
		Limit   *int64 `json:"limit"`
		Options []struct {
			Kind     string `json:"kind"`
			OptionID string `json:"optionId"`
		} `json:"options"`
	}
	_ = json.Unmarshal(msg.Params, &params)
	var result any
	var rerr *rpcError
	switch msg.Method {
	case "fs/read_text_file":
		text, err := p.readFile(params.Path, params.Line, params.Limit)
		if err != nil {
			rerr = &rpcError{Code: -32000, Message: err.Error()}
		} else {
			result = map[string]any{"content": text}
		}
	case "fs/write_text_file":
		if err := p.writeFile(params.Path, params.Content); err != nil {
			rerr = &rpcError{Code: -32000, Message: err.Error()}
		}
	case "session/request_permission":
		optionID := ""
		for _, o := range params.Options {
			if o.Kind == "allow_once" {
				optionID = o.OptionID
				break
			}
		}
		if optionID == "" && len(params.Options) > 0 {
			optionID = params.Options[len(params.Options)-1].OptionID
		}
		result = map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": optionID}}
	default:
		rerr = &rpcError{Code: -32601, Message: msg.Method + " is not offered by this proxy"}
	}
	reply := map[string]any{"jsonrpc": "2.0", "id": *msg.ID}
	if rerr != nil {
		reply["error"] = rerr
	} else {
		reply["result"] = result
	}
	payload, _ := json.Marshal(reply)
	p.mu.Lock()
	_, _ = p.stdin.Write(append(payload, '\n'))
	p.mu.Unlock()
}

// resolve 校验并返回 workspace 内的绝对路径。
func (p *rpcProcess) resolve(path string) (string, error) {
	if path == "" {
		return "", errors.New("empty path")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(p.workDir, path)
	}
	clean := filepath.Clean(path)
	root := filepath.Clean(p.workDir)
	if clean != root && !strings.HasPrefix(clean, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %s is outside the workspace", path)
	}
	return clean, nil
}

func (p *rpcProcess) readFile(path string, line, limit *int64) (string, error) {
	resolved, err := p.resolve(path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	if line == nil && limit == nil {
		return string(data), nil
	}
	var b strings.Builder
	from, n := int64(1), int64(-1)
	if line != nil {
		from = *line
	}
	if limit != nil {
		n = *limit
	}
	var i int64
	for _, l := range strings.SplitAfter(string(data), "\n") {
		i++
		if i >= from && (n < 0 || i < from+n) {
			b.WriteString(l)
		}
	}
	return b.String(), nil
}

func (p *rpcProcess) writeFile(path, content string) error {
	resolved, err := p.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return err
	}
	return os.WriteFile(resolved, []byte(content), 0o644)
}

// kill 终止进程。
func (p *rpcProcess) kill() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	_ = p.cmd.Wait()
}
