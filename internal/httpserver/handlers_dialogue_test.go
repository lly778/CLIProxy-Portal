package httpserver

import (
	"context"
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

func TestDialogueDownloadEnforcesOwnerAndNeverReturnsWireData(t *testing.T) {
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
		{"owner", "/logs/dialogue/" + id, "owner", http.StatusOK},
		{"other user", "/logs/dialogue/" + id, "other", http.StatusNotFound},
		{"admin", "/admin/logs/dialogue/" + id, "admin", http.StatusOK},
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
			if w.Code == http.StatusOK && (!strings.Contains(w.Body.String(), "用户：\n你好") || !strings.Contains(w.Body.String(), "助手：\n你好！")) {
				t.Fatalf("download body = %q", w.Body.String())
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
	if err := st.SaveGatewayCapture(context.Background(), store.GatewayCapture{ID: id, UserID: "owner", APIKeyHash: "hash-a", CPARequestID: "1234abcd", CreatedAt: now, Method: "POST", Path: "/v1/responses", StatusCode: 200}); err != nil {
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
}

func TestDialogueDownloadReconstructsSharedRequestMessages(t *testing.T) {
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
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "用户：\n第一轮消息") || !strings.Contains(w.Body.String(), "助手：\n本轮回复") {
		t.Fatalf("shared dialogue download = %d %q", w.Code, w.Body.String())
	}
}
