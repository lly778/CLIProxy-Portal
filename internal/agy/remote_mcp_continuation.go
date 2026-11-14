package agy

// Called under s.mu; the request mutex separately decides whether another
// HTTP reader already owns this continuation.
func remoteContinuationCandidate(s *session) bool {
	if s.running {
		return false
	}
	found := false
	for _, c := range s.pending {
		if c.Returned || c.Replayed {
			continue
		}
		if c.remote == nil {
			return false
		}
		select {
		case <-c.remote.approvalReady:
			found = true
		default:
			return false
		}
	}
	return found
}

// A restored remote operation may already have a real/unknown result, or may
// have been approved before dispatch. Neither case expects a client tool
// output. Resume it using explicit current authority and preserved results.
func (m *Manager) prepareRemoteContinuation(s *session, r *request) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := 0
	if prefix(s.history, requestFingerprints(r.Items)) {
		start = len(s.items)
	}
	if start > len(r.Items) {
		return false, fail(409, "remote_operation_pending", "远程续接历史不完整")
	}
	for _, item := range r.Items[start:] {
		if isToolResultItem(stringField(item, "type")) {
			return false, nil
		}
	}
	var calls []*pendingTool
	for _, id := range pendingIDs(s) {
		c := s.pending[id]
		if c.Returned || c.Replayed {
			continue
		}
		if c.remote == nil {
			return false, nil
		}
		select {
		case <-c.remote.approvalReady:
		default:
			return false, nil
		}
		calls = append(calls, c)
	}
	if len(calls) == 0 {
		return false, nil
	}
	if start > len(r.Items) || len(r.Items[start:]) > 0 && !onlyFollowupMessages(r.Items[start:]) {
		return false, fail(409, "remote_operation_pending", "请用原会话历史及新增消息续接远程结果")
	}
	extras, err := promptBlocks(r.Items[start:])
	if err != nil {
		return false, err
	}
	tools, loaded := requestedTools(s, *r)
	policy, err := requestToolPolicy(*r, tools)
	if err != nil {
		return false, err
	}
	update, hash, err := sessionToolDelta(s, tools)
	if err != nil {
		return false, err
	}
	for _, c := range calls {
		select {
		case <-c.Ready:
			continue
		default:
		}
		client := r.remoteConnections[c.remote.Binding.Server]
		if client == nil || client.spec.identity() != c.remote.Binding.Specification || m.remoteCredentialIdentity(s.owner, client.spec) != c.remote.Binding.Credential {
			return false, fail(400, "remote_authorization_required", "继续尚未派发的远程操作需要本轮原声明和原授权")
		}
		found := false
		for _, t := range tools {
			if t.key() == c.Tool.key() && digest(t) == digest(c.Tool) {
				found = true
				break
			}
		}
		if !found {
			return false, fail(409, "remote_authorization_changed", "远程原始契约已改变，未继续派发")
		}
	}
	if text := policy.text(); text != "" {
		extras = append([]any{map[string]string{"type": "text", "text": text}}, extras...)
	}
	last := calls[len(calls)-1]
	last.Extra = extras
	last.Update = update
	commitToolCatalog(s, tools, loaded, hash)
	s.policy = policy
	commitRemoteConnections(s, r)
	if r.ToolsProvided {
		s.deferredTools = deferredCatalog(r.Tools)
	}
	for _, c := range calls {
		c.remote.client = s.remoteConnections[c.remote.Binding.Server]
	}
	return true, nil
}
