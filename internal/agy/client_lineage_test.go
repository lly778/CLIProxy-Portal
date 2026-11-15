package agy

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func protocolDisk(t *testing.T, m *Manager) *sessionDiskCache {
	t.Helper()
	m.cfg.Secret = strings.Repeat("s", 64)
	d, err := openSessionDiskCache(t.TempDir(), m.cfg.Secret)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.db.Close() })
	m.disk = d
	return d
}

func TestClientConversationUsesExistingStableHeadersNotTurns(t *testing.T) {
	m := &Manager{cfg: RuntimeConfig{Secret: strings.Repeat("s", 64)}}
	h := make(http.Header)
	h.Set("Session_id", "private-session")
	h.Set("X-Codex-Turn-Metadata", `{"turn_id":"first"}`)
	key := m.clientConversationKey(h)
	h.Set("X-Codex-Turn-Metadata", `{"turn_id":"second"}`)
	if len(key) != 64 || key != m.clientConversationKey(h) || strings.Contains(key, "private-session") {
		t.Fatal("turn change broke stable private conversation identity")
	}
	h.Del("Session_id")
	if m.clientConversationKey(h) != "" {
		t.Fatal("a turn ID became a conversation identity")
	}
	h.Set("X-Codex-Turn-Metadata", `{"thread_id":"chat","turn_id":"first"}`)
	key = m.clientConversationKey(h)
	h.Set("X-Codex-Turn-Metadata", `{"thread_id":"chat","turn_id":"second"}`)
	if key == "" || key != m.clientConversationKey(h) {
		t.Fatal("existing thread metadata did not establish stable identity")
	}
	h.Set("X-Codex-Turn-Metadata", `{"thread_id":7}`)
	if m.clientConversationKey(h) != "" {
		t.Fatal("invalid metadata was accepted")
	}
}

func TestClientConversationDisambiguatesExactHistoriesAcrossRestart(t *testing.T) {
	m := &Manager{sessions: map[string]*session{}}
	d := protocolDisk(t, m)
	h := make(http.Header)
	h.Set("Session_id", "private-chat-a")
	a := m.clientConversationKey(h)
	h.Set("Session_id", "private-chat-b")
	b := m.clientConversationKey(h)
	items := cacheMatchItems()
	for _, v := range []struct{ id, key string }{{"native-a", a}, {"native-b", b}, {"legacy", ""}} {
		record := diskTestRecord(v.id, "owner", time.Now())
		record.Items, record.History = items, requestFingerprints(items)
		if v.key != "" {
			record.ClientBindings = []string{v.key}
		}
		if err := d.put(record, true); err != nil {
			t.Fatal(err)
		}
	}
	req := request{Model: "agy/test", clientKey: a, Items: append(copyItems(items), map[string]any{"role": "user", "content": "continue"})}
	for _, phase := range []string{"before restart", "after restart"} {
		if phase == "after restart" {
			var path string
			if err := d.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
				t.Fatal(err)
			}
			_ = d.db.Close()
			var err error
			d, err = openSessionDiskCache(strings.TrimSuffix(path, "portal-sessions.db"), m.cfg.Secret)
			if err != nil {
				t.Fatal(err)
			}
			defer d.db.Close()
			m.disk = d
		}
		found, err := m.find("owner", req)
		if err != nil || found == nil || found.ID != "native-a" {
			t.Fatal(phase, "conversation routing lost exact identity", err)
		}
		if found, err = m.find("other-owner", req); err != nil || found != nil {
			t.Fatal("conversation header crossed owner boundary", err)
		}
	}
	// An existing owned response ID remains stronger than an optional header.
	req.Previous = "resp-native-b"
	if found, err := m.find("owner", req); err != nil || found == nil || found.ID != "native-b" {
		t.Fatal("client header overrode an explicit owned response chain", err)
	}
	var raw string
	if err := d.db.QueryRow(`SELECT client_key FROM session_client_routes LIMIT 1`).Scan(&raw); err != nil || len(raw) != 64 || strings.Contains(raw, "private-chat") {
		t.Fatal("raw client conversation identity leaked into routing index", err)
	}
}

