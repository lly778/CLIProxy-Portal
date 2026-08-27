package httpserver

import (
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/security"
	"cliproxy-portal/internal/webui"
)

func (s *Server) adminDashboard(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	from, to := time.Now().UTC().AddDate(0, 0, -7), time.Now().UTC()
	a, usageErr := s.Keys.GlobalUsage(r.Context(), from, to, 0)
	summary, _, _, _ := s.usageViews(a, from, to)
	statuses := []struct{ label, status string }{{"待审批", string(domain.StatusPending)}, {"已批准", string(domain.StatusApproved)}, {"已停用", string(domain.StatusSuspended)}, {"全部用户", ""}}
	v := webui.AdminDashboardView{LayoutView: s.layout(u, currentToken(r), "管理概览", "admin-dashboard"), Usage: summary}
	for _, x := range statuses {
		n, _ := s.Store.CountUsers(r.Context(), x.status)
		v.Counts = append(v.Counts, webui.CountView{Label: x.label, Value: strconv.Itoa(n)})
	}
	start := time.Now()
	_, healthErr := s.Keys.CPAMP.Health(r.Context())
	state, label, msg := "healthy", "正常", "CPAMP 可访问"
	if healthErr != nil {
		state, label, msg = "error", "异常", "CPAMP 暂时不可访问"
	}
	v.Health = []webui.HealthCheckView{{Component: "门户数据库", Status: "healthy", StatusLabel: "正常", Message: "SQLite WAL", CheckedAt: s.formatTime(time.Now())}, {Component: "CPA-Manager-Plus", Status: state, StatusLabel: label, Message: msg, CheckedAt: s.formatTime(time.Now()), Latency: time.Since(start).Round(time.Millisecond).String()}}
	if usageErr != nil {
		v.Error = "全局用量暂时不可用"
	} else {
		type rank struct {
			user          domain.User
			calls, tokens int64
		}
		byUser := map[string]*rank{}
		var unlinked cpamp.UsageSummary
		for _, stat := range a.APIKeyStats {
			key, keyErr := s.Store.KeyByHash(r.Context(), stat.APIKeyHash)
			if keyErr != nil {
				unlinked.TotalCalls += stat.Calls
				unlinked.SuccessCalls += stat.SuccessCalls
				unlinked.FailureCalls += stat.FailureCalls
				unlinked.InputTokens += stat.InputTokens
				unlinked.OutputTokens += stat.OutputTokens
				unlinked.TotalTokens += stat.TotalTokens
				continue
			}
			target, userErr := s.Store.UserByID(r.Context(), key.UserID)
			if userErr != nil {
				continue
			}
			x := byUser[target.ID]
			if x == nil {
				x = &rank{user: target}
				byUser[target.ID] = x
			}
			x.calls += stat.Calls
			x.tokens += stat.TotalTokens
		}
		v.UnlinkedSummary, _, _, _ = s.usageViews(cpamp.AnalyticsResponse{Summary: &unlinked}, from, to)
		ranks := make([]rank, 0, len(byUser))
		for _, x := range byUser {
			ranks = append(ranks, *x)
		}
		sort.Slice(ranks, func(i, j int) bool { return ranks[i].calls > ranks[j].calls })
		max := int64(1)
		if len(ranks) > 0 && ranks[0].calls > 0 {
			max = ranks[0].calls
		}
		for i, x := range ranks {
			if i >= 10 {
				break
			}
			v.TopUsers = append(v.TopUsers, webui.UserUsageRankView{Rank: i + 1, User: s.userView(x.user), Requests: compactNumber(x.calls), Tokens: compactNumber(x.tokens), Percent: int(x.calls * 100 / max)})
		}
	}
	audits, _ := s.Store.ListAudit(r.Context(), 8, 0)
	v.RecentAudit = s.auditViews(audits)
	_ = s.UI.Render(w, webui.PageAdminDashboard, v)
}

