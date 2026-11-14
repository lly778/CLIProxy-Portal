package agy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const responseReplayTTL = 10 * time.Minute
const responseReplayGrace = 30 * time.Second
const responseReplayLimit = 32 << 20

type cachedResponse struct {
	RequestID string      `json:"request_id,omitempty"`
	Status    int         `json:"status"`
	Headers   http.Header `json:"headers"`
	Body      []byte      `json:"body"`
}

type responseReplay struct {
	originRequest string
	mu            sync.Mutex
	changed       chan struct{}
	headers       http.Header
	status        int
	body          bytes.Buffer
	progress      int
	done          bool
	stored        bool
	expires       time.Time
	readers       int
	orphan        *time.Timer
	cancel        context.CancelFunc
	generation    uint64
}

// A tracing ID alone is not a cross-user or cross-body idempotency key. Bind it
// to the authenticated owner and complete canonical payload. Without a stable
// identity/response chain, run normally instead of matching unrelated text.
func responseReplayKey(owner string, h http.Header, body []byte) string {
	if owner == "" || len(owner) > 256 {
		return ""
	}
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if decoder.Decode(&payload) != nil {
		return ""
	}
	if decoder.Decode(new(any)) != io.EOF {
		return ""
	}
	identity := map[string]string{}
	for _, name := range []string{"Idempotency-Key", "Session_id", "Conversation_id"} {
		if v := strings.TrimSpace(h.Get(name)); v != "" && len(v) <= 256 {
			identity[name] = v
		}
	}
	var turn map[string]any
	if v := h.Get("X-Codex-Turn-Metadata"); len(v) <= 4096 && json.Unmarshal([]byte(v), &turn) == nil {
		for _, name := range []string{"turn_id", "session_id", "thread_id"} {
			if v, ok := turn[name].(string); ok && v != "" && len(v) <= 256 {
				identity[name] = v
			}
		}
	}
	if previous, _ := payload["previous_response_id"].(string); previous != "" && len(previous) <= 256 {
		identity["previous_response_id"] = previous
	}
	// Tool result IDs also identify the exact continuation when the client has
	// no request header. They remain scoped to this owner and the whole body.
	if len(identity) == 0 {
		if req, err := parseRequest(body); err == nil {
			for _, item := range req.Items {
				if isToolResultItem(stringField(item, "type")) && resultCallID(item) != "" {
					identity["tool:"+resultCallID(item)] = "result"
				}
			}
		}
	}
	if len(identity) == 0 {
		if v := strings.TrimSpace(h.Get("X-Client-Request-Id")); v != "" && len(v) <= 256 {
			identity["X-Client-Request-Id"] = v
		}
	}
	if len(identity) == 0 {
		return ""
	}
	return wireDigest([]any{owner, identity, payload})
}

func (e *responseReplay) wakeLocked() {
	close(e.changed)
	e.changed = make(chan struct{})
}

type replayWriter struct {
	e      *responseReplay
	header http.Header
}

func (w *replayWriter) Header() http.Header { return w.header }
func (w *replayWriter) WriteHeader(status int) {
	w.e.mu.Lock()
	defer w.e.mu.Unlock()
	if status < 200 {
		if status == http.StatusProcessing {
			w.e.progress++
			w.e.wakeLocked()
		}
		return
	}
	if w.e.status == 0 {
		w.e.status = status
		w.e.headers = w.header.Clone()
		w.e.wakeLocked()
	}
}
func (w *replayWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	w.e.mu.Lock()
	defer w.e.mu.Unlock()
	if len(p) > responseReplayLimit-w.e.body.Len() {
		return 0, fail(503, "response_buffer_full", "AGY 响应暂存空间不足")
	}
	n, err := w.e.body.Write(p)
	w.e.wakeLocked()
	return n, err
}
func (w *replayWriter) Flush() {}

