package agy

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func cacheMatchItems() []map[string]any {
	return []map[string]any{
		{"role": "user", "content": "read and edit the requested file"},
		{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "inspect before editing"}}},
		{"type": "function_call", "call_id": "old-read", "name": "read", "arguments": `{"path":"a"}`},
		{"type": "function_call_output", "call_id": "old-read", "output": "actual contents"},
		{"type": "custom_tool_call", "call_id": "old-edit", "name": "apply_patch", "input": "actual patch"},
		{"type": "custom_tool_call_output", "call_id": "old-edit", "output": "actual edit result"},
		{"role": "assistant", "content": "done"},
	}
}

func renamedCacheMatchItems(items []map[string]any) []map[string]any {
	items = copyItems(items)
	for i, item := range items {
		item["id"], item["status"] = fmt.Sprintf("transport-%d", i), "completed"
		if id := stringField(item, "call_id"); id != "" {
			item["call_id"] = "renamed-" + id
		}
		if item["type"] == "reasoning" {
			item["content"], item["encrypted_content"] = nil, nil
		}
	}
	items[1], items[2] = items[2], items[1]
	return items
}

func TestAGYDiskAndMemoryHistoryMatchingAgree(t *testing.T) {
	d, err := openSessionDiskCache(t.TempDir(), strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	base := cacheMatchItems()
	v := diskTestRecord("same-native", "owner:key", time.Now())
	v.Items, v.History = base, requestFingerprints(base)
	if err := d.put(v, true); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func([]map[string]any)
		match  bool
	}{
		{"renamed IDs and Codex metadata", func([]map[string]any) {}, true},
		{"changed arguments", func(v []map[string]any) { v[1]["arguments"] = `{"path":"other"}` }, false},
		{"changed reasoning", func(v []map[string]any) { v[2]["summary"] = []any{} }, false},
		{"changed tool output", func(v []map[string]any) { v[3]["output"] = "different contents" }, false},
		{"changed custom input", func(v []map[string]any) { v[4]["input"] = "different patch" }, false},
		{"changed visible answer", func(v []map[string]any) { v[6]["content"] = "different answer" }, false},
		{"broken result link", func(v []map[string]any) { v[3]["call_id"] = "unrelated-call" }, false},
		{"duplicate call alias", func(v []map[string]any) { v[4]["call_id"] = v[1]["call_id"]; v[5]["call_id"] = v[1]["call_id"] }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := renamedCacheMatchItems(base)
			tc.change(items)
			items = append(items, map[string]any{"role": "user", "content": "continue"})
			r := request{Model: v.Model, Items: canonicalHistoryItems(items)}
			before := copyItems(r.Items)
			warm := &session{ID: v.ID, owner: v.Owner, model: v.Model, items: base, history: v.History, responses: v.Responses}
			m := &Manager{sessions: map[string]*session{"warm": warm}}
			inMemory, memoryErr := m.find(v.Owner, r)
			onDisk, diskErr := d.find(v.Owner, r, nil)
			if memoryErr != nil || diskErr != nil || (inMemory != nil) != tc.match || (onDisk != nil) != tc.match {
				t.Fatalf("memory/disk disagree: memory=%v/%v disk=%v/%v", inMemory != nil, memoryErr, onDisk != nil, diskErr)
			}
			if onDisk != nil && onDisk.ID != v.ID {
				t.Fatal("native session identity changed")
			}
			if !reflect.DeepEqual(r.Items, before) {
				t.Fatal("candidate lookup modified client history")
			}
			for _, owner := range []string{"other:key", "owner:other-key"} {
				if s, err := d.find(owner, r, nil); err != nil || s != nil {
					t.Fatal("renamed IDs crossed an owner/key boundary", err)
				}
			}
		})
	}
	duplicate := v
	duplicate.ID, duplicate.Responses = "second-native", map[string]bool{"second-response": true}
	if err := d.put(duplicate, true); err != nil {
		t.Fatal(err)
	}
	r := request{Model: v.Model, Items: canonicalHistoryItems(renamedCacheMatchItems(base))}
	if _, err := d.find(v.Owner, r, nil); err == nil {
		t.Fatal("multiple genuinely matching native sessions were not ambiguous")
	}
	if s, err := d.find(v.Owner, request{Previous: "resp-same-native"}, nil); err != nil || s == nil || s.ID != v.ID {
		t.Fatal("explicit response chain was not kept authoritative", err)
	}
}