func (s *Server) adminUsage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	from, to, name := s.usageRange(r)
	var target *webui.UserView
	var a cpamp.AnalyticsResponse
	var err error
	userID := strings.TrimSpace(r.URL.Query().Get("user"))
	if userID != "" {
		targetUser, findErr := s.Store.UserByID(r.Context(), userID)
		if findErr != nil {
			s.errorPage(w, r, 404, "用户不存在", nil)
			return
		}
		targetView := s.userView(targetUser)
		target = &targetView
		a, err = s.Keys.Usage(r.Context(), userID, from, to, 0)
	} else {
		a, err = s.Keys.GlobalUsage(r.Context(), from, to, 0)
	}
	summary, daily, models, requests := s.usageViews(a, from, to)
	modelsByRequests, modelsByTokens := modelUsageCharts(models)
	formFrom, formTo := s.usageRangeFormDates(from, to, name)
	if name == "today" {
		summary.WindowLabel = "今天"
	}
	v := webui.UsageView{LayoutView: s.layout(u, currentToken(r), "全局使用量", "admin-usage"), FormAction: "/admin/usage", UserID: userID, Target: target, From: formFrom, To: formTo, Range: name, IsAdmin: true, Summary: summary, Daily: daily, Trend: usageTrend(daily), ByModelRequests: modelsByRequests, ByModelTokens: modelsByTokens, Requests: requests}
	if err != nil {
		v.Error = "全局用量暂时不可用"
	} else if userID == "" {
		v.ShowUserStats = true
		v.ByUserRequests, v.ByUserTokens, v.UnlinkedRequests, v.UnlinkedTokens, v.HasUnlinked = s.adminUsageByUser(r, a.APIKeyStats, name, formFrom, formTo)
	}
	_ = s.UI.Render(w, webui.PageUsage, v)
}

func (s *Server) adminUsageByUser(r *http.Request, stats []cpamp.APIKeyUsageStat, rangeName, formFrom, formTo string) ([]webui.UserUsageView, []webui.UserUsageView, string, string, bool) {
	type aggregate struct {
		user          domain.User
		calls, tokens int64
	}
	byUser := make(map[string]*aggregate)
	var unlinkedCalls, unlinkedTokens int64
	for _, stat := range stats {
		key, err := s.Store.KeyByHash(r.Context(), stat.APIKeyHash)
		if err != nil {
			unlinkedCalls += stat.Calls
			unlinkedTokens += stat.TotalTokens
			continue
		}
		user, err := s.Store.UserByID(r.Context(), key.UserID)
		if err != nil {
			unlinkedCalls += stat.Calls
			unlinkedTokens += stat.TotalTokens
			continue
		}
		item := byUser[user.ID]
		if item == nil {
			item = &aggregate{user: user}
			byUser[user.ID] = item
		}
		item.calls += stat.Calls
		item.tokens += stat.TotalTokens
	}
	rows := make([]webui.UserUsageView, 0, len(byUser))
	for _, item := range byUser {
		query := url.Values{"user": {item.user.ID}, "range": {rangeName}}
		if rangeName == "custom" {
			query.Set("from", formFrom)
			query.Set("to", formTo)
		}
		rows = append(rows, webui.UserUsageView{User: s.userView(item.user), UsageURL: "/admin/usage?" + query.Encode(), Requests: compactNumber(item.calls), Tokens: compactNumber(item.tokens), RequestValue: item.calls, TokenValue: item.tokens})
	}
	byRequests, byTokens := userUsageCharts(rows)
	return byRequests, byTokens, compactNumber(unlinkedCalls), compactNumber(unlinkedTokens), unlinkedCalls > 0 || unlinkedTokens > 0
}