func (m *Manager) replayResponse(w http.ResponseWriter, r *http.Request, execute http.HandlerFunc) {
	clientCtx := r.Context()
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if err != nil || len(body) > MaxBody {
		WriteError(w, fail(413, "request_too_large", "AGY 请求超过大小限制"))
		return
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	key := responseReplayKey(r.Header.Get("X-Portal-Owner"), r.Header, body)
	if key == "" {
		execute(w, r)
		return
	}
	key = digest([]string{r.URL.Path, key})
	if m.disk != nil {
		key = m.disk.hash("response-replay:" + key)
	}
	m.replayMu.Lock()
	if m.replays == nil {
		m.replays = map[string]*responseReplay{}
	}
	for k, e := range m.replays {
		e.mu.Lock()
		expired := e.done && time.Now().After(e.expires) && e.readers == 0
		e.mu.Unlock()
		if expired {
			delete(m.replays, k)
		}
	}
	e := m.replays[key]
	if e == nil {
		e = m.loadResponseReplay(key)
		if e != nil {
			m.replays[key] = e
		}
	}
	fresh := e == nil
	if fresh {
		// Cache pressure does not reject a new request or introduce a tombstone.
		if len(m.replays) >= 64 {
			m.replayMu.Unlock()
			execute(w, r)
			return
		}
		life := m.life
		if life == nil {
			life = r.Context()
		}
		ctx, cancel := context.WithCancel(life)
		origin := r.Header.Get("X-Portal-Request-ID")
		if origin == "" || len(origin) > 128 {
			origin = newID("agy_request_")
			r.Header.Set("X-Portal-Request-ID", origin)
		}
		e = &responseReplay{originRequest: origin, changed: make(chan struct{}), cancel: cancel, generation: m.replayGeneration}
		m.replays[key] = e
		r = r.Clone(ctx)
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	e.mu.Lock()
	if e.orphan != nil {
		e.orphan.Stop()
		e.orphan = nil
	}
	e.readers++
	mode := "running"
	if fresh {
		mode = "new"
	} else if e.done {
		mode = "completed"
	}
	e.mu.Unlock()
	m.replayMu.Unlock()
	if !fresh {
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &payload)
		_, trace := m.traceRequest(r, r.Header.Get("X-Portal-Owner"), payload.Model)
		trace.emit(Observation{Kind: "http_replay", State: mode, Category: "http_replay", OriginRequestID: e.originRequest})
		trace.phase("replaying_response")
		defer trace.phase("request_finished")
	}
	w.Header().Set("X-Portal-AGY-Replay", mode)
	if fresh {
		go func() {
			rw := &replayWriter{e: e, header: make(http.Header)}
			defer func() {
				// Execution outlives the HTTP handler, so its panic must not
				// escape the server's normal request recovery boundary.
				panicked := recover() != nil
				e.mu.Lock()
				noResponse := e.status == 0
				e.mu.Unlock()
				if noResponse || panicked {
					WriteError(rw, fail(503, "worker_lost", "AGY 请求未完成"))
				}
				m.finishResponseReplay(key, e)
			}()
			execute(rw, r)
		}()
	}
	defer m.releaseResponseReplay(key, e)
	serveResponseReplay(w, clientCtx, e)
}

func serveResponseReplay(w http.ResponseWriter, ctx context.Context, e *responseReplay) {
	position, progress := 0, 0
	started := false
	for {
		e.mu.Lock()
		status, headers, done, changed := e.status, e.headers, e.done, e.changed
		advance := e.progress != progress
		progress = e.progress
		var chunk []byte
		if position < e.body.Len() {
			end := min(position+32<<10, e.body.Len())
			chunk = append([]byte(nil), e.body.Bytes()[position:end]...)
			position = end
		}
		e.mu.Unlock()
		if advance && !started && status == 0 {
			w.WriteHeader(http.StatusProcessing)
		}
		if !started && status != 0 {
			for name, values := range headers {
				w.Header()[name] = append([]string(nil), values...)
			}
			w.WriteHeader(status)
			started = true
		}
		if len(chunk) > 0 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			_ = http.NewResponseController(w).Flush()
			continue
		}
		if done {
			return
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return
		}
	}
}

