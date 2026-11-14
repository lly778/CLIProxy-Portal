package agy

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const bridgeName = "portal"
const awaitName = "portal_await_tool"
const describeToolName = "portal_describe_tool"

type pendingTool struct {
	remote             *remoteMCPApproval
	ProjectionVersion  int
	Route              string
	trace              *requestTrace
	traceSession       string
	traceRequest       string
	BatchID            string
	ResultItem         map[string]any
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
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		allowed := false
		if err == nil && u.Scheme == "http" && u.Host == r.Host && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && u.User == nil {
			ip := net.ParseIP(u.Hostname())
			allowed = u.Hostname() == "localhost" || ip != nil && ip.IsLoopback()
		}
		if !allowed {
			w.WriteHeader(http.StatusForbidden)
			return
		}
	}
	if id := r.Header.Get("Mcp-Session-Id"); id != "" && id != s.tenant {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		m.catalogStream(w, r, s)
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
	var listedHash string
	var listedRevision uint64
	switch f.Method {
	case "initialize":
		if s.proc != nil {
			s.proc.observeCapability("mcp_http")
		}
		s.mu.Lock()
		s.handshake = true
		s.mu.Unlock()
		tools := map[string]any{}
		if catalogRefreshEnabled(s) {
			tools["listChanged"] = true
			w.Header().Set("Mcp-Session-Id", s.tenant)
		}
		result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": tools}, "serverInfo": map[string]string{"name": "portal-client-tools", "version": "1"}}
	case "ping":
		result = map[string]any{}
	case "tools/list":
		s.mu.Lock()
		list := []any{}
		advertised := s.advertisedTools
		if catalogRefreshEnabled(s) {
			advertised = s.tools
			listedHash, listedRevision = s.toolHash, s.catalogRevision
		}
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
		list = append(list, map[string]any{"name": readToolRecordName, "description": "Read retained original client tool results by a signed checkpoint reference, or page its index. Read-only; never re-executes actions. An expired/unavailable record is not evidence of failure or permission to repeat the action.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"reference": map[string]string{"type": "string"}, "call_id": map[string]string{"type": "string"}, "offset": map[string]string{"type": "integer"}}, "required": []string{"reference"}}})
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
		if unmarshalExact(f.Params, &p) != nil {
			w.WriteHeader(400)
			return
		}
		var call *pendingTool
		newCall := false
		var callEpoch uint64
		s.mu.Lock()
		if p.Name == readToolRecordName {
			allowed := s.policy.Mode != "none" && !s.checkpointing && !s.loading
			owner, model, trace := s.owner, s.model, s.trace
			s.mu.Unlock()
			if !allowed {
				result = mcpError("Tool record lookup is disabled for this request.")
			} else {
				result = m.readToolRecord(r.Context(), owner, model, p.Arguments)
			}
			trace.emit(Observation{Kind: "bridge", Category: "tool_record_read", State: "read", Outcome: receiptOutcome(result)})
			break
		}
		if p.Name == describeToolName {
			s.trace.emit(Observation{Kind: "bridge", State: "describe", Category: "describe"})
			result = mcpError("Client tool is not available under the current policy.")
			name, _ := p.Arguments["name"].(string)
			for _, t := range s.tools {
				if t.key() == name && s.policy.permits(t) {
					var hit bool
					result, hit = describeContract(s, t)
					state := "miss"
					if hit {
						state = "hit"
					}
					s.trace.emit(Observation{Kind: "contract_cache", State: state, Tool: t.key(), DefinitionHash: digest(t), ProjectionVersion: schemaProjectionVersion})
				}
			}
			s.mu.Unlock()
			break
		}
		if nativePromptActive(s) {
			candidate, args, allowed := allowedToolCall(s, p.Name, p.Arguments)
			if allowed && candidate.Type == "tool_search" && candidate.Execution == "server" {
				var event *serverSearchEvent
				result, event = serverToolSearch(s, candidate, args)
				epoch := s.epoch
				s.mu.Unlock()
				if event != nil {
					select {
					case s.events <- agentEvent{Epoch: epoch, Search: event}:
					case <-s.life.Done():
						return
					case <-r.Context().Done():
						return
					}
				}
				break
			}
			if allowed && candidate.remoteBinding != nil {
				conflict := false
				for _, old := range s.pending {
					if old.remote != nil && old.remote.NativeRPC == string(f.ID) && old.remote.Epoch == s.epoch {
						if digest(old.Arguments) != digest(args) || digest(old.Tool) != digest(candidate) || digest(old.remote.Binding) != digest(*candidate.remoteBinding) {
							conflict = true
						} else {
							call = old
						}
						break
					}
				}
				if conflict {
					s.mu.Unlock()
					result = mcpError("This remote request ID was already bound to another operation. No new call was made.")
					break
				}
				if call == nil {
					call, newCall = parkToolCall(s, p.Name, p.Arguments)
					if call != nil && newCall {
						call.remote.NativeRPC = string(f.ID)
					}
				}
				callEpoch = s.epoch
			}
		}
		if p.Name == awaitName {
			s.trace.emit(Observation{Kind: "bridge", State: "await", Category: "await"})
			id, _ := p.Arguments["call_id"].(string)
			call = s.pending[id]
		} else if nativePromptActive(s) && call == nil {
			call, newCall = parkToolCall(s, p.Name, p.Arguments)
			callEpoch = s.epoch
		}
		s.mu.Unlock()
		if call == nil {
			result = mcpError("Tool is not allowed or its call has expired.")
			break
		}
		if newCall {
			if call.remote != nil {
				m.startRemoteJobs(s)
			}
			if call.remote == nil || call.remote.Binding.ApprovalRequired {
				select {
				case s.events <- agentEvent{Epoch: callEpoch, Call: call}:
				case <-s.life.Done():
					return
				case <-r.Context().Done():
					s.cancel()
					return
				}
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
				delivered := err == nil
				// A successful HTTP write after native completion does not prove
				// the finished model turn consumed this result. Replay it as
				// context in the next prompt instead of losing the result.
				if call.remote != nil {
					delivered = delivered && s.running && call.remote.Epoch == s.epoch && s.promptCompletedEpoch != s.epoch
				}
				if delivered && !call.Returned {
					call.receipt("result_delivered")
				}
				call.Returned = call.Returned || delivered
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
				call.receipt("wait_cancelled")
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
	if err := json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": f.ID, "result": result}); err == nil && listedHash != "" {
		catalogFetched(s, listedHash, listedRevision)
	}
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
