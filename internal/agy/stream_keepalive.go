package agy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const streamKeepaliveInterval = 15 * time.Second

// Keep connection liveness separate from model progress and replay storage.
// Only a committed SSE response enables comments; ordinary JSON and HTTP
// validation errors retain their original status and body. One mutex serializes
// comments, application writes and flushes, including replay subscribers.
type streamKeepaliveWriter struct {
	http.ResponseWriter
	mu       sync.Mutex
	interval time.Duration
	last     time.Time
	err      error
	closed   bool
	active   bool
	done     chan struct{}
	exited   chan struct{}
	cancel   context.CancelFunc
}

func withStreamKeepalive(w http.ResponseWriter, r *http.Request, interval time.Duration, serve http.HandlerFunc) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	k := &streamKeepaliveWriter{ResponseWriter: w, interval: interval, done: make(chan struct{}), cancel: cancel}
	defer k.close()
	serve(k, r.WithContext(ctx))
}

func (w *streamKeepaliveWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *streamKeepaliveWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ResponseWriter.WriteHeader(status)
	if status >= 200 && status < 300 && !w.active && !w.closed &&
		strings.HasPrefix(strings.ToLower(w.Header().Get("Content-Type")), "text/event-stream") {
		w.active, w.last, w.exited = true, time.Now(), make(chan struct{})
		go w.run()
	}
}

func (w *streamKeepaliveWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.ResponseWriter.Write(p)
	if n > 0 {
		w.last = time.Now()
	}
	w.failLocked(err)
	return n, err
}

func (w *streamKeepaliveWriter) FlushError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	err := http.NewResponseController(w.ResponseWriter).Flush()
	w.failLocked(err)
	return err
}

func (w *streamKeepaliveWriter) failLocked(err error) {
	if err != nil && w.err == nil {
		w.err = err
		w.cancel()
	}
}

func (w *streamKeepaliveWriter) run() {
	defer close(w.exited)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-ticker.C:
			w.mu.Lock()
			if w.closed || w.err != nil {
				w.mu.Unlock()
				return
			}
			if time.Since(w.last) >= w.interval {
				_, err := io.WriteString(w.ResponseWriter, ": keepalive\n\n")
				if err == nil {
					err = http.NewResponseController(w.ResponseWriter).Flush()
				}
				w.last = time.Now()
				w.failLocked(err)
			}
			w.mu.Unlock()
		}
	}
}

func (w *streamKeepaliveWriter) close() {
	w.mu.Lock()
	w.closed = true
	close(w.done)
	exited := w.exited
	w.mu.Unlock()
	if exited != nil {
		<-exited
	}
}