func (s *Server) adminUsers(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	page := parseInt(r.URL.Query().Get("page"), 1)
	if page < 1 {
		page = 1
	}
	const size = 30
	status, q := r.URL.Query().Get("status"), strings.TrimSpace(r.URL.Query().Get("q"))
	users, err := s.Store.ListUsers(r.Context(), status, q, size, (page-1)*size)
	if err != nil {
		s.errorPage(w, r, 500, "用户列表暂时不可用", err)
		return
	}
	total, _ := s.Store.CountUsers(r.Context(), status)
	pages := (total + size - 1) / size
	if pages < 1 {
		pages = 1
	}
	v := webui.AdminUsersView{LayoutView: s.layout(u, currentToken(r), "用户管理", "admin-users"), Query: q, Status: status, Statuses: []string{"pending", "rejected", "approved", "suspended", "suspended_pending", "delete_pending"}, Page: page, PageCount: pages, Total: strconv.Itoa(total)}
	keyOwners := make(map[string]string)
	lastUsedByUser := make(map[string]time.Time)
	var hashes []string
	for _, x := range users {
		keys, keyErr := s.Store.ListKeysByUser(r.Context(), x.ID)
		if keyErr != nil {
			continue
		}
		for _, key := range keys {
			keyOwners[strings.ToLower(strings.TrimSpace(key.Hash))] = x.ID
			hashes = append(hashes, key.Hash)
			if key.LastSeenAt.After(lastUsedByUser[x.ID]) {
				lastUsedByUser[x.ID] = key.LastSeenAt
			}
		}
	}
	usageByUser := make(map[string]cpamp.UsageSummary)
	usageLoaded := true
	now := time.Now().UTC()
	stats, usageErr := s.Keys.APIKeyUsage(r.Context(), hashes, now.AddDate(0, 0, -30), now)
	if usageErr != nil {
		usageLoaded = false
		v.Error = "用户用量暂时不可用，其他账号资料仍可正常查看"
	} else {
		for _, stat := range stats {
			userID, ok := keyOwners[strings.ToLower(strings.TrimSpace(stat.APIKeyHash))]
			if !ok {
				continue
			}
			summary := usageByUser[userID]
			summary.TotalCalls += stat.Calls
			summary.TotalTokens += stat.TotalTokens
			usageByUser[userID] = summary
			if stat.LastSeenMS > 0 {
				lastUsed := time.UnixMilli(stat.LastSeenMS).UTC()
				if lastUsed.After(lastUsedByUser[userID]) {
					lastUsedByUser[userID] = lastUsed
				}
			}
		}
	}
	for _, x := range users {
		usage := webui.UsageSummaryView{Requests: "—", TotalTokens: "—"}
		if usageLoaded {
			summary := usageByUser[x.ID]
			usage.Requests = compactNumber(summary.TotalCalls)
			usage.TotalTokens = compactNumber(summary.TotalTokens)
		}
		row := webui.UserRowView{User: s.userView(x), Pending: x.Status == domain.StatusPending, LastUsed: s.formatTime(lastUsedByUser[x.ID]), CanManage: true, Usage: usage}
		v.Users = append(v.Users, row)
	}
	_ = s.UI.Render(w, webui.PageAdminUsers, v)
}

func (s *Server) adminUser(w http.ResponseWriter, r *http.Request) {
	target, err := s.Store.UserByID(r.Context(), r.PathValue("id"))
	if err != nil {
		s.errorPage(w, r, 404, "用户不存在", nil)
		return
	}
	s.renderAdminUser(w, r, target, "", nil)
}
func (s *Server) renderAdminUser(w http.ResponseWriter, r *http.Request, target domain.User, msg string, errMsg error) {
	actor := currentUser(r)
	to := time.Now().UTC()
	from := target.CreatedAt.UTC()
	if from.IsZero() || !from.Before(to) {
		from = to.AddDate(-10, 0, 0)
	}
	a, _ := s.Keys.Usage(r.Context(), target.ID, from, to, 100)
	summary, daily, models, requests := s.usageViews(a, from, to)
	v := webui.AdminUserDetailView{LayoutView: s.layout(actor, currentToken(r), target.Name, "admin-users"), Target: s.userView(target), Key: s.keyView(target), StatusCard: s.adminStatusCard(target), Usage: summary, Daily: daily, ByModel: models, Requests: requests, CanApprove: target.Status == domain.StatusPending || target.Status == domain.StatusRejected, CanReject: target.Status == domain.StatusPending, CanSuspend: target.Status == domain.StatusApproved, CanUnsuspend: target.Status == domain.StatusSuspended, CanDelete: target.ID != actor.ID, CanReset: target.Status != domain.StatusDeleted, CanChangePhone: target.Status != domain.StatusDeleted, DeleteWarning: "将先撤销 API Key，再永久匿名化账号。此操作不可恢复。"}
	if msg != "" {
		v.Flash = &webui.FlashView{Kind: "success", Message: msg}
	}
	if errMsg != nil {
		v.Error = errMsg.Error()
		w.WriteHeader(400)
	}
	_ = s.UI.Render(w, webui.PageAdminUser, v)
}

