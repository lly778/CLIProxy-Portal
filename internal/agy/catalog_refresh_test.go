package agy

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func refreshFixture(t *testing.T) (*Manager, *session) {
	m, s := recoveryFixture(t, func(*session, frame) any { return nil })
	s.secret = "fixture-secret"
	s.proc.evidence = newRuntimeEvidence(verifiedAGY153, "agy", "1.5.3", 1, nil)
	s.catalogRefreshExperiment = true
	s.tools = []tool{{Type: "function", Name: "old", Parameters: map[string]any{"type": "object"}}}
	s.advertisedTools = append([]tool{}, s.tools...)
	s.toolHash = digest(s.tools)
	return m, s
}

func TestCatalogRefreshRequiresExplicitExperimentAndExactBinary(t *testing.T) {
	m, s := refreshFixture(t)
	s.catalogRefreshExperiment = false
	result := localMCPCall(t, m, s, "initialize", map[string]any{})
	if result["capabilities"].(map[string]any)["tools"].(map[string]any)["listChanged"] != nil {
		t.Fatal("unverified refresh enabled by default")
	}
	s.catalogRefreshExperiment = true
	s.proc.evidence.SHA256 = "different executable"
	if catalogRefreshEnabled(s) {
		t.Fatal("replacement binary inherited experimental support")
	}
	s.proc.evidence.SHA256 = verifiedAGY153
	result = localMCPCall(t, m, s, "initialize", map[string]any{})
	if result["capabilities"].(map[string]any)["tools"].(map[string]any)["listChanged"] != true {
		t.Fatal("explicit candidate unavailable")
	}
	if s.proc.runtimeEvidence().Capabilities["dynamic_tool_refresh"].State != "unknown" {
		t.Fatal("sending capability claimed verified runtime adoption")
	}
}

func TestCatalogRefreshAuthenticatesStreamsAndKeepsPendingActions(t *testing.T) {
	m, s := refreshFixture(t)
	localMCPCall(t, m, s, "initialize", map[string]any{})
	localMCPCall(t, m, s, "tools/list", nil)
	srv := httptest.NewServer(http.HandlerFunc(m.mcp))
	defer srv.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/mcp/"+s.tenant, nil)
	req.Header.Set("Authorization", "Bearer "+s.secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("stream unavailable", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)
	if line, err := reader.ReadString('\n'); err != nil || !strings.Contains(line, "catalog channel") {
		t.Fatal("missing stream handshake", err)
	}
	_, _ = reader.ReadString('\n')
	s.mu.Lock()
	original := &pendingTool{ID: "pending-old", Tool: s.tools[0], Ready: make(chan struct{}), Delivered: true}
	s.pending[original.ID] = original
	next := []tool{{Type: "function", Name: "new", Parameters: map[string]any{"type": "object"}}}
	commitToolCatalog(s, next, nil, digest(next))
	s.mu.Unlock()
	lines := make(chan string, 1)
	go func() {
		var text strings.Builder
		for {
			line, err := reader.ReadString('\n')
			text.WriteString(line)
			if err != nil || line == "\n" {
				lines <- text.String()
				return
			}
		}
	}()
	select {
	case event := <-lines:
		if !strings.Contains(event, "event: message") || !strings.Contains(event, "notifications/tools/list_changed") || !strings.Contains(event, `"params":{}`) {
			t.Fatal("invalid MCP notification", event)
		}
	case <-time.After(time.Second):
		t.Fatal("catalog change not notified")
	}
	listed := localMCPCall(t, m, s, "tools/list", nil)
	encoded, _ := json.Marshal(listed)
	if !strings.Contains(string(encoded), next[0].mcpName()) || strings.Contains(string(encoded), original.Tool.mcpName()) {
		t.Fatal("stale catalog served")
	}
	s.mu.Lock()
	retained := s.pending[original.ID] == original
	ready := false
	select {
	case <-original.Ready:
		ready = true
	default:
	}
	s.mu.Unlock()
	if !retained || ready {
		t.Fatal("refresh changed pending action")
	}
	cancel()
	if s.life.Err() != nil {
		t.Fatal("stream disconnect cancelled native task")
	}
	bad := httptest.NewRequest("GET", "/mcp/"+s.tenant, nil)
	bad.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	m.mcp(w, bad)
	if w.Code != 404 {
		t.Fatal("unauthenticated stream accepted")
	}
	bad.Header.Set("Authorization", "Bearer "+s.secret)
	bad.Header.Set("Mcp-Session-Id", "other-session")
	w = httptest.NewRecorder()
	m.mcp(w, bad)
	if w.Code != 404 {
		t.Fatal("cross-session stream accepted")
	}
}

func TestCatalogStaleFetchCannotAcknowledgeNewRevision(t *testing.T) {
	_, s := refreshFixture(t)
	first := s.toolHash
	s.mu.Lock()
	next := []tool{{Type: "function", Name: "next"}}
	commitToolCatalog(s, next, nil, digest(next))
	s.mu.Unlock()
	catalogFetched(s, first, 0)
	if s.catalogFetchedHash == s.toolHash {
		t.Fatal("stale fetch acknowledged new catalog")
	}
	catalogFetched(s, s.toolHash, s.catalogRevision)
	current := s.catalogFetchedHash
	catalogFetched(s, first, 0)
	if s.catalogFetchedHash != current {
		t.Fatal("late old fetch rolled back acknowledgement")
	}
}

func TestCatalogNotificationHasOnePrimaryAndPromotesStandby(t *testing.T) {
	m, s := refreshFixture(t)
	localMCPCall(t, m, s, "initialize", map[string]any{})
	localMCPCall(t, m, s, "tools/list", nil)
	srv := httptest.NewServer(http.HandlerFunc(m.mcp))
	defer srv.Close()
	open := func() (<-chan string, context.CancelFunc) {
		ctx, cancel := context.WithCancel(t.Context())
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/mcp/"+s.tenant, nil)
		req.Header.Set("Authorization", "Bearer "+s.secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			cancel()
			t.Fatal("catalog stream unavailable", err)
		}
		lines := make(chan string, 8)
		go func() {
			defer resp.Body.Close()
			scan := bufio.NewScanner(resp.Body)
			for scan.Scan() {
				if strings.HasPrefix(scan.Text(), "data:") {
					lines <- scan.Text()
				}
			}
		}()
		return lines, cancel
	}
	first, cancelFirst := open()
	defer cancelFirst()
	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		owned := s.catalogOwner != nil
		s.mu.Unlock()
		if owned {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("primary not selected")
		}
		time.Sleep(time.Millisecond)
	}
	second, cancelSecond := open()
	defer cancelSecond()
	s.mu.Lock()
	next := []tool{{Type: "function", Name: "next"}}
	commitToolCatalog(s, next, nil, digest(next))
	s.mu.Unlock()
	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("primary missed notification")
	}
	select {
	case <-second:
		t.Fatal("same notification broadcast to standby")
	case <-time.After(25 * time.Millisecond):
	}
	cancelFirst()
	select {
	case <-second:
	case <-time.After(time.Second):
		t.Fatal("standby did not take over pending notification")
	}
}

