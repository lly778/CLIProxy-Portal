package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"cliproxy-portal/internal/cpamp"
)

const oldIdentity = "You are Codex, a coding agent based on GPT-5."
const modernIdentity = "You are Codex, an agent based on GPT-5."

func TestGPT5IdentityVariantsAndBoundaries(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"old", oldIdentity, "You are Codex, a coding agent."},
		{"modern", modernIdentity, "You are Codex, an agent."},
		{"different-role", "You are Codex, an assistant based on GPT-5.\nKeep all rules.", "You are Codex, an assistant.\nKeep all rules."},
		{"different-agent", "You are Orion, an AI coding assistant based on GPT-5.", "You are Orion, an AI coding assistant."},
		{"first-person", "I am a coding assistant based on GPT-5.", "I am a coding assistant."},
		{"contraction", "You're a coding assistant based on GPT-5.", "You're a coding assistant."},
		{"curly-contraction", "You’re an assistant based on GPT-5.", "You’re an assistant."},
		{"third-person", "This assistant is a coding agent based on GPT-5.", "This assistant is a coding agent."},
		{"leading-spaces", " \t\r\nYou are an assistant based on GPT-5.", " \t\r\nYou are an assistant."},
		{"case-and-spaces", "YOU ARE an assistant  BASED\tON   gpt-5.", "YOU ARE an assistant."},
		{"wrapped-introduction", "You\nare an assistant\r\n based\ton\nGPT-5.\r\nKeep rules.", "You\nare an assistant.\r\nKeep rules."},
		{"unicode-spaces", "You\u00a0are an assistant\u00a0based\u00a0on\u00a0GPT-5.", "You\u00a0are an assistant."},
		{"quoted-name", "You are \"Codex\", an assistant based on GPT-5.", "You are \"Codex\", an assistant."},
		{"agent-version", "You are Codex v0.159.2 based on GPT-5.", "You are Codex v0.159.2."},
		{"no-period", "You are an assistant based on GPT-5", "You are an assistant"},
		{"punctuation", "You are an assistant based on GPT-5! Keep rules.", "You are an assistant! Keep rules."},
		{"other-version", "You are an assistant based on GPT-5.1.", "You are an assistant based on GPT-5.1."},
		{"other-model", "You are an assistant based on GPT-5-mini.", "You are an assistant based on GPT-5-mini."},
		{"longer-identifier", "You are an assistant based on GPT-50.", "You are an assistant based on GPT-50."},
		{"later-sentence", "You are an editor. Explain based on GPT-5 to users.", "You are an editor. Explain based on GPT-5 to users."},
		{"later-paragraph", "You are an editor\n\nExplain based on GPT-5 to users.", "You are an editor\n\nExplain based on GPT-5 to users."},
		{"quoted-example", "You are an editor quoting \"an assistant based on GPT-5\".", "You are an editor quoting \"an assistant based on GPT-5\"."},
		{"single-quoted-example", "You are an editor quoting 'an assistant based on GPT-5'.", "You are an editor quoting 'an assistant based on GPT-5'."},
		{"curly-quoted-example", "You are an editor quoting “an assistant based on GPT-5”.", "You are an editor quoting “an assistant based on GPT-5”."},
		{"non-identity", "This instruction is based on GPT-5.", "This instruction is based on GPT-5."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := withoutGPT5Identity(tc.input)
			if got != tc.want || changed != (tc.input != tc.want) {
				t.Fatalf("got %q, changed=%v; want %q", got, changed, tc.want)
			}
		})
	}
}

type identityCPAMP struct {
	cpamp.API
	fail bool
}

func (f *identityCPAMP) ListAuthFiles(context.Context) ([]cpamp.AuthFile, error) {
	if f.fail {
		return nil, errors.New("unavailable")
	}
	return []cpamp.AuthFile{{Provider: "codex"}, {Provider: "antigravity"}}, nil
}
func (*identityCPAMP) ListOAuthModelDefinitions(_ context.Context, channel string) ([]cpamp.OAuthModelDefinition, error) {
	if channel == "codex" {
		return []cpamp.OAuthModelDefinition{{ID: "gpt-native"}, {ID: "shared"}}, nil
	}
	return []cpamp.OAuthModelDefinition{{ID: "gemini-real"}, {ID: "shared"}}, nil
}
func (*identityCPAMP) ListOAuthExcludedModels(context.Context, string) ([]string, error) {
	return nil, nil
}
func (*identityCPAMP) ListOAuthModelAliases(_ context.Context, channel string) ([]cpamp.OAuthModelAlias, error) {
	if channel == "antigravity" {
		return []cpamp.OAuthModelAlias{{Name: "gemini-real", Alias: "gpt-alias", Fork: true}}, nil
	}
	return nil, nil
}

