package httpserver

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode"

	"cliproxy-portal/internal/agy"
	"cliproxy-portal/internal/cpamp"
)

func TestAGYRequestLogShowsUpstreamFailureExplanation(t *testing.T) {
	status := int64(502)
	event := cpamp.EventRow{Model: "agy/gemini", Failed: true, FailStatusCode: &status, FailSummary: agy.FailureSummary("upstream_capacity_exhausted")}
	text := requestFailureText(event)
	if !strings.Contains(text, "HTTP 502") || !strings.Contains(text, "No capacity available") || strings.ContainsFunc(text, func(r rune) bool { return unicode.Is(unicode.Han, r) }) || strings.Contains(text, "agy_request_failed") || strings.Contains(text, `{"`) {
		t.Fatalf("request log lost specific reason: %s", text)
	}
}

func TestAGYLogDetailsAreEnglishWithoutChangingStoredOrClientErrors(t *testing.T) {
	for _, tc := range []struct{ code, message, want string }{
		{"session_busy", "该 AGY 会话已有进行中的请求", "Session is busy"},
		{"agy_generation_failed", "AGY 上游调用失败，具体原因未能识别", "specific reason unavailable"},
		{"agent_execution_failed", "AGY 执行因错误终止", "execution terminated"},
		{"invalid_instructions", "instructions 必须为文本", "Instructions must be text"},
		{"opaque_context_unsupported", "不支持加密历史", "encrypted or opaque context"},
		{"unsupported_input_item", "不支持上下文类型", "input item type unsupported"},
		{"image_url_unsupported", "需要内嵌图片", "embedded image data"},
		{"context_too_large", "上下文过大", "input safety limit"},
		{"compaction_tools_pending", "原工具调用仍在交付", "still being delivered"},
		{"unknown_tool_result", "工具结果无法匹配原调用", "does not match an original call"},
		{"invalid_late_result", "补交结果无法核对", "original recovered client call"},
		{"late_result_context_changed", "补交指令不一致", "original model and instructions"},
		{"late_result_history_changed", "补交历史不一致", "No task was restarted"},
		{"late_result_resume_required", "原结果已保存，请先续接", "actual result is already recorded"},
		{"tool_followup_not_deliverable", "新增消息尚未提交", "has not been marked as processed"},
		{"native_session_missing", "原生会话不存在", "saved history is missing"},
		{"session_restore_unsafe", "历史状态不允许自动重建", "does not permit automatic recovery"},
		{"invalid_compaction_history", "工具链不完整", "execution origins do not match"},
		{"tool_record_unavailable", "索引保存失败", "No new checkpoint was committed"},
		{"ambiguous_tool_search", "发现执行来源不明确", "cannot be mixed"},
		{"invalid_remote_approval", "审批响应无效", "explicit approval response"},
		{"remote_authorization_changed", "远程授权改变", "approval was not executed"},
		{"remote_operation_pending", "原远程操作尚未完成", "still pending"},
		{"future_local_error", "尚未支持的新错误", "AGY request failed"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			stored, _ := json.Marshal(map[string]string{"code": tc.code, "message": tc.message})
			status := int64(409)
			event := cpamp.EventRow{Model: "agy/gemini", Failed: true, FailStatusCode: &status, FailSummary: string(stored)}
			text := requestFailureText(event)
			if !strings.Contains(text, "HTTP 409") || !strings.Contains(text, tc.want) || strings.ContainsFunc(text, func(r rune) bool { return unicode.Is(unicode.Han, r) }) || event.FailSummary != string(stored) {
				t.Fatalf("portal detail was not translated independently: %s", text)
			}
			w := httptest.NewRecorder()
			agy.WriteError(w, &agy.Error{Status: 409, Code: tc.code, Message: tc.message})
			var body struct {
				Error struct{ Code, Message string }
			}
			if json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Error.Message != tc.message || body.Error.Code != tc.code {
				t.Fatal("display translation changed the client error")
			}
			ordinary := event
			ordinary.Model = "other/model"
			if tc.code != "agy_generation_failed" && !strings.Contains(requestFailureText(ordinary), tc.message) {
				t.Fatal("AGY display translation changed another provider")
			}
		})
	}
}

func TestAGYPortalShowsOriginalDiagnosticInsteadOfTranslatedSummary(t *testing.T) {
	diagnostic := "RESOURCE_EXHAUSTED (code 429)\r\n  Native Stack: worker.go:123\n  原始诊断 <script>alert(1)</script>"
	stored, _ := json.Marshal(map[string]string{"code": "upstream_resource_exhausted", "message": "上游额度不足或请求受到限流", "cli_diagnostic": diagnostic})
	status := int64(429)
	event := cpamp.EventRow{Model: "agy/gemini", Failed: true, FailStatusCode: &status, FailSummary: string(stored)}
	if got := requestFailureText(event); got != "HTTP 429 · "+diagnostic {
		t.Fatalf("original CLI diagnostic was translated or lost: %s", got)
	}
	event.Model = "other/model"
	if strings.Contains(requestFailureText(event), "worker.go") {
		t.Fatal("AGY diagnostic display changed another provider")
	}
}
