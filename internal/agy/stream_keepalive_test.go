package agy

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStreamKeepaliveCoversFirstTokenAndSilentGap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		withStreamKeepalive(w, r, 10*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
			out := newResponse(w, true, "agy/test")
			_ = out.event("response.created", map[string]any{"response": out.envelope("in_progress", Usage{})})
			time.Sleep(65 * time.Millisecond)
			_ = out.text("real output", false)
			time.Sleep(65 * time.Millisecond)
			_ = out.finish(Usage{})
		})
	}))
	defer server.Close()
	resp, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	before, after, sawText := 0, 0, false
	var body strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		body.WriteString(line + "\n")
		if strings.Contains(line, `"delta":"real output"`) {
			sawText = true
		}
		if line == ": keepalive" {
			if sawText {
				after++
			} else {
				before++
			}
		}
	}
	tracker := &Tracker{Stream: true}
	tracker.Feed([]byte(body.String()))
	tracker.Finish()
	if scanner.Err() != nil || before == 0 || after == 0 || !tracker.Completed || !tracker.ModelStarted {
		t.Fatalf("missing first-token/gap keepalive or completion: before=%d after=%d tracker=%+v err=%v", before, after, tracker, scanner.Err())
	}
}

func TestStreamKeepalivePreservesJSONAndHTTPFailures(t *testing.T) {
	for _, status := range []int{200, 429, 503} {
		w := httptest.NewRecorder()
		withStreamKeepalive(w, httptest.NewRequest("POST", "/", nil), time.Millisecond, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			time.Sleep(15 * time.Millisecond)
			io.WriteString(w, `{"value":"unchanged"}`)
		})
		if w.Code != status || w.Body.String() != `{"value":"unchanged"}` {
			t.Fatalf("JSON/error response changed: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestStreamKeepaliveRetainsFailureAfterResponseStarted(t *testing.T) {
	w := httptest.NewRecorder()
	withStreamKeepalive(w, httptest.NewRequest("POST", "/", nil), time.Millisecond, func(w http.ResponseWriter, _ *http.Request) {
		out := newResponse(w, true, "agy/test")
		_ = out.event("response.created", map[string]any{"response": out.envelope("in_progress", Usage{})})
		time.Sleep(15 * time.Millisecond)
		out.fail(fail(503, "worker_lost", "AGY request failed"))
	})
	tracker := &Tracker{Stream: true}
	tracker.Feed(w.Body.Bytes())
	if !strings.Contains(w.Body.String(), ": keepalive") || tracker.Completed || tracker.Failure != "worker_lost" || strings.Contains(w.Body.String(), "response.completed") {
		t.Fatalf("keepalive concealed failure: %+v %s", tracker, w.Body.String())
	}
}

func TestStreamKeepaliveLivesOutsideResponseReplay(t *testing.T) {
	m := &Manager{life: t.Context()}
	var calls atomic.Int64
	execute := func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		out := newResponse(w, true, "agy/test")
		_ = out.event("response.created", map[string]any{"response": out.envelope("in_progress", Usage{})})
		time.Sleep(30 * time.Millisecond)
		_ = out.finish(Usage{})
	}
	serve := func(w http.ResponseWriter, r *http.Request) { m.replayResponse(w, r, execute) }
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		withStreamKeepalive(w, replayTestRequest(t.Context(), `{"model":"agy/test","stream":true,"input":"hello"}`, "keepalive-replay"), time.Millisecond, serve)
		if i == 0 && !strings.Contains(w.Body.String(), ": keepalive") {
			t.Fatal("waiting subscriber received no keepalive")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("replay executed the original request twice")
	}
	for _, entry := range m.replays {
		if strings.Contains(entry.body.String(), ": keepalive") {
			t.Fatal("connection heartbeat entered response replay storage")
		}
	}
}

type failingKeepaliveWriter struct{ *httptest.ResponseRecorder }

func (w failingKeepaliveWriter) Write([]byte) (int, error)       { return 0, io.ErrClosedPipe }
func (w failingKeepaliveWriter) WriteString(string) (int, error) { return 0, io.ErrClosedPipe }

func TestStreamKeepaliveWriteFailureCancelsDelivery(t *testing.T) {
	withStreamKeepalive(failingKeepaliveWriter{httptest.NewRecorder()}, httptest.NewRequest("POST", "/", nil), time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
			t.Fatal("failed heartbeat left subscriber attached")
		}
	})
}

func TestTrackerIgnoresHandshakeAndHeartbeatUntilActualModelData(t *testing.T) {
	tracker := &Tracker{Stream: true}
	tracker.Feed([]byte(": keepalive\n\nevent: response.created\ndata: {\"type\":\"response.created\"}\n\n"))
	if tracker.ModelStarted || tracker.Completed || tracker.Usage.Known {
		t.Fatal("connection heartbeat counted as model output")
	}
	tracker.Feed([]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"thinking\"}\n\n"))
	if !tracker.ModelStarted || tracker.Completed || tracker.Usage.Known {
		t.Fatal("actual model delta not distinguished from keepalive")
	}
}
