package agy

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

func TestExactJSONPreservesArgumentsResultsAndSchemaThroughRecovery(t *testing.T) {
	const value = "9007199254740993"
	r, err := parseRequest([]byte(`{"model":"agy/test","input":[{"role":"user","content":"resume"},{"type":"function_call_output","call_id":"call","output":{"structuredContent":{"record":9007199254740993},"content":[],"isError":false}}],"tools":[{"type":"function","name":"write","parameters":{"type":"object","properties":{"record":{"type":"integer","const":9007199254740993,"enum":[9007199254740993]}}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var args map[string]any
	if err = unmarshalExact([]byte(`{"record":9007199254740993}`), &args); err != nil {
		t.Fatal(err)
	}
	fn := r.Tools[0]
	if !validArguments(normalizeSchema(fn.Parameters), args, 0) {
		t.Fatal("exact integer rejected")
	}
	wrong := map[string]any{"record": json.Number("9007199254740992")}
	if validArguments(fn.Parameters, wrong, 0) || digest(args) == digest(wrong) {
		t.Fatal("distinct large identifiers collapsed")
	}
	output := r.Items[1]["output"].(map[string]any)
	if output["structuredContent"].(map[string]any)["record"] != json.Number(value) {
		t.Fatal("result rounded")
	}
	copy := copyItems(r.Items)
	encoded, _ := json.Marshal(copy)
	if !strings.Contains(string(encoded), value) {
		t.Fatal("history clone rounded a result")
	}
	disk, err := openSessionDiskCache(t.TempDir(), strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer disk.db.Close()
	c := &pendingTool{ID: "call", Tool: fn, Arguments: args, BatchID: "original-batch", Delivered: true, Ready: make(chan struct{})}
	s := &session{ID: "native-exact", owner: "owner:key", model: r.Model, tools: r.Tools, last: time.Now(), pending: map[string]*pendingTool{c.ID: c}, responses: map[string]bool{}, items: r.Items[:1], history: requestFingerprints(r.Items[:1])}
	if err = deliverTools(s, r.Items[1:]); err != nil {
		t.Fatal(err)
	}
	if err = disk.put(closingCheckpoint(s, nil), true); err != nil {
		t.Fatal(err)
	}
	loaded, err := disk.loadSession(s.ID, s.owner, 0)
	if err != nil || loaded == nil {
		t.Fatal(err)
	}
	restored := loaded.pending[c.ID]
	if restored.BatchID != c.BatchID || restored.Arguments["record"] != json.Number(value) || digest(restored.Output) != digest(c.Output) || restored.OutputHash != c.OutputHash {
		t.Fatal("recovery changed call identity or result")
	}
	if !validArguments(loaded.tools[0].Parameters, restored.Arguments, 0) {
		t.Fatal("restored schema/argument types disagree")
	}
}

func TestExactJSONNumbersHaveBoundedCanonicalIdentity(t *testing.T) {
	for _, v := range []string{"1", "1.0", "1e0", "10e-1"} {
		if digest(map[string]any{"n": json.Number(v)}) != digest(map[string]any{"n": float64(1)}) {
			t.Fatal("equivalent numbers changed history identity", v)
		}
	}
	for _, pair := range [][2]string{{"-9007199254740993", "-9007199254740992"}, {"0.10000000000000001", "0.1"}, {"1e20", "1e21"}} {
		if digest(json.Number(pair[0])) == digest(json.Number(pair[1])) {
			t.Fatal("exact values merged", pair)
		}
	}
	var x map[string]any
	for _, invalid := range []string{`{"n":1e1025}`, `{"n":1e-1025}`, `{"n":` + strings.Repeat("9", 257) + `}`, `{"n":NaN}`, `{"n":1} {"n":2}`} {
		if unmarshalExact([]byte(invalid), &x) == nil {
			t.Fatal("unbounded/invalid JSON accepted")
		}
	}
	for _, v := range []string{"-0", "-1.25", "1.234e-12", "1e1024", "1e-1024"} {
		if err := unmarshalExact([]byte(`{"n":`+v+`}`), &x); err != nil {
			t.Fatal(v, err)
		}
	}
	data := []byte(`{"text":"9007199254740993 and \"1.0\"","n":1.00}`)
	canonical, err := canonicalJSONNumbers(data)
	if err != nil || string(canonical) != `{"text":"9007199254740993 and \"1.0\"","n":1}` {
		t.Fatal("text changed during numeric hashing", string(canonical), err)
	}
	for _, v := range []json.Number{"1.0", "1e3", "-2"} {
		if !validArguments(map[string]any{"type": "integer"}, v, 0) {
			t.Fatal("valid integer representation rejected", v)
		}
	}
	if validArguments(map[string]any{"type": "integer"}, json.Number("1.1"), 0) {
		t.Fatal("fraction accepted as integer")
	}
}

func TestCanonicalNumbersKeepLegacyFiniteFloatHashes(t *testing.T) {
	random := rand.New(rand.NewPCG(37, 71))
	for i := 0; i < 2000; i++ {
		v := math.Float64frombits(random.Uint64())
		if math.IsNaN(v) || math.IsInf(v, 0) || v == 0 {
			continue
		}
		b, _ := json.Marshal(map[string]any{"n": v})
		var exact any
		if err := unmarshalExact(b, &exact); err != nil {
			t.Fatal(err)
		}
		if digest(exact) != wireDigest(map[string]any{"n": v}) {
			t.Fatal("representable legacy float changed identity", string(b))
		}
	}
}

func TestExactNativeNumbersValidateBoundsWithoutFloatRounding(t *testing.T) {
	shell := tool{Type: "shell"}
	args := map[string]any{"action": map[string]any{"commands": []any{"echo fixture"}, "timeout_ms": json.Number("1e3")}}
	if !nativeArgumentsValid(shell, args) {
		t.Fatal("valid exact timeout rejected")
	}
	args["action"].(map[string]any)["timeout_ms"] = json.Number("2147483647.00000001")
	if nativeArgumentsValid(shell, args) {
		t.Fatal("out of range fractional timeout rounded to a valid integer")
	}
	computer := tool{Type: "computer", DisplayWidth: 100, DisplayHeight: 100}
	point := map[string]any{"type": "move", "x": json.Number("99.99999999999999999"), "y": json.Number("0")}
	if !nativeArgumentsValid(computer, map[string]any{"actions": []any{point}}) {
		t.Fatal("in-bounds exact coordinate rounded outside display")
	}
	point["x"] = json.Number("100.00000000000000001")
	if nativeArgumentsValid(computer, map[string]any{"actions": []any{point}}) {
		t.Fatal("out of bounds coordinate accepted")
	}
}

func TestLegacyNativeResultHistoryIndexIsUpgradedWithoutLosingSession(t *testing.T) {
	home, secret := t.TempDir(), strings.Repeat("s", 64)
	disk, err := openSessionDiskCache(home, secret)
	if err != nil {
		t.Fatal(err)
	}
	v := diskTestRecord("native-status-upgrade", "owner:key", time.Now())
	v.Items = []map[string]any{{"role": "user", "content": "edit once"}, {"type": "apply_patch_call", "call_id": "patch", "operation": map[string]any{"type": "delete_file", "path": "fixture"}}, {"type": "apply_patch_call_output", "call_id": "patch", "status": "completed", "output": "real result"}}
	v.History = nil
	for _, item := range v.Items {
		old := map[string]any{}
		for key, value := range item {
			if key != "id" && key != "status" {
				old[key] = value
			}
		}
		if old["type"] == nil && old["role"] != nil {
			old["type"] = "message"
		}
		v.History = append(v.History, wireDigest(old))
	}
	if err = disk.put(v, true); err != nil {
		t.Fatal(err)
	}
	keys := disk.historyKeys(v.History)
	if _, err = disk.db.Exec(`UPDATE sessions SET history_key=? WHERE id=?`, keys[len(keys)-1], v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = disk.db.Exec(`UPDATE cache_meta SET value=? WHERE id=2`, []byte("canonical-tool-ids-v2")); err != nil {
		t.Fatal(err)
	}
	disk.db.Close()
	disk, err = openSessionDiskCache(home, secret)
	if err != nil {
		t.Fatal(err)
	}
	defer disk.db.Close()
	req := request{Model: v.Model, Items: append(copyItems(v.Items), map[string]any{"role": "user", "content": "continue"})}
	loaded, err := disk.find(v.Owner, req, nil)
	if err != nil || loaded == nil || loaded.ID != v.ID || !prefix(loaded.history, requestFingerprints(req.Items)) {
		t.Fatal("upgrade lost original session", err)
	}
	req.Items[2]["status"] = "failed"
	if wrong, err := disk.find(v.Owner, req, nil); err != nil || wrong != nil {
		t.Fatal("changed execution status reused original success history", err)
	}
}
