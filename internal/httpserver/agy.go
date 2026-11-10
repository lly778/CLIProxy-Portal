package httpserver

import (
	"bytes"
	"cliproxy-portal/internal/agy"
	"cliproxy-portal/internal/webui"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

func (s *Server) loadAGYView(r *http.Request, v *webui.AdminUpstreamsView) {
	v.AGYConfigured = s.Keys.AGY != nil
	settings, e := s.Store.AGYSettings(r.Context())
	if e != nil {
		v.AGYError = "AGY 配置暂时无法读取"
		return
	}
	v.AGY = settings
	if !v.AGYConfigured {
		return
	}
	snapshot, e := s.Keys.AGYQuota(r.Context(), false)
	if e != nil {
		v.AGYError = "官方账号额度暂时无法读取"
		return
	}
	v.AGYLoggedIn = snapshot.Account.LoggedIn
	v.AGYAccount = webui.UpstreamAccountView{Account: snapshot.Account.Email, StatusLabel: "已启用", QuotaStatus: "暂无额度数据"}
	if v.AGYAccount.Account == "" && v.AGYLoggedIn {
		v.AGYAccount.Account = "已登录的 Google 账号"
	}
	for _, group := range snapshot.Groups {
		view := webui.UpstreamAccountQuotaGroupView{Label: group.Label}
		for _, window := range group.Windows {
			class := "success"
			if window.RemainingPercent < 20 {
				class = "danger"
			} else if window.RemainingPercent < 50 {
				class = "warning"
			}
			reset := "—"
			if !window.ResetAt.IsZero() {
				reset = s.formatTime(window.ResetAt)
			}
			view.Quotas = append(view.Quotas, webui.UpstreamAccountQuotaView{Label: window.Label, RemainingPercent: window.RemainingPercent, StatusClass: class, ResetAt: reset})
		}
		v.AGYAccount.QuotaGroups = append(v.AGYAccount.QuotaGroups, view)
	}
	if snapshot.Error != "" {
		v.AGYError = snapshot.Error
		if snapshot.Stale {
			v.AGYError += "，当前显示上次同步的额度"
		}
	}
}

func (s *Server) adminAGYAccountStart(w http.ResponseWriter, r *http.Request) {
	s.adminAGYAccountAction(w, r, "/internal/account/login")
}
func (s *Server) adminAGYAccountFinish(w http.ResponseWriter, r *http.Request) {
	s.adminAGYAccountAction(w, r, "/internal/account/finish")
}
func (s *Server) adminAGYAccountLogout(w http.ResponseWriter, r *http.Request) {
	s.adminAGYAccountAction(w, r, "/internal/account/logout")
}
func (s *Server) adminAGYAccountCancel(w http.ResponseWriter, r *http.Request) {
	s.adminAGYAccountAction(w, r, "/internal/account/cancel")
}
func (s *Server) adminAGYAccountAction(w http.ResponseWriter, r *http.Request, path string) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if r.ParseForm() != nil || !s.verifyCSRF(r) {
		clientConfigJSON(w, 403, map[string]string{"error": "请求已失效，请刷新页面"})
		return
	}
	if s.Keys.AGY == nil {
		clientConfigJSON(w, 503, map[string]string{"error": "AGY 服务尚未配置"})
		return
	}
	ctx, done := context.WithTimeout(r.Context(), 55*time.Second)
	defer done()
	body, _ := json.Marshal(map[string]string{"id": r.FormValue("id"), "code": r.FormValue("code")})
	resp, err := s.Keys.AGY.Request(ctx, "POST", path, "admin:"+currentUser(r).ID, bytes.NewReader(body))
	if err != nil {
		clientConfigJSON(w, 503, map[string]string{"error": "官方账号操作未完成，请重试"})
		return
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		clientConfigJSON(w, 503, map[string]string{"error": "读取操作结果失败"})
		return
	}
	if resp.StatusCode >= 400 {
		var v struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &v)
		message := v.Error.Message
		if message == "" {
			message = "官方账号操作失败，请重试"
		}
		clientConfigJSON(w, resp.StatusCode, map[string]string{"error": message})
		return
	}
	if path == "/internal/account/login" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
		return
	}
	if path == "/internal/account/cancel" {
		clientConfigJSON(w, 200, map[string]string{"message": "已取消授权"})
		return
	}
	message := "已退出账号"
	if path == "/internal/account/finish" {
		message = "账号登录成功"
		if err = s.refreshAGYModels(ctx); err != nil {
			message = "账号已登录，模型目录请稍后刷新"
		}
	}
	s.Keys.InvalidateUpstreamReadCache()
	s.audit(r, currentUser(r), "agy.account.change", "agy", message)
	clientConfigJSON(w, 200, map[string]string{"message": message})
}
func (s *Server) refreshAGYModels(ctx context.Context) error {
	if s.Keys.AGY == nil {
		return nil
	}
	var models []agy.Model
	if err := s.Keys.AGY.Read(ctx, "/internal/catalog", &models); err != nil {
		return err
	}
	v, e := s.Store.AGYSettings(ctx)
	if e != nil {
		return e
	}
	expected := v.Revision
	rules := map[string]agy.ModelRule{}
	for _, rule := range v.Rules {
		rules[rule.ID] = rule
	}
	v.Models = models
	v.Rules = nil
	for _, m := range models {
		rule := rules[m.ID]
		rule.ID = m.ID
		valid := func(value string) bool {
			if value == "" {
				return true
			}
			for _, l := range m.Levels {
				if value == l {
					return true
				}
			}
			return false
		}
		if !valid(rule.Effort) {
			rule.Effort = ""
		}
		if !valid(rule.Cap) {
			rule.Cap = ""
		}
		v.Rules = append(v.Rules, rule)
	}
	if e = s.Store.SaveAGYSettings(ctx, v, expected); e != nil {
		return e
	}
	s.Keys.InvalidateUpstreamReadCache()
	return nil
}
