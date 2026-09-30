package gateway

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"sync"
	"time"
)

type connectionTimingKey struct{}

// TimingListener instruments accepted connections without parsing or buffering
// their contents. Only the HTTP/1 gateway listener uses it.
func TimingListener(listener net.Listener) net.Listener {
	return &timingListener{Listener: listener}
}

type timingListener struct{ net.Listener }

func (l *timingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &timingConn{Conn: c, acceptedAt: time.Now()}, nil
}

type timingConn struct {
	net.Conn
	mu           sync.Mutex
	acceptedAt   time.Time
	idle         bool
	readAt       time.Time
	start        time.Time
	startSource  string
	firstWriteAt time.Time
	writeFailed  bool
	finish       func(time.Time, time.Time, bool)
}

func (c *timingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.mu.Lock()
		if c.idle && c.readAt.IsZero() {
			// Exclude keep-alive idle time. A blocked Read's start would include
			// all of that idle time, so use receipt of the first available bytes.
			c.readAt = time.Now()
		}
		c.mu.Unlock()
	}
	return n, err
}

func (c *timingConn) Write(p []byte) (int, error) {
	at := time.Now()
	n, err := c.Conn.Write(p)
	c.mu.Lock()
	// A 100 Continue/103 Early Hints response is not the model's reply.
	informational := len(p) >= 12 && bytes.HasPrefix(p, []byte("HTTP/1.")) && p[9] == '1'
	if n > 0 && !informational && c.firstWriteAt.IsZero() {
		c.firstWriteAt = at
	}
	if err != nil {
		c.writeFailed = true
	}
	c.mu.Unlock()
	return n, err
}

// ConfigureServerTiming must be paired with TimingListener. ConnState fires
// Idle/Closed after net/http finishes flushing the response, unlike a handler
// defer, which runs before the final buffered response/trailer write.
func ConfigureServerTiming(server *http.Server) {
	server.ConnContext = func(ctx context.Context, conn net.Conn) context.Context {
		if c, ok := conn.(*timingConn); ok {
			return context.WithValue(ctx, connectionTimingKey{}, c)
		}
		return ctx
	}
	server.ConnState = func(conn net.Conn, state http.ConnState) {
		c, ok := conn.(*timingConn)
		if !ok {
			return
		}
		c.mu.Lock()
		switch state {
		case http.StateActive:
			c.start, c.startSource = c.acceptedAt, "connection_accepted"
			if c.start.IsZero() {
				c.start, c.startSource = c.readAt, "request_received"
			}
			if c.start.IsZero() {
				// HTTP/1 pipelining can already have the next headers buffered.
				// Do not assign the previous request or keep-alive idle to it.
				c.start, c.startSource = time.Now(), "headers_received"
			}
			c.acceptedAt, c.readAt = time.Time{}, time.Time{}
			c.idle, c.writeFailed, c.firstWriteAt = false, false, time.Time{}
		case http.StateIdle, http.StateClosed, http.StateHijacked:
			finish, firstWrite, failed := c.finish, c.firstWriteAt, c.writeFailed
			c.finish = nil
			c.idle = state == http.StateIdle
			c.readAt = time.Time{}
			c.mu.Unlock()
			if finish != nil {
				finish(time.Now(), firstWrite, failed || state == http.StateHijacked)
			}
			return
		}
		c.mu.Unlock()
	}
}

func requestStart(r *http.Request) (time.Time, string, *timingConn) {
	if c, ok := r.Context().Value(connectionTimingKey{}).(*timingConn); ok {
		c.mu.Lock()
		start, source := c.start, c.startSource
		c.mu.Unlock()
		if !start.IsZero() {
			return start, source, c
		}
	}
	return time.Now(), "handler_started", nil
}
