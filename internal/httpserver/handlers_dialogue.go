package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"cliproxy-portal/internal/gateway"
	"cliproxy-portal/internal/store"
)

func (s *Server) downloadDialogue(w http.ResponseWriter, r *http.Request) {
	if s.CaptureVault == nil {
		http.NotFound(w, r)
		return
	}
	id := r.PathValue("id")
	row, err := s.Store.GatewayCaptureByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.errorPage(w, r, http.StatusInternalServerError, "交互记录暂时不可用", err)
		return
	}
	user := currentUser(r)
	if row.CreatedAt.Before(time.Now().Add(-7*24*time.Hour)) || (!strings.HasPrefix(r.URL.Path, "/admin/") && row.UserID != user.ID) {
		http.NotFound(w, r)
		return
	}
	if row.RequestContentType != gateway.StructuredCaptureContentType {
		http.NotFound(w, r)
		return
	}
	if s.downloadStructuredInteraction(w, r, row) && strings.HasPrefix(r.URL.Path, "/admin/") {
		s.audit(r, user, "gateway.dialogue.download", id, "用户与助手文本及实际工具交互")
	}
}

func (s *Server) downloadStructuredInteraction(w http.ResponseWriter, r *http.Request, row store.GatewayCapture) bool {
	requestEvents, err := s.captureSideEvents(r, row, "request")
	if err != nil {
		s.errorPage(w, r, http.StatusGone, "交互记录已清理", err)
		return false
	}
	responseEvents, err := s.captureSideEvents(r, row, "response")
	if err != nil {
		s.errorPage(w, r, http.StatusGone, "交互记录已清理", err)
		return false
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Query().Get("format") != "txt" {
		payload := struct {
			Format            string                `json:"format"`
			CreatedAt         string                `json:"created_at"`
			Model             string                `json:"model"`
			StatusCode        int                   `json:"status_code"`
			RequestTruncated  bool                  `json:"request_truncated"`
			ResponseTruncated bool                  `json:"response_truncated"`
			Request           []gateway.RecordEvent `json:"request"`
			Response          []gateway.RecordEvent `json:"response"`
		}{"cliproxy.interaction.v1", row.CreatedAt.Format(time.RFC3339), row.RequestedModel, row.StatusCode, row.RequestTruncated, row.ResponseTruncated, requestEvents, responseEvents}
		encoded, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			s.errorPage(w, r, http.StatusInternalServerError, "交互记录暂时不可用", err)
			return false
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="interaction-%s.json"`, row.ID))
		_, _ = w.Write(append(encoded, '\n'))
		return true
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="interaction-%s.txt"`, row.ID))
	_, _ = fmt.Fprintf(w, "模型交互记录\n时间：%s\n模型：%s\n状态：HTTP %d\n\n请求中的消息：\n", row.CreatedAt.Format(time.RFC3339), row.RequestedModel, row.StatusCode)
	writeRecordEvents(w, requestEvents)
	_, _ = fmt.Fprint(w, "\n本次模型响应：\n")
	writeRecordEvents(w, responseEvents)
	if row.RequestTruncated || row.ResponseTruncated {
		_, _ = fmt.Fprint(w, "\n注意：原始数据或结构化记录超过单侧 16 MB 上限，本记录可能不完整。\n")
	}
	return true
}

func (s *Server) captureSideEvents(r *http.Request, row store.GatewayCapture, side string) ([]gateway.RecordEvent, error) {
	parts, err := s.Store.GatewayCaptureMessages(r.Context(), row.ID, side)
	if err != nil {
		return nil, err
	}
	events := make([]gateway.RecordEvent, 0, len(parts))
	for _, part := range parts {
		if part.Role != gateway.StructuredMessageRole {
			return nil, errors.New("unexpected interaction record format")
		}
		value, err := s.CaptureVault.ReadSharedMessage(row.UserID, part.Role, part.ID)
		if err != nil {
			return nil, err
		}
		var event gateway.RecordEvent
		if err := json.Unmarshal([]byte(value), &event); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

func writeRecordEvents(w http.ResponseWriter, events []gateway.RecordEvent) {
	if len(events) == 0 {
		_, _ = fmt.Fprint(w, "（无可提取的文本或工具交互）\n")
		return
	}
	for index, event := range events {
		switch event.Type {
		case "message":
			role := "用户"
			if event.Role == "assistant" {
				role = "助手"
			}
			_, _ = fmt.Fprintf(w, "\n[%d] %s\n%s\n", index+1, role, event.Content)
		case "tool_call":
			_, _ = fmt.Fprintf(w, "\n[%d] 助手调用工具：%s\n", index+1, emptyLabel(event.ToolName))
			if event.ToolCallID != "" {
				_, _ = fmt.Fprintf(w, "调用 ID：%s\n", event.ToolCallID)
			}
			if event.Arguments != "" {
				_, _ = fmt.Fprintf(w, "参数：\n%s\n", event.Arguments)
			}
		case "tool_result":
			_, _ = fmt.Fprintf(w, "\n[%d] 工具结果：%s\n", index+1, emptyLabel(event.ToolName))
			if event.ToolCallID != "" {
				_, _ = fmt.Fprintf(w, "调用 ID：%s\n", event.ToolCallID)
			}
			if event.Result != "" {
				_, _ = fmt.Fprintf(w, "结果：\n%s\n", event.Result)
			}
		}
	}
}

func emptyLabel(value string) string {
	if value == "" {
		return "（未提供名称）"
	}
	return value
}
