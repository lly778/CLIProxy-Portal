package agy

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	imagepng "image/png"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAGYOfficialToolsUseLivePolicyAndNeverReadServerReferences(t *testing.T) {
	s := &session{ID: "own-session", running: true, policy: toolPolicy{Mode: "auto"}}
	m := &Manager{sessions: map[string]*session{"tenant": s}}
	check := func(name string, args map[string]any) string {
		b, _ := json.Marshal(map[string]any{"conversationId": s.ID, "toolCall": map[string]any{"name": name, "args": args}})
		w := httptest.NewRecorder()
		m.officialPolicyHTTP(w, httptest.NewRequest("POST", "/", bytes.NewReader(b)))
		var reply map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &reply)
		return reply["decision"]
	}
	args := map[string]any{"Prompt": "blue cat", "ImageName": "blue_cat"}
	if check("generate_image", args) != "allow" || !s.policy.Used {
		t.Fatal("official image tool denied")
	}
	args["ImagePaths"] = []any{"/another-user/private.png"}
	if check("generate_image", args) != "deny" {
		t.Fatal("server reference files accepted")
	}
	delete(args, "ImagePaths")
	args["ImageName"] = "../escape"
	if check("generate_image", args) != "deny" {
		t.Fatal("image path traversal accepted")
	}
	if check("run_command", map[string]any{"CommandLine": "whoami"}) != "deny" {
		t.Fatal("official shell enabled")
	}
	if check("read_url_content", map[string]any{"Url": "http://127.0.0.1/private"}) != "deny" {
		t.Fatal("local URL accepted")
	}
	s.internal = true
	if check("search_web", map[string]any{"query": "Google"}) != "deny" {
		t.Fatal("summary worker executed a tool")
	}
	s.internal = false
	s.policy.Mode = "none"
	if check("search_web", map[string]any{"query": "Google"}) != "deny" {
		t.Fatal("tool_choice none bypassed")
	}
	s.policy.Mode, s.running = "auto", false
	if check("search_web", map[string]any{"query": "Google"}) != "deny" {
		t.Fatal("inactive session accepted")
	}
}

func TestAGYOfficialForcedToolsAndUnavailableDeclarations(t *testing.T) {
	for _, choice := range []string{`{"type":"web_search"}`, `{"type":"image_generation"}`, `{"type":"allowed_tools","mode":"required","tools":[{"type":"web_search"}]}`} {
		p, err := requestToolPolicy(request{ToolChoice: json.RawMessage(choice)}, nil)
		if err != nil || p.Mode != "required" {
			t.Fatalf("official tool choice rejected: %v", err)
		}
		search := !strings.Contains(choice, "image_generation")
		if p.permitsOfficial("search_web") != search || p.permitsOfficial("generate_image") == search || p.permits(tool{Name: "exec_command"}) {
			t.Fatal("official tool choice leaked client scope")
		}
	}
	r, err := parseRequest([]byte(`{"model":"agy/test","input":"test","tools":[{"type":"web_search"},{"type":"image_generation"},{"type":"file_search"}]}`))
	if err != nil || len(r.Tools) != 0 || len(r.UnavailableTools) != 1 || r.UnavailableTools[0] != "file_search" {
		t.Fatal("official services confused with client or unavailable tools")
	}
}

func TestAGYOfficialImageOutputIsSessionScopedAndTracked(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "agy", "brain", "own-session")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := imagepng.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	png := encoded.Bytes()
	path := filepath.Join(dir, "blue_cat_123.png")
	if err := os.WriteFile(path, png, 0600); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	o := newResponse(w, true, "agy/test")
	s := &session{ID: "own-session"}
	start := officialToolEvent{ID: "native-image", Name: "generate_image", Status: "in_progress", Args: map[string]any{"ImageName": "blue_cat"}}
	if err := o.official(home, s, start); err != nil {
		t.Fatal(err)
	}
	// Codex deserializes result as String even on output_item.added. A null
	// result makes the initial native image item invalid before it completes.
	if value, ok := o.output[0]["result"].(string); !ok || value != "" {
		t.Fatal("initial image result must be an empty string")
	}
	start.Status = "completed"
	if err := o.official(home, s, start); err != nil {
		t.Fatal(err)
	}
	if err := o.official(home, s, start); err != nil || len(o.output) != 1 {
		t.Fatal("native completion duplicated an image")
	}
	if err := o.text("Generated image.", false); err != nil {
		t.Fatal(err)
	}
	if err := o.finish(Usage{Known: true, Input: 2, Output: 3, Total: 5}); err != nil {
		t.Fatal(err)
	}
	var tracker Tracker
	tracker.Stream = true
	tracker.Feed(w.Body.Bytes())
	tracker.Finish()
	if !tracker.Completed || tracker.Usage.Total != 5 || !strings.Contains(w.Body.String(), `"type":"image_generation_call"`) || !strings.Contains(w.Body.String(), `"output_format":"png"`) {
		t.Fatal("image response lost its image or usage")
	}
	if _, _, err := officialGeneratedImage(home, "another-session", "blue_cat", time.Now()); err == nil {
		t.Fatal("another session's image returned")
	}
	if _, _, err := officialGeneratedImage(home, "own-session", "blue_cat", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("old image returned for new generation")
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "blue_cat_private.png"), png, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, "blue_cat_private.png"), filepath.Join(dir, "blue_cat_link.png")); err == nil {
		_ = os.Remove(path)
		if _, _, err := officialGeneratedImage(home, "own-session", "blue_cat", time.Now()); err == nil {
			t.Fatal("symlink leaked another directory")
		}
	}
}

func TestAGYOfficialUpdateDoesNotBecomeClientToolCall(t *testing.T) {
	life, cancel := context.WithCancel(t.Context())
	defer cancel()
	s := &session{ID: "native", running: true, life: life, cancel: cancel, events: make(chan agentEvent, 4), reported: map[string]reportedTool{}}
	m := &Manager{sessions: map[string]*session{"tenant": s}}
	m.update(json.RawMessage(`{"sessionId":"native","update":{"sessionUpdate":"tool_call","toolCallId":"google-search","name":"search_web","rawInput":{"query":"Google"},"status":"in_progress"}}`))
	e := <-s.events
	if e.Official == nil || e.Call != nil || len(s.officialActive) != 1 {
		t.Fatal("official execution became a client action")
	}
}

func TestAGYOfficialHookFailsClosedWithoutService(t *testing.T) {
	t.Setenv("PORTAL_AGY_SECRET_FILE", filepath.Join(t.TempDir(), "missing"))
	var out bytes.Buffer
	if err := RunOfficialToolPolicy(strings.NewReader(`{"toolCall":{"name":"generate_image"}}`), &out); err != nil || !strings.Contains(out.String(), `"decision":"deny"`) {
		t.Fatal("hook accepted missing policy service")
	}
}
