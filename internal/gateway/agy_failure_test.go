package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/agy"
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/resourcecontrol"
	"cliproxy-portal/internal/store"
)

func TestAGYGatewayRecordsSpecificFailureForJSONAndSSE(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, now := context.Background(), time.Now()
	const raw = "sk-failure-test"
	hash := cpamp.HashAPIKey(raw)
	u := domain.User{ID: "failure-user", Phone: "13800138001", Name: "test", PasswordHash: "unused", Role: domain.RoleUser, Status: domain.StatusApproved, CreatedAt: now, UpdatedAt: now}
	if err = st.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateKey(ctx, domain.APIKey{ID: "failure-key", UserID: u.ID, Hash: hash, LastFour: "test", Status: "active", IssuedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err = st.SaveAGYSettings(ctx, agy.Settings{Enabled: true, Models: []agy.Model{{ID: "gemini"}}, Rules: []agy.ModelRule{{ID: "gemini", Enabled: true}}}, 0); err != nil {
		t.Fatal(err)
	}
	vault, err := NewVault(t.TempDir(), []byte(strings.Repeat("v", 32)))
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []struct {
		code, message string
		status        int
	}{
		{"upstream_capacity_exhausted", "No capacity available", 502},
		{"session_busy", "上一笔请求", 409},
		{"compaction_tools_pending", "交付", 409},
		{"missing_tool_result", "回传完整", 400},
	} {
		for _, stream := range []bool{false, true} {
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Portal-Followup-Limit") != "0" {
					t.Error("gateway did not reserve tool-result admission capacity")
				}
				encoded, _ := json.Marshal(map[string]any{"status": "failed", "error": map[string]string{"code": failure.code}})
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\"}}\n\n"))
					_, _ = w.Write([]byte("data: {\"type\":\"response.failed\",\"response\":" + string(encoded) + "}\n\n"))
				} else {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(failure.status)
					_, _ = w.Write(encoded)
				}
			}))
			client, _ := agy.NewClient(worker.URL, strings.Repeat("a", 64))
			g := &Gateway{store: st, AGY: client, vault: vault}
			policy := resourcecontrol.DefaultPolicy()
			policy.Mode = "enforce"
			g.Resources = resourcecontrol.NewController(policy, "")
			for _, prior := range []string{"", "idle_timeout", "client_write_failed"} {
				m := &requestMeasurement{row: store.GatewayRequestTiming{StartedAt: now, FailureReason: prior}}
				m.requestRead()
				r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"agy/gemini","input":"test"}`))
				r.Header.Set("Authorization", "Bearer "+raw)
				// Previous transport failures can arise while copying a successful SSE
				// response; direct HTTP errors have their own status set before copying.
				if prior != "" && !stream {
					continue
				}
				w := httptest.NewRecorder()
				if !g.routeAGY(w, r, m, nil) {
					t.Fatal("AGY request not handled")
				}
				if prior != "" {
					if m.row.FailureReason != prior {
						t.Fatalf("transport failure overwritten: %s", m.row.FailureReason)
					}
					continue
				}
				var summary struct{ Code, Message string }
				if json.Unmarshal([]byte(m.row.FailureReason), &summary) != nil || summary.Code != failure.code || !strings.Contains(summary.Message, failure.message) || m.row.StatusCode != failure.status {
					t.Fatalf("stream=%v lost specific reason: %+v", stream, m.row)
				}
			}
			worker.Close()
		}
	}
}
