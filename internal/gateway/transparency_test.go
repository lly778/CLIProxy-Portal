package gateway

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/store"
)

func captureGatewayForTest(t *testing.T, upstream string) (*Gateway, *store.Store, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	user := domain.User{ID: "capture-owner", Phone: "13800138001", Name: "Capture owner", PasswordHash: "hash", Role: domain.RoleUser, Status: domain.StatusApproved, CreatedAt: now, UpdatedAt: now}
	if err := st.CreateUser(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	const key = "capture-test-key"
	if err := st.CreateKey(context.Background(), domain.APIKey{ID: "capture-key", UserID: user.ID, Hash: cpamp.HashAPIKey(key), LastFour: "-key", Status: "active", IssuedAt: now}); err != nil {
		t.Fatal(err)
	}
	vault, err := NewVault(t.TempDir(), []byte(strings.Repeat("s", 32)))
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(upstream, service.NewKeys(st, &gatewayCPAMP{}), st, vault, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := g.WaitCaptures(ctx); err != nil {
			t.Error(err)
		}
		g.transport.CloseIdleConnections()
		_ = st.Close()
	})
	return g, st, key
}

func gzipForTest(t *testing.T, raw []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func TestCapturedDialoguePreservesHeadersAndCompressedWireBodies(t *testing.T) {
	requestBody := gzipForTest(t, []byte(`{"model":"gpt-6-sol","messages":[{"role":"user","content":"hello"}]}`))
	responseBody := gzipForTest(t, []byte(`{"choices":[{"message":{"role":"assistant","content":"world"}}]}`))
	wantHeaders := map[string]string{
		"Cookie": "client-cookie=1", "Accept-Encoding": "gzip", "Content-Encoding": "gzip",
		"Forwarded": "for=192.0.2.1;proto=https", "X-Forwarded-For": "192.0.2.1",
		"X-Forwarded-Host": "original.example", "X-Forwarded-Proto": "https",
		"If-None-Match": "dialogue-etag", "If-Modified-Since": "Tue, 29 Sep 2026 00:00:00 GMT",
		"Content-Type": "application/json",
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "original.example" || r.URL.RawQuery != "trace=a%2Fb&extra=x%3By" {
			t.Errorf("request target changed: host=%q query=%q", r.Host, r.URL.RawQuery)
		}
		for name, want := range wantHeaders {
			if got := r.Header.Get(name); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(body, requestBody) {
			t.Errorf("request wire body changed: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Set-Cookie", "upstream-cookie=1")
		w.Header().Set("ETag", "upstream-etag")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(responseBody)
	}))
	defer upstream.Close()
	g, st, key := captureGatewayForTest(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "http://original.example/v1/chat/completions?trace=a%2Fb&extra=x%3By", bytes.NewReader(requestBody))
	for name, value := range wantHeaders {
		req.Header.Set(name, value)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	response := httptest.NewRecorder()
	g.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusBadRequest || !bytes.Equal(response.Body.Bytes(), responseBody) {
		t.Fatalf("response status/body changed: %d %q", response.Code, response.Body.Bytes())
	}
	for name, want := range map[string]string{"Content-Encoding": "gzip", "Set-Cookie": "upstream-cookie=1", "ETag": "upstream-etag"} {
		if got := response.Header().Get(name); got != want {
			t.Errorf("response %s = %q, want %q", name, got, want)
		}
	}
	if err := g.WaitCaptures(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListGatewayCaptures(context.Background(), "capture-owner", 100)
	if err != nil || len(rows) != 1 || rows[0].RequestedModel != "gpt-6-sol" || rows[0].StatusCode != http.StatusBadRequest {
		t.Fatalf("compressed dialogue not captured: %+v, %v", rows, err)
	}
	for _, side := range []string{"request", "response"} {
		parts, err := st.GatewayCaptureMessages(context.Background(), rows[0].ID, side)
		if err != nil || len(parts) != 1 {
			t.Fatalf("%s capture missing: %+v %v", side, parts, err)
		}
	}
}

func TestCapturePersistenceDoesNotHoldResponseOpen(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	}))
	defer upstream.Close()
	g, _, key := captureGatewayForTest(t, upstream.URL)
	server := httptest.NewServer(g.Handler())
	defer server.Close()
	// Simulate capture storage waiting behind another record, while the model
	// response must finish without waiting for that storage lock.
	g.vault.LockSharedMessages()
	locked := true
	defer func() {
		if locked {
			g.vault.UnlockSharedMessages()
		}
	}()
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-6-sol","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || !bytes.Contains(body, []byte("done")) {
		t.Fatalf("record storage blocked response completion: %q %v", body, err)
	}
	g.vault.UnlockSharedMessages()
	locked = false
}

func TestCapturedStreamFlushesBeforeUpstreamCompletes(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Trailer", "X-Stream-Result")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		w.Header().Set("X-Stream-Result", "complete")
	}))
	defer upstream.Close()
	g, _, key := captureGatewayForTest(t, upstream.URL)
	server := httptest.NewServer(g.Handler())
	defer server.Close()
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-6-sol","messages":[{"role":"user","content":"hello"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+key)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	first, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(first, "first") {
		t.Fatalf("first SSE frame was buffered: %q %v", first, err)
	}
	close(release)
	rest, err := io.ReadAll(reader)
	if err != nil || !bytes.Contains(rest, []byte("[DONE]")) || resp.Trailer.Get("X-Stream-Result") != "complete" {
		t.Fatalf("stream or trailers changed: %q %v %+v", rest, err, resp.Trailer)
	}
}

func TestDialogueDoesNotAddCompressionOrForwardingHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range []string{"Accept-Encoding", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
			if value := r.Header.Get(name); value != "" {
				t.Errorf("gateway added %s: %q", name, value)
			}
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	g, _, key := captureGatewayForTest(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-6-sol","input":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	response := httptest.NewRecorder()
	g.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusNoContent {
		t.Fatalf("response status changed: %d", response.Code)
	}
}

func TestGatewayDelegatesPathsAndAuthenticationToCPA(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer client-key" {
			t.Error("client authorization changed")
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("CPA owns this route: " + r.URL.RequestURI()))
	}))
	defer upstream.Close()
	g, _, _ := captureGatewayForTest(t, upstream.URL)
	for _, path := range []string{"/", "/v0/management/config", "/v0/resource/plugins/example", "/future/api?value=a%2Fb"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer client-key")
		resp := httptest.NewRecorder()
		g.Handler().ServeHTTP(resp, req)
		if resp.Code != http.StatusUnauthorized || resp.Body.String() != "CPA owns this route: "+path {
			t.Errorf("gateway changed upstream result for %s: %d %q", path, resp.Code, resp.Body.String())
		}
	}
}
