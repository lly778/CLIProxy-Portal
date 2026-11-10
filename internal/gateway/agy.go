package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"cliproxy-portal/internal/agy"
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/store"
)

// routeAGY returns false only when the original request remains replayable to
// CPA. It is opt-in at the deployment level; the normal gateway is unchanged.
func (g *Gateway) routeAGY(w http.ResponseWriter, r *http.Request, m *requestMeasurement, capture *requestCapture) bool {
	if g.AGY == nil || r.Method != "POST" || !(isDialoguePath(r.Method, r.URL.Path) || r.URL.Path == "/v1/responses/compact") {
		return false
	}
	spools := g.identitySpools()
	wire, e := spools.create()
	if e != nil {
		agy.WriteError(w, e)
		return true
	}
	size, e := identityCopy(r.Context(), wire, r.Body)
	if e != nil {
		spools.close()
		if m != nil {
			m.failure("upload_interrupted", 502)
		}
		agy.WriteError(w, &agy.Error{Status: 502, Code: "upload_interrupted", Message: "上传请求未完成"})
		return true
	}
	_ = r.Body.Close()
	// Closing happens at the end of the enclosing handler through r.Body.
	body := &spooledBody{Reader: io.NewSectionReader(wire, 0, size), cleanup: spools.close, wire: wire, size: size, spools: spools}
	r.Body = body
	decoded, n, e := decodeIdentitySpool(r.Context(), spools, wire, size, r.Header.Get("Content-Encoding"))
	if e != nil {
		return false
	}
	body.decoded, body.decodedSize = decoded, n
	var header struct {
		Model string `json:"model"`
	}
	// Inspect only bounded metadata; a large CPA request must never be buffered
	// into memory merely to decide which channel owns it.
	scanner := &identityJSON{ctx: r.Context(), file: decoded, r: bufio.NewReader(identityContextReader{r.Context(), io.NewSectionReader(decoded, 0, n)})}
	seen := false
	e = scanner.object(func(key string) error {
		if key != "model" {
			return scanner.skip()
		}
		if seen {
			return errIdentityJSON
		}
		seen = true
		var err error
		header.Model, err = scanner.metadata(2048)
		return err
	})
	if e != nil || !strings.HasPrefix(header.Model, agy.Prefix) {
		return false
	}
	defer r.Body.Close()
	var native *store.AGYUsage
	if m != nil {
		native = &store.AGYUsage{Requested: header.Model, Model: header.Model}
		m.mu.Lock()
		m.row.AGY = native
		m.row.CPARequestID = "agy:" + m.row.ID
		m.mu.Unlock()
		if capture != nil {
			capture.cpaRequestID = "agy:" + m.row.ID
		}
	}
	failure := func(err error) {
		var v *agy.Error
		status := 502
		reason := "agy_error"
		if errors.As(err, &v) {
			status = v.Status
			reason = v.Code
		}
		if m != nil {
			m.failure(reason, status)
		}
		if capture != nil {
			capture.status = status
		}
		agy.WriteError(w, err)
	}
	if r.URL.Path != "/v1/responses" && r.URL.Path != "/v1/responses/compact" {
		failure(&agy.Error{Status: 400, Code: "unsupported_endpoint", Message: "AGY 当前仅支持 Responses 接口"})
		return true
	}
	key, e := g.store.KeyByHash(r.Context(), cpamp.HashAPIKey(apiKey(r)))
	if e != nil || key.Status != "active" {
		failure(&agy.Error{Status: 401, Code: "invalid_api_key", Message: "API Key 无效或已撤销"})
		return true
	}
	user, e := g.store.UserByID(r.Context(), key.UserID)
	if e != nil || user.Status != domain.StatusApproved {
		failure(&agy.Error{Status: 403, Code: "account_disabled", Message: "当前门户账号不可调用"})
		return true
	}
	settings, e := g.store.AGYSettings(r.Context())
	if e != nil {
		failure(e)
		return true
	}
	rule, ok := settings.Resolve(header.Model)
	if !ok {
		failure(&agy.Error{Status: 403, Code: "model_disabled", Message: "该 AGY 模型尚未开放给当前账号"})
		return true
	}
	if native != nil {
		native.Model = agy.Prefix + rule.ID
	}
	if size > agy.MaxBody || n > agy.MaxBody {
		failure(&agy.Error{Status: 413, Code: "request_too_large", Message: "AGY 请求超过 10 MiB 限制"})
		return true
	}
	var payload map[string]json.RawMessage
	data, e := io.ReadAll(io.NewSectionReader(decoded, 0, n))
	if e != nil || json.Unmarshal(data, &payload) != nil {
		failure(&agy.Error{Status: 400, Code: "invalid_request", Message: "AGY 请求格式无效"})
		return true
	}
	payload["model"], _ = json.Marshal(agy.Prefix + rule.ID)
	if rule.Effort != "" {
		var v map[string]any
		if len(payload["reasoning"]) > 0 {
			_ = json.Unmarshal(payload["reasoning"], &v)
		}
		if v == nil {
			v = map[string]any{}
		}
		if v["effort"] == nil || v["effort"] == "" {
			v["effort"] = rule.Effort
		}
		payload["reasoning"], _ = json.Marshal(v)
	}
	data, _ = json.Marshal(payload)
	var reasoning struct {
		Effort string `json:"effort"`
	}
	_ = json.Unmarshal(payload["reasoning"], &reasoning)
	if rule.Cap != "" {
		levels := []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}
		rank := func(v string) int {
			for i, l := range levels {
				if v == l {
					return i
				}
			}
			return -1
		}
		if reasoning.Effort == "" {
			reasoning.Effort = rule.Cap
		} else if rank(reasoning.Effort) < 0 || rank(rule.Cap) < 0 {
			failure(&agy.Error{Status: 400, Code: "unsupported_effort", Message: "无法应用该思考强度上限"})
			return true
		} else if rank(reasoning.Effort) > rank(rule.Cap) {
			reasoning.Effort = rule.Cap
		}
		payload["reasoning"], _ = json.Marshal(reasoning)
		data, _ = json.Marshal(payload)
	}
	if native != nil {
		native.Effort = reasoning.Effort
	}
	workerPath := "/internal/responses"
	if r.URL.Path == "/v1/responses/compact" {
		workerPath = "/internal/compact"
	}
	ctx := agy.WithModelProgress(r.Context(), func() {
		if watch := idleWatch(r); watch != nil {
			watch.progress()
		}
	})
	resp, e := g.AGY.Request(ctx, "POST", workerPath, key.UserID+":"+key.Hash, bytes.NewReader(data))
	if e != nil {
		failure(&agy.Error{Status: 503, Code: "worker_unavailable", Message: "AGY 内部服务暂不可用"})
		return true
	}
	defer resp.Body.Close()
	if m != nil {
		m.upstreamResponse(resp)
		m.mu.Lock()
		m.row.CPARequestID = "agy:" + m.row.ID
		m.mu.Unlock()
		if resp.StatusCode >= 400 {
			m.failure("agy_request_failed", resp.StatusCode)
		}
	}
	if capture != nil {
		capture.status = resp.StatusCode
		capture.responseContentType = resp.Header.Get("Content-Type")
		if resp.Header.Get("Content-Encoding") != "" {
			capture.responseContentEncoding = resp.Header.Get("Content-Encoding")
		}
	}
	for _, name := range []string{"Content-Type", "Cache-Control"} {
		if v := resp.Header.Get(name); v != "" {
			w.Header().Set(name, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	tracker := &agy.Tracker{Stream: strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")}
	defer func() {
		tracker.Finish()
		if native != nil {
			native.Usage = tracker.Usage
			native.ResponseID = tracker.ResponseID
			if tracker.Effort != "" {
				native.Effort = tracker.Effort
			}
		}
		if resp.StatusCode < 400 && !tracker.Completed && m != nil {
			reason := tracker.Failure
			if reason == "" {
				reason = "agy_stream_incomplete"
			}
			m.failure(reason, 502)
		}
	}()
	buffer := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buffer)
		if n > 0 {
			tracker.Feed(buffer[:n])
			if watch := idleWatch(r); watch != nil {
				watch.progress()
			}
			if capture != nil {
				_, _ = capture.response.Write(buffer[:n])
			}
			if _, e = w.Write(buffer[:n]); e != nil {
				if m != nil {
					m.failure("client_write_failed", resp.StatusCode)
				}
				return true
			}
			if http.NewResponseController(w).Flush() != nil {
				if m != nil {
					m.failure("client_write_failed", resp.StatusCode)
				}
				return true
			}
		}
		if err != nil {
			if err != io.EOF && m != nil {
				m.failure("agy_stream_interrupted", resp.StatusCode)
			}
			break
		}
	}
	_ = g.store.SetKeyLastSeen(r.Context(), key.Hash, time.Now())
	return true
}

// Images use a short-lived, per-file signed capability. They never enter the
// model gateway or require exposing a user's API key to an image renderer.
func (g *Gateway) serveAGYImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "HEAD" {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(405)
		return
	}
	path := "/internal/images/" + strings.TrimPrefix(r.URL.Path, "/v1/agy/images/")
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	resp, err := g.AGY.Request(r.Context(), r.Method, path, "", nil)
	if err != nil {
		http.Error(w, "Image unavailable", 502)
		return
	}
	defer resp.Body.Close()
	for _, name := range []string{"Content-Type", "Content-Length", "Cache-Control", "X-Content-Type-Options", "Access-Control-Allow-Origin", "Last-Modified"} {
		if v := resp.Header.Get(name); v != "" {
			w.Header().Set(name, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method != "HEAD" {
		_, _ = io.Copy(w, io.LimitReader(resp.Body, 4<<20+1))
	}
}

type spooledBody struct {
	io.Reader
	cleanup           func()
	wire, decoded     *os.File
	size, decodedSize int64
	spools            *identitySpools
}

func (b *spooledBody) Close() error {
	if b.cleanup != nil {
		b.cleanup()
		b.cleanup = nil
	}
	return nil
}
