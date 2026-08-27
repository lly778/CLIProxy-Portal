package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/security"
	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/webui"
)

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	if currentUser(r).IsAdmin() {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u.Status != domain.StatusApproved {
		http.Redirect(w, r, "/status", http.StatusSeeOther)
		return
	}
	from, to := time.Now().UTC().AddDate(0, 0, -7), time.Now().UTC()
	analytics, err := s.Keys.Usage(r.Context(), u.ID, from, to, 10)
	summary, _, _, requests := s.usageViews(analytics, from, to)
	v := webui.DashboardView{LayoutView: s.layout(u, currentToken(r), "账户概览", "dashboard"), Greeting: u.Name + "，你好", StatusCard: s.statusCard(u), Key: s.keyView(u), Usage: summary, RecentUsage: requests}
	if msg := r.URL.Query().Get("quota_msg"); msg != "" {
		v.Flash = &webui.FlashView{Kind: "success", Message: msg}
	}
	v.ShowClaimHint = !v.Key.CanRevoke
	if quota, _ := s.Keys.UpstreamQuota(r.Context()); quota.TotalAccounts > 0 {
		v.Quota = s.quotaPoolView(quota, v.CSRFToken, "/dashboard")
	}
	if err != nil {
		v.Notice = "用量数据暂时不可用"
	}
	if models, e := s.Keys.Models(r.Context()); e == nil {
		for i, m := range models {
			if i >= 6 {
				break
			}
			v.Models = append(v.Models, webui.ModelView{Name: m.ID, Provider: m.OwnedBy, Available: true})
		}
	}
	_ = s.UI.Render(w, webui.PageDashboard, v)
}

func (s *Server) statusPage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	v := webui.StatusView{LayoutView: s.layout(u, currentToken(r), "账号状态", "status"), StatusCard: s.statusCard(u)}
	v.CanResubmit = u.Status == domain.StatusRejected
	if msg := r.URL.Query().Get("msg"); msg != "" {
		v.Flash = &webui.FlashView{Kind: "success", Message: msg}
	}
	v.Timeline = append(v.Timeline, webui.StatusEventView{Status: "pending", Label: "提交注册申请", At: s.formatTime(u.CreatedAt), Current: u.Status == domain.StatusPending})
	if !u.ApprovedAt.IsZero() {
		v.Timeline = append(v.Timeline, webui.StatusEventView{Status: "approved", Label: "管理员已批准", At: s.formatTime(u.ApprovedAt), Current: u.Status == domain.StatusApproved})
	}
	if u.Status == domain.StatusRejected {
		v.Timeline = append(v.Timeline, webui.StatusEventView{Status: "rejected", Label: "申请被拒绝", At: s.formatTime(u.UpdatedAt), Note: u.RejectionReason, Current: true})
		v.Notices = append(v.Notices, webui.NoticeView{Kind: "warning", Title: "可以重新提交", Message: "请在下方修正资料后重新申请。"})
	}
	_ = s.UI.Render(w, webui.PageStatus, v)
}

func (s *Server) resubmitGet(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u.Status != domain.StatusRejected {
		http.Redirect(w, r, "/status", http.StatusSeeOther)
		return
	}
	s.renderResubmit(w, r, u, "")
}
func (s *Server) resubmitPost(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u.Status != domain.StatusRejected {
		http.Redirect(w, r, "/status", http.StatusSeeOther)
		return
	}
	if !s.verifyCSRF(r) {
		s.errorPage(w, r, 403, "请求已失效，请重试", nil)
		return
	}
	if r.FormValue("password") != r.FormValue("confirm_password") {
		s.renderResubmit(w, r, u, "两次输入的密码不一致")
		return
	}
	if r.FormValue("rule") != "accept" {
		s.renderResubmit(w, r, u, "请先确认使用规则")
		return
	}
	if err := s.Accounts.Resubmit(r.Context(), u, r.FormValue("name"), r.FormValue("password"), parseInt(r.FormValue("rules_version"), 0), s.clientIP(r)); err != nil {
		s.renderResubmit(w, r, u, err.Error())
		return
	}
	http.Redirect(w, r, "/status?msg="+urlQuery("申请已重新提交"), http.StatusSeeOther)
}
func (s *Server) renderResubmit(w http.ResponseWriter, r *http.Request, u domain.User, msg string) {
	p, err := s.Store.LatestPolicy(r.Context())
	if err != nil {
		s.errorPage(w, r, 500, "规则暂时不可用", err)
		return
	}
	v := webui.ResubmitView{LayoutView: s.layout(u, currentToken(r), "重新提交", "status"), Name: u.Name, Rules: []webui.RuleView{{ID: "accept", Title: p.Title, Body: p.Body}}, RulesVersion: strconv.Itoa(p.Version)}
	v.Error = msg
	if msg != "" {
		w.WriteHeader(http.StatusBadRequest)
	}
	_ = s.UI.Render(w, webui.PageResubmit, v)
}

