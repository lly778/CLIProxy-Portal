package agy

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type RuntimeConfig struct {
	Command, Home, Secret    string
	MaxSessions              int
	IdleTimeout, ToolTimeout time.Duration
}
type agentEvent struct {
	Text      string
	Reasoning bool
	Call      *pendingTool
	Result    *promptResult
	Err       error
	Official  *officialToolEvent
}
type promptResult struct {
	Stop  string `json:"stopReason"`
	Usage struct {
		Input     *int64 `json:"inputTokens"`
		Output    *int64 `json:"outputTokens"`
		Cached    *int64 `json:"cachedReadTokens"`
		Reasoning *int64 `json:"thoughtTokens"`
		Total     *int64 `json:"totalTokens"`
	} `json:"usage"`
}
type reportedTool struct {
	Server, Name string
	Conflict     bool
	Official     bool
}
type session struct {
	mu                                                 sync.Mutex
	requestMu                                          sync.Mutex
	ID, tenant, secret, owner, model, system, toolHash string
	effort                                             string
	options                                            []configOption
	tools                                              []tool
	advertisedTools, loadedTools                       []tool
	history                                            []string
	items                                              []map[string]any
	policy                                             toolPolicy
	attempts                                           map[string]bool
	mcpWaits                                           map[string]chan struct{}
	responses                                          map[string]bool
	pending                                            map[string]*pendingTool
	reported                                           map[string]reportedTool
	events                                             chan agentEvent
	life                                               context.Context
	cancel                                             context.CancelFunc
	last                                               time.Time
	waitAt                                             time.Time
	running, handshake                                 bool
	internal                                           bool
	loading                                            bool
	persisted                                          bool
	usageCursor                                        int64
	proc                                               *process
	officialActive                                     map[string]bool
	officialVersion                                    int
}
type Manager struct {
	cfg             RuntimeConfig
	mu              sync.Mutex
	startMu         sync.Mutex
	proc            *process
	sessions        map[string]*session
	disk            *sessionDiskCache
	cacheError      bool
	quotaMu         sync.Mutex
	quota           QuotaSnapshot
	quotaAt         time.Time
	quotaFlight     chan struct{}
	quotaGeneration uint64
	quotaReadTest   func(context.Context) (QuotaSnapshot, error)
	authMu          sync.Mutex
	authJob         *loginJob
	authCommandTest func(context.Context, string) *exec.Cmd
	mcpURL          string
	mcpServer       *http.Server
	slot            chan struct{}
	life            context.Context
	cancel          context.CancelFunc
	summarizeTest   func(context.Context, string, []any, func()) (string, error)
	checkpointCache map[string]checkpointCacheEntry
	attemptMu       sync.Mutex
	attempts        map[string]promptAttempt
}

