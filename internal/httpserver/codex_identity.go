package httpserver

import (
	"encoding/json"
	"net/http"
	"strings"
)

func (s *Server) adminCodexIdentityCompatibility(w http.ResponseWriter, r *http.Request) {
	if !s.verifyCSRF(r) {
		s.errorPage(w, r, http.StatusForbidden, "请求已失效，请重试", nil)
		return
	}
	channel, ok := s.upstreamChannel(w, r)
	if !ok {
		return
	}
	if channel == "codex" {
		s.errorPage(w, r, http.StatusBadRequest, "Codex 渠道不支持删除 GPT-5 身份描述", nil)
		return
	}
	if s.Cfg.GatewayListenAddr == "" {
		s.errorPage(w, r, http.StatusServiceUnavailable, "请先启用门户网关", nil)
		return
	}
	values := r.PostForm["enabled"]
	if len(values) > 1 {
		s.errorPage(w, r, http.StatusBadRequest, "指令兼容选项无效", nil)
		return
	}
	value := "false" // An unchecked checkbox submits no value.
	if len(values) == 1 {
		value = values[0]
	}
	if value != "true" && value != "false" {
		s.errorPage(w, r, http.StatusBadRequest, "指令兼容选项无效", nil)
		return
	}
	enabled := value == "true"
	if err := s.Store.SetCodexIdentityCompatibility(r.Context(), channel, enabled); err != nil {
		s.errorPage(w, r, http.StatusInternalServerError, "保存指令兼容设置失败", err)
		return
	}
	message := "已关闭 GPT-5 身份描述删除"
	if enabled {
		message = "已开启 GPT-5 身份描述删除"
	}
	s.audit(r, currentUser(r), "upstream.codex_identity.update", channel, message)
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"enabled": enabled})
		return
	}
	http.Redirect(w, r, upstreamRedirectURL(channel, "msg", message), http.StatusSeeOther)
}
