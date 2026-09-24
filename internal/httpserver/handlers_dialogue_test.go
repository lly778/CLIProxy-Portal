package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/config"
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/gateway"
	"cliproxy-portal/internal/store"
)

func TestLegacyStandaloneDialogueIsNotDownloadable(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	vault, err := gateway.NewVault(t.TempDir(), []byte(strings.Repeat("x", 32)))
	if err != nil {
		t.Fatal(err)
	}
	const id = "0123456789abcdef0123456789abcdef"
	for kind, text := range map[string]string{"request": "用户：\n你好", "response": "助手：\n你好！"} {
		writer, err := vault.Create(id, kind)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = writer.Write([]byte(text))
		if _, err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SaveGatewayCapture(context.Background(), store.GatewayCapture{ID: id, UserID: "owner", APIKeyHash: "hash", CreatedAt: time.Now().UTC(), Method: "POST", Path: "/v1/responses", RequestedModel: "gpt-test", StatusCode: 200}); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st, CaptureVault: vault, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, test := range []struct {
		name, path, userID string
		wantStatus         int
	}{
		{"owner", "/logs/dialogue/" + id, "owner", http.StatusNotFound},
		{"other user", "/logs/dialogue/" + id, "other", http.StatusNotFound},
		{"admin", "/admin/logs/dialogue/" + id, "admin", http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			user := domain.User{ID: test.userID, Name: test.userID}
			if test.userID == "admin" {
				user.Role = domain.RoleAdmin
			}
			r := httptest.NewRequest(http.MethodGet, test.path, nil)
			r.SetPathValue("id", id)
			r = r.WithContext(context.WithValue(r.Context(), userKey, user))
			w := httptest.NewRecorder()
			s.downloadDialogue(w, r)
			if w.Code != test.wantStatus {
				t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "你好") {
				t.Fatalf("legacy content leaked: %q", w.Body.String())
			}
		})
	}
}

func TestRequestViewLinksOnlyMatchingCapture(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	const id = "fedcba9876543210fedcba9876543210"
	if err := st.SaveGatewayCapture(context.Background(), store.GatewayCapture{ID: id, UserID: "owner", APIKeyHash: "hash-a", CPARequestID: "1234abcd", CreatedAt: now, Method: "POST", Path: "/v1/responses", StatusCode: 200, RequestContentType: gateway.StructuredCaptureContentType}); err != nil {
		t.Fatal(err)
	}
	vault, err := gateway.NewVault(t.TempDir(), []byte(strings.Repeat("x", 32)))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st, CaptureVault: vault, Cfg: config.Config{TimeZone: time.UTC}}
	event := cpamp.EventRow{RequestID: "1234abcd", APIKeyHash: "hash-a", TimestampMS: now.UnixMilli()}
	if got := s.requestView(event).CaptureID; got != id {
		t.Fatalf("matching capture id = %q", got)
	}
	event.APIKeyHash = "hash-b"
	if got := s.requestView(event).CaptureID; got != "" {
		t.Fatalf("other key gained link %q", got)
	}
	event.APIKeyHash = "hash-a"
	const oldID = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	if err := st.SaveGatewayCapture(context.Background(), store.GatewayCapture{ID: oldID, UserID: "owner", APIKeyHash: "hash-a", CPARequestID: "1234abcd", CreatedAt: now.Add(time.Second), Method: "POST", Path: "/v1/responses", StatusCode: 200}); err != nil {
		t.Fatal(err)
	}
	if got := s.requestView(event).CaptureID; got != "" {
		t.Fatalf("legacy capture gained a download link %q", got)
	}
	oldAt := now.Add(-8 * 24 * time.Hour)
	const retainedID = "cccccccccccccccccccccccccccccccc"
	if err := st.SaveGatewayCapture(context.Background(), store.GatewayCapture{ID: retainedID, UserID: "owner", APIKeyHash: "hash-a", CPARequestID: "old12345", CreatedAt: oldAt, Method: "POST", Path: "/v1/responses", StatusCode: 200, RequestContentType: gateway.StructuredCaptureContentType}); err != nil {
		t.Fatal(err)
	}
	event.RequestID, event.TimestampMS = "old12345", oldAt.UnixMilli()
	if got := s.requestView(event).CaptureID; got != retainedID {
		t.Fatalf("old matching capture id = %q, want %q", got, retainedID)
	}
}