func TestIdentityPayloadRewritesOnlyInstructionContent(t *testing.T) {
	for _, tc := range []struct{ name, path, body, expected, model string }{
		{"responses", "/v1/responses", `{"model":"gemini-real","instructions":"` + oldIdentity + `\nKeep based on GPT-5 in the rest.","input":"` + oldIdentity + `","tools":[{"name":"based on GPT-5"}],"opaque":9007199254740993}`, "You are Codex, a coding agent.", "gemini-real"},
		{"title", "/v1/responses", `{"model":"gpt-alias","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"` + modernIdentity + `\nTitle rules."}]},{"role":"user","content":"` + modernIdentity + `"}]}`, "You are Codex, an agent.", "gpt-alias"},
		{"chat", "/v1/chat/completions", `{"model":"gemini-real","messages":[{"role":"system","content":"` + oldIdentity + `"},{"role":"user","content":"` + oldIdentity + `"},{"role":"tool","content":"` + oldIdentity + `"}]}`, "You are Codex, a coding agent.", "gemini-real"},
		{"variant-roles", "/v1/chat/completions", `{"model":"gemini-real","messages":[{"role":"developer","content":"You are Orion, an assistant based  on\nGPT-5."},{"role":"user","content":"You are Orion, an assistant based  on\nGPT-5."},{"role":"tool","content":"You are Orion, an assistant based  on\nGPT-5."}]}`, "You are Orion, an assistant.", "gemini-real"},
		{"anthropic", "/v1/messages", `{"model":"gemini-real","system":[{"type":"text","text":"` + modernIdentity + `","cache_control":{"type":"ephemeral"}}]}`, "You are Codex, an agent.", "gemini-real"},
		{"native-gemini", "/v1beta/models/gemini-real:streamGenerateContent", `{"systemInstruction":{"parts":[{"text":"` + oldIdentity + `"}]},"contents":[{"role":"user","parts":[{"text":"` + oldIdentity + `"}]}]}`, "You are Codex, a coding agent.", "gemini-real"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, model, changed := rewriteIdentityPayloadForTest(t, []byte(tc.body), tc.path)
			if !changed || model != tc.model || !bytes.Contains(got, []byte(tc.expected)) {
				t.Fatalf("rewrite = %s, %q, %v", got, model, changed)
			}
			var before, after map[string]json.RawMessage
			_ = json.Unmarshal([]byte(tc.body), &before)
			_ = json.Unmarshal(got, &after)
			for _, field := range []string{"tools", "opaque", "contents"} {
				if !bytes.Equal(before[field], after[field]) {
					t.Fatalf("unrelated %s changed: %s", field, after[field])
				}
			}
			if tc.name == "responses" && (!bytes.Contains(after["instructions"], []byte("Keep based on GPT-5 in the rest.")) || !bytes.Equal(before["input"], after["input"])) {
				t.Fatal("rest of instruction or user input was changed")
			}
			for _, key := range []string{"input", "messages"} {
				var b, a []json.RawMessage
				if json.Unmarshal(before[key], &b) == nil && json.Unmarshal(after[key], &a) == nil {
					for i, item := range b {
						var role struct{ Role string }
						_ = json.Unmarshal(item, &role)
						if (role.Role == "user" || role.Role == "tool") && !bytes.Equal(item, a[i]) {
							t.Fatalf("%s message changed", role.Role)
						}
					}
				}
			}
		})
	}
	for _, raw := range []string{`not-json`, `{"input":"` + oldIdentity + `"}`, `{"instructions":"Quoted: ` + oldIdentity + `"}`, `{"instructions":"You are Codex, an agent based on GPT-6."}`, `{"input":[{"type":"function_call_output","role":"developer","content":"` + oldIdentity + `"}]}`} {
		got, _, changed := rewriteIdentityPayloadForTest(t, []byte(raw), "/v1/responses")
		if changed || string(got) != raw {
			t.Fatalf("non-target payload was changed: %s", got)
		}
	}
}

