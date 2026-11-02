package cpamp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConfiguredReasoningUsesProviderKeyAndMapsAliases(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer provider-secret" {
			t.Errorf("wrong metadata endpoint or credential")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"source","effort":{"supported_levels":["low","high","max","unknown"],"default_level":"high"}}]}`))
	}))
	defer upstream.Close()
	client, err := New("https://manager.invalid", "manager-secret")
	if err != nil {
		t.Fatal(err)
	}
	config := []byte("openai-compatibility:\n  - base-url: " + upstream.URL + "/v1\n    prefix: team\n    api-key-entries:\n      - api-key: provider-secret\n    models:\n      - name: source\n        alias: alias\n")
	models := client.ListConfiguredModelReasoning(t.Context(), config)
	seen := map[string]bool{}
	for _, model := range models {
		if strings.Join(model.Levels, ",") != "low,high,max" {
			t.Fatalf("wrong declared levels: %+v", model)
		}
		seen[model.ID] = true
	}
	for _, id := range []string{"source", "alias", "team/source", "team/alias"} {
		if !seen[id] {
			t.Fatalf("missing %s", id)
		}
	}
}

func TestConfiguredReasoningDoesNotFollowRedirects(t *testing.T) {
	hits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer target.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer upstream.Close()
	client, err := New("https://manager.invalid", "manager-secret")
	if err != nil {
		t.Fatal(err)
	}
	config := []byte("openai-compatibility:\n  - base-url: " + upstream.URL + "\n    api-key-entries:\n      - api-key: provider-secret\n")
	if got := client.ListConfiguredModelReasoning(t.Context(), config); len(got) != 0 || hits != 0 {
		t.Fatal("redirect followed or untrusted metadata returned")
	}
}