func TestLegacySharedDialogueIsNotDownloadable(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	vault, err := gateway.NewVault(t.TempDir(), []byte(strings.Repeat("x", 32)))
	if err != nil {
		t.Fatal(err)
	}
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	messageID, _, err := vault.SaveSharedMessage("owner", "用户", "第一轮消息")
	if err != nil {
		t.Fatal(err)
	}
	replyID, _, err := vault.SaveSharedMessage("owner", "助手", "本轮回复")
	if err != nil {
		t.Fatal(err)
	}
	for kind, content := range map[string]string{"request": "", "response": ""} {
		writer, err := vault.Create(id, kind)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = writer.Write([]byte(content))
		if _, err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	row := store.GatewayCapture{ID: id, UserID: "owner", APIKeyHash: "hash", CreatedAt: time.Now().UTC(), Method: "POST", Path: "/v1/responses", StatusCode: 200}
	if err := st.SaveGatewayCaptureWithMessages(ctx, row, []store.GatewayCaptureMessage{{Side: "request", ID: messageID, Role: "用户"}, {Side: "response", ID: replyID, Role: "助手"}}); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st, CaptureVault: vault, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r := httptest.NewRequest(http.MethodGet, "/logs/dialogue/"+id, nil)
	r.SetPathValue("id", id)
	r = r.WithContext(context.WithValue(r.Context(), userKey, domain.User{ID: "owner"}))
	w := httptest.NewRecorder()
	s.downloadDialogue(w, r)
	if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "第一轮消息") {
		t.Fatalf("legacy shared dialogue download = %d %q", w.Code, w.Body.String())
	}
}

func TestStructuredInteractionDownloadPreservesToolsAndOwnerBoundary(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	vault, err := gateway.NewVault(t.TempDir(), []byte(strings.Repeat("x", 32)))
	if err != nil {
		t.Fatal(err)
	}
	const id = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for _, side := range []string{"request", "response"} {
		writer, err := vault.Create(id, side)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	requestEvents := []gateway.RecordEvent{
		{Type: "message", Role: "user", Content: "检查状态"},
		{Type: "tool_call", Role: "assistant", ToolName: "shell", ToolCallID: "call-1", Arguments: `{"command":"pwd"}`},
		{Type: "tool_result", Role: "tool", ToolName: "shell", ToolCallID: "call-1", Result: "/work"},
	}
	responseEvents := []gateway.RecordEvent{{Type: "message", Role: "assistant", Content: "完成"}}
	var references []store.GatewayCaptureMessage
	for _, side := range []struct {
		name   string
		events []gateway.RecordEvent
	}{{"request", requestEvents}, {"response", responseEvents}} {
		for _, event := range side.events {
			encoded, _ := json.Marshal(event)
			messageID, _, err := vault.SaveSharedMessage("owner", gateway.StructuredMessageRole, string(encoded))
			if err != nil {
				t.Fatal(err)
			}
			references = append(references, store.GatewayCaptureMessage{Side: side.name, ID: messageID, Role: gateway.StructuredMessageRole})
		}
	}
	row := store.GatewayCapture{ID: id, UserID: "owner", APIKeyHash: "hash", CreatedAt: time.Now().UTC().Add(-8 * 24 * time.Hour), Method: "POST", Path: "/v1/responses", RequestedModel: "gpt-test", StatusCode: 200, RequestContentType: gateway.StructuredCaptureContentType, ResponseContentType: gateway.StructuredCaptureContentType}
	if err := st.SaveGatewayCaptureWithMessages(ctx, row, references); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st, CaptureVault: vault, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, tc := range []struct {
		name, path, userID string
		status             int
	}{
		{"owner json", "/logs/dialogue/" + id, "owner", http.StatusOK},
		{"owner text", "/logs/dialogue/" + id + "?format=txt", "owner", http.StatusOK},
		{"other user", "/logs/dialogue/" + id, "other", http.StatusNotFound},
		{"admin", "/admin/logs/dialogue/" + id, "admin", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := domain.User{ID: tc.userID, Name: tc.userID}
			if tc.userID == "admin" {
				user.Role = domain.RoleAdmin
			}
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			r.SetPathValue("id", id)
			r = r.WithContext(context.WithValue(r.Context(), userKey, user))
			w := httptest.NewRecorder()
			s.downloadDialogue(w, r)
			if w.Code != tc.status {
				t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
			}
			if w.Code != http.StatusOK {
				return
			}
			if !strings.Contains(tc.path, "format=txt") {
				var exported struct {
					Format   string                `json:"format"`
					Request  []gateway.RecordEvent `json:"request"`
					Response []gateway.RecordEvent `json:"response"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &exported); err != nil || exported.Format != "cliproxy.interaction.v1" || len(exported.Request) != 3 || len(exported.Response) != 1 {
					t.Fatalf("JSON export = %+v, %v", exported, err)
				}
				if !strings.Contains(w.Header().Get("Content-Disposition"), ".json") {
					t.Fatal("JSON export did not use a JSON filename")
				}
			} else if !strings.Contains(w.Body.String(), "助手调用工具：shell") || !strings.Contains(w.Body.String(), "工具结果：shell") || !strings.Contains(w.Body.String(), "检查状态") || !strings.Contains(w.Body.String(), "完成") {
				t.Fatalf("text export = %q", w.Body.String())
			}
		})
	}
}