func TestMCPOriginRejectsCrossSiteWithValidBearer(t *testing.T) {
	m, s := refreshFixture(t)
	for _, origin := range []string{"https://evil.example", "http://127.0.0.1:9999", "null", "http://localhost:9876/path"} {
		req := httptest.NewRequest("POST", "http://localhost:9876/mcp/"+s.tenant, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		req.Header.Set("Authorization", "Bearer "+s.secret)
		req.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		m.mcp(w, req)
		if w.Code != 403 {
			t.Fatal("cross-site origin accepted", origin, w.Code)
		}
	}
	req := httptest.NewRequest("POST", "http://localhost:9876/mcp/"+s.tenant, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Authorization", "Bearer "+s.secret)
	req.Header.Set("Origin", "http://localhost:9876")
	w := httptest.NewRecorder()
	m.mcp(w, req)
	if w.Code != 200 {
		t.Fatal("same-origin loopback rejected", w.Code)
	}
}

func TestToolEvidenceReadPermissionFollowsCurrentPolicy(t *testing.T) {
	m, s := refreshFixture(t)
	s.running, s.handshake = true, true
	s.reported["read-evidence"] = reportedTool{Server: bridgeName, Name: readToolRecordName}
	raw := json.RawMessage(`{"sessionId":"native","toolCall":{"toolCallId":"read-evidence"},"options":[{"optionId":"yes","kind":"allow_once"},{"optionId":"no","kind":"reject_once"}]}`)
	for _, restricted := range []bool{false, true} {
		s.policy.Mode = "auto"
		if restricted {
			s.policy.Mode = "none"
		}
		result, err := m.permission("session/request_permission", raw)
		if err != nil {
			t.Fatal(err)
		}
		want := "yes"
		if restricted {
			want = "no"
		}
		if result.(map[string]any)["outcome"].(map[string]string)["optionId"] != want {
			t.Fatal("evidence helper permission did not follow policy")
		}
	}
}