func (s *Server) keyPage(w http.ResponseWriter, r *http.Request) {
	s.renderKey(w, r, "", "", domain.APIKey{})
}
func (s *Server) keyTest(w http.ResponseWriter, r *http.Request) {
	if !s.verifyCSRF(r) {
		s.errorPage(w, r, http.StatusForbidden, "请求已失效，请重试", nil)
		return
	}
	u := currentUser(r)
	models, err := s.Keys.TestKey(r.Context(), u.ID)
	if err != nil {
		if errors.Is(err, service.ErrKeyExternallyRevoked) {
			s.audit(r, u, "key.test.external_revoked", u.ID, "CPA 中已不存在")
			s.renderKey(w, r, "API Key 已从 CPA 或 CPAMP 删除，门户已同步为“外部撤销”，现在可以重新领取。", "", domain.APIKey{})
			return
		}
		s.renderKey(w, r, err.Error(), "", domain.APIKey{})
		return
	}
	s.audit(r, u, "key.test", u.ID, fmt.Sprintf("发现 %d 个模型", len(models)))
	s.renderKey(w, r, "", fmt.Sprintf("API Key 可用，已发现 %d 个模型。实际模型连接请前往“可用模型”页面测试。", len(models)), domain.APIKey{})
}
func (s *Server) keyClaim(w http.ResponseWriter, r *http.Request) {
	if !s.verifyCSRF(r) {
		s.errorPage(w, r, 403, "请求已失效，请重试", nil)
		return
	}
	u := currentUser(r)
	if !security.VerifyPassword(u.PasswordHash, r.FormValue("password")) {
		s.renderKey(w, r, "请重新输入正确的登录密码", "", domain.APIKey{})
		return
	}
	secret, key, err := s.Keys.Issue(r.Context(), u)
	if err != nil {
		s.renderKey(w, r, err.Error(), "", domain.APIKey{})
		return
	}
	s.audit(r, u, "key.issue", key.ID, "claim")
	s.renderKey(w, r, "", secret, key)
}
func (s *Server) keyRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.verifyCSRF(r) {
		s.errorPage(w, r, 403, "请求已失效，请重试", nil)
		return
	}
	u := currentUser(r)
	if !security.VerifyPassword(u.PasswordHash, r.FormValue("password")) {
		s.renderKey(w, r, "请重新输入正确的登录密码", "", domain.APIKey{})
		return
	}
	pending, err := s.Keys.Revoke(r.Context(), u)
	if err != nil || pending {
		key, keyErr := s.Store.ActiveKey(r.Context(), u.ID)
		if keyErr != nil {
			s.renderKey(w, r, "撤销状态无法确认，请稍后重试", "", domain.APIKey{})
			return
		}
		jobErr := s.Store.EnqueueJob(r.Context(), domain.SyncJob{UserID: u.ID, Action: "revoke", KeyHash: key.Hash, NextAt: time.Now().UTC().Add(s.Cfg.PendingRetry), CreatedAt: time.Now().UTC()})
		if jobErr != nil {
			s.renderKey(w, r, "无法建立撤销重试任务，请联系管理员", "", domain.APIKey{})
			return
		}
		s.audit(r, u, "key.revoke.pending", key.ID, "等待 CPAMP 确认")
		s.renderKey(w, r, "", "撤销正在后台重试", domain.APIKey{})
		return
	}
	msg := "API Key 已撤销"
	s.audit(r, u, "key.revoke", u.ID, msg)
	s.renderKey(w, r, "", msg, domain.APIKey{})
}
func (s *Server) keyAcknowledge(w http.ResponseWriter, r *http.Request) {
	if !s.verifyCSRF(r) {
		s.errorPage(w, r, 403, "请求已失效", nil)
		return
	}
	http.Redirect(w, r, "/key", http.StatusSeeOther)
}
func (s *Server) renderKey(w http.ResponseWriter, r *http.Request, errMsg, secretOrFlash string, key domain.APIKey) {
	u := currentUser(r)
	v := webui.KeyPageView{LayoutView: s.layout(u, currentToken(r), "API Key", "key"), User: s.userView(u), Key: s.keyView(u)}
	v.Error = errMsg
	if key.ID != "" {
		v.Key.Status = "active"
		v.Key.StatusLabel = "正常"
		v.Key.Last4 = key.LastFour
		v.Key.Alias = key.Alias
		v.Key.OneTimeSecret = secretOrFlash
		v.Key.APIBaseURL = s.Cfg.CPAAPIBaseURL
		v.Key.ConfigSnippet = "OPENAI_BASE_URL=" + s.Cfg.CPAAPIBaseURL + "/v1\nOPENAI_API_KEY=" + secretOrFlash
		v.Key.Examples = []webui.ConfigExampleView{{Name: "OpenAI 兼容", Hint: "环境变量", Code: v.Key.ConfigSnippet}, {Name: "Codex", Hint: "环境变量", Code: v.Key.ConfigSnippet}}
	} else if secretOrFlash != "" {
		v.Flash = &webui.FlashView{Kind: "success", Message: secretOrFlash}
	}
	if errMsg != "" {
		w.WriteHeader(http.StatusBadRequest)
	}
	_ = s.UI.Render(w, webui.PageKey, v)
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	from, to, rangeName := s.usageRange(r)
	a, err := s.Keys.Usage(r.Context(), u.ID, from, to, 0)
	summary, daily, models, requests := s.usageViews(a, from, to)
	modelsByRequests, modelsByTokens := modelUsageCharts(models)
	formFrom, formTo := s.usageRangeFormDates(from, to, rangeName)
	if rangeName == "today" {
		summary.WindowLabel = "今天"
	}
	v := webui.UsageView{LayoutView: s.layout(u, currentToken(r), "我的使用量", "usage"), FormAction: "/usage", From: formFrom, To: formTo, Range: rangeName, Summary: summary, Daily: daily, Trend: usageTrend(daily), ByModelRequests: modelsByRequests, ByModelTokens: modelsByTokens, Requests: requests}
	if msg := r.URL.Query().Get("quota_msg"); msg != "" {
		v.Flash = &webui.FlashView{Kind: "success", Message: msg}
	}
	if quota, _ := s.Keys.UpstreamQuota(r.Context()); quota.TotalAccounts > 0 {
		v.Quota = s.quotaPoolView(quota, v.CSRFToken, "/usage")
	}
	if err != nil {
		v.Error = "用量服务暂时不可用"
	}
	_ = s.UI.Render(w, webui.PageUsage, v)
}

