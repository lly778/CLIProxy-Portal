package agy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

const bridgeName = "portal"
const awaitName = "portal_await_tool"
const describeToolName = "portal_describe_tool"

type pendingTool struct {
	ID                 string
	Tool               tool
	Arguments          map[string]any
	Output             any
	OutputHash         string
	Ready              chan struct{}
	Delivered          bool
	Update             string
	Relayed            bool
	Returned, Replayed bool
	Extra              []any
	Cancelled          bool
	Replying           int
	ReplyDone          chan struct{}
}

func (m *Manager) mcp(w http.ResponseWriter, r *http.Request) {
	tenant := strings.TrimPrefix(r.URL.Path, "/mcp/")
	m.mu.Lock()
	s := m.sessions[tenant]
	m.mu.Unlock()
	if s == nil || !authorized(r, s.secret) {
		http.NotFound(w, r)
		return
	}
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var f frame
	d := json.NewDecoder(io.LimitReader(r.Body, MaxBody+1))
	if d.Decode(&f) != nil || f.Version != "2.0" {
		w.WriteHeader(400)
		return
	}
	if len(f.ID) == 0 {
		if f.Method == "notifications/cancelled" {
			m.cancelMCP(s, f.Params)
		}
		w.WriteHeader(202)
		return
	}
	var result any
	switch f.Method {
	case "initialize":
		s.mu.Lock()
		s.handshake = true
		s.mu.Unlock()
		result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "portal-client-tools", "version": "1"}}
	case "ping":
		result = map[string]any{}
	case "tools/list":
		s.mu.Lock()
		list := []any{}
		advertised := s.advertisedTools
		if advertised == nil {
			advertised = s.tools
		}
		for _, t := range advertised {
			if excludedClientTool(t) {
				continue
			}
			schema := mcpInputSchema(t.Parameters)
			if t.Type == "computer" || t.Type == "computer_use_preview" {
				schema = nativeComputerMCPSchema(t)
			}
			if t.Type == "custom" {
				schema = map[string]any{"type": "object", "properties": map[string]any{"input": map[string]string{"type": "string"}}, "required": []string{"input"}, "additionalProperties": false}
			}
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			description := t.key() + ": " + t.Description
			if t.Type == "custom" && len(t.Format) > 0 {
				b, _ := json.Marshal(t.Format)
				description += "\nThe input text must follow this client format: " + string(b)
			}
			list = append(list, compactMCPDescriptor(map[string]any{"name": t.mcpName(), "description": description, "inputSchema": schema}))
		}
		s.mu.Unlock()
		list = append(list, map[string]any{"name": awaitName, "description": "Wait for an already running client tool. Never run the original tool again.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"call_id": map[string]string{"type": "string"}}, "required": []string{"call_id"}}})
		list = append(list, map[string]any{"name": callToolName, "description": "Invoke a currently available client tool by its exact qualified name when it is absent from direct tools or its schema has changed. Follow the latest client tool configuration update. For custom tools pass arguments={input: raw text}.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}, "arguments": map[string]any{"type": "object"}}, "required": []string{"name", "arguments"}}})
		list = append(list, map[string]any{"name": describeToolName, "description": "Read the complete original client contract for a large tool before invoking it. This only describes an active tool; it does not execute it.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "required": []string{"name"}}})
		result = map[string]any{"tools": list}
	case "tools/call":
		cancelCall, cleanup, ok := registerMCP(s, string(f.ID))
		if !ok {
			w.WriteHeader(409)
			return
		}
		defer cleanup()
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if json.Unmarshal(f.Params, &p) != nil {
			w.WriteHeader(400)
			return
		}
		var call *pendingTool
		newCall := false
		s.mu.Lock()
		if p.Name == describeToolName {
			result = mcpError("Client tool is not available under the current policy.")
			name, _ := p.Arguments["name"].(string)
			for _, t := range s.tools {
				if t.key() == name && s.policy.permits(t) {
					b, _ := json.Marshal(t)
					if len(b) <= MaxText {
						result = map[string]any{"content": []any{map[string]string{"type": "text", "text": string(b)}}}
					}
				}
			}
			s.mu.Unlock()
			break
		}
		if p.Name == awaitName {
			id, _ := p.Arguments["call_id"].(string)
			call = s.pending[id]
		} else if s.running {
			call, newCall = parkToolCall(s, p.Name, p.Arguments)
		}
		s.mu.Unlock()
		if call == nil {
			result = mcpError("Tool is not allowed or its call has expired.")
			break
		}
		if newCall {
			select {
			case s.events <- agentEvent{Call: call}:
			case <-s.life.Done():
				return
			case <-r.Context().Done():
				s.cancel()
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(200)
		_ = http.NewResponseController(w).Flush()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		relay := time.NewTimer(80 * time.Second)
		defer relay.Stop()
		for {
			select {
			case <-call.Ready:
				s.mu.Lock()
				result = mcpOutput(call.Output, call.Update)
				result.(map[string]any)["content"] = append(result.(map[string]any)["content"].([]any), call.Extra...)
				if call.Replying == 0 {
					call.ReplyDone = make(chan struct{})
				}
				call.Replying++
				s.mu.Unlock()
				_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(15 * time.Second))
				err := mcpReply(w, f.ID, result)
				s.mu.Lock()
				call.Returned = call.Returned || err == nil
				call.Replying--
				if call.Replying == 0 {
					close(call.ReplyDone)
				}
				s.mu.Unlock()
				return
			case <-relay.C:
				s.mu.Lock()
				call.Relayed = true
				s.mu.Unlock()
				mcpReply(w, f.ID, map[string]any{"content": []any{map[string]string{"type": "text", "text": "Client tool is still running. Call " + awaitName + " with call_id=" + call.ID + " to await this exact result. Do not repeat the original action."}}})
				return
			case <-ticker.C:
				if _, e := io.WriteString(w, ": waiting for client tool\n\n"); e != nil {
					return
				}
				if http.NewResponseController(w).Flush() != nil {
					return
				}
			case <-s.life.Done():
				return
			case <-r.Context().Done():
				return
			case <-cancelCall:
				s.mu.Lock()
				// Cancellation acknowledges no success and cannot stop a program
				// already executing on the remote client.
				call.Cancelled = true
				s.mu.Unlock()
				mcpReply(w, f.ID, mcpError("The MCP wait was cancelled. Client execution may still be running; do not repeat the action."))
				return
			}
		}
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": f.ID, "error": rpcError{-32601, "Method unavailable"}})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": f.ID, "result": result})
}
func mcpError(text string) any {
	return map[string]any{"isError": true, "content": []any{map[string]string{"type": "text", "text": text}}}
}
func mcpOutput(v any, update string) any {
	blocks, err := toolResultBlocks(v)
	if err != nil {
		return mcpError("Invalid client tool result.")
	}
	if update != "" {
		blocks = append(blocks, map[string]string{"type": "text", "text": update})
	}
	result := map[string]any{"content": blocks}
	if toolResultFailed(v) {
		result["isError"] = true
	}
	return result
}
func mcpReply(w http.ResponseWriter, id json.RawMessage, result any) error {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	if _, err := io.WriteString(w, "event: message\ndata: "+string(b)+"\n\n"); err != nil {
		return err
	}
	return http.NewResponseController(w).Flush()
}
func shortContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 30*time.Second)
}