func (s *Server) adminUserAction(w http.ResponseWriter, r *http.Request) {
	if !s.verifyCSRF(r) {
		s.errorPage(w, r, 403, "请求已失效", nil)
		return
	}
	actor := currentUser(r)
	target, err := s.Store.UserByID(r.Context(), r.PathValue("id"))
	if err != nil {
		s.errorPage(w, r, 404, "用户不存在", nil)
		return
	}
	action := r.PathValue("action")
	msg := "操作已完成"
	switch action {
	case "approve":
		err = s.Accounts.Approve(r.Context(), actor, target, r.FormValue("note"), s.clientIP(r))
		msg = "用户已批准"
	case "reject":
		err = s.Accounts.Reject(r.Context(), actor, target, r.FormValue("reason"), s.clientIP(r))
		msg = "申请已拒绝"
	case "unsuspend":
		err = s.Store.SetUserStatus(r.Context(), target.ID, domain.StatusApproved, "")
		s.audit(r, actor, "user.unsuspend", target.ID, "")
		msg = "账号已恢复"
	case "suspend":
		if target.IsAdmin() {
			count, _ := s.Store.CountAdmins(r.Context())
			if count <= 1 {
				err = errors.New("必须保留至少一位有效管理员")
			} else {
				err = s.suspendOrDelete(r, actor, target, false)
			}
		} else {
			err = s.suspendOrDelete(r, actor, target, false)
		}
		msg = "账号已停用"
	case "delete":
		if !security.VerifyPassword(actor.PasswordHash, r.FormValue("admin_password")) {
			err = errors.New("管理员密码错误")
		} else if target.ID == actor.ID {
			err = errors.New("不能删除当前登录管理员")
		} else if target.IsAdmin() {
			count, _ := s.Store.CountAdmins(r.Context())
			if target.Status == domain.StatusApproved && count <= 1 {
				err = errors.New("必须保留至少一位有效管理员")
			} else {
				err = s.suspendOrDelete(r, actor, target, true)
				msg = "账号已删除或进入撤销队列"
			}
		} else {
			err = s.suspendOrDelete(r, actor, target, true)
			msg = "账号已删除或进入撤销队列"
		}
	case "reset-password":
		var code string
		code, err = s.Accounts.GenerateReset(r.Context(), actor, target, s.clientIP(r))
		if err == nil {
			msg = "一次性重置码：" + code + "（15 分钟内有效，仅显示本次）"
		}
	case "change-phone":
		err = s.Accounts.ChangePhone(r.Context(), actor, target, r.FormValue("phone"), r.FormValue("admin_password"), s.clientIP(r))
		msg = "手机号已修改"
	default:
		err = errors.New("未知操作")
	}
	if err != nil {
		s.renderAdminUser(w, r, target, "", err)
		return
	}
	if action == "delete" {
		http.Redirect(w, r, "/admin/users?msg="+urlQuery(msg), http.StatusSeeOther)
		return
	}
	target, _ = s.Store.UserByID(r.Context(), target.ID)
	s.renderAdminUser(w, r, target, msg, nil)
}

func (s *Server) suspendOrDelete(r *http.Request, actor, target domain.User, del bool) error {
	pending, err := s.Keys.Revoke(r.Context(), target)
	action := "suspend"
	pendingStatus := domain.StatusSuspendedPending
	if del {
		action = "delete"
		pendingStatus = domain.StatusDeletePending
	}
	if err != nil || pending {
		key, _ := s.Store.ActiveKey(r.Context(), target.ID)
		_ = s.Store.SetUserStatus(r.Context(), target.ID, pendingStatus, r.FormValue("reason"))
		idErr := s.Store.EnqueueJob(r.Context(), domain.SyncJob{UserID: target.ID, Action: action, KeyHash: key.Hash, NextAt: time.Now().UTC().Add(s.Cfg.PendingRetry), CreatedAt: time.Now().UTC()})
		if idErr != nil {
			return idErr
		}
		s.audit(r, actor, "user."+action+".pending", target.ID, "等待 CPAMP 撤销")
		return nil
	}
	if del {
		err = s.Store.AnonymizeUser(r.Context(), target.ID, "已删除用户-"+security.ShortID(target.ID))
	} else {
		err = s.Store.SetUserStatus(r.Context(), target.ID, domain.StatusSuspended, r.FormValue("reason"))
	}
	if err == nil {
		s.audit(r, actor, "user."+action, target.ID, r.FormValue("reason"))
	}
	return err
}

