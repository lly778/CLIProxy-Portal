package agy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func compactTestText(blocks []any) string {
	var b strings.Builder
	for _, raw := range blocks {
		v := raw.(map[string]string)
		if v["type"] == "text" {
			b.WriteString(v["text"])
		}
	}
	return b.String()
}

func compactTestImage(data string) map[string]any {
	return map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + data}
}

func compactTestTranscript(blocks []any) string {
	var b strings.Builder
	for _, raw := range blocks {
		v := raw.(map[string]string)
		if v["type"] == "text" {
			b.WriteString(v["text"])
		} else {
			b.WriteString("\x00IMAGE:" + v["mimeType"] + ":" + v["data"] + "\x00")
		}
	}
	return b.String()
}

func TestAGYImageCompactionPreservesOrderedHistoryAndLatestImage(t *testing.T) {
	latest := map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Current question"}, compactTestImage("Yw==")}}
	items := []map[string]any{
		{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "First image context"}, compactTestImage("YQ=="), map[string]any{"type": "input_text", "text": "Between images"}, compactTestImage("Yg==")}},
		{"role": "assistant", "content": strings.Repeat("汉", 90000)},
		latest,
	}
	original := requestFingerprints(items)
	blocks, err := promptBlocksLimit(items[:2], MaxBody)
	if err != nil {
		t.Fatal(err)
	}
	var normalized []any
	for _, raw := range blocks {
		v := raw.(map[string]string)
		if v["type"] == "text" {
			normalized = append(normalized, map[string]string{"type": "text", "text": v["text"] + "\n"})
		} else {
			normalized = append(normalized, v)
		}
	}
	var seen []any
	calls := 0
	m := &Manager{summarizeTest: func(_ context.Context, _ string, prompt []any, _ func()) (string, error) {
		calls++
		if len(compactTestText(prompt)) > MaxText {
			t.Fatal("multimodal summary exceeded text budget")
		}
		seen = append(seen, prompt[1:len(prompt)-1]...)
		return "Preserved visual facts from both old images", nil
	}}
	out, err := m.compactItems(t.Context(), request{owner: "a:key", Model: "agy/test", Items: items}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls < 3 || compactTestTranscript(seen) != compactTestTranscript(normalized) {
		t.Fatal("image data, MIME type, order or historical text lost")
	}
	if !reflect.DeepEqual(out[len(out)-1], latest) || !reflect.DeepEqual(requestFingerprints(items), original) {
		t.Fatal("latest image or original history changed")
	}
	if _, err := promptBlocks(out); err != nil {
		t.Fatal(err)
	}
}