func NewManager(cfg RuntimeConfig) (*Manager, error) {
	if cfg.Command == "" || !filepath.IsAbs(cfg.Command) || !filepath.IsAbs(cfg.Home) || len(cfg.Secret) < 32 {
		return nil, errors.New("AGY runtime requires absolute paths and a private service secret")
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = 2
	}
	if cfg.MaxSessions > 16 {
		return nil, errors.New("AGY session limit exceeds 16")
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 60 * time.Second
	}
	if cfg.ToolTimeout <= 0 {
		cfg.ToolTimeout = 30 * time.Minute
	}
	if err := prepareHome(cfg.Home); err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(context.Background())
	m := &Manager{cfg: cfg, sessions: map[string]*session{}, slot: make(chan struct{}, cfg.MaxSessions), life: life, cancel: cancel}
	disk, e := openSessionDiskCache(cfg.Home, cfg.Secret)
	if e != nil {
		cancel()
		return nil, e
	}
	m.disk = disk
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		cancel()
		_ = m.disk.db.Close()
		return nil, e
	}
	m.mcpURL = "http://" + l.Addr().String() + "/mcp/"
	m.mcpServer = &http.Server{Handler: http.HandlerFunc(m.mcp), ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 16 << 10}
	go m.mcpServer.Serve(l)
	go m.sweep()
	return m, nil
}
func prepareHome(home string) error {
	dir := filepath.Join(home, "config", "plugins", "portal-bridge", "agents")
	for _, d := range []string{dir, filepath.Join(home, "agy"), filepath.Join(home, "workspace")} {
		if e := os.MkdirAll(d, 0700); e != nil {
			return e
		}
	}
	files := map[string]string{filepath.Join(filepath.Dir(dir), "plugin.json"): `{"name":"portal-bridge"}`, filepath.Join(dir, "portal-bridge.md"): officialAgentDefinition}
	hooks, err := officialHooks()
	if err != nil {
		return err
	}
	files[filepath.Join(filepath.Dir(dir), "hooks.json")] = hooks
	for p, v := range files {
		if e := os.WriteFile(p, []byte(v), 0600); e != nil {
			return e
		}
	}
	return nil
}
func (m *Manager) Close() {
	m.cancel()
	_ = m.mcpServer.Close()
	m.mu.Lock()
	p := m.proc
	for _, s := range m.sessions {
		s.cancel()
	}
	m.sessions = map[string]*session{}
	m.mu.Unlock()
	if p != nil {
		p.close()
	}
	if m.disk != nil {
		_ = m.disk.db.Close()
	}
}
func (m *Manager) ensure(ctx context.Context) (*process, error) {
	m.startMu.Lock()
	defer m.startMu.Unlock()
	m.mu.Lock()
	p := m.proc
	m.mu.Unlock()
	if p != nil && p.alive() {
		return p, nil
	}
	p, e := startProcess(m.cfg.Command, m.cfg.Home, filepath.Join(m.cfg.Home, "workspace"), m.update, m.permission)
	if e != nil {
		return nil, e
	}
	ctx, cancel := shortContext(ctx)
	defer cancel()
	var init struct {
		Protocol     int            `json:"protocolVersion"`
		Capabilities map[string]any `json:"agentCapabilities"`
	}
	e = p.call(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}, "clientInfo": map[string]string{"name": "cliproxy-portal", "version": "1"}}, &init)
	if e != nil || init.Protocol != 1 {
		p.close()
		if e != nil {
			return nil, e
		}
		return nil, fail(503, "protocol_unsupported", "该 AGY 版本未提供兼容的 ACP 协议")
	}
	p.canLoad, _ = init.Capabilities["loadSession"].(bool)
	m.mu.Lock()
	m.proc = p
	m.mu.Unlock()
	return p, nil
}
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{State: "not_connected", Sessions: len(m.sessions), MaxSessions: m.cfg.MaxSessions, CacheError: m.cacheError}
	if m.disk != nil {
		s.DiskSessions = m.disk.count.Load()
	}
	for _, v := range m.sessions {
		v.mu.Lock()
		if v.running {
			s.ActiveSessions++
		}
		if !v.waitAt.IsZero() {
			s.WaitingSessions++
		}
		v.mu.Unlock()
	}
	if m.proc != nil && m.proc.alive() {
		s.State = "connected"
		s.Ready = true
	}
	return s
}
func (m *Manager) Discover(ctx context.Context) ([]Model, error) {
	select {
	case m.slot <- struct{}{}:
		defer func() { <-m.slot }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	p, e := m.ensure(ctx)
	if e != nil {
		return nil, e
	}
	ctx, cancel := shortContext(ctx)
	defer cancel()
	var n newSession
	if e = p.call(ctx, "session/new", map[string]any{"cwd": filepath.Join(m.cfg.Home, "workspace"), "mcpServers": []any{}}, &n); e != nil {
		return nil, e
	}
	defer func() {
		c, done := shortContext(context.Background())
		defer done()
		_ = p.call(c, "session/close", map[string]string{"sessionId": n.ID}, nil)
	}()
	models := []Model{}
	seen := map[string]bool{}
	// Config options expose canonical model families; availableModels can
	// additionally contain legacy effort suffixes that are not picker values.
	for _, o := range n.Options {
		if o.ID == "model" || o.Category == "model" {
			for _, v := range o.Options {
				if validID(v.Value) && !seen[v.Value] {
					models = append(models, Model{ID: v.Value, Name: v.Name})
					seen[v.Value] = true
				}
			}
		}
	}
	if len(models) == 0 {
		for _, v := range n.Models.Available {
			if validID(v.ID) && !seen[v.ID] {
				models = append(models, Model{ID: v.ID, Name: v.Name})
				seen[v.ID] = true
			}
		}
	}
	if len(models) > 64 {
		return nil, fail(502, "catalog_too_large", "AGY 模型目录超过当前接入上限")
	}
	for i := range models {
		var v struct {
			Options []configOption `json:"configOptions"`
		}
		if e = p.call(ctx, "session/set_config_option", map[string]string{"sessionId": n.ID, "configId": "model", "value": models[i].ID}, &v); e != nil {
			return nil, e
		}
		for _, o := range v.Options {
			if o.ID == "thought_level" || o.Category == "thought_level" {
				for _, level := range o.Options {
					value := PublicEffort(level.Value)
					if validID(value) {
						models[i].Levels = append(models[i].Levels, value)
					}
				}
			}
		}
	}
	return models, nil
}
func (m *Manager) create(ctx context.Context, owner string, r request) (*session, error) {
	return m.createSession(ctx, owner, r, false)
}
func (m *Manager) createSession(ctx context.Context, owner string, r request, internal bool) (*session, error) {
	system, e := systemText(r)
	if e != nil {
		return nil, e
	}
	largeContext, e := oversizedToolContext(eagerTools(r.Tools))
	if e != nil {
		return nil, e
	}
	largeContext += clientCapabilityText(eagerTools(r.Tools))
	if len(system)+len(largeContext) > MaxText {
		return nil, fail(413, "tool_definitions_too_large", "系统指令与大型工具定义合计超过安全输入上限")
	}
	loaded, e := historicalLoadedTools(r)
	if e != nil {
		return nil, e
	}
	p, e := m.ensure(ctx)
	if e != nil {
		return nil, e
	}
	ctx, cancel := shortContext(ctx)
	defer cancel()
	life, done := context.WithCancel(m.life)
	s := &session{tenant: newID("s_"), secret: newID("token_"), owner: owner, model: r.Model, system: digest(system), toolHash: digest(r.Tools), tools: r.Tools, pending: map[string]*pendingTool{}, reported: map[string]reportedTool{}, responses: map[string]bool{}, events: make(chan agentEvent, 256), life: life, cancel: done, last: time.Now(), proc: p}
	s.advertisedTools = eagerTools(r.Tools)
	s.loadedTools = loaded
	s.tools = mergeTools(s.advertisedTools, loaded)
	if len(s.tools) > 256 {
		done()
		return nil, fail(400, "too_many_tools", "当前已加载工具数量超过 256 个")
	}
	s.toolHash = digest(s.tools)
	s.internal = internal
	s.officialVersion = officialToolsVersion
	// Summary workers share the same session bound as ordinary generations.
	// They may replace completed cache entries, never pending tools.
	victim, reserved := m.reserveSession(s)
	if !reserved {
		done()
		return nil, fail(429, "session_limit", "AGY 会话已满，请关闭或等待旧会话结束")
	}
	if victim != nil {
		e = m.archiveSession(ctx, victim)
		victim.requestMu.Unlock()
		if e != nil {
			m.drop(s)
			m.mu.Lock()
			m.sessions[victim.tenant] = victim
			m.mu.Unlock()
			return nil, e
		}
	}
	var n newSession
	params := map[string]any{"cwd": filepath.Join(m.cfg.Home, "workspace"), "mcpServers": []any{map[string]any{"type": "http", "name": bridgeName, "url": m.mcpURL + s.tenant, "headers": []any{map[string]string{"name": "Authorization", "value": "Bearer " + s.secret}}}}}
	if internal {
		params["mcpServers"] = []any{}
	}
	// This is an Antigravity extension, not a claim of portable role semantics.
	if system+largeContext != "" {
		params["_meta"] = map[string]any{"antigravity": map[string]any{"systemPrompt": map[string]string{"append": system + largeContext}}}
	}
	if e = p.call(ctx, "session/new", params, &n); e != nil {
		m.drop(s)
		return nil, e
	}
	m.mu.Lock()
	s.mu.Lock()
	s.ID = n.ID
	s.options = n.Options
	s.mu.Unlock()
	m.mu.Unlock()
	if e = m.selectOptions(ctx, s, r); e != nil {
		m.drop(s)
		return nil, e
	}
	return s, nil
}

// Completed sessions are a bounded cache, not occupied generation slots. A new
// conversation can replace the oldest idle entry; active generations, pending
// tools and responses still being delivered must remain attached to their owner.
// The caller holds the returned victim's request lock until it is closed.
func (m *Manager) reserveSession(s *session) (*session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var victim *session
	if len(m.sessions) >= m.cfg.MaxSessions {
		var oldest time.Time
		for _, candidate := range m.sessions {
			candidate.mu.Lock()
			idle := !candidate.running && candidate.waitAt.IsZero()
			last := candidate.last
			candidate.mu.Unlock()
			if idle && (victim == nil || last.Before(oldest)) {
				victim, oldest = candidate, last
			}
		}
		if victim == nil || !victim.requestMu.TryLock() {
			return nil, false
		}
		victim.mu.Lock()
		idle := !victim.running && victim.waitAt.IsZero()
		victim.mu.Unlock()
		if !idle {
			victim.requestMu.Unlock()
			return nil, false
		}
		delete(m.sessions, victim.tenant)
	}
	m.sessions[s.tenant] = s
	return victim, true
}
func (m *Manager) selectOptions(ctx context.Context, s *session, r request) error {
	for _, selectValue := range []struct{ key, value string }{{"model", strings.TrimPrefix(r.Model, Prefix)}, {"thought_level", r.Reasoning.Effort}} {
		if selectValue.value == "" {
			continue
		}
		key, value := selectValue.key, selectValue.value
		s.mu.Lock()
		options := append([]configOption(nil), s.options...)
		s.mu.Unlock()
		found := false
		for _, o := range options {
			if o.ID == key || o.Category == key {
				key = o.ID
				for _, v := range o.Options {
					if PublicEffort(v.Value) == PublicEffort(value) {
						value = v.Value
						found = true
					}
				}
			}
		}
		if !found {
			return fail(400, "unsupported_model_option", "模型或思考档位不在 AGY 当前声明中")
		}
		var out struct {
			Options []configOption `json:"configOptions"`
		}
		if e := s.proc.call(ctx, "session/set_config_option", map[string]string{"sessionId": s.ID, "configId": key, "value": value}, &out); e != nil {
			return e
		}
		s.mu.Lock()
		s.options = out.Options
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.effort = r.Reasoning.Effort
	if s.effort == "" {
		for _, option := range s.options {
			if option.ID == "thought_level" || option.Category == "thought_level" {
				s.effort = PublicEffort(option.Current)
				break
			}
		}
	}
	s.mu.Unlock()
	return nil
}
func (m *Manager) drop(s *session) {
	m.settleAttempts(s, false)
	if s.cancel != nil {
		s.cancel()
	}
	m.mu.Lock()
	delete(m.sessions, s.tenant)
	m.mu.Unlock()
	if s.ID != "" && s.proc != nil && s.proc.alive() {
		s.proc.notify("session/cancel", map[string]string{"sessionId": s.ID})
		go func() {
			ctx, cancel := shortContext(context.Background())
			defer cancel()
			_ = s.proc.call(ctx, "session/close", map[string]string{"sessionId": s.ID}, nil)
		}()
	}
}
func (m *Manager) sweep() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-m.life.Done():
			return
		case <-t.C:
			m.mu.Lock()
			all := []*session{}
			for _, s := range m.sessions {
				all = append(all, s)
			}
			m.mu.Unlock()
			for _, s := range all {
				s.mu.Lock()
				expired := !s.proc.alive() || (!s.waitAt.IsZero() && time.Since(s.waitAt) > m.cfg.ToolTimeout) || (!s.running && s.waitAt.IsZero() && time.Since(s.last) > 30*time.Minute)
				s.mu.Unlock()
				if expired && s.requestMu.TryLock() {
					s.mu.Lock()
					idle := !s.running && s.waitAt.IsZero()
					s.mu.Unlock()
					if idle && s.proc.alive() {
						ctx, cancel := shortContext(m.life)
						if m.archiveSession(ctx, s) == nil {
							m.mu.Lock()
							delete(m.sessions, s.tenant)
							m.mu.Unlock()
						}
						cancel()
					} else {
						m.drop(s)
					}
					s.requestMu.Unlock()
				}
			}
			_ = m.pruneSessionCache(time.Now())
		}
	}
}
func (m *Manager) update(raw json.RawMessage) {
	var p struct {
		ID     string `json:"sessionId"`
		Update struct {
			Kind    string `json:"sessionUpdate"`
			Content struct {
				Text string `json:"text"`
			} `json:"content"`
			CallID   string         `json:"toolCallId"`
			Server   string         `json:"mcpServerName"`
			Name     string         `json:"mcpToolName"`
			ToolName string         `json:"name"`
			Status   string         `json:"status"`
			Args     map[string]any `json:"rawInput"`
			Meta     map[string]any `json:"_meta"`
		} `json:"update"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	m.mu.Lock()
	var s *session
	for _, v := range m.sessions {
		if v.ID == p.ID {
			s = v
			break
		}
	}
	m.mu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	loading := s.loading
	s.mu.Unlock()
	if loading {
		return
	}
	u := p.Update
	if u.Kind == "tool_call" || u.Kind == "tool_call_update" {
		if s.internal {
			select {
			case s.events <- agentEvent{Err: fail(502, "compaction_tool_denied", "摘要过程不允许执行工具")}:
			default:
				s.cancel()
			}
			return
		}
		server, name := u.Server, u.Name
		if v, ok := u.Meta["mcpServerName"].(string); ok {
			server = v
		}
		if v, ok := u.Meta["mcpToolName"].(string); ok {
			name = v
		}
		official := server == "" && name == "" && isOfficialTool(u.ToolName)
		if official {
			name = u.ToolName
		}
		s.mu.Lock()
		old := s.reported[u.CallID]
		if old.Server != "" && server != "" && old.Server != server || old.Name != "" && name != "" && old.Name != name {
			old.Conflict = true
		}
		if server != "" {
			old.Server = server
		}
		if name != "" {
			old.Name = name
		}
		old.Official = old.Official || official
		s.reported[u.CallID] = old
		if official {
			if s.officialActive == nil {
				s.officialActive = map[string]bool{}
			}
			if u.Status == "completed" || u.Status == "failed" {
				delete(s.officialActive, u.CallID)
			} else {
				s.officialActive[u.CallID] = true
			}
		}
		s.mu.Unlock()
		if official {
			m.sendOfficialEvent(s, officialToolEvent{ID: u.CallID, Name: name, Status: u.Status, Args: u.Args})
		}
		return
	}
	if u.Kind == "agent_message_chunk" || u.Kind == "agent_thought_chunk" {
		if u.Content.Text == "" {
			return
		}
		if u.Kind == "agent_message_chunk" {
			if failure := agentFailure(u.Content.Text); failure != nil {
				select {
				case s.events <- agentEvent{Err: failure}:
				case <-s.life.Done():
				default:
					s.cancel()
				}
				return
			}
		}
		select {
		case s.events <- agentEvent{Text: u.Content.Text, Reasoning: u.Kind == "agent_thought_chunk"}:
		case <-s.life.Done():
		default:
			s.cancel()
		}
	}
}

// The official CLI sometimes reports service errors as a warning message and
// then returns end_turn. Do not classify that as a successful assistant answer.
func agentFailure(text string) error {
	v := strings.ToLower(strings.TrimSpace(text))
	if !strings.HasPrefix(v, "⚠") {
		return nil
	}
	if strings.Contains(v, "service has been disabled") || strings.Contains(v, "account has been suspended") {
		return fail(503, "account_disabled", "AGY 官方账号服务已停用，请更换账号或向官方申诉")
	}
	if strings.Contains(v, "quota") || strings.Contains(v, "resource exhausted") || strings.Contains(v, "rate limit") {
		return fail(429, "quota_exhausted", "AGY 当前额度不足或触发上游限流")
	}
	if strings.Contains(v, "authentication") || strings.Contains(v, "sign in") || strings.Contains(v, "unauthenticated") {
		return fail(503, "auth_required", "AGY 需要重新登录")
	}
	if strings.Contains(v, "api error") {
		return fail(502, "agent_api_error", "AGY 上游调用失败")
	}
	return nil
}
func (m *Manager) permission(method string, raw json.RawMessage) (any, error) {
	if method != "session/request_permission" {
		return nil, errors.New("unsupported client capability")
	}
	var p struct {
		ID   string `json:"sessionId"`
		Call struct {
			ID string `json:"toolCallId"`
		} `json:"toolCall"`
		Options []struct {
			ID   string `json:"optionId"`
			Kind string `json:"kind"`
		} `json:"options"`
	}
	_ = json.Unmarshal(raw, &p)
	m.mu.Lock()
	var s *session
	for _, v := range m.sessions {
		if v.ID == p.ID {
			s = v
			break
		}
	}
	m.mu.Unlock()
	allowed := false
	if s != nil {
		s.mu.Lock()
		v, ok := s.reported[p.Call.ID]
		if ok && !v.Conflict && v.Server == bridgeName && s.handshake && s.running {
			allowed = v.Name == awaitName || v.Name == callToolName || v.Name == describeToolName
			for _, t := range s.tools {
				allowed = allowed || t.mcpName() == v.Name
			}
		}
		if ok && !v.Conflict && v.Official && v.Server == "" && !s.internal && s.running {
			allowed = s.policy.permitsOfficial(v.Name)
		}
		s.mu.Unlock()
	}
	kind := "reject_once"
	if allowed {
		kind = "allow_once"
	}
	for _, o := range p.Options {
		if o.Kind == kind {
			return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": o.ID}}, nil
		}
	}
	return map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}, nil
}
