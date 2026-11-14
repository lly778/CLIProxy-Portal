package agy

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

type remoteMCPApproval struct {
	Binding       remoteMCPBinding
	CallID        string
	NativeRPC     string
	Epoch         uint64
	Approved      bool
	ApprovalHash  string
	ApprovalItem  map[string]any
	Attempted     bool
	Outcome       string
	started       bool
	approvalReady chan struct{}
	client        *remoteMCPClient
}

type savedRemoteMCPApproval struct {
	Binding                       remoteMCPBinding
	CallID, NativeRPC             string
	Approved, Received, Attempted bool
	Outcome                       string
	ApprovalHash                  string
	ApprovalItem                  map[string]any
	ResultItem                    map[string]any
}

type remoteMCPEvent struct {
	Item map[string]any
	Call *pendingTool
}

var errRemoteAuthorityChanged = errors.New("remote authorization changed")

func receivedReady(c *pendingTool) <-chan struct{} {
	if c.remote != nil {
		return c.remote.approvalReady
	}
	return c.Ready
}
func receivedHash(c *pendingTool) string {
	if c.remote != nil {
		return c.remote.ApprovalHash
	}
	return c.OutputHash
}

func approvalRequestItem(c *pendingTool) map[string]any {
	arguments, _ := json.Marshal(c.Arguments)
	return map[string]any{"id": c.ID, "type": "mcp_approval_request", "server_label": c.remote.Binding.Server, "name": c.remote.Binding.Name, "arguments": string(arguments)}
}

func remoteResultItem(c *pendingTool, result any, reason string) map[string]any {
	arguments, _ := json.Marshal(c.Arguments)
	output, _ := json.Marshal(result)
	item := map[string]any{"id": c.remote.CallID, "type": "mcp_call", "status": "completed", "server_label": c.remote.Binding.Server, "name": c.remote.Binding.Name, "arguments": string(arguments), "output": string(output), "error": nil, "approval_request_id": nil}
	if c.remote.Binding.ApprovalRequired {
		item["approval_request_id"] = c.ID
	}
	if reason != "" {
		item["error"] = reason
	}
	return item
}

// The durable marker is committed before sending an operation. A restarted
// bridge may report an unknown outcome, but must never retry that same action.
func (m *Manager) claimRemoteAttempt(s *session, c *pendingTool) error {
	id := digest([]string{"remote-mcp-attempt-v1", s.owner, s.ID, c.ID})
	if m.disk != nil {
		ctx, cancel := context.WithTimeout(s.life, 500*time.Millisecond)
		defer cancel()
		result, err := m.disk.db.ExecContext(ctx, `INSERT INTO remote_mcp_attempts(id,owner,expires_ms) VALUES(?,?,?) ON CONFLICT(id) DO NOTHING`, id, m.disk.hash(s.owner), time.Now().Add(sessionRetention).UnixMilli())
		if err != nil {
			return errors.New("remote attempt journal unavailable")
		}
		count, _ := result.RowsAffected()
		if count != 1 {
			return errors.New("remote execution outcome unknown")
		}
		return nil
	}
	m.remoteAttemptsMu.Lock()
	defer m.remoteAttemptsMu.Unlock()
	if m.remoteAttempts == nil {
		m.remoteAttempts = map[string]time.Time{}
	}
	if _, exists := m.remoteAttempts[id]; exists {
		return errors.New("remote execution outcome unknown")
	}
	if len(m.remoteAttempts) >= 4096 {
		for key, expiry := range m.remoteAttempts {
			if time.Now().After(expiry) {
				delete(m.remoteAttempts, key)
			}
		}
		if len(m.remoteAttempts) >= 4096 {
			return errors.New("remote attempt journal unavailable")
		}
	}
	m.remoteAttempts[id] = time.Now().Add(sessionRetention)
	return nil
}

func remoteAllowedLocked(s *session, c *pendingTool, client *remoteMCPClient) bool {
	if client == nil || !s.policy.permits(c.Tool) || s.loading || s.checkpointing {
		return false
	}
	for _, t := range s.tools {
		if t.key() == c.Tool.key() && t.remoteBinding != nil && digest(*t.remoteBinding) == digest(c.remote.Binding) && digest(t) == digest(c.Tool) {
			return true
		}
	}
	return false
}

