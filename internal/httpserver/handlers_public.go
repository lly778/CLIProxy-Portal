package httpserver

import (
	"context"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/webui"
)

func (s *Server) loginGet(w http.ResponseWriter, r *http.Request) {
	if token := s.sessionToken(r); token != "" {
		if _, _, err := s.Accounts.ResolveSession(r.Context(), token); err == nil {
			http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
			return
		}
	}
	v := webui.LoginView{LayoutView: webui.LayoutView{Title: "登录", Brand: "CLIProxy 账号门户", CSRFToken: s.preCSRF(w, r)}, Phone: r.URL.Query().Get("phone"), Next: r.URL.Query().Get("next")}
	if msg := r.URL.Query().Get("msg"); msg != "" {
		v.Flash = &webui.FlashView{Kind: "success", Message: msg}
	}
	_ = s.UI.Render(w, webui.PageLogin, v)
}
func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	csrf := s.preCSRF(w, r)
	if !s.verifyCSRF(r) {
		s.errorPage(w, r, 403, "请求已失效，请重试", nil)
		return
	}
	phone := r.FormValue("phone")
	ip := s.clientIP(r)
	key := "login:" + ip + ":" + phone
	if !s.limit.Allow(key, 10, time.Hour, 15*time.Minute) {
		s.renderLoginError(w, csrf, phone, "尝试次数过多，请稍后再试")
		return
	}
	u, err := s.Accounts.Authenticate(r.Context(), phone, r.FormValue("password"))
	if err != nil {
		s.renderLoginError(w, csrf, phone, service.ErrInvalidCredentials.Error())
		return
	}
	s.limit.Reset(key)
	token, _, err := s.Accounts.CreateSession(r.Context(), u, ip, r.UserAgent())
	if err != nil {
		s.errorPage(w, r, 500, "登录暂时不可用", err)
		return
	}
	max := int((30 * 24 * time.Hour).Seconds())
	if u.IsAdmin() {
		max = int((8 * time.Hour).Seconds())
	}
	s.setSession(w, token, max)
	next := r.FormValue("next")
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/dashboard"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}
func (s *Server) renderLoginError(w http.ResponseWriter, csrf, phone, msg string) {
	w.WriteHeader(http.StatusUnauthorized)
	_ = s.UI.Render(w, webui.PageLogin, webui.LoginView{LayoutView: webui.LayoutView{Title: "登录", Brand: "CLIProxy 账号门户", CSRFToken: csrf, Error: msg}, Phone: phone})
}

func (s *Server) registerGet(w http.ResponseWriter, r *http.Request) {
	open, _ := s.Store.RegistrationOpen(r.Context())
	policy, err := s.Store.LatestPolicy(r.Context())
	if err != nil {
		s.errorPage(w, r, 500, "注册暂时不可用", err)
		return
	}
	v := webui.RegisterView{LayoutView: webui.LayoutView{Title: "申请注册", Brand: "CLIProxy 账号门户", CSRFToken: s.preCSRF(w, r)}, Rules: []webui.RuleView{{ID: "accept", Title: policy.Title, Body: policy.Body}}, RulesVersion: itoa(policy.Version), RulesRequired: true}
	if !open {
		v.Error = "当前已暂停新注册"
	}
	_ = s.UI.Render(w, webui.PageRegister, v)
}
func (s *Server) registerPost(w http.ResponseWriter, r *http.Request) {
	csrf := s.preCSRF(w, r)
	if !s.verifyCSRF(r) {
		s.errorPage(w, r, 403, "请求已失效，请重试", nil)
		return
	}
	open, err := s.Store.RegistrationOpen(r.Context())
	if err != nil {
		s.errorPage(w, r, 500, "注册暂时不可用", err)
		return
	}
	if !open {
		s.renderRegisterError(w, csrf, "当前已暂停新注册")
		return
	}
	ip := s.clientIP(r)
	if !s.limit.Allow("register:"+ip, 5, time.Hour, time.Hour) {
		s.renderRegisterError(w, csrf, "提交过于频繁，请稍后再试")
		return
	}
	if r.FormValue("password") != r.FormValue("confirm_password") {
		s.renderRegisterError(w, csrf, "两次输入的密码不一致")
		return
	}
	if r.FormValue("rule") != "accept" {
		s.renderRegisterError(w, csrf, "请先确认使用规则")
		return
	}
	version := parseInt(r.FormValue("rules_version"), 0)
	_, err = s.Accounts.Register(r.Context(), r.FormValue("phone"), r.FormValue("name"), r.FormValue("password"), version, ip)
	if err != nil {
		s.renderRegisterError(w, csrf, err.Error())
		return
	}
	http.Redirect(w, r, "/login?msg="+urlQuery("注册申请已提交，请等待管理员审批"), http.StatusSeeOther)
}
func (s *Server) renderRegisterError(w http.ResponseWriter, csrf, msg string) {
	policy, _ := s.Store.LatestPolicy(rctx())
	w.WriteHeader(http.StatusBadRequest)
	_ = s.UI.Render(w, webui.PageRegister, webui.RegisterView{LayoutView: webui.LayoutView{Title: "申请注册", Brand: "CLIProxy 账号门户", CSRFToken: csrf, Error: msg}, Rules: []webui.RuleView{{ID: "accept", Title: policy.Title, Body: policy.Body}}, RulesVersion: itoa(policy.Version), RulesRequired: true})
}

