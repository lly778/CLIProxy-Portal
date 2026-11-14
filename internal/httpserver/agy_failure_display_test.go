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
		{"compaction_tools_pending", "原工具调用仍在交付", "still being delivered"},
		{"unknown_tool_result", "工具结果无法匹配原调用", "does not match an original call"},
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