func (m *Manager) authorizeRemoteDispatch(s *session, c *pendingTool, client *remoteMCPClient) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !remoteAllowedLocked(s, c, client) {
		return errRemoteAuthorityChanged
	}
	if err := m.claimRemoteAttempt(s, c); err != nil {
		return err
	}
	c.remote.Attempted = true
	c.remote.Outcome = "unknown"
	c.receipt("execution_started")
	return nil
}

func (m *Manager) startRemoteJobs(s *session) {
	s.mu.Lock()
	var calls []*pendingTool
	for _, c := range s.pending {
		if c.remote != nil && !c.remote.started {
			select {
			case <-c.Ready:
				continue
			default:
			}
			c.remote.started = true
			calls = append(calls, c)
		}
	}
	s.mu.Unlock()
	for _, c := range calls {
		go m.runRemoteJob(s, c)
	}
}

func (m *Manager) runRemoteJob(s *session, c *pendingTool) {
	select {
	case <-c.remote.approvalReady:
	case <-s.life.Done():
		return
	}
	s.mu.Lock()
	binding := c.remote.Binding
	client := c.remote.client
	if client == nil {
		client = s.remoteConnections[binding.Server]
	}
	approved := c.remote.Approved
	alreadyAttempted := c.remote.Attempted
	allowed := s.policy.permits(c.Tool) && !s.loading && !s.checkpointing
	declared := false
	for _, t := range s.tools {
		if t.key() == c.Tool.key() && t.remoteBinding != nil && digest(*t.remoteBinding) == digest(binding) && digest(t) == digest(c.Tool) {
			declared = true
			break
		}
	}
	allowed = allowed && declared
	if client != nil {
		allowed = allowed && client.spec.identity() == binding.Specification && m.remoteCredentialIdentity(s.owner, client.spec) == binding.Credential && client.spec.permits(binding.Name)
	} else {
		allowed = false
	}
	s.mu.Unlock()
	var result any
	reason := ""
	var releaseCapacity func(int)
	capacity := 0
	switch {
	case !approved:
		reason = "approval_denied"
		result = mcpError("The caller denied this remote operation. No remote call was made.")
	case alreadyAttempted:
		reason = "execution_unknown"
		result = mcpError("This remote operation may already have executed. Its outcome is unknown; do not repeat it.")
	case !allowed:
		reason = "authorization_changed"
		result = mcpError("The remote tool, service, credential or current policy changed. No remote call was made.")
	default:
		duration := m.cfg.ToolTimeout
		if duration <= 0 {
			duration = 10 * time.Minute
		}
		ctx, cancel := context.WithTimeout(s.life, duration)
		defer cancel()
		var available bool
		capacity, releaseCapacity, available = m.reserveRemoteCapacity(ctx, s)
		args, _ := json.Marshal(c.Arguments)
		limit := (capacity - 2*len(args) - 4096) / 2
		if !available || limit < 1024 {
			reason = "remote_capacity"
			result = mcpError("Remote operation capacity is exhausted. No call was made; compact or rebuild the conversation before more remote work.")
		} else if err := m.authorizeRemoteDispatch(s, c, client); err != nil {
			reason = "execution_unknown"
			result = mcpError("The remote execution marker could not be claimed. The earlier outcome may be unknown; do not repeat the operation.")
			if errors.Is(err, errRemoteAuthorityChanged) {
				reason = "authorization_changed"
				result = mcpError("Remote authorization changed before dispatch. No remote call was made.")
			}
		} else {
			ctx = context.WithValue(ctx, remoteMCPResponseLimit{}, limit)
			actual, err := client.call(ctx, binding.Name, c.Arguments)
			if err != nil {
				reason = "remote_error"
				var remote *remoteMCPError
				if errors.As(err, &remote) && remote.Uncertain {
					reason = "execution_unknown"
				}
				result = mcpError("The remote service did not provide a usable successful response. The operation may have executed; do not automatically repeat it.")
			} else if _, err := toolResultBlocks(actual); err != nil {
				reason = "result_unrepresentable"
				result = mcpError("The remote operation returned an unsupported result. It may have executed; do not repeat it.")
			} else {
				result = actual
			}
		}
	}
	s.mu.Lock()
	c.Output, c.OutputHash = result, digest(result)
	c.remote.client = nil
	c.remote.Outcome = receiptOutcome(result)
	if reason == "execution_unknown" || reason == "result_unrepresentable" {
		c.remote.Outcome = "unknown"
	} else if !c.remote.Attempted {
		c.remote.Outcome = "not_executed"
	}
	c.ResultItem = remoteResultItem(c, result, reason)
	encoded, _ := json.Marshal(c.ResultItem)
	if capacity > 0 && len(encoded) > capacity {
		result = mcpError("The remote result exceeds the remaining record capacity. Its outcome may be unknown; do not repeat the operation.")
		reason = "result_unrepresentable"
		c.Output, c.OutputHash = result, digest(result)
		c.remote.Outcome = "unknown"
		c.ResultItem = remoteResultItem(c, result, reason)
		encoded, _ = json.Marshal(c.ResultItem)
	}
	c.receipt("result_received")
	item := c.ResultItem
	epoch := s.epoch
	s.mu.Unlock()
	if releaseCapacity != nil {
		releaseCapacity(len(encoded))
	}
	select {
	case s.events <- agentEvent{Epoch: epoch, Remote: &remoteMCPEvent{Item: item, Call: c}}:
	case <-s.life.Done():
	}
	s.mu.Lock()
	close(c.Ready)
	s.mu.Unlock()
}

