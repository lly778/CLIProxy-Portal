package agy

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Observation contains correlation metadata only. Never put prompts, tool
// arguments, results or credentials here. Receipt states describe transport,
// not proof that a document was saved or an external action succeeded.
type Observation struct {
	ID, Owner, RequestID, SessionID, Model       string
	Kind, State, CallID, Category, Tool, Outcome string
	ArgumentsHash, ResultHash, DefinitionHash    string
	ResponseID, OriginRequestID                  string
	Reason                                       string
	Route                                        string
	RuntimeSHA                                   string
	ProjectionVersion                            int
	BatchID                                      string
	CallIDs                                      []string
	ModelCalls                                   int
	At                                           int64
	Usage                                        Usage
	Compaction                                   *compactionProof `json:",omitempty"`
}

type observationRequestKey struct{}
type traceKey struct{}

func WithObservationRequest(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, observationRequestKey{}, id)
}

type requestTrace struct {
	mu                                    sync.Mutex
	m                                     *Manager
	owner, request, model, session, stage string
	runtimeSHA                            string
}

func traceFrom(ctx context.Context) *requestTrace {
	t, _ := ctx.Value(traceKey{}).(*requestTrace)
	return t
}

func (m *Manager) traceRequest(r *http.Request, owner, model string) (*http.Request, *requestTrace) {
	id := r.Header.Get("X-Portal-Request-ID")
	if id == "" || len(id) > 128 {
		id = newID("agy_request_")
	}
	t := &requestTrace{m: m, owner: owner, request: id, model: model}
	t.phase("accepted")
	return r.WithContext(context.WithValue(r.Context(), traceKey{}, t)), t
}

func (t *requestTrace) bind(id string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.session = id
	t.mu.Unlock()
}

func (t *requestTrace) emit(v Observation) {
	if t == nil || t.m.cfg.Observe == nil {
		return
	}
	t.mu.Lock()
	v.Owner, v.RequestID, v.Model = t.owner, t.request, t.model
	v.RuntimeSHA = t.runtimeSHA
	if v.SessionID == "" {
		v.SessionID = t.session
	}
	t.mu.Unlock()
	v.At = time.Now().UnixMilli()
	if v.ID == "" {
		v.ID = newID("obs_")
	}
	if err := t.m.cfg.Observe(v); err != nil {
		// An audit write must not turn a completed client action into a retry.
		slog.Warn("AGY observation persistence failed", "kind", v.Kind)
	}
}

func (t *requestTrace) phase(state string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.stage == state {
		t.mu.Unlock()
		return
	}
	t.stage = state
	t.mu.Unlock()
	t.emit(Observation{Kind: "phase", State: state})
}

func (c *pendingTool) receipt(state string) {
	if c.trace == nil || c.trace.m.cfg.Observe == nil {
		return
	}
	category := "client_tool"
	kind := "tool"
	if c.remote != nil {
		category, kind = "remote_mcp", "remote_tool"
	}
	if isClientSearch(c.Tool) {
		category = "tool_search"
	}
	c.trace.emit(Observation{
		ID:        digest([]string{c.trace.owner, c.traceSession, c.ID, state}),
		SessionID: c.traceSession, Kind: kind, State: state, BatchID: c.BatchID,
		CallID: c.ID, Category: category, Tool: c.Tool.key(),
		ArgumentsHash: digest(c.Arguments), ResultHash: c.OutputHash, DefinitionHash: digest(c.Tool),
		Outcome: c.outcome(), Route: c.Route, ProjectionVersion: c.ProjectionVersion,
	})
}

// Only explicit structured results establish an outcome. Human-readable text
// and a generated function_call.status=completed establish no execution result.
func receiptOutcome(v any) string {
	if text, ok := v.(string); ok {
		var structured map[string]any
		if unmarshalExact([]byte(text), &structured) != nil {
			return "unknown"
		}
		v = structured
	}
	m, ok := v.(map[string]any)
	if !ok {
		return "unknown"
	}
	if toolResultFailed(m) {
		return "failed"
	}
	if m["isError"] == false || m["is_error"] == false {
		return "success"
	}
	return "unknown"
}

func (c *pendingTool) outcome() string {
	if c.remote != nil {
		return c.remote.Outcome
	}
	if state := receiptOutcome(c.Output); state != "unknown" {
		return state
	}
	m, _ := c.Output.(map[string]any)
	if c.Tool.Type == "apply_patch" && m["status"] == "completed" {
		return "success"
	}
	if c.Tool.Type == "shell" {
		entries, _ := m["output"].([]any)
		if len(entries) == 0 {
			return "unknown"
		}
		for _, raw := range entries {
			entry, _ := raw.(map[string]any)
			outcome, _ := entry["outcome"].(map[string]any)
			if outcome["type"] == "timeout" {
				return "failed"
			}
			exit, ok := numberInt(outcome["exit_code"], -2147483647, 2147483647)
			if !ok || outcome["type"] != "exit" {
				return "unknown"
			}
			if exit != 0 {
				return "failed"
			}
		}
		return "success"
	}
	return "unknown"
}

func (m *Manager) consumeObservedUsage(s *session, trace *requestTrace) Usage {
	u, calls := m.consumeUsageCalls(s)
	if calls > 0 && trace != nil {
		s.mu.Lock()
		cursor := s.usageCursor
		s.mu.Unlock()
		trace.emit(Observation{ID: digest([]string{trace.owner, s.ID, "model_usage", strconv.FormatInt(cursor, 10)}), SessionID: s.ID, Kind: "model_usage", State: "observed", Usage: u, ModelCalls: calls})
	}
	return u
}

func rebuildObservationReason(s *session, req request) string {
	system, _ := systemText(req)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.interrupted:
		return "interrupted_turn"
	case s.model != req.Model:
		return "model_changed"
	case s.system != digest(system):
		return "instructions_changed"
	case s.persisted && s.officialVersion < officialToolsVersion:
		return "official_tools_changed"
	default:
		return "history_changed"
	}
}