func (s *Server) quotaRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.verifyCSRF(r) {
		s.errorPage(w, r, http.StatusForbidden, "请求已失效，请重试", nil)
		return
	}
	next := r.FormValue("next")
	if next != "/usage" && next != "/dashboard" {
		next = "/dashboard"
	}
	status, err := s.Keys.StartQuotaRefresh()
	message := status.Message
	if err != nil {
		message = err.Error()
	} else {
		s.audit(r, currentUser(r), "quota.refresh", "codex", "用户手动刷新")
	}
	http.Redirect(w, r, next+"?quota_msg="+urlQuery(message), http.StatusSeeOther)
}

func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	to := time.Now().UTC()
	from := u.CreatedAt.UTC()
	if from.IsZero() || !from.Before(to) {
		from = to.AddDate(-10, 0, 0)
	}
	a, err := s.Keys.Usage(r.Context(), u.ID, from, to, 100)
	_, _, _, requests := s.usageViews(a, from, to)
	v := webui.ActivityView{LayoutView: s.layout(u, currentToken(r), "模型请求日志", "activity"), Requests: requests}
	if err != nil {
		v.Error = "模型请求日志暂时不可用"
	}
	_ = s.UI.Render(w, webui.PageActivity, v)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	v := webui.HealthView{LayoutView: s.layout(u, currentToken(r), "系统状态", "health")}
	start := time.Now()
	err := s.Store.Ping(r.Context())
	v.Checks = append(v.Checks, healthView("门户服务", err, time.Since(start), s))
	start = time.Now()
	_, err = s.Keys.CPAMP.Health(r.Context())
	v.Checks = append(v.Checks, healthView("用量与管理服务", err, time.Since(start), s))
	_ = s.UI.Render(w, webui.PageHealth, v)
}

