package agy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const remoteMCPProtocol = "2025-06-18"

type remoteMCPError struct {
	Kind      string
	Status    int
	Uncertain bool
}

func (e *remoteMCPError) Error() string { return "Remote MCP request failed (" + e.Kind + ")." }

var remoteBlockedNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2001::/32"), netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
}

func remotePublicIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range remoteBlockedNetworks {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// Resolve and validate on every new connection, then dial the validated IP.
// Redirects, ambient proxy settings and automatic RPC retries are disabled.
func newRemoteMCPTransport() *http.Transport {
	return &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 20 * time.Second, MaxResponseHeaderBytes: 16 << 10, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errors.New("invalid remote address")
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(ips) == 0 {
			return nil, errors.New("remote address unavailable")
		}
		for _, ip := range ips {
			if !remotePublicIP(ip.IP) {
				return nil, errors.New("remote address is not public")
			}
		}
		var last error
		for _, ip := range ips {
			conn, e := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if e == nil {
				return conn, nil
			}
			last = e
		}
		return nil, last
	}}
}

type remoteMCPClient struct {
	spec              remoteMCPSpec
	http              *http.Client
	seq               atomic.Uint64
	mu                sync.Mutex
	session, protocol string
}

func newRemoteMCPClient(spec remoteMCPSpec, transport http.RoundTripper) *remoteMCPClient {
	if transport == nil {
		transport = newRemoteMCPTransport()
	}
	return &remoteMCPClient{spec: spec, http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func validRemoteSessionID(id string) bool {
	if len(id) > 256 {
		return false
	}
	for _, c := range id {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

func (c *remoteMCPClient) rpc(ctx context.Context, method string, params any, notify bool) (map[string]any, error) {
	id := c.seq.Add(1)
	message := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	if !notify {
		message["id"] = id
	}
	body, err := json.Marshal(message)
	if err != nil {
		return nil, &remoteMCPError{Kind: "invalid_request"}
	}
	r, err := http.NewRequestWithContext(ctx, "POST", c.spec.URL, bytes.NewReader(body))
	if err != nil {
		return nil, &remoteMCPError{Kind: "invalid_request"}
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	if c.spec.authorization != "" {
		r.Header.Set("Authorization", "Bearer "+c.spec.authorization)
	}
	c.mu.Lock()
	session, protocol := c.session, c.protocol
	c.mu.Unlock()
	if session != "" {
		r.Header.Set("Mcp-Session-Id", session)
	}
	if protocol != "" {
		r.Header.Set("MCP-Protocol-Version", protocol)
	}
	uncertain := method == "tools/call"
	resp, err := c.http.Do(r)
	if err != nil {
		return nil, &remoteMCPError{Kind: "transport", Uncertain: uncertain}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &remoteMCPError{Kind: "http_status", Status: resp.StatusCode, Uncertain: uncertain}
	}
	if notify {
		return nil, nil
	}
	var reply map[string]any
	decode := func(data []byte) bool {
		var v map[string]any
		if unmarshalExact(data, &v) != nil || v["jsonrpc"] != "2.0" {
			return false
		}
		if got, ok := v["id"].(json.Number); ok && got.String() == strconv.FormatUint(id, 10) {
			reply = v
			return true
		}
		return false
	}
	limit := MaxBody
	if bounded, ok := ctx.Value(remoteMCPResponseLimit{}).(int); ok && bounded > 0 && bounded < limit {
		limit = bounded
	}
	limited := io.LimitReader(resp.Body, int64(limit)+1)
	if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		scanner := bufio.NewScanner(limited)
		scanner.Buffer(make([]byte, 4096), MaxBody)
		var event strings.Builder
		frames := 0
		total := 0
		for scanner.Scan() {
			line := scanner.Text()
			total += len(line) + 1
			if total > limit {
				break
			}
			if line == "" {
				if event.Len() > 0 {
					frames++
					if frames > 256 {
						break
					}
					if decode([]byte(event.String())) {
						break
					}
					event.Reset()
				}
				continue
			}
			if strings.HasPrefix(line, "data:") {
				if event.Len() > 0 {
					event.WriteByte('\n')
				}
				event.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
	} else if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "application/json") {
		data, e := io.ReadAll(limited)
		if e == nil && len(data) <= limit {
			decode(data)
		}
	}
	if reply == nil {
		return nil, &remoteMCPError{Kind: "invalid_response", Uncertain: uncertain}
	}
	if reply["error"] != nil {
		return nil, &remoteMCPError{Kind: "rpc_error", Uncertain: uncertain}
	}
	result, ok := reply["result"].(map[string]any)
	if !ok {
		return nil, &remoteMCPError{Kind: "invalid_result", Uncertain: uncertain}
	}
	if method == "initialize" {
		sid := resp.Header.Get("Mcp-Session-Id")
		if !validRemoteSessionID(sid) {
			return nil, &remoteMCPError{Kind: "invalid_session"}
		}
		version, _ := result["protocolVersion"].(string)
		if version != remoteMCPProtocol && version != "2025-03-26" {
			return nil, &remoteMCPError{Kind: "unsupported_protocol"}
		}
		c.mu.Lock()
		c.session, c.protocol = sid, version
		c.mu.Unlock()
	}
	if !remoteDepthValid(result, 0) {
		return nil, &remoteMCPError{Kind: "response_depth", Uncertain: uncertain}
	}
	return c.redact(result).(map[string]any), nil
}

func (c *remoteMCPClient) redact(value any) any {
	switch v := value.(type) {
	case string:
		if c.spec.authorization != "" {
			return strings.ReplaceAll(v, c.spec.authorization, "[redacted remote authorization]")
		}
		return v
	case map[string]any:
		out := map[string]any{}
		for k, item := range v {
			key := c.redact(k).(string)
			out[key] = c.redact(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = c.redact(item)
		}
		return out
	}
	return value
}

func remoteDepthValid(value any, depth int) bool {
	if depth > 64 {
		return false
	}
	switch v := value.(type) {
	case map[string]any:
		for _, item := range v {
			if !remoteDepthValid(item, depth+1) {
				return false
			}
		}
	case []any:
		for _, item := range v {
			if !remoteDepthValid(item, depth+1) {
				return false
			}
		}
	}
	return true
}

func (c *remoteMCPClient) initialize(ctx context.Context) error {
	_, err := c.rpc(ctx, "initialize", map[string]any{"protocolVersion": remoteMCPProtocol, "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "portal-remote-mcp", "version": "1"}}, false)
	if err != nil {
		return err
	}
	_, err = c.rpc(ctx, "notifications/initialized", map[string]any{}, true)
	return err
}

func (c *remoteMCPClient) list(ctx context.Context) ([]tool, error) {
	var out []tool
	cursor := ""
	seen := map[string]bool{}
	cursors := map[string]bool{}
	for page := 0; page < 8; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		result, err := c.rpc(ctx, "tools/list", params, false)
		if err != nil {
			return nil, err
		}
		items, ok := result["tools"].([]any)
		if !ok || len(items) > 256 {
			return nil, &remoteMCPError{Kind: "invalid_catalog"}
		}
		for _, raw := range items {
			entry, ok := raw.(map[string]any)
			if !ok {
				return nil, &remoteMCPError{Kind: "invalid_catalog"}
			}
			name, _ := entry["name"].(string)
			schema, ok := entry["inputSchema"].(map[string]any)
			if name == "" || len(c.spec.Label)+1+len(name) > 256 || seen[name] || !ok {
				return nil, &remoteMCPError{Kind: "invalid_catalog"}
			}
			seen[name] = true
			if !c.spec.permits(name) || excludedClientTool(tool{Name: name, Namespace: c.spec.Label}) {
				continue
			}
			description, _ := entry["description"].(string)
			if len(description) > MaxText/2 {
				return nil, &remoteMCPError{Kind: "catalog_limit"}
			}
			out = append(out, tool{Type: "function", Name: name, Namespace: c.spec.Label, Description: description, Parameters: schema, Execution: "server"})
			if len(out) > 256 {
				return nil, &remoteMCPError{Kind: "catalog_limit"}
			}
		}
		cursor, _ = result["nextCursor"].(string)
		if cursor == "" {
			return out, nil
		}
		if len(cursor) > 2048 || cursors[cursor] {
			return nil, &remoteMCPError{Kind: "invalid_cursor"}
		}
		cursors[cursor] = true
	}
	return nil, &remoteMCPError{Kind: "catalog_limit"}
}

func (c *remoteMCPClient) call(ctx context.Context, name string, args map[string]any) (map[string]any, error) {
	if !c.spec.permits(name) {
		return nil, &remoteMCPError{Kind: "tool_not_allowed"}
	}
	result, err := c.rpc(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, false)
	if err != nil {
		return nil, err
	}
	if _, ok := result["content"].([]any); !ok {
		return nil, &remoteMCPError{Kind: "invalid_tool_result", Uncertain: true}
	}
	if result["isError"] == nil {
		result["isError"] = false
	} else if _, ok := result["isError"].(bool); !ok {
		return nil, &remoteMCPError{Kind: "invalid_tool_result", Uncertain: true}
	}
	return result, nil
}