// A native RPC can finish before a long remote call. Keep the HTTP response
// open for those jobs; client approval and client execution still use the
// existing explicit wait/continuation flow.
func remoteWorkOnly(s *session) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	waiting := false
	for _, c := range s.pending {
		if c.Returned || c.Replayed {
			continue
		}
		select {
		case <-c.Ready:
			continue
		default:
		}
		if c.remote == nil {
			return false
		}
		select {
		case <-c.remote.approvalReady:
			waiting = true
		default:
			return false
		}
	}
	return waiting
}

func (o *response) remoteEvent(event remoteMCPEvent) error {
	index := len(o.output)
	o.output = append(o.output, event.Item)
	if err := o.event("response.output_item.added", map[string]any{"output_index": index, "item": event.Item}); err != nil {
		return err
	}
	return o.event("response.output_item.done", map[string]any{"output_index": index, "item": event.Item})
}

func validateRemoteApproval(c *pendingTool, item map[string]any, req *request) (any, error) {
	if item["type"] != "mcp_approval_response" {
		return nil, fail(400, "invalid_remote_approval", "请回传匹配的远程 MCP 审批响应")
	}
	approved, ok := item["approve"].(bool)
	if !ok {
		return nil, fail(400, "invalid_remote_approval", "远程 MCP 审批响应缺少明确 approve 布尔值")
	}
	if approved {
		if req == nil {
			return nil, fail(400, "remote_authorization_required", "审批通过时需要本轮远程 MCP 声明与授权")
		}
		client := req.remoteConnections[c.remote.Binding.Server]
		if client == nil || client.spec.identity() != c.remote.Binding.Specification {
			return nil, fail(409, "remote_authorization_changed", "远程 MCP 服务或授权范围已改变，原审批没有执行")
		}
		if c.trace == nil || c.trace.m.remoteCredentialIdentity(c.trace.owner, client.spec) != c.remote.Binding.Credential {
			return nil, fail(409, "remote_authorization_changed", "远程 MCP 授权账号已改变，原审批没有执行")
		}
		matches := false
		for _, t := range req.Tools {
			if t.key() == c.Tool.key() && digest(t) == digest(c.Tool) {
				matches = true
				break
			}
		}
		if !matches {
			return nil, fail(409, "remote_authorization_changed", "远程 MCP 原始契约已变化，原审批没有执行")
		}
	}
	return approved, nil
}