func (s *Server) usageRange(r *http.Request) (time.Time, time.Time, string) {
	now := time.Now().UTC()
	name := r.URL.Query().Get("range")
	if name == "" {
		name = "7d"
	}
	days := 7
	if name == "30d" {
		days = 30
	}
	to := now
	from := now.AddDate(0, 0, -days)
	if name == "today" {
		localNow := now.In(s.Cfg.TimeZone)
		from = time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, s.Cfg.TimeZone).UTC()
	} else if name == "custom" {
		f, e1 := time.ParseInLocation("2006-01-02", r.URL.Query().Get("from"), s.Cfg.TimeZone)
		t, e2 := time.ParseInLocation("2006-01-02", r.URL.Query().Get("to"), s.Cfg.TimeZone)
		if e1 == nil && e2 == nil && t.After(f) && t.Sub(f) <= 93*24*time.Hour {
			from = f.UTC()
			to = t.Add(24 * time.Hour).UTC()
		} else {
			name = "7d"
		}
	}
	return from, to, name
}

func (s *Server) usageRangeFormDates(from, to time.Time, name string) (string, string) {
	localFrom := from.In(s.Cfg.TimeZone)
	localTo := to.In(s.Cfg.TimeZone)
	if name == "custom" {
		localTo = localTo.AddDate(0, 0, -1)
	}
	return localFrom.Format("2006-01-02"), localTo.Format("2006-01-02")
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	items, err := s.Keys.TestKey(r.Context(), u.ID)
	s.renderModels(w, r, items, err)
}

func (s *Server) modelsTest(w http.ResponseWriter, r *http.Request) {
	if !s.verifyCSRF(r) {
		s.writeModelTestJSON(w, http.StatusForbidden, modelTestResponse{Status: "danger", StatusLabel: "请求失败", Message: "请求已失效，请刷新页面后重试。"})
		return
	}
	u := currentUser(r)
	selected := strings.TrimSpace(r.FormValue("model"))
	result, err := s.Keys.TestModel(r.Context(), u.ID, selected, s.Cfg.CPAAPIBaseURL)
	now := time.Now().UTC()
	record := domain.ModelTestResult{UserID: u.ID, Model: selected, Status: "success", Message: "已完成一次最小真实请求，Key、CPA 与模型上游链路正常。", LatencyMS: result.Latency.Milliseconds(), TestedAt: now}
	if err != nil {
		if errors.Is(err, service.ErrKeyExternallyRevoked) {
			s.audit(r, u, "key.test.external_revoked", u.ID, "CPA 中已不存在")
		}
		s.audit(r, u, "key.model_test.failed", selected, err.Error())
		record.Status, record.Message = "danger", err.Error()
		if strings.Contains(err.Error(), "已连接") {
			record.Status = "warning"
		}
	} else {
		latency := result.Latency.Round(time.Millisecond)
		s.audit(r, u, "key.model_test", selected, "latency="+latency.String())
	}
	if selected != "" && len(selected) <= 256 && !strings.ContainsAny(selected, "\r\n\x00") {
		if saveErr := s.Store.UpsertModelTestResult(r.Context(), record); saveErr != nil {
			s.Logger.Error("save model test result", "user_id", u.ID, "model", selected, "error", saveErr)
		}
	}
	response := modelTestResponse{OK: err == nil, Model: selected, Status: record.Status, StatusLabel: modelTestStatusLabel(record.Status), Message: record.Message, Latency: modelTestLatency(record.LatencyMS), TestedAt: s.formatTime(record.TestedAt)}
	s.writeModelTestJSON(w, http.StatusOK, response)
}

type modelTestResponse struct {
	OK          bool   `json:"ok"`
	Model       string `json:"model,omitempty"`
	Status      string `json:"status"`
	StatusLabel string `json:"statusLabel"`
	Message     string `json:"message"`
	Latency     string `json:"latency,omitempty"`
	TestedAt    string `json:"testedAt,omitempty"`
}

func (s *Server) writeModelTestJSON(w http.ResponseWriter, status int, response modelTestResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}

func modelTestStatusLabel(status string) string {
	switch status {
	case "success":
		return "连接成功"
	case "warning":
		return "已连接但不可用"
	default:
		return "连接失败"
	}
}

func modelTestLatency(ms int64) string {
	if ms <= 0 {
		return "<1 ms"
	}
	return fmt.Sprintf("%d ms", ms)
}

