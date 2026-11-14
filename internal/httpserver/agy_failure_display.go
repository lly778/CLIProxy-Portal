package httpserver

import (
	"strings"
	"unicode"

	"cliproxy-portal/internal/cpamp"
)

// Translate only the portal's request-log detail. Wire errors and stored
// diagnostics retain their original messages and codes.
func agyFailureDisplay(e cpamp.EventRow, code, message string) string {
	if !strings.HasPrefix(e.Model, "agy/") && !strings.HasPrefix(e.RequestID, "agy:") && !strings.HasPrefix(e.RequestID, "resp_agy_") && !strings.HasPrefix(code, "agy_") {
		return message
	}
	if text := agyFailureDisplayMessages[code]; text != "" {
		return text
	}
	if strings.ContainsFunc(message, func(r rune) bool { return unicode.Is(unicode.Han, r) }) {
		return "AGY request failed."
	}
	return message
}

var agyFailureDisplayMessages = map[string]string{
	"remote_authorization_changed":   "The remote service, credential, scope or original contract changed. The approval was not executed.",
	"remote_operation_pending":       "The original remote approval or operation is still pending. Resume it before rebuilding the session.",
	"invalid_remote_mcp":             "Invalid remote MCP declaration, endpoint or scope.",
	"invalid_remote_approval":        "Return an explicit approval response matching the original remote request.",
	"remote_authorization_required":  "Include the remote declaration and its authorization with this approval request.",
	"remote_mcp_unavailable":         "Remote MCP initialization or catalog loading failed. No remote operation was executed.",
	"native_session_missing":         "The original native session or its saved history is missing.",
	"session_restore_unsafe":         "The native session is missing, but the current history or tool state does not permit automatic recovery. The saved record was retained.",
	"invalid_compaction_history":     "Tool call IDs, result types or execution origins do not match in the compaction history. The original history was retained.",
	"tool_record_unavailable":        "The tool evidence archive could not be saved. No new checkpoint was committed.",
	"ambiguous_tool_search":          "Client and server tool discovery cannot be mixed in the same request.",
	"session_busy":                   "Session is busy with the previous request. Wait for it to finish before sending another message.",
	"compaction_session_unavailable": "The original session is unavailable for compaction. Resume the task first.",
	"compaction_history_changed":     "Compaction history does not match the original session. Use the latest history and tool results.",
	"compaction_tools_pending":       "Tool results are still being delivered. Wait before compacting.",
	"compaction_turn_running":        "The original turn is still running. Wait before compacting.",
	"missing_tool_result":            "Waiting for all client tool results in the current batch.",
	"unknown_tool_result":            "The tool result does not match an original call; the current batch is unchanged.",
	"tool_followup_not_deliverable":  "Tool results were already delivered; send the new message as a separate follow-up. It has not been marked as processed.",
	"waiting_for_tools":              "Waiting for client tool results.",
	"result_changed":                 "The result differs from the previously received result for the same tool call.",
	"followup_queue_full":            "The follow-up queue is full. Wait for earlier messages to finish.",
	"followup_history_changed":       "The original session history changed. Retry with the latest history.",
	"upstream_capacity_exhausted":    "No capacity available for the upstream model.",
	"upstream_overloaded":            "The upstream model or service is overloaded.",
	"upstream_resource_exhausted":    "Upstream quota exhausted or rate limit reached.",
	"quota_exhausted":                "Upstream quota exhausted or rate limit reached.",
	"upstream_access_denied":         "Upstream access denied (PERMISSION_DENIED).",
	"upstream_stream_idle_timeout":   "Upstream CLI stream idle timeout.",
	"upstream_deadline_exceeded":     "Upstream request timed out (DEADLINE_EXCEEDED).",
	"upstream_invalid_argument":      "Upstream rejected the request parameters (INVALID_ARGUMENT).",
	"upstream_tool_call_expired":     "Tool is not allowed or its call has expired.",
	"upstream_unavailable":           "Upstream service unavailable (UNAVAILABLE).",
	"account_disabled":               "AGY account service is disabled or suspended.",
	"auth_required":                  "AGY authentication failed. Sign in again.",
	"agent_error":                    "AGY upstream request failed; specific reason unavailable.",
	"agent_api_error":                "AGY upstream request failed; specific reason unavailable.",
	"agy_generation_failed":          "AGY upstream request failed; specific reason unavailable.",
	"agent_execution_failed":         "AGY upstream request failed; specific reason unavailable.",
	"model_idle_timeout":             "Timed out waiting for the AGY model.",
	"worker_lost":                    "The AGY session process has terminated.",
}
