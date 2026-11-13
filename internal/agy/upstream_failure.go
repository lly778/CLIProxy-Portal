package agy

import (
	"encoding/json"
	"strings"
)

// Inspect only provider diagnostics, never ordinary assistant text. Publish
// recognized reasons, not raw diagnostics containing credentials or user data.
func upstreamFailure(text string) *Error {
	v := strings.ToLower(text)
	has := func(parts ...string) bool {
		for _, part := range parts {
			if strings.Contains(v, part) {
				return true
			}
		}
		return false
	}
	code, status := "", 502
	switch {
	case has("service has been disabled", "account has been suspended"):
		code, status = "account_disabled", 503
	case has("no capacity available"):
		code = "upstream_capacity_exhausted"
	case has("model is overloaded", "model overloaded", "server is overloaded"):
		code = "upstream_overloaded"
	case has("quota", "resource exhausted", "resource_exhausted", "rate limit", "too many requests"):
		code, status = "upstream_resource_exhausted", 429
	case has("unauthenticated", "authentication", "sign in", "invalid_grant", "invalid credentials", "token has expired"):
		code, status = "auth_required", 503
	case has("permission_denied", "permission denied", "access denied", "forbidden"):
		code = "upstream_access_denied"
	case has("stream idle timeout"):
		code = "upstream_stream_idle_timeout"
	case has("deadline_exceeded", "deadline exceeded", "request timed out"):
		code = "upstream_deadline_exceeded"
	case has("invalid_argument", "invalid argument"):
		code = "upstream_invalid_argument"
	case has("tool is not allowed or its call has expired"):
		code = "upstream_tool_call_expired"
	case has("unavailable"):
		code = "upstream_unavailable"
	default:
		return nil
	}
	return &Error{Status: status, Code: code, Message: publicFailureMessage(code)}
}

func publicFailureMessage(code string) string {
	switch code {
	case "session_busy":
		return "原 AGY 会话仍在处理上一笔请求，请等待完成后再提交新消息"
	case "compaction_session_unavailable":
		return "无法安全续接原 AGY 会话进行摘要，请先续接原任务"
	case "compaction_history_changed":
		return "压缩输入与原 AGY 会话不一致，请使用最新历史和工具结果"
	case "compaction_tools_pending":
		return "原工具调用仍在交付，等待本轮交付完成后才能压缩"
	case "compaction_turn_running":
		return "原 AGY 仍在生成，等待本轮完成后才能压缩"
	case "missing_tool_result":
		return "当前批次的工具结果尚未回传完整，原调用保持等待"
	case "waiting_for_tools":
		return "原 AGY 会话正在等待客户端工具结果"
	case "result_changed":
		return "同一工具调用的结果与此前已接收的结果不一致"
	case "followup_queue_full":
		return "追加消息队列已满，请等待前面的消息处理完成"
	case "followup_history_changed":
		return "原会话历史已变化，未执行排队消息，请按最新历史重试"
	case "upstream_capacity_exhausted":
		return "上游模型容量不足（No capacity available），暂时无法生成回复"
	case "upstream_overloaded":
		return "上游模型或服务过载（overloaded）"
	case "upstream_resource_exhausted", "quota_exhausted":
		return "上游额度不足或请求受到限流"
	case "upstream_access_denied":
		return "上游拒绝访问（PERMISSION_DENIED）"
	case "upstream_stream_idle_timeout":
		return "上游 CLI 等待回复超时（stream idle timeout）"
	case "upstream_deadline_exceeded":
		return "上游处理超时（DEADLINE_EXCEEDED）"
	case "upstream_invalid_argument":
		return "上游拒绝了请求参数（INVALID_ARGUMENT）"
	case "upstream_tool_call_expired":
		return "上游工具调用已过期或不被允许（Tool is not allowed or its call has expired）"
	case "upstream_unavailable":
		return "上游服务暂不可用（UNAVAILABLE）"
	case "account_disabled":
		return "AGY 官方账号服务已停用，请更换账号或向官方申诉"
	case "auth_required":
		return "AGY 官方账号认证失败，需要重新登录"
	case "agent_error", "agent_api_error", "agy_generation_failed", "agent_execution_failed":
		return "AGY 上游调用失败，具体原因未能识别"
	case "model_idle_timeout":
		return "AGY 等待模型回复超时"
	case "worker_lost":
		return "AGY 会话进程已终止"
	}
	return ""
}

// An SSE response has already sent HTTP headers. Keep the request log's final
// status faithful to the bounded application error instead of relabeling every
// local conflict or missing result as an upstream HTTP 502.
func FailureStatus(code string) int {
	switch code {
	case "session_busy", "compaction_session_unavailable", "compaction_history_changed", "compaction_tools_pending", "compaction_turn_running", "waiting_for_tools", "result_changed", "followup_history_changed":
		return 409
	case "followup_queue_full":
		return 429
	case "missing_tool_result":
		return 400
	default:
		return 502
	}
}

// The existing request-log formatter reads this bounded, allowlisted summary.
// No extra database/CLI query or schema change is needed.
func FailureSummary(code string) string {
	message := publicFailureMessage(code)
	if message == "" {
		return code
	}
	b, _ := json.Marshal(map[string]string{"code": code, "message": message})
	return string(b)
}
