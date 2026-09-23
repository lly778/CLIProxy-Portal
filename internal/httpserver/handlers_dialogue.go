package httpserver

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

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
		s.errorPage(w, r, http.StatusInternalServerError, "对话记录暂时不可用", err)
		return
	}
	user := currentUser(r)
	if row.CreatedAt.Before(time.Now().Add(-7*24*time.Hour)) || (!strings.HasPrefix(r.URL.Path, "/admin/") && row.UserID != user.ID) {
		http.NotFound(w, r)
		return
	}
	requestText, err := s.captureSideDialogue(r, row, "request")
	if err != nil {
		s.errorPage(w, r, http.StatusGone, "对话记录已清理", err)
		return
	}
	responseText, err := s.captureSideDialogue(r, row, "response")
	if err != nil {
		s.errorPage(w, r, http.StatusGone, "对话记录已清理", err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="dialogue-%s.txt"`, id))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = fmt.Fprintf(w, "时间：%s\n模型：%s\n状态：HTTP %d\n\n用户与助手对话（请求）：\n%s\n\n助手回复：\n%s\n", row.CreatedAt.Format(time.RFC3339), row.RequestedModel, row.StatusCode, requestText, responseText)
	if row.RequestTruncated || row.ResponseTruncated {
		_, _ = fmt.Fprint(w, "\n注意：原始数据超过单侧 16 MB 上限，本记录可能不完整。\n")
	}
	if strings.HasPrefix(r.URL.Path, "/admin/") {
		s.audit(r, user, "gateway.dialogue.download", id, "仅用户及助手文本")
	}
}

func (s *Server) captureSideDialogue(r *http.Request, row store.GatewayCapture, side string) (string, error) {
	parts, err := s.Store.GatewayCaptureMessages(r.Context(), row.ID, side)
	if err != nil {
		return "", err
	}
	if len(parts) == 0 {
		legacy, err := s.CaptureVault.Read(row.ID, side)
		return string(legacy), err
	}
	var text strings.Builder
	for _, part := range parts {
		message, err := s.CaptureVault.ReadSharedMessage(row.UserID, part.Role, part.ID)
		if err != nil {
			return "", err
		}
		if text.Len() > 0 {
			text.WriteString("\n\n")
		}
		text.WriteString(part.Role)
		text.WriteString("：\n")
		text.WriteString(message)
	}
	return text.String(), nil
}