func (s *Server) adminApprovalAction(w http.ResponseWriter, r *http.Request) {
	r.SetPathValue("id", r.PathValue("id"))
	s.adminUserAction(w, r)
}
func (s *Server) adminApprovals(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	pending, _ := s.Store.ListUsers(r.Context(), string(domain.StatusPending), "", 100, 0)
	rejected, _ := s.Store.ListUsers(r.Context(), string(domain.StatusRejected), "", 30, 0)
	v := webui.AdminApprovalsView{LayoutView: s.layout(u, currentToken(r), "审批队列", "admin-approvals"), PendingCount: strconv.Itoa(len(pending))}
	for _, x := range pending {
		v.Pending = append(v.Pending, webui.ApprovalView{ApplicationID: x.ID, User: s.userView(x), SubmittedAt: s.formatTime(x.CreatedAt), RulesVersion: strconv.Itoa(x.PolicyVersion), CanApprove: true, CanReject: true})
	}
	for _, x := range rejected {
		v.Rejected = append(v.Rejected, webui.ApprovalView{ApplicationID: x.ID, User: s.userView(x), SubmittedAt: s.formatTime(x.UpdatedAt), RulesVersion: strconv.Itoa(x.PolicyVersion), Reason: x.RejectionReason})
	}
	_ = s.UI.Render(w, webui.PageAdminApprovals, v)
}

func (s *Server) adminAdmins(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	admins, _ := s.Store.ListUsers(r.Context(), "", "", 1000, 0)
	count, _ := s.Store.CountAdmins(r.Context())
	v := webui.AdminAdminsView{LayoutView: s.layout(u, currentToken(r), "管理员", "admin-admins"), CanCreate: true, CanManage: true}
	for _, x := range admins {
		if !x.IsAdmin() || x.Status == domain.StatusDeleted {
			continue
		}
		v.Admins = append(v.Admins, webui.AdminRowView{User: s.userView(x), LastLoginAt: s.formatTime(x.LastLoginAt), CreatedAt: s.formatTime(x.CreatedAt), CanDisable: x.ID != u.ID && count > 1, IsLastAdmin: count == 1})
	}
	_ = s.UI.Render(w, webui.PageAdminAdmins, v)
}
func (s *Server) adminCreate(w http.ResponseWriter, r *http.Request) {
	if !s.verifyCSRF(r) {
		s.errorPage(w, r, 403, "请求已失效", nil)
		return
	}
	actor := currentUser(r)
	_, err := s.Accounts.CreateAdmin(r.Context(), r.FormValue("phone"), r.FormValue("name"), r.FormValue("password"), &actor, s.clientIP(r))
	if err != nil {
		s.errorPage(w, r, 400, err.Error(), nil)
		return
	}
	http.Redirect(w, r, "/admin/admins", http.StatusSeeOther)
}
func (s *Server) adminDisable(w http.ResponseWriter, r *http.Request) {
	if !s.verifyCSRF(r) {
		s.errorPage(w, r, 403, "请求已失效", nil)
		return
	}
	target, err := s.Store.UserByID(r.Context(), r.PathValue("id"))
	if err != nil {
		s.errorPage(w, r, 404, "管理员不存在", nil)
		return
	}
	count, _ := s.Store.CountAdmins(r.Context())
	if !target.IsAdmin() || count <= 1 || target.ID == currentUser(r).ID {
		s.errorPage(w, r, 400, "必须保留至少一位有效管理员", nil)
		return
	}
	_ = s.Store.SetUserStatus(r.Context(), target.ID, domain.StatusSuspended, "管理员停用")
	_ = s.Store.DeleteUserSessions(r.Context(), target.ID, "")
	s.audit(r, currentUser(r), "admin.disable", target.ID, "")
	http.Redirect(w, r, "/admin/admins", http.StatusSeeOther)
}

