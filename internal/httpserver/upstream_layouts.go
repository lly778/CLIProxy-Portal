package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"cliproxy-portal/internal/store"
)

func (s *Server) upstreamCardOrder(ctx context.Context, scope string) (string, string) {
	order, err := s.Store.UpstreamCardOrder(ctx, scope)
	if err != nil {
		s.Logger.Error("read shared card layout", "scope", scope, "error", err)
		return "[]", "共享排列暂时无法读取，请稍后刷新。"
	}
	payload, _ := json.Marshal(order)
	return string(payload), ""
}

func (s *Server) adminUpstreamLayoutSave(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fail := func(code int, message string) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": message})
	}
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	if err := r.ParseForm(); err != nil {
		fail(http.StatusBadRequest, "布局表单无效或过大")
		return
	}
	if !s.verifyCSRF(r) {
		fail(http.StatusForbidden, "请求已失效，请刷新后重试")
		return
	}
	scope := r.PostForm.Get("scope")
	var order []string
	if err := json.Unmarshal([]byte(r.PostForm.Get("order")), &order); err != nil || order == nil {
		fail(http.StatusBadRequest, "布局顺序无效")
		return
	}
	if err := store.ValidateUpstreamCardLayout(scope, order); err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Store.SetUpstreamCardOrder(r.Context(), scope, order); err != nil {
		s.Logger.Error("save shared card layout", "scope", scope, "error", err)
		fail(http.StatusInternalServerError, "共享布局保存失败，请稍后重试")
		return
	}
	s.audit(r, currentUser(r), "upstream_layout.update", scope, "共享卡片排列；"+strconv.Itoa(len(order))+" 张卡片，不改变模型配置")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
