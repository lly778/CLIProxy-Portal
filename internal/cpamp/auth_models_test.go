package cpamp

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAuthAvailabilityAndRegisteredModels(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+testAdminKey {
			t.Error("unexpected management request")
		}
		switch r.URL.Path {
		case pathAuthFiles:
			_, _ = w.Write([]byte(`{"files":[
			{"name":"account.json","id":"runtime","provider":"antigravity","auth_index":"opaque","status":"active","disabled":false,"unavailable":false,"cooldowns":[],"access_token":"secret"},
			{"name":"legacy.json","provider":"antigravity","status":"active"},
			{"name":"unknown.json","provider":"antigravity","status":"active","disabled":false,"unavailable":false,"cooldowns":null},
			{"name":"null.json","provider":"antigravity","status":"active","disabled":null,"unavailable":null,"cooldowns":[]},
			{"name":"cooling.json","provider":"antigravity","status":"active","disabled":false,"unavailable":false,"cooldowns":[{"scope":"model","model_key":"gemini-model","retry_at":"2026-10-04T10:06:12Z","last_error":"private"}]}
			]}`))
		case pathAuthFiles + "/models":
			if r.URL.Query().Get("name") != "runtime" || r.URL.Query().Get("auth_index") != "opaque" {
				t.Error("identity missing")
			}
			_, _ = w.Write([]byte(`{"models":[{"id":"gemini-model","owned_by":"antigravity","access_token":"secret"}]}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	files, err := client.ListAuthFiles(t.Context())
	if err != nil || len(files) != 5 {
		t.Fatalf("files %#v, %v", files, err)
	}
	for i, want := range []bool{true, false, false, false, true} {
		if files[i].AvailabilityKnown != want {
			t.Errorf("file %d known=%v", i, files[i].AvailabilityKnown)
		}
	}
	models, err := client.ListAuthFileModels(t.Context(), files[0])
	if err != nil || len(models) != 1 || models[0].ID != "gemini-model" {
		t.Fatalf("models %#v, %v", models, err)
	}
	encoded, _ := json.Marshal(struct {
		Files  []AuthFile
		Models []Model
	}{files, models})
	if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "private") {
		t.Fatal("secret/error body retained in metadata")
	}
	if _, err := client.ListAuthFileModels(t.Context(), AuthFile{Name: "account.json"}); err == nil {
		t.Fatal("missing identity accepted")
	}
}
