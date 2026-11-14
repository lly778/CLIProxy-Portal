package agy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"sort"
	"strconv"
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
		_ = m.stopResponseReplays(false)
		m.mu.Lock()
		all := []*session{}
		for _, s := range m.sessions {
			all = append(all, s)
		}
		m.mu.Unlock()
		for _, s := range all {
			s.requestMu.Lock()
			m.preserveSession(s, nil)
			m.drop(s)
			s.requestMu.Unlock()
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
	withStreamKeepalive(w, r, streamKeepaliveInterval, func(w http.ResponseWriter, r *http.Request) {
		m.replayResponse(w, r, m.executeResponsesHTTP)
	})
}

func (m *Manager) executeResponsesHTTP(w http.ResponseWriter, r *http.Request) {
	if !m.beginRequest() {
		WriteError(w, fail(503, "worker_stopping", "AGY 服务正在停止"))
		return
	}
	defer m.requests.Done()
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
	if raw := r.Header.Get("X-Portal-Followup-Limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 0 || limit > maxQueuedFollowups {
			WriteError(w, fail(400, "invalid_request", "内部追加消息队列上限无效"))
			return
		}
		req.followupLimit = &limit
	}
	if e = m.expandCheckpoints(owner, &req); e != nil {
		WriteError(w, e)
		return
	}
	req.bindRequest = true
	req.owner = owner
	r, trace := m.traceRequest(r, owner, req.Model)
	defer trace.phase("request_finished")
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
	trace.phase("waiting_generation_slot")
	queue := time.NewTimer(10 * time.Second)
	defer queue.Stop()
	checkpoint := isCodexCompactionRequest(req)
	var s *session
	slotHeld := false
	select {
	case m.slot <- struct{}{}:
		slotHeld = true
	default:
		if !checkpoint {
			candidate, err := m.find(owner, req)
			if err != nil {
				WriteError(w, err)
				return
			}
			if candidate != nil && canQueueFollowup(candidate, req) {
				s = candidate
			}
		}
		if s == nil {
			select {
			case m.slot <- struct{}{}:
				slotHeld = true
			case <-queue.C:
				WriteError(w, fail(429, "busy", "AGY 正忙，请稍后重试"))
				return
			case <-r.Context().Done():
				return
			}
		}
	}
	defer func() {
		if slotHeld {
			<-m.slot
		}
	}()
	if checkpoint {
		s, e = m.prepareCodexCompaction(owner, &req)
	} else if s == nil {
		s, e = m.find(owner, req)
	}
	if e != nil {
		WriteError(w, e)
		return
	}
	trace.phase("session_loading")
	if len(req.remoteSpecs) > 0 {
		metadataCtx, done := context.WithTimeout(r.Context(), 30*time.Second)
		e = m.prepareRemoteTools(metadataCtx, owner, &req)
		done()
		if e != nil {
			WriteError(w, e)
			return
		}
	}
	out := newResponse(w, req.Stream, req.Model)
	out.trace = trace
	trace.emit(Observation{Kind: "response", State: "created", ResponseID: out.id})
	out.imageBase = publicImageBase(r.Header.Get("X-Portal-Image-Base-URL"))
	out.imageSecret = m.cfg.Secret
	out.parallel = req.Parallel
	out.unavailableTools = append([]string(nil), req.UnavailableTools...)
	// Begin the accepted stream before native session setup, compaction or
	// first-token waiting. The delivery layer can now keep the HTTP connection
	// alive without inventing model output or writing comments into the replay.
	if req.Stream {
		if e = out.event("response.created", map[string]any{"response": out.envelope("in_progress", Usage{})}); e != nil {
			return
		}
	}
	for _, item := range req.remoteCatalogItems {
		if e = out.remoteEvent(remoteMCPEvent{Item: item}); e != nil {
			return
		}
	}
	sendItems := req.Items
	completeHistory := req.Previous == ""
	if s != nil {
		s.mu.Lock()
		req.Items, _ = alignToolIDs(s, req.Items)
		s.mu.Unlock()
	}
	if s != nil && mergeResultHistory(s, &req) {
		completeHistory = true
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
		original := s
		system, _ := systemText(req)
		if s.interrupted {
			req.ResumeNote = interruptedResumeNote(s)
		}
		if checkpoint {
			if s.officialVersion < officialToolsVersion {
				out.fail(fail(409, "compaction_session_unavailable", "原 AGY 会话需要先正常续接，未改动原工具调用"))
				return
			}
			trace.phase("restoring_native_session")
			s, e = m.restoreNativeSession(r.Context(), s, req)
		} else if s.interrupted || s.system != digest(system) || s.model != req.Model || s.persisted && s.officialVersion < officialToolsVersion {
			trace.phase("rebuilding_session")
			trace.emit(Observation{Kind: "session", State: "rebuild", SessionID: s.ID, Reason: rebuildObservationReason(s, req)})
			s, e = m.rebuildSession(r.Context(), s, req)
		} else {
			trace.phase("restoring_session")
			s, e = m.restoreSession(r.Context(), s, req)
		}
		if isMissingSession(e) && !checkpoint {
			trace.phase("rebuilding_missing_session")
			s, e = m.rebuildMissingSession(r.Context(), original, req)
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
			out.fail(sizeErr)
			return
		}
		trace.phase("creating_session")
		s, e = m.create(r.Context(), owner, req)
		if e != nil {
			out.fail(e)
			return
		}
	}
	binding := s
	defer func() {
		if req.remoteLease > 0 {
			s.mu.Lock()
			if s.remoteLease == req.remoteLease {
				s.remoteConnections = nil
			}
			s.mu.Unlock()
		}
	}()
	defer func() { binding.mu.Lock(); binding.loading = false; binding.mu.Unlock() }()
	queuedLocked, finishQueued, queueErr := m.waitUserFollowup(r.Context(), s, &req, out, func() {
		if slotHeld {
			<-m.slot
			slotHeld = false
		}
	}, func() error {
		select {
		case m.slot <- struct{}{}:
			slotHeld = true
			return nil
		case <-r.Context().Done():
			return r.Context().Err()
		}
	})
	if queueErr != nil {
		out.fail(queueErr)
		return
	}
	if queuedLocked {
		sendItems = req.Items
		completeHistory = true
	}
	if !slotHeld {
		select {
		case m.slot <- struct{}{}:
			slotHeld = true
		case <-r.Context().Done():
			return
		}
	}
	if !queuedLocked && !s.requestMu.TryLock() {
		out.fail(fail(409, "session_busy", "该 AGY 会话已有进行中的请求"))
		return
	}
	s.mu.Lock()
	s.loading = false
	s.trace = trace
	trace.bind(s.ID)
	if s.proc != nil {
		evidence := s.proc.runtimeEvidence()
		trace.mu.Lock()
		trace.runtimeSHA = evidence.SHA256
		trace.mu.Unlock()
	}
	for _, call := range s.pending {
		if call.trace == nil {
			call.trace = trace
			if call.traceSession == "" {
				call.traceSession = s.ID
			}
			if call.traceRequest != "" {
				call.trace = &requestTrace{m: m, owner: owner, request: call.traceRequest, model: req.Model, session: call.traceSession}
			} else {
				call.traceRequest = trace.request
			}
		}
	}
	s.mu.Unlock()
	trace.phase("session_ready")
	locked := s
	defer func() { locked.requestMu.Unlock() }()
	if finishQueued != nil {
		defer finishQueued()
	}
	if !s.proc.alive() || s.life.Err() != nil {
		m.preserveSession(s, nil)
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
	if checkpoint {
		// Recheck after acquiring the response lock: another request may have
		// advanced this session since the read-only routing decision.
		if s.model != req.Model || !prefix(history, requestFingerprints(req.Items)) {
			s.mu.Unlock()
			out.fail(fail(409, "compaction_history_changed", "原 AGY 会话已推进，请使用最新工具结果重新压缩"))
			return
		}
		waiting := false
		for _, call := range s.pending {
			if call.Returned || call.Replayed {
				continue
			}
			if !call.Delivered || call.Replying > 0 {
				s.mu.Unlock()
				out.fail(fail(409, "compaction_tools_pending", "原工具调用仍在交付，请等待本轮工具交付完成后再压缩"))
				return
			}
			waiting = true
		}
		if running && !waiting {
			s.mu.Unlock()
			out.fail(fail(409, "compaction_turn_running", "原 AGY 仍在生成，请等待本轮完成后再压缩"))
			return
		}
		same = true
		priorPolicy := s.policy
		s.checkpointing = true
		defer func() {
			s.mu.Lock()
			s.checkpointing = false
			s.policy = priorPolicy
			s.mu.Unlock()
		}()
	}
	s.mu.Unlock()
	if !same || len(history) > 0 && !prefix(history, requestFingerprints(req.Items)) && completeHistory {
		if !req.ToolsProvided {
			s.mu.Lock()
			req.Tools = mergeTools(s.tools, s.deferredTools)
			s.mu.Unlock()
			req.ToolsProvided = true
		}
		trace.phase("rebuilding_session")
		trace.emit(Observation{Kind: "session", State: "rebuild", SessionID: s.ID, Reason: rebuildObservationReason(s, req)})
		s, e = m.rebuildSession(r.Context(), s, req)
		if e != nil {
			out.fail(e)
			return
		}
		locked.requestMu.Unlock()
		locked = s
		locked.requestMu.Lock()
		s.mu.Lock()
		s.loading = false
		s.trace = trace
		trace.bind(s.ID)
		if s.proc != nil {
			evidence := s.proc.runtimeEvidence()
			trace.mu.Lock()
			trace.runtimeSHA = evidence.SHA256
			trace.mu.Unlock()
		}
		s.mu.Unlock()
		trace.phase("session_ready")
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
		trace.phase("receiving_tool_results")
		resumingRemote, resumeErr := m.prepareRemoteContinuation(s, &req)
		if resumeErr != nil {
			out.fail(resumeErr)
			return
		}
		if !resumingRemote {
			e = deliverToolsForRequest(s, req.Items, &req)
		}
		if e != nil {
			out.fail(e)
			return
		}
		m.startRemoteJobs(s)
		if !running {
			blocks, waiting, err := lateResultPrompt(s)
			if err != nil {
				out.fail(err)
				return
			}
			if waiting && remoteWorkOnly(s) {
				rememberRequest(s, req, fps)
				s.mu.Lock()
				s.running = false
				s.mu.Unlock()
				m.startUsage(s)
				trace.phase("waiting_remote_service")
			} else if waiting || len(blocks) == 0 {
				out.fail(fail(409, "waiting_for_tools", "AGY 正在等待完整工具结果"))
				return
			} else {
				rememberRequest(s, req, fps)
				m.startUsage(s)
				trace.phase("model_generating")
				m.startPrompt(s, blocks)
			}
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
		blocks = appendResumeNote(blocks, req.ResumeNote)
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
		rememberRequest(s, req, fps)
		s.mu.Lock()
		commitToolCatalog(s, nextTools, loaded, toolHash)
		if !checkpoint {
			commitRemoteConnections(s, &req)
		}
		if req.ToolsProvided {
			s.deferredTools = deferredCatalog(req.Tools)
		}
		s.policy = policy
		s.mu.Unlock()
		m.startUsage(s)
		trace.phase("model_generating")
		m.startPrompt(s, blocks)
	}
	trace.phase("model_generating")
	s.mu.Lock()
	s.history = fps
	s.items = append([]map[string]any(nil), req.Items...)
	s.requestPrevious = req.Previous
	if req.Previous != "" {
		s.responses[req.Previous] = false
	}
	s.waitAt = time.Time{}
	s.mu.Unlock()
	// A submitted model turn may think or run official tools without emitting
	// chunks. Only cancellation, worker failure, or native completion ends it.
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
			m.preserveSession(s, out)
			m.drop(s)
		}
	}()
	for {
		select {
		case event := <-s.events:
			s.mu.Lock()
			stale := event.Epoch != 0 && event.Epoch != s.epoch
			if event.Remote != nil && s.pending[event.Remote.Call.ID] == event.Remote.Call {
				stale = false
			}
			s.mu.Unlock()
			if stale {
				continue
			}
			if event.Err != nil {
				out.fail(event.Err)
				return
			}
			if event.Search != nil {
				if e = out.search(*event.Search); e != nil {
					return
				}
			}
			if event.Remote != nil {
				select {
				case <-event.Remote.Call.Ready:
				case <-r.Context().Done():
					return
				case <-s.life.Done():
					return
				}
				s.mu.Lock()
				known := false
				for _, item := range s.items {
					if item["type"] == "mcp_call" && item["id"] == event.Remote.Item["id"] && itemFingerprint(item) == itemFingerprint(event.Remote.Item) {
						known = true
						break
					}
				}
				s.mu.Unlock()
				if !known {
					e = out.remoteEvent(*event.Remote)
				}
				if e != nil {
					return
				}
				s.mu.Lock()
				active := s.running
				s.mu.Unlock()
				if !active {
					event.Result = &promptResult{Stop: "end_turn"}
				}
			}
			if event.Official != nil {
				if e = out.official(m.cfg.Home, s, *event.Official); e != nil {
					out.fail(e)
					return
				}
			}
			if event.Text != "" {
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
				event.Call.BatchID = out.batchID
				event.Call.Delivered = true
				s.mu.Unlock()
				if e = out.call(event.Call); e != nil {
					return
				}
				s.mu.Lock()
				event.Call.receipt("dispatched")
				s.mu.Unlock()
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
				usage := m.consumeObservedUsage(s, trace)
				accrued = addStepUsage(accrued, usage)
				blocks, waiting, lateErr := lateResultPrompt(s)
				if lateErr != nil {
					out.fail(lateErr)
					return
				}
				if len(blocks) > 0 {
					rememberRequest(s, req, fps)
					trace.phase("model_generating")
					m.startPrompt(s, blocks)
					continue
				}
				if waiting {
					if remoteWorkOnly(s) {
						continue
					}
					trace.phase("waiting_client_tools")
					// Expose calls already queued even if native completion raced
					// the tool batch timer. Never mark a pending action successful.
					s.mu.Lock()
					calls := []*pendingTool{}
					for _, id := range pendingIDs(s) {
						c := s.pending[id]
						if !c.Delivered {
							c.BatchID = out.batchID
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
						s.mu.Lock()
						c.receipt("dispatched")
						s.mu.Unlock()
					}
					recordToolBatch(s, out)
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
				if err := m.finishCodexCheckpoint(r.Context(), req, out); err != nil {
					out.fail(err)
					return
				}
				recordToolBatch(s, out)
				if out.finish(accrued) != nil {
					return
				}
				m.remember(s, out)
				s.mu.Lock()
				s.pending = map[string]*pendingTool{}
				s.mu.Unlock()
				completed = true
				return
			}
		case <-batchC:
			trace.phase("waiting_client_tools")
			usage := m.consumeObservedUsage(s, trace)
			recordToolBatch(s, out)
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
		loading := s.loading
		s.mu.Unlock()
		if match {
			if loading {
				m.mu.Unlock()
				return nil, fail(409, "session_busy", "原 AGY 会话正在恢复，请等待当前续接完成")
			}
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
	wireResults := map[string]map[string]any{}
	historical := map[string]string{}
	for _, old := range s.items {
		if isToolResultItem(stringField(old, "type")) {
			historical[resultCallID(old)] = itemFingerprint(old)
		}
	}
	loaded := append([]tool{}, s.loadedTools...)
	if req != nil && req.ToolsProvided && (hasServerSearch(req.Tools) || hasServerSearch(s.tools)) {
		loaded = authorizedServerTools(req.Tools, loaded)
	}
	for _, v := range items {
		kind, _ := v["type"].(string)
		if isToolResultItem(kind) {
			id := resultCallID(v)
			if _, duplicate := wireResults[id]; duplicate {
				return fail(400, "duplicate_result", "同一工具结果重复出现")
			}
			wireResults[id] = v
			if s.pending[id] == nil && (historical[id] == "" || historical[id] != itemFingerprint(v)) {
				return fail(400, "unknown_tool_result", "工具结果无法匹配原调用，未改动当前批次")
			}
			if c := s.pending[id]; c != nil {
				if !c.Delivered {
					return fail(409, "tool_not_delivered", "工具调用尚未完整交付")
				}
				if _, ok := results[id]; ok {
					return fail(400, "duplicate_result", "同一工具结果重复出现")
				}
				if c.remote != nil {
					value, err := validateRemoteApproval(c, v, req)
					if err != nil {
						return err
					}
					results[id] = value
				} else if isNativeClientTool(c.Tool.Type) {
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
					expectedKind := "function_call_output"
					if c.Tool.Type == "custom" {
						expectedKind = "custom_tool_call_output"
					}
					if kind != expectedKind {
						return fail(400, "invalid_tool_result", "工具结果类型与本次客户端调用不匹配")
					}
					if _, present := v["output"]; !present {
						return fail(400, "invalid_tool_result", "工具结果缺少 output，原调用保持等待")
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
	if err := missingToolResults(s, results); err != nil {
		return err
	}
	if len(results) == 0 {
		return fail(409, "waiting_for_tools", "AGY 正在等待客户端工具结果")
	}
	for id, v := range results {
		c := s.pending[id]
		select {
		case <-receivedReady(c):
			if digest(v) != receivedHash(c) {
				return fail(409, "result_changed", "同一工具的结果不能被重复改写")
			}
		default:
		}
	}
	if req != nil && req.ToolsProvided && hasServerSearch(req.Tools) {
		loaded = authorizedServerTools(req.Tools, loaded)
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
		if c := s.pending[id]; !c.Returned && !c.Replayed {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 && (len(extra) > 0 || update != "") {
		return fail(409, "tool_followup_not_deliverable", "工具结果已交付，请将新增消息单独追加到原对话；本次新增内容未被标记为已处理")
	}
	// Commit revocations and schema updates before waking any MCP waiter.
	commitToolCatalog(s, nextTools, loaded, toolHash)
	if req != nil && req.ToolsProvided {
		s.deferredTools = deferredCatalog(req.Tools)
	}
	s.policy = policy
	if req != nil {
		commitRemoteConnections(s, req)
	}
	if len(ids) > 0 {
		s.pending[ids[len(ids)-1]].Extra = extra
		if update != "" {
			s.pending[ids[len(ids)-1]].Update = update
		}
	}
	for id, v := range results {
		c := s.pending[id]
		select {
		case <-receivedReady(c):
		default:
			if c.remote != nil {
				c.remote.Approved = v.(bool)
				c.remote.ApprovalHash = digest(v)
				c.remote.ApprovalItem = maps.Clone(wireResults[id])
				c.remote.client = s.remoteConnections[c.remote.Binding.Server]
				c.receipt("approval_received")
				close(c.remote.approvalReady)
				continue
			}
			c.ResultItem = maps.Clone(wireResults[id])
			c.Output = v
			c.OutputHash = digest(v)
			c.receipt("result_received")
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
}

type response struct {
	batchID                string
	trace                  *requestTrace
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
	return &response{batchID: newID("batch_"), w: w, stream: stream, id: newID("resp_agy_"), model: model, created: time.Now().Unix(), textIndex: -1, reasonIndex: -1}
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
	if c.remote != nil {
		return o.remoteEvent(remoteMCPEvent{Item: approvalRequestItem(c), Call: c})
	}
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
	o.trace.phase("response_delivery")
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
	o.trace.phase("response_failed")
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
