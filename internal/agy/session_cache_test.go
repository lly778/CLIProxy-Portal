package agy

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func diskTestRecord(id, owner string, last time.Time) savedSession {
	return savedSession{ID: id, Owner: owner, Model: "agy/test", System: digest(""), ToolHash: digest([]tool(nil)), History: []string{digest("request-" + id), digest("response-" + id)}, Responses: map[string]bool{"resp-" + id: true}, Last: last}
}

func TestAGYDiskSessionRoutingSurvivesRestartAndIsOwnerScoped(t *testing.T) {
	home := t.TempDir()
	secret := strings.Repeat("s", 64)
	d, err := openSessionDiskCache(home, secret)
	if err != nil {
		t.Fatal(err)
	}
	v := diskTestRecord("native-a", "alice:key-a", time.Now())
	if err = d.put(v, true); err != nil {
		t.Fatal(err)
	}
	if d.count.Load() != 1 {
		t.Fatal("incorrect disk count")
	}
	_ = d.db.Close()
	d, err = openSessionDiskCache(home, secret)
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	for _, owner := range []string{"bob:key-a", "alice:key-b"} {
		if got, e := d.find(owner, request{Previous: "resp-native-a"}, nil); e != nil || got != nil {
			t.Fatal("foreign owner or key could restore a session")
		}
	}
	s, err := d.find(v.Owner, request{Previous: "resp-native-a"}, nil)
	if err != nil || s == nil || s.ID != v.ID || !reflect.DeepEqual(s.history, v.History) {
		t.Fatal("routing state not restored after restart")
	}
	if got, e := d.find(v.Owner, request{Previous: "resp-native-a"}, map[string]bool{v.ID: true}); e != nil || got != nil {
		t.Fatal("warm session was duplicated by the disk index")
	}
	if err = d.put(v, false); err != nil {
		t.Fatal(err)
	}
	if got, e := d.find(v.Owner, request{Previous: "resp-native-a"}, nil); e != nil || got != nil {
		t.Fatal("partial turn was recovered as a completed session")
	}
	if d.count.Load() != 1 {
		t.Fatal("updating a session changed the count")
	}
	if err = d.put(v, true); err != nil {
		t.Fatal(err)
	}
	v.Responses["resp-native-a"] = false
	if err = d.put(v, true); err != nil {
		t.Fatal(err)
	}
	if got, e := d.find(v.Owner, request{Previous: "resp-native-a"}, nil); e != nil || got != nil {
		t.Fatal("consumed response chain remained recoverable")
	}
	if _, err = d.db.Exec(`UPDATE sessions SET payload=? WHERE id=?`, []byte("tampered"), v.ID); err != nil {
		t.Fatal(err)
	}
	// A new response index makes this corrupted record addressable.
	_, _ = d.db.Exec(`INSERT INTO responses(owner,response,session_id) VALUES(?,?,?)`, d.hash(v.Owner), d.hash("new-response"), v.ID)
	if _, err = d.find(v.Owner, request{Previous: "new-response"}, nil); err == nil {
		t.Fatal("corrupt encrypted metadata was accepted")
	}
	if _, err = openSessionDiskCache(home, strings.Repeat("x", 64)); err == nil {
		t.Fatal("another service key could open the cache")
	}
}