func (m *Manager) finishResponseReplay(key string, e *responseReplay) {
	e.mu.Lock()
	result := cachedResponse{RequestID: e.originRequest, Status: e.status, Headers: e.headers, Body: e.body.Bytes()}
	e.expires = time.Now().Add(responseReplayTTL)
	tracker := &Tracker{Stream: strings.Contains(e.headers.Get("Content-Type"), "text/event-stream")}
	tracker.Feed(result.Body)
	tracker.Finish()
	var object struct {
		Object string `json:"object"`
	}
	compacted := !tracker.Stream && json.Unmarshal(result.Body, &object) == nil && object.Object == "response.compaction"
	complete := result.Status >= 200 && result.Status < 300 && (tracker.Completed || compacted)
	e.mu.Unlock()
	m.replayMu.Lock()
	current := e.generation == m.replayGeneration
	stored := current && complete && m.saveResponseReplay(key, e.expires, result)
	e.mu.Lock()
	e.done, e.stored = true, stored
	if e.orphan != nil {
		e.orphan.Stop()
		e.orphan = nil
	}
	e.wakeLocked()
	if (!complete || stored && e.readers == 0) && m.replays[key] == e {
		delete(m.replays, key)
	}
	e.mu.Unlock()
	m.replayMu.Unlock()
	if e.cancel != nil {
		e.cancel()
	}
}

func (m *Manager) releaseResponseReplay(key string, e *responseReplay) {
	m.replayMu.Lock()
	defer m.replayMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.readers--
	if e.readers == 0 {
		if !e.done {
			e.orphan = time.AfterFunc(responseReplayGrace, func() {
				e.mu.Lock()
				defer e.mu.Unlock()
				if e.readers == 0 && !e.done && e.cancel != nil {
					e.cancel()
				}
			})
		} else if e.stored && m.replays[key] == e {
			delete(m.replays, key)
		}
	}
}

func (m *Manager) loadResponseReplay(key string) *responseReplay {
	if m.disk == nil {
		return nil
	}
	var expires int64
	var sealed []byte
	if m.disk.db.QueryRow(`SELECT expires_ms,payload FROM response_replays WHERE key=? AND expires_ms>?`, key, time.Now().UnixMilli()).Scan(&expires, &sealed) != nil || len(sealed) > responseReplayLimit*2 {
		return nil
	}
	plain, err := m.disk.open("response-replay:"+key, sealed)
	var result cachedResponse
	if err != nil || json.Unmarshal(plain, &result) != nil || len(result.Body) > responseReplayLimit {
		return nil
	}
	e := &responseReplay{originRequest: result.RequestID, changed: make(chan struct{}), status: result.Status, headers: result.Headers, done: true, stored: true, expires: time.UnixMilli(expires), generation: m.replayGeneration}
	e.body.Write(result.Body)
	return e
}

func (m *Manager) saveResponseReplay(key string, expires time.Time, result cachedResponse) bool {
	if m.disk == nil {
		return false
	}
	plain, err := json.Marshal(result)
	if err != nil {
		return false
	}
	sealed, err := m.disk.seal("response-replay:"+key, plain)
	if err != nil {
		return false
	}
	tx, err := m.disk.db.Begin()
	if err != nil {
		return false
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT OR REPLACE INTO response_replays(key,expires_ms,payload) VALUES(?,?,?)`, key, expires.UnixMilli(), sealed); err != nil {
		return false
	}
	if _, err = tx.Exec(`DELETE FROM response_replays WHERE expires_ms<? OR key NOT IN (SELECT key FROM response_replays ORDER BY expires_ms DESC LIMIT 128)`, time.Now().UnixMilli()); err != nil {
		return false
	}
	if _, err = tx.Exec(`DELETE FROM response_replays WHERE key IN (SELECT key FROM (SELECT key,SUM(length(payload)) OVER (ORDER BY expires_ms DESC,key DESC) AS used FROM response_replays) WHERE used>?)`, 64<<20); err != nil {
		return false
	}
	return tx.Commit() == nil
}

func (m *Manager) clearResponseReplays() error {
	return m.stopResponseReplays(true)
}

func (m *Manager) stopResponseReplays(purge bool) error {
	m.replayMu.Lock()
	defer m.replayMu.Unlock()
	m.replayGeneration++
	for _, e := range m.replays {
		if e.cancel != nil {
			e.cancel()
		}
	}
	m.replays = nil
	if purge && m.disk != nil {
		_, err := m.disk.db.Exec(`DELETE FROM response_replays`)
		return err
	}
	return nil
}