func TestClientConversationNeverOverridesHistoryOrMergesOtherChats(t *testing.T) {
	m := &Manager{sessions: map[string]*session{}}
	d := protocolDisk(t, m)
	items := cacheMatchItems()
	h := make(http.Header)
	h.Set("Session_id", "a")
	key := m.clientConversationKey(h)
	v := diskTestRecord("native-a", "owner", time.Now())
	v.Items, v.History, v.ClientBindings = items, requestFingerprints(items), []string{key}
	if err := d.put(v, true); err != nil {
		t.Fatal(err)
	}
	changed := append(copyItems(items), map[string]any{"role": "user", "content": "continue"})
	changed[3]["output"] = "fabricated replacement result"
	if found, err := m.find("owner", request{Model: v.Model, clientKey: key, Items: changed}); err != nil || found != nil {
		t.Fatal("stable identity bypassed actual result verification", err)
	}
	h.Set("Session_id", "b")
	if found, err := m.find("owner", request{Model: v.Model, clientKey: m.clientConversationKey(h), Items: append(copyItems(items), map[string]any{"role": "user", "content": "continue"})}); err != nil || found != nil {
		t.Fatal("independent chat attached to another bound history", err)
	}
	// The same header can have intentional compacted/forked contexts. It is
	// still not sufficient to choose between two genuinely matching candidates.
	v.ID = "native-duplicate"
	v.Responses = map[string]bool{"resp-native-duplicate": true}
	if err := d.put(v, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.find("owner", request{Model: v.Model, clientKey: key, Items: append(copyItems(items), map[string]any{"role": "user", "content": "continue"})}); err == nil {
		t.Fatal("ambiguous native histories were silently merged")
	}
}

func TestExistingClientHeaderBindsSuccessfulTurnsWithoutClientChanges(t *testing.T) {
	m, s := recoveryFixture(t, func(target *session, _ frame) any {
		target.events <- agentEvent{Text: "retained answer"}
		return map[string]any{"stopReason": "end_turn"}
	})
	d := protocolDisk(t, m)
	if _, err := d.db.Exec(`CREATE TABLE reply_writes(n INTEGER); INSERT INTO reply_writes VALUES(0); CREATE TRIGGER count_reply_write AFTER INSERT ON response_replays BEGIN UPDATE reply_writes SET n=n+1; END`); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/internal/responses", strings.NewReader(`{"model":"agy/test","previous_response_id":"previous","input":"inspect"}`))
	r.Header.Set("X-Portal-Owner", "owner")
	r.Header.Set("Session_id", "existing-client-chat")
	w := httptest.NewRecorder()
	m.responsesHTTP(w, r)
	var result struct{ ID string }
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || result.ID == "" {
		t.Fatal("normal existing-client request failed", w.Body.String(), err)
	}
	var saved int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&saved); err != nil || saved != 0 {
		t.Fatal("normal response unexpectedly wrote a full session snapshot", saved, err)
	}
	if err := m.archiveSession(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	delete(m.sessions, s.tenant)
	cold, err := m.find("owner", request{Model: s.model, clientKey: m.clientConversationKey(r.Header), Items: append(copyItems(s.items), map[string]any{"role": "user", "content": "next"})})
	if err != nil || cold == nil || cold.ID != s.ID || len(cold.clientBindings) != 1 {
		t.Fatal("client conversation binding did not survive archival", err)
	}
	var count int
	if err = d.db.QueryRow(`SELECT COUNT(*) FROM session_client_routes`).Scan(&count); err != nil || count != 1 {
		t.Fatal("existing client header was not persistently bound", count, err)
	}
	if err = d.db.QueryRow(`SELECT n FROM reply_writes`).Scan(&count); err != nil || count != 1 {
		t.Fatal("completed response was redundantly written after atomic completion", count, err)
	}
}

func TestBoundClientCannotForkAwayFromUnresolvedFirstTool(t *testing.T) {
	m, s := recoveryFixture(t, func(*session, frame) any { t.Fatal("conflict started inference"); return nil })
	d := protocolDisk(t, m)
	continuityCall(s) // No earlier closed tool history exists to prove a safe tail recovery.
	h := make(http.Header)
	h.Set("Session_id", "same-client-chat")
	key := m.clientConversationKey(h)
	s.clientBindings = []string{key}
	if err := d.put(closingCheckpoint(s, nil), true); err != nil {
		t.Fatal(err)
	}
	m.sessions = map[string]*session{}
	req := request{Model: s.model, clientKey: key, Items: []map[string]any{s.items[0], {"role": "user", "content": "continue after closing"}}}
	_, err := m.find("owner", req)
	var conflict *Error
	if !errors.As(err, &conflict) || conflict.Code != "conversation_history_conflict" {
		t.Fatal("missing first call silently became a new conversation", err)
	}
}
