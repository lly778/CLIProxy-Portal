package agy

func appendRemoteHistory(items []map[string]any, item map[string]any) []map[string]any {
	if item == nil {
		return items
	}
	for _, old := range items {
		if old["type"] == item["type"] && ((item["type"] == "mcp_approval_response" && resultCallID(old) == resultCallID(item)) || (item["type"] != "mcp_approval_response" && old["id"] == item["id"])) {
			return items
		}
	}
	return append(items, item)
}

// Called under s.mu. Capture the actual result when known; otherwise a begun
// remote operation becomes explicitly unknown, never an approval-as-result.
func snapshotRemoteWaiting(v *savedSession, c *pendingTool) *savedWaitingTool {
	r := c.remote
	received := false
	select {
	case <-r.approvalReady:
		received = true
	default:
	}
	saved := &savedRemoteMCPApproval{Binding: r.Binding, CallID: r.CallID, NativeRPC: r.NativeRPC, Approved: r.Approved, Received: received, Attempted: r.Attempted, Outcome: r.Outcome, ApprovalHash: r.ApprovalHash, ApprovalItem: r.ApprovalItem}
	ready := c.OutputHash != ""
	output, item := c.Output, c.ResultItem
	if !ready && r.Attempted {
		ready = true
		output = mcpError("The bridge stopped after starting this remote operation. Its outcome is unknown; do not repeat it.")
		item = remoteResultItem(c, output, "execution_unknown")
		saved.Outcome = "unknown"
	}
	if r.Binding.ApprovalRequired {
		v.Items = appendRemoteHistory(v.Items, approvalRequestItem(c))
		v.Items = appendRemoteHistory(v.Items, r.ApprovalItem)
	}
	if ready {
		saved.ResultItem = item
		v.Items = appendRemoteHistory(v.Items, item)
	}
	if c.Returned || c.Replayed {
		return nil
	}
	return &savedWaitingTool{Remote: saved, ProjectionVersion: c.ProjectionVersion, Route: c.Route, BatchID: c.BatchID, ObservationRequest: c.traceRequest, ObservationSession: c.traceSession, ID: c.ID, Tool: c.Tool, Arguments: c.Arguments, Update: c.Update, Extra: c.Extra, Ready: ready, Output: output}
}

func restoreRemoteWaiting(c *pendingTool, saved savedWaitingTool) {
	r := saved.Remote
	if r == nil {
		return
	}
	c.remote = &remoteMCPApproval{Binding: r.Binding, CallID: r.CallID, NativeRPC: r.NativeRPC, Approved: r.Approved, ApprovalHash: r.ApprovalHash, ApprovalItem: r.ApprovalItem, Attempted: r.Attempted, Outcome: r.Outcome, approvalReady: make(chan struct{})}
	binding := r.Binding
	c.Tool.remoteBinding = &binding
	if r.Received {
		close(c.remote.approvalReady)
	}
	if saved.Ready {
		reason := ""
		if r.Outcome == "unknown" {
			reason = "execution_unknown"
		}
		c.ResultItem = remoteResultItem(c, c.Output, reason)
		if r.ResultItem != nil {
			c.ResultItem = r.ResultItem
		}
	}
}
