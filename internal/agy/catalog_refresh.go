package agy

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

// Callers hold s.mu. A changed definition revokes the old dispatch contract
// immediately; this notification is only a request to fetch the new catalog.
func commitToolCatalog(s *session, tools, loaded []tool, hash string) {
	previous := s.toolHash
	s.tools, s.loadedTools, s.toolHash = tools, loaded, hash
	if previous == hash || !catalogRefreshEnabled(s) {
		return
	}
	s.catalogRevision++
	if s.catalogSignal != nil {
		close(s.catalogSignal)
	}
	s.catalogSignal = make(chan struct{})
}

func catalogRefreshEnabled(s *session) bool {
	return s.proc != nil && (s.proc.capabilitySupported("dynamic_tool_refresh") || s.catalogRefreshExperiment && s.proc.catalogRefreshCandidate())
}

// GET is the authenticated Streamable HTTP notification channel, separate
// from client tool calls. Disconnecting it never cancels a pending action.
func (m *Manager) catalogStream(w http.ResponseWriter, r *http.Request, s *session) {
	s.mu.Lock()
	if !catalogRefreshEnabled(s) || !s.handshake {
		s.mu.Unlock()
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.catalogStreams >= 4 {
		s.mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	s.catalogStreams++
	stream := make(chan struct{})
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.catalogStreams--
		if s.catalogOwner == stream {
			s.catalogOwner = nil
			if s.catalogSignal != nil {
				close(s.catalogSignal)
			}
			s.catalogSignal = make(chan struct{})
		}
		s.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Mcp-Session-Id", s.tenant)
	write := func(data string) bool {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(15 * time.Second))
		if _, err := io.WriteString(w, data); err != nil {
			return false
		}
		return http.NewResponseController(w).Flush() == nil
	}
	if !write(": catalog channel\n\n") {
		return
	}
	heartbeat := time.NewTicker(12 * time.Second)
	defer heartbeat.Stop()
	var sent uint64
	var lastSent time.Time
	for {
		s.mu.Lock()
		if s.catalogSignal == nil {
			s.catalogSignal = make(chan struct{})
		}
		if s.catalogOwner == nil {
			s.catalogOwner = stream
			sent = 0
		}
		primary := s.catalogOwner == stream
		signal, revision, trace := s.catalogSignal, s.catalogRevision, s.trace
		pending := s.catalogFetchedHash != s.toolHash
		s.mu.Unlock()
		if primary && (revision > sent || pending && revision > 0 && time.Since(lastSent) >= 24*time.Second) {
			sent = revision
			if pending {
				if !write("event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/tools/list_changed\",\"params\":{}}\n\n") {
					return
				}
				trace.emit(Observation{Kind: "bridge", Category: "catalog_refresh", State: "notified", Reason: fmt.Sprintf("revision_%d", revision)})
				lastSent = time.Now()
			}
		}
		select {
		case <-signal:
		case <-heartbeat.C:
			if !write(": catalog heartbeat\n\n") {
				return
			}
		case <-r.Context().Done():
			return
		case <-s.life.Done():
			return
		}
	}
}

func catalogFetched(s *session, hash string, revision uint64) {
	s.mu.Lock()
	previous := s.catalogFetchedHash
	current, trace := s.toolHash == hash && revision == s.catalogRevision, s.trace
	if current {
		s.catalogFetchedHash = hash
	}
	s.mu.Unlock()
	if current && revision > 0 && previous != hash {
		// A successful fetch is observable; model adoption is not inferred.
		trace.emit(Observation{Kind: "bridge", Category: "catalog_refresh", State: "fetched", Reason: fmt.Sprintf("revision_%d", revision)})
	}
}
