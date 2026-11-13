package agy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
)

type frame struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
type rpcReply struct {
	value json.RawMessage
	err   error
}
type process struct {
	cmd       *exec.Cmd
	in        io.WriteCloser
	writeMu   sync.Mutex
	mu        sync.Mutex
	pending   map[string]chan rpcReply
	done      chan struct{}
	once      sync.Once
	seq       atomic.Int64
	canLoad   bool
	onUpdate  func(json.RawMessage)
	onRequest func(string, json.RawMessage) (any, error)
}

func startProcess(command, home, workspace string, update func(json.RawMessage), request func(string, json.RawMessage) (any, error)) (*process, error) {
	cmd := exec.Command(command, "acp", "--gemini-dir", home, "--app-data-dir", "agy", "--agent", "portal-bridge", "--process-per-session", "--disable-telemetry")
	cmd.Dir = workspace
	configureProcess(cmd)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return nil, err
	}
	// Raw agent diagnostics can contain prompts, URLs, or login credentials.
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		in.Close()
		out.Close()
		return nil, fail(503, "worker_unavailable", "AGY 进程无法启动，请检查服务器安装")
	}
	p := &process{cmd: cmd, in: in, pending: map[string]chan rpcReply{}, done: make(chan struct{}), onUpdate: update, onRequest: request}
	go p.read(out)
	go func() { _ = cmd.Wait(); p.close() }()
	return p, nil
}

func (p *process) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}
func (p *process) close() {
	p.once.Do(func() {
		close(p.done)
		_ = p.in.Close()
		killProcess(p.cmd)
		p.mu.Lock()
		defer p.mu.Unlock()
		for id, ch := range p.pending {
			ch <- rpcReply{err: fail(503, "worker_lost", "AGY 会话进程已结束，请重新发起对话")}
			delete(p.pending, id)
		}
	})
}
func (p *process) write(v any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if !p.alive() {
		return errors.New("ACP closed")
	}
	return json.NewEncoder(p.in).Encode(v)
}
func (p *process) notify(method string, params any) {
	_ = p.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}
func (p *process) call(ctx context.Context, method string, params any, out any) error {
	id, _ := json.Marshal(p.seq.Add(1))
	key := string(id)
	ch := make(chan rpcReply, 1)
	p.mu.Lock()
	if !p.alive() {
		p.mu.Unlock()
		return fail(503, "worker_lost", "AGY 进程已结束")
	}
	p.pending[key] = ch
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.pending, key); p.mu.Unlock() }()
	if err := p.write(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "method": method, "params": params}); err != nil {
		return fail(503, "transport_error", "AGY 协议连接中断")
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		if out != nil {
			return json.Unmarshal(r.value, out)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return fail(503, "worker_lost", "AGY 进程已结束")
	}
}
func (p *process) read(r io.Reader) {
	defer p.close()
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64<<10), MaxBody*3)
	for s.Scan() {
		var f frame
		if json.Unmarshal(s.Bytes(), &f) != nil || f.Version != "2.0" {
			return
		}
		if f.Method != "" {
			if len(f.ID) == 0 {
				if f.Method == "session/update" && p.onUpdate != nil {
					p.onUpdate(f.Params)
				}
				continue
			}
			// A permission request must not block reception of model updates or RPC replies.
			go func(f frame) {
				result, err := p.onRequest(f.Method, f.Params)
				v := map[string]any{"jsonrpc": "2.0", "id": f.ID}
				if err != nil {
					v["error"] = &rpcError{-32601, "Client capability unavailable"}
				} else {
					v["result"] = result
				}
				_ = p.write(v)
			}(f)
			continue
		}
		p.mu.Lock()
		ch := p.pending[string(f.ID)]
		delete(p.pending, string(f.ID))
		p.mu.Unlock()
		if ch != nil {
			r := rpcReply{value: f.Result}
			if f.Error != nil {
				r.err = mapRPCError(f.Error)
			}
			ch <- r
		}
	}
}
func mapRPCError(e *rpcError) error {
	if failure := upstreamFailure(e.Message); failure != nil {
		return failure
	}
	// Keep unknown diagnostics private; they may contain credentials or prompts.
	if e.Code == -32000 {
		return fail(503, "auth_required", "AGY 需要在服务器重新登录")
	}
	return fail(502, "agent_error", publicFailureMessage("agent_error"))
}
