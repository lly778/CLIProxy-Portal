package agy

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

type remoteTestTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t remoteTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	u := *r.URL
	u.Scheme, u.Host = t.target.Scheme, t.target.Host
	copy.URL = &u
	return t.base.RoundTrip(copy)
}

func TestRemoteMCPSpecAuthorizationIsEphemeralAndScopeIsExact(t *testing.T) {
	input := `{"type":"mcp","server_label":"docs","server_url":"https://mcp.example.test/mcp","authorization":"fixture-token","allowed_tools":["read"],"require_approval":"always"}`
	spec, err := parseRemoteMCPSpec([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(spec)
	if strings.Contains(string(encoded), "fixture-token") || !spec.permits("read") || spec.permits("write") {
		t.Fatal("authorization leaked or scope widened")
	}
	changed := spec
	changed.authorization = "other-token"
	if changed.identity() != spec.identity() || changed.authorizationIdentity() == spec.authorizationIdentity() {
		t.Fatal("credential identity is not isolated from public declaration")
	}
	unrestricted := spec
	unrestricted.Allowed = nil
	deny := unrestricted
	deny.Allowed = []string{}
	if deny.identity() == unrestricted.identity() || deny.permits("read") || !unrestricted.permits("read") {
		t.Fatal("empty scope collapsed into all tools")
	}
	for _, mutation := range []string{
		strings.Replace(input, "https://mcp.example.test/mcp", "http://mcp.example.test/mcp", 1),
		strings.Replace(input, "https://mcp.example.test/mcp", "https://127.0.0.1/mcp", 1),
		strings.Replace(input, "https://mcp.example.test/mcp", "https://100.64.1.1/mcp", 1),
		strings.Replace(input, "https://mcp.example.test/mcp", "https://user:secret@mcp.example.test/mcp", 1),
		strings.Replace(input, "https://mcp.example.test/mcp", "https://mcp.example.test/mcp?token=secret", 1),
		strings.Replace(input, `"always"`, `"automatic"`, 1),
		strings.Replace(input, `["read"]`, `["read","read"]`, 1),
		strings.Replace(input, `"fixture-token"`, `"fixture\r\nAuthorization: secret"`, 1),
	} {
		if _, err := parseRemoteMCPSpec([]byte(mutation)); err == nil {
			t.Fatal("invalid remote declaration accepted")
		}
	}
}

func TestRemoteMCPStreamableHTTPUsesOnlyRemoteCredentialAndExactResult(t *testing.T) {
	var calls, lists atomic.Int64
	var arguments string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-remote-token" || r.Header.Get("X-Portal-Owner") != "" || !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			t.Error("wrong credentials or accept header")
		}
		var f map[string]any
		data, _ := io.ReadAll(r.Body)
		if unmarshalExact(data, &f) != nil {
			t.Error("invalid JSON-RPC")
		}
		result := map[string]any{}
		method := stringField(f, "method")
		if method != "initialize" && (r.Header.Get("Mcp-Session-Id") != "remote-session" || r.Header.Get("MCP-Protocol-Version") != remoteMCPProtocol) {
			t.Error("negotiated session/protocol not used")
		}
		switch method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "remote-session")
			result = map[string]any{"protocolVersion": remoteMCPProtocol, "capabilities": map[string]any{"tools": map[string]any{}}}
		case "notifications/initialized":
			w.WriteHeader(202)
			return
		case "tools/list":
			lists.Add(1)
			name := "read"
			if f["params"].(map[string]any)["cursor"] != nil {
				name = "forbidden"
			}
			result = map[string]any{"tools": []any{map[string]any{"name": name, "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"record": map[string]any{"const": json.Number("9007199254740993")}}}}}}
			if name == "read" {
				result["nextCursor"] = "page-two"
			}
		case "tools/call":
			calls.Add(1)
			arguments = string(data)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
			reply := map[string]any{"jsonrpc": "2.0", "id": f["id"], "result": map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "failed operation; fixture-remote-token"}, map[string]any{"type": "image", "mimeType": "image/png", "data": "YQ=="}}, "structuredContent": map[string]any{"record": json.Number("9007199254740993")}}}
			encoded, _ := json.Marshal(reply)
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", encoded)
			return
		default:
			t.Error("unexpected method", method)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": f["id"], "result": result})
	}))
	defer srv.Close()
	target, _ := url.Parse(srv.URL)
	spec := remoteMCPSpec{Label: "docs", URL: "https://mcp.example.test/mcp", Allowed: []string{"read"}, Approval: "always", authorization: "fixture-remote-token"}
	c := newRemoteMCPClient(spec, remoteTestTransport{target, http.DefaultTransport})
	if err := c.initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	tools, err := c.list(t.Context())
	if err != nil || lists.Load() != 2 || len(tools) != 1 || tools[0].Execution != "server" {
		t.Fatal("discovery/filter failed", err)
	}
	result, err := c.call(t.Context(), "read", map[string]any{"record": json.Number("9007199254740993")})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	for _, want := range []string{"9007199254740993", "image/png", "YQ==", "redacted remote authorization"} {
		if !strings.Contains(string(encoded), want) {
			t.Fatal("remote result lost content", want)
		}
	}
	if !toolResultFailed(result) || strings.Contains(string(encoded), "fixture-remote-token") || !strings.Contains(arguments, "9007199254740993") || calls.Load() != 1 {
		t.Fatal("result status, precision, privacy or call count failed")
	}
	if _, err = c.call(t.Context(), "forbidden", map[string]any{}); err == nil || calls.Load() != 1 {
		t.Fatal("forbidden tool sent to remote service")
	}
}

func TestRemoteMCPNeverRetriesUncertainExecutionOrFollowsRedirect(t *testing.T) {
	for _, mode := range []string{"redirect", "truncated", "wrong_id", "rpc_error", "invalid_error_flag"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch mode {
				case "redirect":
					w.Header().Set("Location", "https://other.example.test/secret")
					w.WriteHeader(302)
				case "truncated":
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"id\":")
				case "wrong_id":
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"jsonrpc":"2.0","id":99,"result":{}}`)
				case "rpc_error":
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"private upstream diagnostic"}}`)
				case "invalid_error_flag":
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"isError":"false","content":[]}}`)
				}
			}))
			defer srv.Close()
			target, _ := url.Parse(srv.URL)
			c := newRemoteMCPClient(remoteMCPSpec{Label: "docs", URL: "https://mcp.example.test/mcp", Approval: "always"}, remoteTestTransport{target, http.DefaultTransport})
			_, err := c.call(t.Context(), "write", map[string]any{})
			failure, ok := err.(*remoteMCPError)
			if !ok || calls.Load() != 1 || strings.Contains(err.Error(), "private upstream diagnostic") {
				t.Fatal("RPC replay or diagnostic leak", err)
			}
			if !failure.Uncertain {
				t.Fatal("unknown execution reported as confirmed failure")
			}
		})
	}
}

func TestRemoteMCPRejectsPrivateAndReservedAddresses(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "198.18.0.1", "192.0.2.1", "::1", "::ffff:127.0.0.1", "fd00::1", "2001:db8::1", "64:ff9b::a00:1"} {
		if remotePublicIP(net.ParseIP(ip)) {
			t.Fatal("unsafe address allowed", ip)
		}
	}
	for _, ip := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if !remotePublicIP(net.ParseIP(ip)) {
			t.Fatal("public address rejected", ip)
		}
	}
}
