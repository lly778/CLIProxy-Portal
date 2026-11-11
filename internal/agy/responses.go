package agy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

func (m *Manager) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /internal/status", func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(m.Status()) })
	mux.HandleFunc("POST /internal/resident-limit", m.residentLimitHTTP)
	mux.HandleFunc("GET /internal/catalog", func(w http.ResponseWriter, r *http.Request) {
		models, e := m.Discover(r.Context())
		if e != nil {
			WriteError(w, e)
			return
		}
		_ = json.NewEncoder(w).Encode(models)
	})
	mux.HandleFunc("GET /internal/quota", m.quotaHTTP)
	mux.HandleFunc("GET /internal/account", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(readAccount(m.cfg.Home))
	})
	mux.HandleFunc("POST /internal/account/login", m.loginStartHTTP)
	mux.HandleFunc("POST /internal/account/finish", m.loginFinishHTTP)
	mux.HandleFunc("POST /internal/account/cancel", m.loginCancelHTTP)
	mux.HandleFunc("POST /internal/account/logout", m.logoutHTTP)
	mux.HandleFunc("POST /internal/cancel", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		all := []*session{}
		for _, s := range m.sessions {
			all = append(all, s)
		}
		m.mu.Unlock()
		for _, s := range all {
			m.drop(s)
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /internal/responses", m.responsesHTTP)
	mux.HandleFunc("POST /internal/compact", m.compactHTTP)
	mux.HandleFunc("POST /internal/official-tool-policy", m.officialPolicyHTTP)
	mux.HandleFunc("GET /internal/images/{session}/{name}", m.imageHTTP)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, m.cfg.Secret) {
			WriteError(w, fail(401, "unauthorized", "内部服务身份验证失败"))
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}
func (m *Manager) responsesHTTP(w http.ResponseWriter, r *http.Request) {
	owner := r.Header.Get("X-Portal-Owner")
	if owner == "" || len(owner) > 256 {
		WriteError(w, fail(401, "owner_required", "缺少请求所属用户"))
		return
	}
	b, e := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if e != nil || len(b) > MaxBody {
		WriteError(w, fail(413, "request_too_large", "AGY 请求超过大小限制"))
		return
	}
	req, e := parseRequest(b)
	if e != nil {
		WriteError(w, e)
		return
	}
	if e = m.expandCheckpoints(owner, &req); e != nil {
		WriteError(w, e)
		return
	}
	req.owner = owner
	if _, e = systemText(req); e != nil {
		WriteError(w, e)
		return
	}
	// Validate all content, including historical tool images, before releasing
	// any parked tool or changing the active allowlist.
	if _, e = promptBlocksLimit(req.Items, MaxBody); e != nil {
		WriteError(w, e)
		return
	}
	queue := time.NewTimer(10 * time.Second)
	defer queue.Stop()
	select {
	case m.slot <- struct{}{}:
		defer func() { <-m.slot }()
	case <-queue.C:
		WriteError(w, fail(429, "busy", "AGY 正忙，请稍后重试"))
		return
	case <-r.Context().Done():
		return
	}
	s, e := m.find(owner, req)
	if e != nil {
		WriteError(w, e)
		return
	}
	out := newResponse(w, req.Stream, req.Model)
	out.imageBase = publicImageBase(r.Header.Get("X-Portal-Image-Base-URL"))
	out.imageSecret = m.cfg.Secret
	out.parallel = req.Parallel
	out.unavailableTools = append([]string(nil), req.UnavailableTools...)
	sendItems := req.Items
	completeHistory := req.Previous == ""
	if s != nil {
		s.mu.Lock()
		req.Items, _ = alignToolIDs(s, req.Items)
		s.mu.Unlock()
	}
	if s != nil && req.Previous != "" {
		s.mu.Lock()
		completeHistory = len(s.items) > 0
		if completeHistory && !prefix(s.history, requestFingerprints(req.Items)) && !fullHistoryInput(req.Items) {
			req.Items = append(append([]map[string]any(nil), s.items...), req.Items...)
		}
		s.mu.Unlock()
		sendItems = req.Items
	}
	sendItems = req.Items
	if s != nil && s.proc == nil {
		system, _ := systemText(req)
		if s.system != digest(system) || s.model != req.Model || s.persisted && s.officialVersion < officialToolsVersion {
			s, e = m.rebuildSession(r.Context(), s, req)
		} else {
			s, e = m.restoreSession(r.Context(), s, req)
		}
		if e != nil {
			out.fail(e)
			return
		}
	}
	if s == nil {
		if _, sizeErr := promptBlocks(sendItems); contextTooLarge(sizeErr) {
			progress := modelProgress(w)
			if req.Stream {
				last := time.Time{}
				progress = func() {
					if time.Since(last) < 2*time.Second {
						return
					}
					last = time.Now()
					_ = out.event("response.in_progress", map[string]any{"response": out.envelope("in_progress", Usage{}), "stage": "compacting_history"})
				}
			}
			sendItems, e = m.compactItems(r.Context(), req, progress)
			if e != nil {
				out.fail(e)
				return
			}
		} else if sizeErr != nil {
			WriteError(w, sizeErr)
			return
		}
		s, e = m.create(r.Context(), owner, req)
		if e != nil {
			out.fail(e)
			return
		}
	}
	if !s.requestMu.TryLock() {
		out.fail(fail(409, "session_busy", "该 AGY 会话已有进行中的请求"))
		return
	}
	locked := s
	defer func() { locked.requestMu.Unlock() }()
	if !s.proc.alive() || s.life.Err() != nil {
		m.drop(s)
		out.fail(fail(503, "worker_lost", "AGY 会话已失效，请重新发起对话"))
		return
	}
	system, e := systemText(req)
	if e != nil {
		out.fail(e)
		return
	}
	s.mu.Lock()
	same := s.system == digest(system) && s.model == req.Model
	running := s.running
	history := append([]string(nil), s.history...)
	s.mu.Unlock()
	if !same || len(history) > 0 && !prefix(history, requestFingerprints(req.Items)) && completeHistory {
		if !req.ToolsProvided {
			s.mu.Lock()
			req.Tools = append([]tool{}, s.tools...)
			s.mu.Unlock()
			req.ToolsProvided = true
		}
		s, e = m.rebuildSession(r.Context(), s, req)
		if e != nil {
			out.fail(e)
			return
		}
		locked.requestMu.Unlock()
		locked = s
		locked.requestMu.Lock()
		running = false
		history = nil
		sendItems = req.Items
	}
	if len(history) == 0 {
		if _, sizeErr := promptBlocks(sendItems); contextTooLarge(sizeErr) {
			sendItems, e = m.compactItems(r.Context(), req, modelProgress(w))
			if e != nil {
				out.fail(e)
				m.drop(s)
				return
			}
		} else if sizeErr != nil {
			out.fail(sizeErr)
			m.drop(s)
			return
		}
	}
	if !running {
		e = m.selectOptions(r.Context(), s, req)
	} else {
		s.mu.Lock()
		if req.Reasoning.Effort != "" && s.effort != req.Reasoning.Effort {
			e = fail(409, "effort_changed", "工具等待期间不能更改思考强度")
		}
		s.mu.Unlock()
	}
	if e != nil {
		out.fail(e)
		return
	}
	s.mu.Lock()
	out.effort = s.effort
	s.mu.Unlock()
	fps := requestFingerprints(req.Items)
	s.mu.Lock()
	policyTools, _ := requestedTools(s, req)
	policy, policyErr := requestToolPolicy(req, policyTools)
	awaiting := hasWaitingResults(s)
	s.mu.Unlock()
	if policyErr != nil {
		out.fail(policyErr)
		return
	}
	if running || awaiting {
		if e = deliverToolsForRequest(s, req.Items, &req); e != nil {
			out.fail(e)
			return
		}
		if !running {
			blocks, waiting, err := lateResultPrompt(s)
			if err != nil {
				out.fail(err)
				return
			}
			if waiting || len(blocks) == 0 {
				out.fail(fail(409, "waiting_for_tools", "AGY 正在等待完整工具结果"))
				return
			}
			m.startUsage(s)
			m.startPrompt(s, blocks)
		}
	} else {
		items := sendItems
		if prefix(history, fps) {
			items = items[len(history):]
		}
		blocks, err := promptBlocks(items)
		if err != nil {
			out.fail(err)
			return
		}
		if len(blocks) == 0 {
			out.fail(fail(400, "missing_input", "没有新增消息或待处理工具结果"))
			return
		}
		if text := policy.text(); text != "" {
			blocks = append([]any{map[string]string{"type": "text", "text": text}}, blocks...)
		}
		s.mu.Lock()
		nextTools, loaded := requestedTools(s, req)
		delta, toolHash, deltaErr := sessionToolDelta(s, nextTools)
		s.mu.Unlock()
		if deltaErr != nil {
			out.fail(deltaErr)
			return
		}
		if delta != "" {
			size := len(delta)
			for _, raw := range blocks {
				if block, ok := raw.(map[string]string); ok {
					size += len(block["text"])
				}
			}
			if size > MaxText {
				out.fail(fail(413, "tool_update_too_large", "工具定义更新与最新输入合计超过单次安全上限"))
				return
			}
			blocks = append([]any{map[string]string{"type": "text", "text": delta}}, blocks...)
		}
		s.mu.Lock()
		s.running = true
		s.reported = map[string]reportedTool{}
		s.last = time.Now()
		s.mu.Unlock()
		// Invalidate the previous completed checkpoint before changing native
		// state. A crash must not restore a partially completed tool turn.
		if err := m.saveSession(s, false); err != nil {
			s.mu.Lock()
			s.running = false
			s.mu.Unlock()
			out.fail(err)
			return
		}
		s.mu.Lock()
		s.tools, s.loadedTools, s.toolHash = nextTools, loaded, toolHash
		s.policy = policy
		s.mu.Unlock()
		m.startUsage(s)
		m.startPrompt(s, blocks)
	}
	s.mu.Lock()
	s.history = fps
	s.items = append([]map[string]any(nil), req.Items...)
	if req.Previous != "" {
		s.responses[req.Previous] = false
	}
	s.waitAt = time.Time{}
	s.mu.Unlock()
	// No synthetic heartbeat: only genuine model chunks reset this watchdog.
	idle := time.NewTimer(m.cfg.IdleTimeout)
	defer idle.Stop()
	var batch *time.Timer
	var batchC <-chan time.Time
	completed := false
	continuations := 0
	var accrued Usage
	defer func() {
		if batch != nil {
			batch.Stop()
		}
		if !completed {
			m.drop(s)
		}
	}()
	for {
		select {
		case event := <-s.events:
			if event.Err != nil {
				out.fail(event.Err)
				return
			}
			if event.Official != nil {
				if !idle.Stop() {
					select {
					case <-idle.C:
					default:
					}
				}
				idle.Reset(m.cfg.IdleTimeout)
				if e = out.official(m.cfg.Home, s, *event.Official); e != nil {
					out.fail(e)
					return
				}
			}
			if event.Text != "" {
				if !idle.Stop() {
					select {
					case <-idle.C:
					default:
					}
				}
				idle.Reset(m.cfg.IdleTimeout)
				if e = out.text(event.Text, event.Reasoning); e != nil {
					return
				}
			}
			if event.Call != nil {
				s.mu.Lock()
				if event.Call.Delivered {
					s.mu.Unlock()
					continue
				}
				event.Call.Delivered = true
				s.mu.Unlock()
				if e = out.call(event.Call); e != nil {
					return
				}
				if batch == nil {
					batch = time.NewTimer(150 * time.Millisecond)
					batchC = batch.C
				}
			}
			if event.Result != nil {
				if err := waitMCPReplies(r.Context(), s); err != nil {
					out.fail(err)
					return
				}
				s.mu.Lock()
				s.running = false
				s.last = time.Now()
				s.mu.Unlock()
				if event.Result.Stop != "end_turn" && event.Result.Stop != "stop" && event.Result.Stop != "max_tokens" {
					out.fail(fail(502, "agent_incomplete", "AGY 未正常完成本轮生成"))
					return
				}
				usage := m.consumeUsage(s)
				accrued = addStepUsage(accrued, usage)
				blocks, waiting, lateErr := lateResultPrompt(s)
				if lateErr != nil {
					out.fail(lateErr)
					return
				}
				if len(blocks) > 0 {
					m.startPrompt(s, blocks)
					continue
				}
				if waiting {
					// Expose calls already queued even if native completion raced
					// the tool batch timer. Never mark a pending action successful.
					s.mu.Lock()
					calls := []*pendingTool{}
					for _, id := range pendingIDs(s) {
						c := s.pending[id]
						if !c.Delivered {
							c.Delivered = true
							calls = append(calls, c)
						}
					}
					s.waitAt = time.Now()
					s.mu.Unlock()
					for _, c := range calls {
						if out.call(c) != nil {
							return
						}
					}
					if out.finish(accrued) != nil {
						return
					}
					m.remember(s, out)
					completed = true
					return
				}
				if event.Result.Stop == "max_tokens" {
					if continuations >= 3 {
						out.fail(fail(502, "continuation_limit", "AGY 已连续续写 3 次仍达到输出上限，请继续提问"))
						return
					}
					continuations++
					m.startPrompt(s, []any{map[string]string{"type": "text", "text": "Continue exactly where your answer stopped. Do not repeat existing text or completed tool actions."}})
					continue
				}
				s.mu.Lock()
				requiredMissing := s.policy.Mode == "required" && !s.policy.Used
				s.mu.Unlock()
				if requiredMissing {
					out.fail(fail(502, "required_tool_not_called", "AGY 未调用本次请求指定的客户端工具"))
					return
				}
				if out.finish(accrued) != nil {
					return
				}
				m.remember(s, out)
				s.mu.Lock()
				s.pending = map[string]*pendingTool{}
				s.mu.Unlock()
				_ = m.saveSession(s, true)
				completed = true
				return
			}
		case <-batchC:
			usage := m.consumeUsage(s)
			if e = out.finish(addStepUsage(accrued, usage)); e != nil {
				return
			}
			s.mu.Lock()
			s.waitAt = time.Now()
			s.last = time.Now()
			s.mu.Unlock()
			m.remember(s, out)
			completed = true
			return
		case <-idle.C:
			out.fail(fail(504, "model_idle_timeout", "AGY 连续 60 秒没有生成进展"))
			return
		case <-s.life.Done():
			out.fail(fail(503, "worker_lost", "AGY 会话已终止"))
			return
		case <-r.Context().Done():
			return
		}
	}
}
func (m *Manager) find(owner string, r request) (*session, error) {
	r.Items = canonicalHistoryItems(r.Items)
	m.mu.Lock()
	var found *session
	live := map[string]bool{}
	for _, s := range m.sessions {
		live[s.ID] = true
		if s.owner != owner {
			continue
		}
		s.mu.Lock()
		match := sessionMatchesRequest(s, r)
		s.mu.Unlock()
		if match {
			if found != nil && found != s {
				m.mu.Unlock()
				return nil, fail(409, "ambiguous_session", "无法确认 AGY 会话，请新建对话")
			}
			found = s
		}
	}
	disk := m.disk
	m.mu.Unlock()
	if disk != nil {
		cold, err := disk.find(owner, r, live)
		if err != nil {
			var e *Error
			if errors.As(err, &e) {
				return nil, e
			}
			return nil, fail(503, "session_cache_unavailable", "AGY 硬盘会话索引暂不可用")
		}
		if cold != nil {
			if found != nil {
				return nil, fail(409, "ambiguous_session", "无法确认 AGY 会话，请新建对话")
			}
			found = cold
		}
	}
	if found == nil && r.Previous != "" {
		return nil, fail(409, "session_expired", "该 AGY 响应链已过期或不属于当前用户，请新建对话")
	}
	return found, nil
}
func deliverTools(s *session, items []map[string]any) error {
	return deliverToolsForRequest(s, items, nil)
}

func deliverToolsForRequest(s *session, items []map[string]any, req *request) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	results := map[string]any{}
	loaded := append([]tool{}, s.loadedTools...)
	for _, v := range items {
		kind, _ := v["type"].(string)
		if isToolResultItem(kind) {
			id := resultCallID(v)
			if c := s.pending[id]; c != nil {
				if !c.Delivered {
					return fail(409, "tool_not_delivered", "工具调用尚未完整交付")
				}
				if _, ok := results[id]; ok {
					return fail(400, "duplicate_result", "同一工具结果重复出现")
				}
				if isNativeClientTool(c.Tool.Type) {
					value, err := nativeResultValue(c, v)
					if err != nil {
						return err
					}
					results[id] = value
				} else if c.Tool.Type == "tool_search" {
					if v["type"] != "tool_search_output" || v["execution"] != "client" || (v["status"] != nil && v["status"] != "completed") {
						return fail(400, "invalid_tool_search_result", "请返回匹配的客户端工具发现结果")
					}
					if _, ok := v["tools"].([]any); !ok {
						return fail(400, "invalid_tool_search_result", "工具发现结果必须包含工具定义数组")
					}
					newTools, err := parseLoadedTools(v["tools"])
					if err != nil {
						return err
					}
					loaded = mergeTools(newTools, loaded)
					results[id] = map[string]any{"tools": newTools}
				} else {
					if kind != "function_call_output" && kind != "custom_tool_call_output" {
						return fail(400, "invalid_tool_result", "工具结果类型与本次客户端调用不匹配")
					}
					if _, err := toolResultBlocks(v["output"]); err != nil {
						return err
					}
					results[id] = v["output"]
					if req != nil && c.Tool.Type == "function" && c.Tool.Name == "ToolSearch" {
						discovered, err := referencedTools(v["output"], req.Tools)
						if err != nil {
							return err
						}
						loaded = mergeTools(discovered, loaded)
						results[id] = filterDiscoveryResult(v["output"], 0)
					}
				}
			}
		}
	}
	for id, c := range s.pending {
		select {
		case <-c.Ready:
			continue
		default:
		}
		if !c.Delivered {
			continue
		}
		if _, ok := results[id]; !ok {
			return fail(400, "missing_tool_result", "请返回本轮全部工具调用结果")
		}
	}
	if len(results) == 0 {
		return fail(409, "waiting_for_tools", "AGY 正在等待客户端工具结果")
	}
	for id, v := range results {
		c := s.pending[id]
		select {
		case <-c.Ready:
			if digest(v) != c.OutputHash {
				return fail(409, "result_changed", "同一工具的结果不能被重复改写")
			}
		default:
		}
	}
	nextTools := append([]tool{}, s.tools...)
	if req != nil {
		nextTools, _ = requestedTools(s, *req)
		// Newly discovered tools only become callable after this exact search
		// result and every other delivered result have passed validation.
		search := false
		for _, t := range nextTools {
			search = search || isClientSearch(t)
		}
		if search {
			nextTools = mergeTools(eagerTools(req.Tools), loaded)
			if !req.ToolsProvided {
				oldLoaded := map[string]bool{}
				for _, t := range s.loadedTools {
					oldLoaded[t.key()] = true
				}
				base := []tool{}
				for _, t := range s.tools {
					if !oldLoaded[t.key()] {
						base = append(base, t)
					}
				}
				nextTools = mergeTools(base, loaded)
			}
		} else {
			loaded = nil
		}
	}
	if len(nextTools) > 256 {
		return fail(400, "too_many_tools", "当前已加载工具数量超过 256 个")
	}
	update, toolHash, err := sessionToolDelta(s, nextTools)
	if err != nil {
		return err
	}
	extra, err := resultSuffix(s, items)
	if err != nil {
		return err
	}
	policy := s.policy
	if req != nil {
		policy, err = requestToolPolicy(*req, nextTools)
		if err != nil {
			return err
		}
		if text := policy.text(); text != "" {
			extra = append([]any{map[string]string{"type": "text", "text": text}}, extra...)
		}
	}
	ids := make([]string, 0, len(results))
	for id := range results {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	// Commit revocations and schema updates before waking any MCP waiter.
	s.tools, s.loadedTools, s.toolHash = nextTools, loaded, toolHash
	s.policy = policy
	s.pending[ids[len(ids)-1]].Extra = extra
	if update != "" {
		s.pending[ids[len(ids)-1]].Update = update
	}
	for id, v := range results {
		c := s.pending[id]
		select {
		case <-c.Ready:
		default:
			c.Output = v
			c.OutputHash = digest(v)
			close(c.Ready)
		}
	}
	return nil
}
func (m *Manager) remember(s *session, out *response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses[out.id] = true
	for _, v := range out.output {
		s.history = append(s.history, itemFingerprint(v))
		s.items = append(s.items, v)
	}
	canonicalSessionHistory(s)
	if len(s.responses) > 256 {
		s.cancel()
	}
}

type response struct {
	w                      http.ResponseWriter
	stream, started        bool
	id, model              string
	created                int64
	seq                    int
	output                 []map[string]any
	messageID              string
	textValue              string
	textIndex              int
	bytes                  int
	imageBytes             int
	reasonID, reasonValue  string
	reasonIndex            int
	effort                 string
	unavailableTools       []string
	parallel               *bool
	officialCalls          map[string]*officialResponseCall
	imageBase, imageSecret string
	imageRefs              []string
	imageLinks             []string
	imagePending           string
}

func newResponse(w http.ResponseWriter, stream bool, model string) *response {
	return &response{w: w, stream: stream, id: newID("resp_agy_"), model: model, created: time.Now().Unix(), textIndex: -1, reasonIndex: -1}
}
func (o *response) envelope(status string, usage Usage) map[string]any {
	out := o.output
	if out == nil {
		out = []map[string]any{}
	}
	v := map[string]any{"id": o.id, "object": "response", "created_at": o.created, "status": status, "model": o.model, "output": out, "error": nil, "incomplete_details": nil, "parallel_tool_calls": true}
	if o.parallel != nil {
		v["parallel_tool_calls"] = *o.parallel
	}
	if o.effort != "" {
		v["reasoning"] = map[string]string{"effort": o.effort}
	}
	if len(o.unavailableTools) > 0 {
		v["metadata"] = map[string]string{"portal_unavailable_tools": strings.Join(o.unavailableTools, ",")}
	}
	if usage.Known {
		v["usage"] = map[string]any{"input_tokens": usage.Input, "output_tokens": usage.Output, "total_tokens": usage.Total, "input_tokens_details": map[string]int64{"cached_tokens": usage.Cache}, "output_tokens_details": map[string]int64{"reasoning_tokens": usage.Reasoning}}
	} else {
		v["usage"] = nil
	}
	return v
}
func (o *response) event(kind string, v map[string]any) error {
	if !o.stream {
		return nil
	}
	if !o.started {
		o.w.Header().Set("Content-Type", "text/event-stream")
		o.w.Header().Set("Cache-Control", "no-store")
		o.w.WriteHeader(200)
		o.started = true
		if kind != "response.created" {
			initial := o.envelope("in_progress", Usage{})
			initial["output"] = []any{}
			if e := o.event("response.created", map[string]any{"response": initial}); e != nil {
				return e
			}
		}
	}
	v["type"] = kind
	v["sequence_number"] = o.seq
	o.seq++
	b, _ := json.Marshal(v)
	if _, e := io.WriteString(o.w, "event: "+kind+"\ndata: "+string(b)+"\n\n"); e != nil {
		return e
	}
	return http.NewResponseController(o.w).Flush()
}
func (o *response) writeText(v string, reasoning bool) error {
	o.bytes += len(v)
	if o.bytes > 2<<20 {
		return fail(502, "output_limit", "AGY 单步输出超过限制")
	}
	if reasoning {
		if o.reasonIndex < 0 {
			o.reasonIndex = len(o.output)
			o.reasonID = newID("rs_")
			item := map[string]any{"id": o.reasonID, "type": "reasoning", "summary": []any{}}
			o.output = append(o.output, item)
			if e := o.event("response.output_item.added", map[string]any{"output_index": o.reasonIndex, "item": item}); e != nil {
				return e
			}
			if e := o.event("response.reasoning_summary_part.added", map[string]any{"item_id": o.reasonID, "output_index": o.reasonIndex, "summary_index": 0, "part": map[string]string{"type": "summary_text", "text": ""}}); e != nil {
				return e
			}
		}
		o.reasonValue += v
		return o.event("response.reasoning_summary_text.delta", map[string]any{"item_id": o.reasonID, "delta": v, "output_index": o.reasonIndex, "summary_index": 0})
	}
	if o.textIndex < 0 {
		o.textIndex = len(o.output)
		o.messageID = newID("msg_")
		item := map[string]any{"id": o.messageID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}
		o.output = append(o.output, item)
		if e := o.event("response.output_item.added", map[string]any{"output_index": o.textIndex, "item": item}); e != nil {
			return e
		}
		if e := o.event("response.content_part.added", map[string]any{"item_id": o.messageID, "output_index": o.textIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}); e != nil {
			return e
		}
	}
	o.textValue += v
	return o.event("response.output_text.delta", map[string]any{"item_id": o.messageID, "output_index": o.textIndex, "content_index": 0, "delta": v})
}
func (o *response) call(c *pendingTool) error {
	t := c.Tool
	if isNativeClientTool(t.Type) {
		item := nativeOutputItem(c)
		index := len(o.output)
		o.output = append(o.output, item)
		initial := map[string]any{}
		for key, value := range item {
			initial[key] = value
		}
		initial["status"] = "in_progress"
		if err := o.event("response.output_item.added", map[string]any{"output_index": index, "item": initial}); err != nil {
			return err
		}
		return o.event("response.output_item.done", map[string]any{"output_index": index, "item": item})
	}
	typ := "function_call"
	v := map[string]any{"id": newID("fc_"), "status": "completed", "call_id": c.ID, "name": t.Name}
	if t.Type == "tool_search" {
		v = map[string]any{"id": newID("ts_"), "type": "tool_search_call", "status": "completed", "execution": "client", "call_id": c.ID, "arguments": c.Arguments}
		index := len(o.output)
		o.output = append(o.output, v)
		initial := map[string]any{"id": v["id"], "type": "tool_search_call", "status": "in_progress", "execution": "client", "call_id": c.ID, "arguments": map[string]any{}}
		if err := o.event("response.output_item.added", map[string]any{"output_index": index, "item": initial}); err != nil {
			return err
		}
		return o.event("response.output_item.done", map[string]any{"output_index": index, "item": v})
	}
	if t.Namespace != "" {
		v["namespace"] = t.Namespace
	}
	if t.Type == "custom" {
		typ = "custom_tool_call"
		v["input"] = c.Arguments["input"]
	} else {
		b, _ := json.Marshal(c.Arguments)
		v["arguments"] = string(b)
	}
	v["type"] = typ
	index := len(o.output)
	o.output = append(o.output, v)
	initial := map[string]any{}
	for k, value := range v {
		initial[k] = value
	}
	initial["status"] = "in_progress"
	if typ == "function_call" {
		initial["arguments"] = ""
	} else {
		initial["input"] = ""
	}
	if e := o.event("response.output_item.added", map[string]any{"output_index": index, "item": initial}); e != nil {
		return e
	}
	if typ == "function_call" {
		if e := o.event("response.function_call_arguments.delta", map[string]any{"item_id": v["id"], "output_index": index, "delta": v["arguments"]}); e != nil {
			return e
		}
		if e := o.event("response.function_call_arguments.done", map[string]any{"item_id": v["id"], "output_index": index, "arguments": v["arguments"]}); e != nil {
			return e
		}
	} else {
		if e := o.event("response.custom_tool_call_input.delta", map[string]any{"item_id": v["id"], "output_index": index, "delta": v["input"]}); e != nil {
			return e
		}
		if e := o.event("response.custom_tool_call_input.done", map[string]any{"item_id": v["id"], "output_index": index, "input": v["input"]}); e != nil {
			return e
		}
	}
	return o.event("response.output_item.done", map[string]any{"output_index": index, "item": v})
}
func (o *response) finish(u Usage) error {
	if o.imagePending != "" {
		text, _ := rewriteImageParts(o.imagePending, o.imageRefs, true)
		o.imagePending = ""
		if err := o.writeText(text, false); err != nil {
			return err
		}
	}
	if err := o.finishImages(); err != nil {
		return err
	}
	if o.reasonIndex >= 0 {
		part := map[string]string{"type": "summary_text", "text": o.reasonValue}
		item := o.output[o.reasonIndex]
		item["summary"] = []any{part}
		if e := o.event("response.reasoning_summary_text.done", map[string]any{"item_id": o.reasonID, "output_index": o.reasonIndex, "summary_index": 0, "text": o.reasonValue}); e != nil {
			return e
		}
		if e := o.event("response.reasoning_summary_part.done", map[string]any{"item_id": o.reasonID, "output_index": o.reasonIndex, "summary_index": 0, "part": part}); e != nil {
			return e
		}
		if e := o.event("response.output_item.done", map[string]any{"output_index": o.reasonIndex, "item": item}); e != nil {
			return e
		}
	}
	if o.textIndex >= 0 {
		part := map[string]any{"type": "output_text", "text": o.textValue, "annotations": []any{}}
		item := o.output[o.textIndex]
		item["status"] = "completed"
		item["content"] = []any{part}
		if e := o.event("response.output_text.done", map[string]any{"item_id": o.messageID, "output_index": o.textIndex, "content_index": 0, "text": o.textValue}); e != nil {
			return e
		}
		if e := o.event("response.content_part.done", map[string]any{"item_id": o.messageID, "output_index": o.textIndex, "content_index": 0, "part": part}); e != nil {
			return e
		}
		if e := o.event("response.output_item.done", map[string]any{"output_index": o.textIndex, "item": item}); e != nil {
			return e
		}
	}
	if o.stream {
		return o.event("response.completed", map[string]any{"response": o.envelope("completed", u)})
	}
	o.w.Header().Set("Content-Type", "application/json")
	o.started = true
	return json.NewEncoder(o.w).Encode(o.envelope("completed", u))
}
func (o *response) fail(err error) {
	if !o.started {
		WriteError(o.w, err)
		return
	}
	var e *Error
	if !errors.As(err, &e) {
		e = &Error{502, "agent_error", "AGY 请求中断"}
	}
	v := o.envelope("failed", Usage{})
	v["error"] = map[string]string{"code": e.Code, "message": e.Message}
	_ = o.event("response.failed", map[string]any{"response": v})
}