func TestAGYDiskHistoryLookupAndUnboundedRetentionCount(t *testing.T) {
	d, err := openSessionDiskCache(t.TempDir(), strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	items := []map[string]any{{"role": "user", "content": "task"}, {"role": "assistant", "content": "answer"}}
	v := diskTestRecord("history-a", "owner:key", time.Now())
	v.History = requestFingerprints(items)
	if err = d.put(v, true); err != nil {
		t.Fatal(err)
	}
	r := request{Model: v.Model, Items: append(items, map[string]any{"role": "user", "content": "continue"})}
	s, err := d.find(v.Owner, r, nil)
	if err != nil || s == nil || s.ID != v.ID {
		t.Fatal("full-history continuation could not find its disk session")
	}
	for i := 0; i < 150; i++ {
		other := diskTestRecord("other-"+digest(i)[:12], v.Owner, time.Now())
		if err = d.put(other, true); err != nil {
			t.Fatal(err)
		}
	}
	if d.count.Load() != 151 {
		t.Fatal("disk sessions were capped or evicted by count")
	}
	v.ID = "history-b"
	v.Responses = map[string]bool{"resp-b": true}
	if err = d.put(v, true); err != nil {
		t.Fatal(err)
	}
	if _, err = d.find(v.Owner, r, nil); err == nil {
		t.Fatal("identical history from different sessions was not treated as ambiguous")
	}
}

func TestAGYDiskRetentionDeletesOnlyExpiredIdleOwnedFiles(t *testing.T) {
	home := t.TempDir()
	base := filepath.Join(home, "agy", "conversations")
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	d, err := openSessionDiskCache(home, strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	now := time.Now()
	for _, entry := range []struct {
		id   string
		last time.Time
	}{{"expired", now.Add(-sessionRetention - time.Hour)}, {"live", now.Add(-sessionRetention - time.Hour)}, {"recent", now.Add(-time.Hour)}} {
		if err = d.put(diskTestRecord(entry.id, "owner:key", entry.last), true); err != nil {
			t.Fatal(err)
		}
		for _, suffix := range []string{".db", ".db-wal", ".db-shm"} {
			if err = os.WriteFile(filepath.Join(base, entry.id+suffix), []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	_ = os.WriteFile(filepath.Join(base, "untracked.db"), []byte("keep"), 0600)
	m := &Manager{cfg: RuntimeConfig{Home: home}, disk: d, slot: make(chan struct{}, 1), sessions: map[string]*session{"live": {ID: "live", running: true}}}
	if err = m.pruneSessionCache(now); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(base, "expired.db")); !os.IsNotExist(err) {
		t.Fatal("expired native history not removed")
	}
	for _, id := range []string{"live", "recent", "untracked"} {
		if _, err = os.Stat(filepath.Join(base, id+".db")); err != nil {
			t.Fatal("live, recent or unrelated history was removed")
		}
	}
	if d.count.Load() != 2 {
		t.Fatal("expired routing metadata was not removed")
	}
	if got, e := d.find("owner:key", request{Previous: "resp-live"}, nil); e != nil || got != nil {
		t.Fatal("expired history remained addressable")
	}
	for _, id := range []string{"../outside", "a/b", "a\\b", ".", ""} {
		if safeNativeSessionID(id) {
			t.Fatal("unsafe native filename accepted")
		}
	}
}

// A minimal in-process ACP transport verifies restore ordering and prevents
// replayed history from being streamed or historical tools from being run.
func diskTestProcess(t *testing.T, onCall func(frame) any) *process {
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	p := &process{cmd: &exec.Cmd{}, in: inWriter, pending: map[string]chan rpcReply{}, done: make(chan struct{}), canLoad: true}
	go p.read(outReader)
	go func() {
		defer outWriter.Close()
		defer inReader.Close()
		d := json.NewDecoder(inReader)
		for {
			var f frame
			if d.Decode(&f) != nil {
				return
			}
			if len(f.ID) == 0 {
				continue
			}
			result := onCall(f)
			_ = json.NewEncoder(outWriter).Encode(map[string]any{"jsonrpc": "2.0", "id": f.ID, "result": result})
		}
	}()
	t.Cleanup(func() { p.close(); _ = outReader.Close() })
	return p
}

func TestAGYRestoreUsesNativeLoadAndParksOldestCompletedSession(t *testing.T) {
	d, err := openSessionDiskCache(t.TempDir(), strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	var mu sync.Mutex
	var methods []string
	p := diskTestProcess(t, func(f frame) any {
		mu.Lock()
		methods = append(methods, f.Method)
		mu.Unlock()
		if f.Method == "session/load" {
			var params map[string]any
			_ = json.Unmarshal(f.Params, &params)
			if params["sessionId"] != "cold" || len(params["mcpServers"].([]any)) != 1 {
				t.Error("wrong native session or missing rebound MCP tools")
			}
			return map[string]any{"configOptions": []any{map[string]any{"id": "model", "options": []any{map[string]any{"value": "test"}}}}}
		}
		return map[string]any{}
	})
	life, cancel := context.WithCancel(t.Context())
	defer cancel()
	old := &session{ID: "warm", tenant: "warm", owner: "owner:key", model: "agy/test", history: []string{"first"}, responses: map[string]bool{"resp-warm": true}, last: time.Now().Add(-time.Minute), proc: p, cancel: func() {}}
	m := &Manager{cfg: RuntimeConfig{Home: t.TempDir(), MaxSessions: 1}, disk: d, proc: p, life: life, sessions: map[string]*session{"warm": old}, mcpURL: "http://127.0.0.1:1234/mcp/"}
	r := request{Model: "agy/test"}
	v := diskTestRecord("cold", "owner:key", time.Now())
	cold := &session{ID: v.ID, owner: v.Owner, model: v.Model, system: v.System, toolHash: v.ToolHash, history: v.History, responses: v.Responses, last: v.Last}
	s, err := m.restoreSession(t.Context(), cold, r)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "cold" || s.loading || len(m.sessions) != 1 || m.sessions[s.tenant] != s {
		t.Fatal("restored session not registered correctly")
	}
	mu.Lock()
	got := append([]string(nil), methods...)
	mu.Unlock()
	if len(got) < 2 || got[0] != "session/close" || got[1] != "session/load" {
		t.Fatalf("worker not closed before loading: %v", got)
	}
	if v, e := d.find("owner:key", request{Previous: "resp-warm"}, nil); e != nil || v == nil {
		t.Fatal("evicted session not saved to disk")
	}
	s.mu.Lock()
	s.loading = true
	s.mu.Unlock()
	m.update(json.RawMessage(`{"sessionId":"cold","update":{"sessionUpdate":"agent_message_chunk","content":{"text":"old replay"}}}`))
	if len(s.events) != 0 {
		t.Fatal("native replay leaked into the current response")
	}
}