func TestAGYDiskIndexUpgradesExistingHistoryOnce(t *testing.T) {
	home, secret := t.TempDir(), strings.Repeat("s", 64)
	d, err := openSessionDiskCache(home, secret)
	if err != nil {
		t.Fatal(err)
	}
	v := diskTestRecord("old-index", "owner:key", time.Now())
	v.Items = cacheMatchItems()
	v.History = requestFingerprints(v.Items)
	if err := d.put(v, true); err != nil {
		t.Fatal(err)
	}
	keys := d.historyKeys(v.History)
	if _, err := d.db.Exec(`UPDATE sessions SET history_key=? WHERE id=?`, keys[len(keys)-1], v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.Exec(`DELETE FROM cache_meta WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	d.db.Close()
	r := request{Model: v.Model, Items: canonicalHistoryItems(renamedCacheMatchItems(v.Items))}
	for i := 0; i < 2; i++ {
		d, err = openSessionDiskCache(home, secret)
		if err != nil {
			t.Fatal(err)
		}
		s, err := d.find(v.Owner, r, nil)
		if err != nil || s == nil || s.ID != v.ID || d.count.Load() != 1 {
			t.Fatalf("existing cache not matched after upgrade/restart %d: %v", i, err)
		}
		if i == 0 {
			_, err = d.db.Exec(`CREATE TRIGGER no_second_reindex BEFORE UPDATE OF history_key ON sessions BEGIN SELECT RAISE(ABORT,'unexpected repeat migration'); END`)
			if err != nil {
				t.Fatal(err)
			}
		}
		d.db.Close()
	}
}

func TestAGYDiskCandidatesRequireFullValidationAcrossPages(t *testing.T) {
	d, err := openSessionDiskCache(t.TempDir(), strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	v := diskTestRecord("z-correct", "owner:key", time.Now())
	v.Items = cacheMatchItems()
	v.History = requestFingerprints(v.Items)
	if err := d.put(v, true); err != nil {
		t.Fatal(err)
	}
	key := d.routingKey(v)
	for i := 0; i < 130; i++ {
		wrong := v
		wrong.ID = fmt.Sprintf("a-wrong-%03d", i)
		wrong.Responses = nil
		wrong.Items = copyItems(v.Items)
		wrong.Items[3]["output"] = "not the actual result"
		wrong.History = requestFingerprints(wrong.Items)
		if err := d.put(wrong, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.db.Exec(`UPDATE sessions SET history_key=? WHERE id LIKE 'a-wrong-%'`, key); err != nil {
		t.Fatal(err)
	}
	r := request{Model: v.Model, Items: canonicalHistoryItems(renamedCacheMatchItems(v.Items))}
	s, err := d.find(v.Owner, r, nil)
	if err != nil || s == nil || s.ID != v.ID {
		t.Fatal("unverified index candidates hid the real session", err)
	}
}

func TestAGYArchivedRenamedHistoryLoadsNativeSessionWithoutReplay(t *testing.T) {
	var newSessions, loads, prompts atomic.Int64
	m, s := recoveryFixture(t, func(*session, frame) any { return nil })
	d, err := openSessionDiskCache(m.cfg.Home, strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	m.disk = d
	options := []any{map[string]any{"id": "model", "options": []any{map[string]any{"value": "test"}}}}
	p := diskTestProcess(t, func(f frame) any {
		if f.Method == "session/new" {
			newSessions.Add(1)
			return map[string]any{"sessionId": "unexpected-rebuild", "configOptions": options}
		}
		if f.Method == "session/load" {
			loads.Add(1)
			var params map[string]any
			_ = json.Unmarshal(f.Params, &params)
			if params["sessionId"] != "native" {
				t.Error("a different native session was loaded")
			}
		}
		if f.Method == "session/prompt" {
			prompts.Add(1)
			text := string(f.Params)
			if strings.Contains(text, "old history") || strings.Contains(text, "actual contents") || !strings.Contains(text, "new request only") || len(text) > 4096 {
				t.Error("the archived transcript was replayed to AGY")
			}
			m.mu.Lock()
			for _, restored := range m.sessions {
				if restored.ID == "native" {
					restored.events <- agentEvent{Text: "continued original session"}
				}
			}
			m.mu.Unlock()
			return map[string]any{"stopReason": "end_turn"}
		}
		return map[string]any{"configOptions": options}
	})
	m.proc, s.proc = p, p
	s.officialVersion = officialToolsVersion
	s.items = cacheMatchItems()
	s.items[0]["content"] = strings.Repeat("old history ", MaxText/12+100)
	s.history = requestFingerprints(s.items)
	if err := m.archiveSession(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	delete(m.sessions, s.tenant)
	m.mu.Unlock()
	items := renamedCacheMatchItems(s.items)
	items = append(items, map[string]any{"role": "user", "content": "new request only"})
	body, _ := json.Marshal(map[string]any{"model": s.model, "input": items})
	w := recoveryRequest(m, string(body))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "continued original session") || loads.Load() != 1 || newSessions.Load() != 0 || prompts.Load() != 1 {
		t.Fatalf("archived continuation rebuilt or failed: status=%d loads=%d new=%d prompts=%d response=%s", w.Code, loads.Load(), newSessions.Load(), prompts.Load(), w.Body.String())
	}
}