func (s *Server) renderModels(w http.ResponseWriter, r *http.Request, items []cpamp.Model, catalogErr error) {
	u := currentUser(r)
	v := webui.ModelsView{LayoutView: s.layout(u, currentToken(r), "可用模型", "models"), CheckedAt: s.formatTime(time.Now().UTC()), Available: catalogErr == nil}
	if catalogErr != nil {
		if errors.Is(catalogErr, service.ErrKeyExternallyRevoked) {
			v.UnavailableMessage = "API Key 已被外部撤销，请先前往 API Key 页面重新领取。"
		} else {
			v.UnavailableMessage = catalogErr.Error()
		}
	}
	results, resultErr := s.Store.ListModelTestResults(r.Context(), u.ID)
	if resultErr != nil {
		v.Error = "无法读取已保存的模型测试结果"
	}
	resultByModel := make(map[string]domain.ModelTestResult, len(results))
	for _, result := range results {
		resultByModel[result.Model] = result
	}
	items = append([]cpamp.Model(nil), items...)
	sort.SliceStable(items, func(i, j int) bool {
		left, right := strings.ToLower(items[i].ID), strings.ToLower(items[j].ID)
		if left == right {
			return strings.ToLower(items[i].OwnedBy) < strings.ToLower(items[j].OwnedBy)
		}
		return left < right
	})
	for _, m := range items {
		view := webui.ModelView{Name: m.ID, Provider: m.OwnedBy, Available: true, UpdatedAt: v.CheckedAt}
		if result, ok := resultByModel[m.ID]; ok {
			view.Tested = true
			view.TestStatus, view.TestStatusLabel = result.Status, modelTestStatusLabel(result.Status)
			view.TestMessage = result.Message
			view.TestLatency = modelTestLatency(result.LatencyMS)
			view.TestedAt = s.formatTime(result.TestedAt)
		}
		v.Models = append(v.Models, view)
	}
	_ = s.UI.Render(w, webui.PageModels, v)
}

func (s *Server) profile(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	policies, _ := s.Store.ListPolicies(r.Context(), 1000)
	v := webui.ProfileView{LayoutView: s.layout(u, currentToken(r), "个人资料", "profile"), Profile: s.userView(u), PhoneHelp: "如需修改，请联系管理员人工核验。", NameHelp: "账号批准后姓名不能自行修改。", RulesVersion: strconv.Itoa(u.PolicyVersion)}
	for _, p := range policies {
		if p.Version == u.PolicyVersion {
			v.Rules = []webui.RuleView{{Title: p.Title, Body: p.Body}}
			break
		}
	}
	if len(v.Rules) == 0 {
		v.Rules = []webui.RuleView{{Title: "注册时确认的使用规则", Body: "版本 " + strconv.Itoa(u.PolicyVersion)}}
	}
	if msg := r.URL.Query().Get("msg"); msg != "" {
		v.Flash = &webui.FlashView{Kind: "success", Message: msg}
	}
	_ = s.UI.Render(w, webui.PageProfile, v)
}
func (s *Server) passwordGet(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	min := 10
	if u.IsAdmin() {
		min = 14
	}
	_ = s.UI.Render(w, webui.PagePassword, webui.PasswordView{LayoutView: s.layout(u, currentToken(r), "修改密码", "profile"), MinimumLength: min})
}
func (s *Server) passwordPost(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if !s.verifyCSRF(r) {
		s.errorPage(w, r, 403, "请求已失效", nil)
		return
	}
	min := 10
	if u.IsAdmin() {
		min = 14
	}
	render := func(msg string) {
		w.WriteHeader(400)
		v := webui.PasswordView{LayoutView: s.layout(u, currentToken(r), "修改密码", "profile"), MinimumLength: min}
		v.Error = msg
		_ = s.UI.Render(w, webui.PagePassword, v)
	}
	if r.FormValue("new_password") != r.FormValue("confirm_password") {
		render("两次输入的新密码不一致")
		return
	}
	if err := s.Accounts.ChangePassword(r.Context(), u, r.FormValue("current_password"), r.FormValue("new_password"), currentSession(r).TokenHash, s.clientIP(r)); err != nil {
		v := webui.PasswordView{LayoutView: s.layout(u, currentToken(r), "修改密码", "profile"), MinimumLength: min}
		v.Error = err.Error()
		w.WriteHeader(400)
		_ = s.UI.Render(w, webui.PagePassword, v)
		return
	}
	http.Redirect(w, r, "/profile?msg="+urlQuery("密码已修改"), http.StatusSeeOther)
}

func (s *Server) audit(r *http.Request, actor domain.User, action, target, detail string) {
	_ = s.Store.Audit(r.Context(), domain.AuditEvent{ActorUserID: actor.ID, ActorLabel: actor.Name, Action: action, TargetID: target, TargetLabel: target, Detail: detail, IP: s.clientIP(r), CreatedAt: time.Now().UTC()})
}