func TestGatewayIdentityCompatibilityRoutingCompressionAndPassthrough(t *testing.T) {
	for _, tc := range []struct {
		name, model                            string
		enabled, compressed, fail, wantChanged bool
	}{
		{name: "disabled", model: "gemini-real"},
		{name: "native-model", model: "gemini-real", enabled: true, wantChanged: true},
		{name: "alias-with-gpt-name", model: "gpt-alias", enabled: true, wantChanged: true},
		{name: "gzip", model: "gemini-real", enabled: true, compressed: true, wantChanged: true},
		{name: "codex", model: "gpt-native", enabled: true},
		{name: "shared-codex", model: "shared", enabled: true},
		{name: "unknown", model: "unknown", enabled: true, compressed: true},
		{name: "failed-discovery", model: "gemini-real", enabled: true, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := []byte(`{"model":"` + tc.model + `","instructions":"` + oldIdentity + `\nRemaining rules.","reasoning":{"effort":"medium"},"opaque":9007199254740993}`)
			if tc.compressed {
				wire = gzipForTest(t, wire)
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if r.Header.Get("X-Client-Extension") != "unchanged" || r.Header.Get("Authorization") != "Bearer capture-test-key" {
					t.Error("unrelated headers were changed")
				}
				if tc.wantChanged {
					if r.Header.Get("Content-Encoding") != "" || r.Header.Get("Content-MD5") != "" || r.ContentLength != int64(len(body)) || r.Header.Get("Content-Length") != strconv.Itoa(len(body)) {
						t.Error("modified body metadata is inconsistent")
					}
					var got struct{ Instructions string }
					if json.Unmarshal(body, &got) != nil || got.Instructions != "You are Codex, a coding agent.\nRemaining rules." || !bytes.Contains(body, []byte("9007199254740993")) {
						t.Fatalf("unexpected effective request: %s", body)
					}
				} else if !bytes.Equal(body, wire) || r.Header.Get("Content-MD5") != "original-digest" || (tc.compressed && r.Header.Get("Content-Encoding") != "gzip") {
					t.Error("non-target request did not pass through unchanged")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: response.completed\ndata: {}\n\n")
			}))
			defer upstream.Close()
			g, st, key := captureGatewayForTest(t, upstream.URL)
			g.keys.CPAMP = &identityCPAMP{fail: tc.fail}
			if tc.enabled {
				if err := st.SetCodexIdentityCompatibility(t.Context(), "antigravity", true); err != nil {
					t.Fatal(err)
				}
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(wire))
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("X-Client-Extension", "unchanged")
			req.Header.Set("Content-MD5", "original-digest")
			if tc.compressed {
				req.Header.Set("Content-Encoding", "gzip")
			}
			response := httptest.NewRecorder()
			g.Handler().ServeHTTP(response, req)
			if response.Code != 200 || response.Body.String() != "event: response.completed\ndata: {}\n\n" {
				t.Fatalf("response changed: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestGatewayIdentityCompatibilityHandlesLargeAndCompressedBodies(t *testing.T) {
	for _, tc := range []struct {
		name          string
		size          int
		gzip, chunked bool
	}{
		{"plain-over-2MiB", 3 << 20, false, false},
		{"chunked-over-2MiB", 3 << 20, false, true},
		{"gzip-over-capture-limit", 17 << 20, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plain := []byte(`{"model":"gemini-real","instructions":"` + oldIdentity + `","input":"` + strings.Repeat("a", tc.size) + `"}`)
			want := bytes.Replace(plain, []byte(" based on GPT-5"), nil, 1)
			wire := plain
			if tc.gzip {
				wire = gzipForTest(t, plain)
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, _ := io.ReadAll(r.Body)
				if !bytes.Equal(got, want) || r.ContentLength != int64(len(want)) || r.Header.Get("Content-Encoding") != "" {
					t.Error("large request was skipped, truncated, or had inconsistent headers")
				}
				w.WriteHeader(204)
			}))
			defer upstream.Close()
			g, st, _ := captureGatewayForTest(t, upstream.URL)
			g.keys.CPAMP = &identityCPAMP{}
			if err := st.SetCodexIdentityCompatibility(t.Context(), "antigravity", true); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(wire))
			req.Header.Set("Authorization", "Bearer manual-test-key")
			if tc.chunked {
				req.ContentLength = -1
			}
			if tc.gzip {
				req.Header.Set("Content-Encoding", "gzip")
			}
			response := httptest.NewRecorder()
			g.Handler().ServeHTTP(response, req)
			if response.Code != 204 {
				t.Fatalf("status %d", response.Code)
			}
			files, err := os.ReadDir(filepath.Join(g.vault.dir, ".identity-buffer"))
			if err != nil || len(files) != 0 {
				t.Fatalf("temporary request files not removed: %v %v", files, err)
			}
		})
	}
}
