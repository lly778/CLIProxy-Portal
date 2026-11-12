package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"
)

const gatewayIdleTimeout = 60 * time.Second

var errGatewayIdleTimeout = errors.New("gateway idle timeout")

type requestIdleKey struct{}

// Bound stalled request transfer and client writes, not model computation.
// Once the complete request has been written upstream, silence between model
// chunks is allowed. The watchdog is armed again only during downstream writes.
type requestIdleWatch struct {
	mu          sync.Mutex
	timeout     time.Duration
	last        time.Time
	timer       *time.Timer
	stopped     bool
	timedOut    bool
	committed   bool
	submitted   bool
	writers     int
	cancel      context.CancelCauseFunc
	controller  *http.ResponseController
	connection  *timingConn
	measurement *requestMeasurement
	onTimeout   func()
}

func idleWatch(r *http.Request) *requestIdleWatch {
	watch, _ := r.Context().Value(requestIdleKey{}).(*requestIdleWatch)
	return watch
}

func (w *requestIdleWatch) progress() {
	w.mu.Lock()
	if !w.stopped && !w.timedOut {
		w.last = time.Now()
	}
	w.mu.Unlock()
}

func (w *requestIdleWatch) upstreamSubmitted() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped || w.timedOut {
		return
	}
	w.submitted = true
	if w.writers == 0 {
		w.timer.Stop()
	}
}

func (w *requestIdleWatch) beginWrite() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.submitted && w.writers == 0 && !w.stopped && !w.timedOut {
		w.last = time.Now()
		w.timer.Reset(w.timeout)
	}
	w.writers++
}

func (w *requestIdleWatch) endWrite() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writers--
	if w.submitted && w.writers == 0 {
		w.timer.Stop()
	}
}

func upstreamSubmissionContext(ctx context.Context, watch *requestIdleWatch) context.Context {
	if watch == nil {
		return ctx
	}
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				watch.upstreamSubmitted()
			}
		},
	})
}

func (w *requestIdleWatch) expired() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.timedOut
}

func (w *requestIdleWatch) responseStarted() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.committed
}

func (w *requestIdleWatch) attachMeasurement(m *requestMeasurement) {
	w.mu.Lock()
	w.measurement = m
	if w.timedOut && m != nil {
		m.failure("idle_timeout", http.StatusGatewayTimeout)
	}
	w.mu.Unlock()
}

func (w *requestIdleWatch) check() {
	w.mu.Lock()
	if w.stopped || w.timedOut || w.submitted && w.writers == 0 {
		w.mu.Unlock()
		return
	}
	if remaining := w.timeout - time.Since(w.last); remaining > 0 {
		w.timer.Reset(remaining)
		w.mu.Unlock()
		return
	}
	w.timedOut = true
	if w.measurement != nil {
		w.measurement.failure("idle_timeout", http.StatusGatewayTimeout)
	}
	// Unblock an upload or a blocked socket write. Before response headers are
	// committed, retain a short write opportunity for the handler's JSON 504.
	now := time.Now()
	writeDeadline := now
	if !w.committed {
		writeDeadline = now.Add(time.Second)
	}
	_ = w.controller.SetReadDeadline(now)
	_ = w.controller.SetWriteDeadline(writeDeadline)
	w.cancel(errGatewayIdleTimeout)
	w.mu.Unlock()
	if w.onTimeout != nil {
		w.onTimeout()
	}
}

func (w *requestIdleWatch) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	w.stopped = true
	w.timer.Stop()
	if !w.timedOut {
		// A subsequent keep-alive request must not inherit this request's state.
		_ = w.controller.SetReadDeadline(time.Time{})
		_ = w.controller.SetWriteDeadline(time.Time{})
	}
	w.cancel(nil)
}

func (g *Gateway) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if g.AGY != nil && strings.HasPrefix(r.URL.Path, "/v1/agy/images/") {
		g.serveAGYImage(w, r)
		return
	}
	if !isDialoguePath(r.Method, r.URL.Path) {
		g.serveResourceHTTP(w, r)
		return
	}
	ctx, cancel := context.WithCancelCause(r.Context())
	duration := g.idleTimeout
	if duration <= 0 {
		duration = gatewayIdleTimeout
	}
	watch := &requestIdleWatch{timeout: duration, last: time.Now(), cancel: cancel, controller: http.NewResponseController(w)}
	watch.onTimeout = func() {
		g.logger.Warn("gateway request idle timeout", "path", r.URL.Path, "idle_seconds", duration.Seconds())
	}
	ctx = context.WithValue(ctx, requestIdleKey{}, watch)
	r = r.WithContext(ctx)
	if connection, ok := ctx.Value(connectionTimingKey{}).(*timingConn); ok {
		watch.connection = connection
		connection.mu.Lock()
		connection.idleWatch = watch
		connection.mu.Unlock()
	}
	watch.mu.Lock()
	watch.timer = time.AfterFunc(duration, watch.check)
	watch.mu.Unlock()
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = &idleProgressBody{ReadCloser: r.Body, watch: watch}
	}
	defer func() {
		if watch.connection == nil {
			watch.stop()
		}
		// Instrumented servers stop on Idle/Closed, after the final flush.
	}()
	g.serveResourceHTTP(&idleResponseWriter{ResponseWriter: w, watch: watch}, r)
}

type idleProgressBody struct {
	io.ReadCloser
	watch *requestIdleWatch
}

func (b *idleProgressBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.watch.progress()
	}
	return n, err
}

type idleResponseWriter struct {
	http.ResponseWriter
	watch *requestIdleWatch
}

func (w *idleResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *idleResponseWriter) WriteHeader(code int) {
	w.watch.beginWrite()
	defer w.watch.endWrite()
	if code >= 200 {
		w.watch.mu.Lock()
		w.watch.committed = true
		w.watch.mu.Unlock()
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *idleResponseWriter) Write(p []byte) (int, error) {
	w.watch.beginWrite()
	defer w.watch.endWrite()
	w.watch.mu.Lock()
	w.watch.committed = true
	w.watch.mu.Unlock()
	n, err := w.ResponseWriter.Write(p)
	if n > 0 {
		w.watch.progress()
	}
	return n, err
}

func (w *idleResponseWriter) FlushError() error {
	w.watch.beginWrite()
	defer w.watch.endWrite()
	w.watch.mu.Lock()
	w.watch.committed = true
	w.watch.mu.Unlock()
	return http.NewResponseController(w.ResponseWriter).Flush()
}

type idleProgressTransport struct {
	base  http.RoundTripper
	watch *requestIdleWatch
}

func (t idleProgressTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.WithContext(upstreamSubmissionContext(r.Context(), t.watch))
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = &idleProgressBody{ReadCloser: r.Body, watch: t.watch}
	}
	resp, err := t.base.RoundTrip(r)
	if resp != nil {
		t.watch.progress()
		if resp.Body != nil {
			resp.Body = &idleProgressBody{ReadCloser: resp.Body, watch: t.watch}
		}
	}
	return resp, err
}

func writeIdleTimeout(w http.ResponseWriter, r *http.Request) {
	if watch := idleWatch(r); watch != nil && watch.responseStarted() {
		// A stream already has its HTTP status. Abort it without appending an
		// unrelated JSON document or a successful stream terminator.
		panic(http.ErrAbortHandler)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "close")
	w.WriteHeader(http.StatusGatewayTimeout)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
		"type": "gateway_timeout", "code": "idle_timeout", "message": "No I/O progress for 60 seconds; request terminated",
	}})
}
