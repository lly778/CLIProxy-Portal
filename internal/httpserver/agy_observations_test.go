package httpserver

import (
	"strings"
	"testing"

	"cliproxy-portal/internal/agy"
)

func TestAGYObservationDetailSeparatesTransportAndSummaryCost(t *testing.T) {
	if agyObservationDetail(nil) != "" {
		t.Fatal("empty history changed legacy display")
	}
	text := agyObservationDetail([]agy.Observation{
		{Kind: "phase", State: "accepted", At: 100},
		{Kind: "phase", State: "queued_followup", At: 500},
		{Kind: "tool", Category: "client_tool", State: "result_received", CallID: "call-one", Tool: "write", Outcome: "unknown", At: 600},
		{Kind: "summary_usage", State: "completed", ModelCalls: 2, Usage: agy.Usage{Known: true, Total: 120}},
		{Kind: "summary_cache", State: "hit"},
		{Kind: "contract_cache", State: "hit", RuntimeSHA: "fixture-runtime"},
		{Kind: "contract_cache", State: "miss"},
		{Kind: "tool_rejected", Tool: "project.write", Route: "portal_call_tool", Reason: "schema_mismatch", ProjectionVersion: 1},
		{Kind: "bridge", State: "completed", Category: "server_tool_search"},
	})
	for _, want := range []string{"+400 ms queued_followup", "call-one", "outcome=unknown", "2 次已确认模型调用", "缓存复用 1 次", "120 tokens", "另计入用量汇总", "reason=schema_mismatch", "projection=1", "SHA256=fixture-runtime", "描述缓存命中 1、读取 1", "服务端目录搜索 1"} {
		if !strings.Contains(text, want) {
			t.Fatal("missing receipt detail", want, text)
		}
	}
}

func TestRemoteMCPDetailDoesNotCountApprovalAsClientExecution(t *testing.T) {
	detail := agyObservationDetail([]agy.Observation{{Kind: "remote_tool", State: "issued", Category: "remote_mcp", CallID: "remote", Tool: "docs.write", Outcome: "not_started"}, {Kind: "remote_tool", State: "approval_received", Category: "remote_mcp", CallID: "remote", Outcome: "not_started"}, {Kind: "remote_tool", State: "execution_started", Category: "remote_mcp", CallID: "remote", Outcome: "unknown"}, {Kind: "remote_tool", State: "result_received", Category: "remote_mcp", CallID: "remote", Outcome: "success"}})
	for _, want := range []string{"客户端调用生成 0", "实际开始派发 1", "审批不是执行完成", "outcome=unknown"} {
		if !strings.Contains(detail, want) {
			t.Fatal("remote execution classified incorrectly", want, detail)
		}
	}
}