func (s *Server) resetGet(w http.ResponseWriter, r *http.Request) {
	v := webui.ResetView{LayoutView: webui.LayoutView{Title: "重置密码", Brand: "CLIProxy 账号门户", CSRFToken: s.preCSRF(w, r)}, MinimumLength: 10}
	_ = s.UI.Render(w, webui.PageReset, v)
}
func (s *Server) resetPost(w http.ResponseWriter, r *http.Request) {
	csrf := s.preCSRF(w, r)
	if !s.verifyCSRF(r) {
		s.errorPage(w, r, 403, "请求已失效，请重试", nil)
		return
	}
	ip := s.clientIP(r)
	if !s.limit.Allow("reset:"+ip, 5, time.Hour, time.Hour) {
		s.renderResetError(w, csrf, "尝试次数过多，请稍后再试")
		return
	}
	if r.FormValue("password") != r.FormValue("confirm_password") {
		s.renderResetError(w, csrf, "两次输入的密码不一致")
		return
	}
	if err := s.Accounts.ResetPassword(r.Context(), r.FormValue("phone"), r.FormValue("code"), r.FormValue("password"), ip); err != nil {
		s.renderResetError(w, csrf, err.Error())
		return
	}
	http.Redirect(w, r, "/login?msg="+urlQuery("密码已重置，请重新登录"), http.StatusSeeOther)
}
func (s *Server) renderResetError(w http.ResponseWriter, csrf, msg string) {
	w.WriteHeader(http.StatusBadRequest)
	_ = s.UI.Render(w, webui.PageReset, webui.ResetView{LayoutView: webui.LayoutView{Title: "重置密码", Brand: "CLIProxy 账号门户", CSRFToken: csrf, Error: msg}, MinimumLength: 10})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if !s.verifyCSRF(r) {
		s.errorPage(w, r, 403, "请求已失效", nil)
		return
	}
	_ = s.Accounts.Logout(r.Context(), currentToken(r))
	s.clearSession(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
func (s *Server) rules(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.LatestPolicy(r.Context())
	if err != nil {
		s.errorPage(w, r, 500, "规则暂时不可用", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("<!doctype html><meta charset=utf-8><title>" + htmlEscape(p.Title) + "</title><main><h1>" + htmlEscape(p.Title) + "</h1><p>版本 " + itoa(p.Version) + "</p><pre style=white-space:pre-wrap>" + htmlEscape(p.Body) + "</pre><p><a href=/>返回门户</a></p></main>"))
}

func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, status int, message string, err error) {
	if err != nil {
		s.Logger.Error("request failed", "path", r.URL.Path, "error", err)
	}
	w.WriteHeader(status)
	_ = s.UI.Render(w, webui.PageError, webui.ErrorView{LayoutView: webui.LayoutView{Title: "错误", Brand: "CLIProxy 账号门户"}, StatusCode: status, Message: message, CanGoBack: true})
}

func rctx() context.Context { return context.Background() }
func itoa(v int) string     { return strconv.Itoa(v) }
func parseInt(v string, fallback int) int {
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}
func urlQuery(v string) string   { return url.QueryEscape(v) }
func htmlEscape(v string) string { return template.HTMLEscapeString(v) }