func TestAGYImageCompactionBatchingAndCacheIdentity(t *testing.T) {
	image := func(data, mime string) any { return map[string]string{"type": "image", "data": data, "mimeType": mime} }
	blocks := []any{}
	for i := 0; i < 9; i++ {
		blocks = append(blocks, image("YQ==", "image/png"))
	}
	segments, err := compactSegments(blocks)
	if err != nil || len(segments) != 3 || compactTestTranscript(blocks) != compactTestTranscript(append(append(append([]any{}, segments[0]...), segments[1]...), segments[2]...)) {
		t.Fatal("images were not batched without loss")
	}
	for _, segment := range segments {
		if len(segment) > compactChunkImages {
			t.Fatal("too many images in a summary segment")
		}
	}
	large := image(strings.Repeat("A", compactChunkImageBytes+4), "image/png")
	segments, err = compactSegments([]any{image("YQ==", "image/png"), large, image("Yg==", "image/png")})
	if err != nil || len(segments) != 3 || len(segments[1]) != 1 || !reflect.DeepEqual(segments[1][0], large) {
		t.Fatal("large image was dropped, modified or grouped with other images")
	}
	calls := 0
	m := &Manager{summarizeTest: func(context.Context, string, []any, func()) (string, error) { calls++; return "visual checkpoint", nil }}
	a, b := image("YQ==", "image/png"), image("Yg==", "image/png")
	for _, input := range [][]any{{a, b}, {a, b}, {b, a}, {a, image("Yg==", "image/jpeg")}} {
		if _, err = m.summarizeHistory(t.Context(), "owner:key", "agy/test", input, nil); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 3 {
		t.Fatal("image changes, order or MIME type were ignored by summary cache")
	}
}

func TestAGYImageCompactionFailureDoesNotReplaceHistory(t *testing.T) {
	items := []map[string]any{{"role": "user", "content": []any{compactTestImage("YQ==")}}, {"role": "assistant", "content": strings.Repeat("x", MaxText+1)}, {"role": "user", "content": "Continue"}}
	before := requestFingerprints(items)
	want := errors.New("vision request failed")
	m := &Manager{summarizeTest: func(context.Context, string, []any, func()) (string, error) { return "", want }}
	if _, err := m.compactItems(t.Context(), request{Model: "agy/test", Items: items}, nil); !errors.Is(err, want) {
		t.Fatalf("vision failure not returned: %v", err)
	}
	if !reflect.DeepEqual(before, requestFingerprints(items)) {
		t.Fatal("history changed after failed vision summary")
	}
}

func TestAGYCompactionCoversEntireHistoryAndPreservesLatestInput(t *testing.T) {
	segments := []string{strings.Repeat("甲", 40000) + "FIRST_MARK", strings.Repeat("乙", 40000) + "MIDDLE_MARK", strings.Repeat("丙", 40000) + "LAST_MARK"}
	seen := ""
	calls := 0
	m := &Manager{summarizeTest: func(_ context.Context, _ string, blocks []any, progress func()) (string, error) {
		calls++
		prompt := compactTestText(blocks)
		if len(prompt) > MaxText {
			t.Fatal("summary exceeded native input budget")
		}
		_, chunk, ok := strings.Cut(prompt, "Next chronological history segment:\n")
		if !ok {
			t.Fatal("missing segment")
		}
		chunk = strings.TrimSuffix(chunk, "\nReturn the consolidated checkpoint only.")
		seen += chunk
		progress()
		return "Checkpoint preserves FIRST_MARK MIDDLE_MARK LAST_MARK", nil
	}}
	items := []map[string]any{}
	for _, text := range segments {
		items = append(items, map[string]any{"role": "user", "content": "historical request"}, map[string]any{"role": "assistant", "content": text})
	}
	latest := map[string]any{"role": "user", "content": "Do not alter this latest request: CURRENT_MARK"}
	items = append(items, latest)
	expected, _ := promptText(items[:len(items)-1])
	progress := 0
	out, err := m.compactItems(t.Context(), request{Model: "agy/test", Items: items}, func() { progress++ })
	if err != nil {
		t.Fatal(err)
	}
	if seen != expected || calls < 3 || progress != calls {
		t.Fatal("old history was truncated or not fully summarized")
	}
	if out[len(out)-1]["content"] != latest["content"] {
		t.Fatal("latest user instruction changed")
	}
	if _, err = promptBlocks(out); err != nil {
		t.Fatal(err)
	}
}

func TestAGYCheckpointBoundToOwnerModelAndSecret(t *testing.T) {
	m := &Manager{cfg: RuntimeConfig{Secret: strings.Repeat("a", 64)}}
	token, err := m.sealCheckpoint("user:key-a", "agy/model", "remember exact fact")
	if err != nil {
		t.Fatal(err)
	}
	restarted := &Manager{cfg: m.cfg}
	text, err := restarted.openCheckpoint("user:key-a", "agy/model", token)
	if err != nil || text != "remember exact fact" {
		t.Fatal("checkpoint lost across restart")
	}
	for _, v := range []struct{ owner, model, token string }{{"user:key-b", "agy/model", token}, {"other:key-a", "agy/model", token}, {"user:key-a", "agy/other", token}, {"user:key-a", "agy/model", token + "!"}, {"user:key-a", "agy/model", "gAAAAA_openai_opaque"}} {
		if _, err = m.openCheckpoint(v.owner, v.model, v.token); err == nil {
			t.Fatal("foreign or tampered checkpoint accepted")
		}
	}
	other := &Manager{cfg: RuntimeConfig{Secret: strings.Repeat("b", 64)}}
	if _, err = other.openCheckpoint("user:key-a", "agy/model", token); err == nil {
		t.Fatal("checkpoint accepted with another service secret")
	}
}

func TestAGYCompactEndpointRoundTripAndPendingTools(t *testing.T) {
	images := 0
	m := &Manager{cfg: RuntimeConfig{Secret: strings.Repeat("s", 64)}, slot: make(chan struct{}, 1), sessions: map[string]*session{}, summarizeTest: func(_ context.Context, _ string, prompt []any, _ func()) (string, error) {
		for _, raw := range prompt {
			if v := raw.(map[string]string); v["type"] == "image" {
				if v["data"] != "YQ==" || v["mimeType"] != "image/png" {
					t.Fatal("compact endpoint changed image data")
				}
				images++
			}
		}
		return "Old task: preserve PROJECT_MARK", nil
	}}
	b, _ := json.Marshal(map[string]any{"model": "agy/test", "input": []map[string]any{{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "old task PROJECT_MARK"}, compactTestImage("YQ==")}}, {"role": "assistant", "content": strings.Repeat("large history ", 20000)}, {"role": "user", "content": "Continue CURRENT_MARK"}}})
	call := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/internal/compact", strings.NewReader(string(b)))
		r.Header.Set("Authorization", "Bearer "+m.cfg.Secret)
		r.Header.Set("X-Portal-Owner", "owner:key")
		w := httptest.NewRecorder()
		m.Handler().ServeHTTP(w, r)
		return w
	}
	w := call()
	if w.Code != 200 {
		t.Fatalf("%d: %s", w.Code, w.Body.String())
	}
	var compact struct {
		Object string           `json:"object"`
		Output []map[string]any `json:"output"`
	}
	if json.Unmarshal(w.Body.Bytes(), &compact) != nil || compact.Object != "response.compaction" || compact.Output[0]["type"] != "compaction" {
		t.Fatal("invalid compact response")
	}
	if images != 1 || strings.Contains(w.Body.String(), "image_url") {
		t.Fatal("old image was not summarized into the compact checkpoint")
	}
	r := request{Model: "agy/test", Items: compact.Output}
	if err := m.expandCheckpoints("owner:key", &r); err != nil {
		t.Fatal(err)
	}
	text, err := promptText(r.Items)
	if err != nil || !strings.Contains(text, "PROJECT_MARK") || !strings.Contains(text, "CURRENT_MARK") {
		t.Fatal("checkpoint replay lost task")
	}
	m.sessions["pending"] = &session{owner: "owner:key", running: true}
	if w := call(); w.Code != http.StatusConflict {
		t.Fatal("compaction discarded pending tools")
	}
}

