package agy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAGYResidentLimitTrimsOnlyCompletedIdleSessions(t *testing.T) {
	d, err := openSessionDiskCache(t.TempDir(), strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	p := diskTestProcess(t, func(frame) any { return map[string]any{} })
	life, cancel := context.WithCancel(t.Context())
	defer cancel()
	m := &Manager{cfg: RuntimeConfig{MaxSessions: 7}, disk: d, proc: p, life: life, sessions: map[string]*session{}, slot: make(chan struct{}, 3)}
	for i, name := range []string{"idle-old", "idle-mid", "idle-new", "running", "tools", "loading", "delivering"} {
		v := diskTestRecord(name, "owner:key", time.Now().Add(time.Duration(i-10)*time.Minute))
		s := &session{ID: name, tenant: name, owner: v.Owner, model: v.Model, history: v.History, responses: v.Responses, last: v.Last, proc: p, cancel: func() {}}
		m.sessions[name] = s
	}
	m.sessions["running"].running = true
	m.sessions["tools"].waitAt = time.Now()
	m.sessions["loading"].loading = true
	m.sessions["delivering"].requestMu.Lock()
	if err := m.SetResidentLimit(t.Context(), 2, 2); err != nil {
		t.Fatal(err)
	}
	if status := m.Status(); status.MaxSessions != 2 || status.Sessions != 4 || cap(m.slot) != 3 || d.count.Load() != 3 {
		t.Fatalf("resident cap changed concurrency or interrupted work: %+v", status)
	}
	for _, name := range []string{"running", "tools", "loading", "delivering"} {
		if m.sessions[name] == nil {
			t.Fatal("active or undelivered session was removed", name)
		}
	}
	if victim, ok := m.reserveSession(&session{tenant: "new"}); ok || victim != nil {
		t.Fatal("new session bypassed a lowered resident cap")
	}
	m.sessions["delivering"].requestMu.Unlock()
	for _, s := range m.sessions {
		s.running, s.loading, s.waitAt = false, false, time.Time{}
	}
	if err := m.trimResidentSessions(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(m.sessions) != 2 || d.count.Load() != 5 {
		t.Fatal("completed sessions did not settle to the new cap")
	}
	if err := m.SetResidentLimit(t.Context(), 5, 3); err != nil {
		t.Fatal(err)
	}
	if victim, ok := m.reserveSession(&session{tenant: "new"}); !ok || victim != nil {
		t.Fatal("increased resident cap was not applied")
	}
	if err := m.SetResidentLimit(t.Context(), 1, 2); err != nil || m.Status().MaxSessions != 5 {
		t.Fatal("an older settings request overwrote the latest limit", err)
	}
	for _, invalid := range []int{-1, 17} {
		if m.SetResidentLimit(t.Context(), invalid, 4) == nil || m.Status().MaxSessions != 5 {
			t.Fatal("invalid resident limit changed runtime state", invalid)
		}
	}
}

func TestAGYResidentLimitRefreshesInflightStatusWithoutStartingCLI(t *testing.T) {
	var limit, reads atomic.Int64
	limit.Store(3)
	entered, release := make(chan struct{}), make(chan struct{})
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/status":
			value := limit.Load()
			if reads.Add(1) == 1 {
				close(entered)
				<-release
			}
			_ = json.NewEncoder(w).Encode(Status{MaxSessions: int(value)})
		case "/internal/resident-limit":
			var v struct {
				Limit int `json:"max_sessions"`
			}
			_ = json.NewDecoder(r.Body).Decode(&v)
			limit.Store(int64(v.Limit))
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Error("resident setting attempted to discover models or start work", r.URL.Path)
		}
	}))
	defer worker.Close()
	c, err := NewClient(worker.URL, strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan Status, 1)
	go func() {
		v, err := c.Status(t.Context())
		if err != nil {
			t.Error(err)
		}
		result <- v
	}()
	<-entered
	if err := c.SetResidentLimit(t.Context(), 5, 2); err != nil {
		t.Fatal(err)
	}
	close(release)
	if v := <-result; v.MaxSessions != 5 {
		t.Fatal("a stale in-flight status hid the saved limit", v)
	}
	if v, err := c.Status(t.Context()); err != nil || v.MaxSessions != 5 || reads.Load() != 2 {
		t.Fatal("status cache was not reused after the update", v, err)
	}
}

func TestAGYResidentLimitEndpointRequiresServiceAuthAndValidatesInput(t *testing.T) {
	m := &Manager{cfg: RuntimeConfig{Secret: strings.Repeat("s", 64), MaxSessions: 3}, sessions: map[string]*session{}}
	h := m.Handler()
	for _, tc := range []struct {
		body string
		auth bool
		code int
	}{
		{`{"max_sessions":5,"revision":1}`, false, 401},
		{`{"max_sessions":-1,"revision":1}`, true, 400},
		{`{"max_sessions":17,"revision":1}`, true, 400},
		{`{"max_sessions":5}`, true, 400},
		{`{"max_sessions":5,"revision":1} {}`, true, 400},
		{`{"max_sessions":5,"revision":1}`, true, 204},
		{`{"max_sessions":0,"revision":2}`, true, 204},
	} {
		r := httptest.NewRequest(http.MethodPost, "/internal/resident-limit", strings.NewReader(tc.body))
		if tc.auth {
			r.Header.Set("Authorization", "Bearer "+m.cfg.Secret)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("setting status=%d, expected=%d", w.Code, tc.code)
		}
	}
	if m.Status().MaxSessions != 0 || m.proc != nil {
		t.Fatal("settings did not apply without starting the official CLI")
	}
}

func TestAGYZeroResidentLimitKeepsUnlimitedEntries(t *testing.T) {
	p := diskTestProcess(t, func(frame) any { return map[string]any{} })
	m := &Manager{cfg: RuntimeConfig{MaxSessions: 3}, sessions: map[string]*session{}, slot: make(chan struct{}, 3)}
	if err := m.SetResidentLimit(t.Context(), 0, 1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("resident-%02d", i)
		s := &session{ID: name, tenant: name, proc: p, cancel: func() {}, last: time.Now().Add(time.Duration(i) * time.Second)}
		if victim, ok := m.reserveSession(s); !ok || victim != nil {
			t.Fatal("zero resident limit still evicted or rejected a session", i)
		}
	}
	if err := m.trimResidentSessions(t.Context()); err != nil || m.Status().Sessions != 20 || m.Status().MaxSessions != 0 || cap(m.slot) != 3 {
		t.Fatal("unlimited resident size changed count or execution slots", err)
	}
	if err := m.SetResidentLimit(t.Context(), 3, 1); err == nil || m.Status().MaxSessions != 0 {
		t.Fatal("the same revision overwrote an unlimited setting")
	}
	if err := m.SetResidentLimit(t.Context(), 2, 2); err != nil || m.Status().Sessions != 2 {
		t.Fatal("a finite limit could not replace unlimited residency", err)
	}
}
