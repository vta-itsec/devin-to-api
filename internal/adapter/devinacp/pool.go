// 本文件实现 `devin acp` 子进程池：每个 devin 会话一个常驻进程。
//
// 每轮 spawn 新进程的代价是 initialize 握手 + session/load 全量重放
// transcript——会话越长每轮越慢。常驻进程把 attach 成本摊到会话首
// 轮；之后的轮次直接 session/prompt。进程空闲超过 idleTTL 由 reaper
// 回收；池满时淘汰最老的空闲进程。
package devinacp

import (
	"sync"
	"time"
)

// attachedProc 是绑定到某 devin 会话的常驻 ACP 进程。
type attachedProc struct {
	proc      *rpcProcess
	sessionID string
	model     string
	lastUsed  time.Time
	busy      bool
}

// procPool 管理 sessionID → 常驻进程的映射。
type procPool struct {
	mu      sync.Mutex
	procs   map[string]*attachedProc
	idleTTL time.Duration
	max     int
	closed  bool
	done    chan struct{}
}

func newProcPool(idleTTL time.Duration, max int) *procPool {
	if idleTTL <= 0 {
		idleTTL = 15 * time.Minute
	}
	if max <= 0 {
		max = 8
	}
	p := &procPool{procs: map[string]*attachedProc{}, idleTTL: idleTTL, max: max, done: make(chan struct{})}
	go p.reapLoop()
	return p
}

// get 取出绑定到 sessionID 的健康进程（标记 busy）；不存在或已死返回 nil。
func (p *procPool) get(sessionID string) *attachedProc {
	p.mu.Lock()
	defer p.mu.Unlock()
	ap := p.procs[sessionID]
	if ap == nil || ap.busy {
		return nil
	}
	select {
	case <-ap.proc.done:
		delete(p.procs, sessionID)
		return nil
	default:
	}
	ap.busy = true
	return ap
}

// put 把进程归还池（会话轮次完成后）；超过容量时淘汰最老空闲进程。
func (p *procPool) put(ap *attachedProc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || ap == nil {
		return
	}
	ap.busy = false
	ap.lastUsed = time.Now()
	p.procs[ap.sessionID] = ap
	if len(p.procs) <= p.max {
		return
	}
	var oldest *attachedProc
	for _, other := range p.procs {
		if other.busy {
			continue
		}
		if oldest == nil || other.lastUsed.Before(oldest.lastUsed) {
			oldest = other
		}
	}
	if oldest != nil {
		delete(p.procs, oldest.sessionID)
		go oldest.proc.kill()
	}
}

// discard 移除并销毁进程（轮次失败/进程崩溃时）。
func (p *procPool) discard(ap *attachedProc) {
	p.mu.Lock()
	if cur := p.procs[ap.sessionID]; cur == ap {
		delete(p.procs, ap.sessionID)
	}
	p.mu.Unlock()
	ap.proc.kill()
}

// reapLoop 周期回收空闲进程。
func (p *procPool) reapLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			p.reap()
		}
	}
}

func (p *procPool) reap() {
	p.mu.Lock()
	var victims []*attachedProc
	cutoff := time.Now().Add(-p.idleTTL)
	for id, ap := range p.procs {
		if ap.busy || ap.lastUsed.After(cutoff) {
			continue
		}
		delete(p.procs, id)
		victims = append(victims, ap)
	}
	p.mu.Unlock()
	for _, ap := range victims {
		ap.proc.kill()
	}
}

// size 返回池中进程数（healthz/调试）。
func (p *procPool) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.procs)
}

// close 关闭池并杀掉所有进程（proxy 退出）。
func (p *procPool) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	procs := p.procs
	p.procs = map[string]*attachedProc{}
	p.mu.Unlock()
	for _, ap := range procs {
		ap.proc.kill()
	}
	close(p.done)
}