func (s *Server) adminPolicy(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	current, err := s.Store.LatestPolicy(r.Context())
	if err != nil {
		s.errorPage(w, r, 500, "规则暂时不可用", err)
		return
	}
	all, _ := s.Store.ListPolicies(r.Context(), 20)
	open, _ := s.Store.RegistrationOpen(r.Context())
	v := webui.AdminPolicyView{LayoutView: s.layout(u, currentToken(r), "使用规则", "admin-policy"), Current: policyView(s, current), EditTitle: current.Title, EditBody: current.Body, RegistrationOpen: open}
	for _, p := range all {
		v.Versions = append(v.Versions, webui.PolicyVersionView{PolicyView: policyView(s, p), Current: p.Version == current.Version})
	}
	_ = s.UI.Render(w, webui.PageAdminPolicy, v)
}
func policyView(s *Server, p domain.Policy) webui.PolicyView {
	return webui.PolicyView{Version: strconv.Itoa(p.Version), Title: p.Title, Body: p.Body, PublishedAt: s.formatTime(p.CreatedAt), PublishedBy: emptyDash(p.CreatedBy)}
}
func (s *Server) adminPolicySave(w http.ResponseWriter, r *http.Request) {
	if !s.verifyCSRF(r) {
		s.errorPage(w, r, 403, "请求已失效", nil)
		return
	}
	title, body := strings.TrimSpace(r.FormValue("title")), strings.TrimSpace(r.FormValue("body"))
	if title == "" || body == "" || len([]rune(body)) > 10000 {
		s.errorPage(w, r, 400, "规则标题和正文不能为空，正文最多 10000 字", nil)
		return
	}
	u := currentUser(r)
	p, err := s.Store.SavePolicy(r.Context(), title, body, u.Name)
	if err != nil {
		s.errorPage(w, r, 500, "保存规则失败", err)
		return
	}
	s.audit(r, u, "policy.publish", strconv.Itoa(p.Version), title)
	http.Redirect(w, r, "/admin/policy", http.StatusSeeOther)
}
func (s *Server) adminRegistration(w http.ResponseWriter, r *http.Request) {
	if !s.verifyCSRF(r) {
		s.errorPage(w, r, 403, "请求已失效", nil)
		return
	}
	open := r.FormValue("open") == "true"
	if err := s.Store.SetRegistrationOpen(r.Context(), open); err != nil {
		s.errorPage(w, r, 500, "更新注册开关失败", err)
		return
	}
	s.audit(r, currentUser(r), "registration.toggle", "", strconv.FormatBool(open))
	http.Redirect(w, r, "/admin/policy", http.StatusSeeOther)
}

func (s *Server) adminAudit(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	page := parseInt(r.URL.Query().Get("page"), 1)
	if page < 1 {
		page = 1
	}
	items, err := s.Store.ListAudit(r.Context(), 100, (page-1)*100)
	if err != nil {
		s.errorPage(w, r, 500, "操作日志暂时不可用", err)
		return
	}
	v := webui.AdminAuditView{LayoutView: s.layout(u, currentToken(r), "操作日志", "admin-audit"), Entries: s.auditViews(items), Page: page, PageCount: page, Total: strconv.Itoa(len(items)), Actions: []string{"user.register", "user.approve", "user.reject", "key.issue", "key.revoke", "user.suspend", "user.delete", "policy.publish"}}
	_ = s.UI.Render(w, webui.PageAdminAudit, v)
}
func (s *Server) adminHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && !s.verifyCSRF(r) {
		s.errorPage(w, r, 403, "请求已失效", nil)
		return
	}
	u := currentUser(r)
	now := time.Now().UTC()
	v := webui.AdminHealthView{LayoutView: s.layout(u, currentToken(r), "系统健康", "admin-health"), LastSyncAt: "由后台每次启动后执行", NextSyncAt: s.formatTime(now.Add(s.Cfg.ReconcileInterval)), SyncInterval: s.Cfg.ReconcileInterval.String()}
	start := time.Now()
	err := s.Store.Ping(r.Context())
	v.Checks = append(v.Checks, healthView("门户数据库", err, time.Since(start), s))
	start = time.Now()
	_, err = s.Keys.CPAMP.Health(r.Context())
	v.Checks = append(v.Checks, healthView("CPA-Manager-Plus", err, time.Since(start), s))
	if err = s.Keys.Reconcile(r.Context()); err != nil {
		v.Messages = append(v.Messages, webui.NoticeView{Kind: "warning", Title: "Key 对账失败", Message: "系统会按计划继续重试。"})
	}
	_ = s.UI.Render(w, webui.PageAdminHealth, v)
}
func healthView(name string, err error, d time.Duration, s *Server) webui.HealthCheckView {
	v := webui.HealthCheckView{Component: name, Status: "healthy", StatusLabel: "正常", Message: "检查通过", CheckedAt: s.formatTime(time.Now()), Latency: d.Round(time.Millisecond).String()}
	if err != nil {
		v.Status = "error"
		v.StatusLabel = "异常"
		v.Message = "暂时不可访问"
	}
	return v
}