func TestAGYCompactionRejectsUnrepresentableHistoryAndNoTruncation(t *testing.T) {
	large := strings.Repeat("x", MaxText+1)
	if _, _, err := splitCompactTail([]map[string]any{{"role": "user", "content": large}}); err == nil {
		t.Fatal("latest input was silently shortened")
	}
	if _, err := promptText([]map[string]any{{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,YQ=="}}}}); err == nil {
		t.Fatal("old image was silently dropped")
	}
	text := strings.Repeat("汉", 100000)
	part, rest := takeUTF8(text, compactChunk-1)
	if part+rest != text || len(part) > compactChunk-1 {
		t.Fatal("UTF-8 split lost bytes")
	}
	m := &Manager{summarizeTest: func(context.Context, string, []any, func()) (string, error) {
		return strings.Repeat("x", compactSummary+1), nil
	}}
	if _, err := m.summarizeHistory(t.Context(), "owner", "agy/test", []any{map[string]string{"type": "text", "text": "source"}}, nil); err == nil {
		t.Fatal("oversized checkpoint was silently truncated")
	}
}

func TestAGYCompactionCacheIsOwnerScopedAndBounded(t *testing.T) {
	calls := 0
	m := &Manager{summarizeTest: func(context.Context, string, []any, func()) (string, error) { calls++; return "checkpoint", nil }}
	for _, owner := range []string{"a:key", "a:key", "b:key"} {
		if _, err := m.summarizeHistory(t.Context(), owner, "agy/test", []any{map[string]string{"type": "text", "text": "same history"}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatal("summary cache crossed owners or failed to reuse")
	}
	for i := 0; i < 100; i++ {
		m.cacheCheckpoint(string(rune(i)), "value")
	}
	if len(m.checkpointCache) > 64 {
		t.Fatal("unbounded checkpoint cache")
	}
}

func TestAGYCompactionProgressUsesInformationalResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusProcessing)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"response.compaction","output":[]}`))
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	resp, err := c.Request(WithModelProgress(t.Context(), func() { count++ }), "POST", "/internal/compact", "owner:key", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || count != 1 {
		t.Fatal("actual summary progress did not reach gateway idle watcher")
	}
	var body map[string]any
	if json.NewDecoder(resp.Body).Decode(&body) != nil || body["object"] != "response.compaction" {
		t.Fatal("progress corrupted compact JSON")
	}
}
