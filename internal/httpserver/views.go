package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

func (s *Server) layout(rUser domain.User, rToken, title, nav string) webui.LayoutView {
	u := s.userView(rUser)
	return webui.LayoutView{Title: title, Brand: "CLIProxy 账号门户", CSRFToken: s.csrf(rToken), ActiveNav: nav, User: &u, IsLoggedIn: true}
}

func (s *Server) csrf(token string) string {
	if token == "" {
		return ""
	}
	return securityCSRF(s.Secret, token)
}

func (s *Server) userView(u domain.User) webui.UserView {
	v := webui.UserView{ID: u.ID, Name: u.Name, Phone: u.Phone, Role: string(u.Role), RoleLabel: roleLabel(u.Role), Status: string(u.Status), StatusLabel: userStatusLabel(u.Status), Initials: initials(u.Name), JoinedAt: s.formatTime(u.CreatedAt), LastLoginAt: s.formatTime(u.LastLoginAt)}
	if key, err := s.Store.ActiveKey(contextBackground(), u.ID); err == nil {
		v.HasKey = true
		v.KeyStatus = key.Status
		v.KeyStatusLabel = keyStatusLabel(key.Status)
		v.KeyLast4 = key.LastFour
		v.KeyCreatedAt = s.formatTime(key.IssuedAt)
		v.KeyLastUsedAt = s.formatTime(key.LastSeenAt)
		v.KeySyncStatus = key.Status
	}
	return v
}

func (s *Server) keyView(u domain.User) webui.KeyView {
	v := webui.KeyView{Status: "none", StatusLabel: "尚未领取", SyncStatus: "none", SyncStatusLabel: "无", CanClaim: u.Status == domain.StatusApproved, APIBaseURL: s.Cfg.CPAAPIBaseURL}
	key, err := s.Store.ActiveKey(contextBackground(), u.ID)
	if err != nil {
		keys, listErr := s.Store.ListKeysByUser(contextBackground(), u.ID)
		if listErr == nil && len(keys) > 0 {
			latest := keys[len(keys)-1]
			if latest.Status == "external_revoked" {
				v.Status, v.StatusLabel = latest.Status, keyStatusLabel(latest.Status)
				v.Last4, v.CreatedAt, v.LastUsedAt = latest.LastFour, s.formatTime(latest.IssuedAt), s.formatTime(latest.LastSeenAt)
				v.SyncStatus, v.SyncStatusLabel = latest.Status, keyStatusLabel(latest.Status)
			}
		}
		return v
	}
	v.Status, v.StatusLabel = key.Status, keyStatusLabel(key.Status)
	v.Last4, v.CreatedAt, v.LastUsedAt = key.LastFour, s.formatTime(key.IssuedAt), s.formatTime(key.LastSeenAt)
	v.SyncStatus, v.SyncStatusLabel = key.Status, keyStatusLabel(key.Status)
	v.CanClaim = false
	v.CanTest = key.Status == "active"
	v.CanRevoke = key.Status == "active"
	return v
}

func (s *Server) statusCard(u domain.User) webui.StatusCardView {
	v := webui.StatusCardView{Status: string(u.Status), StatusLabel: userStatusLabel(u.Status), UpdatedAt: s.formatTime(u.UpdatedAt)}
	switch u.Status {
	case domain.StatusPending:
		v.Heading, v.Description = "正在等待管理员审批", "管理员人工核验手机号和身份后会处理申请。"
	case domain.StatusRejected:
		v.Heading, v.Description, v.Reason = "申请未通过", "您可以修正姓名、密码并重新提交申请。", u.RejectionReason
	case domain.StatusApproved:
		v.Heading, v.Description = "账号可以正常使用", "您可以领取 API Key、查询模型和查看自己的使用量。"
	case domain.StatusSuspended, domain.StatusSuspendedPending:
		v.Heading, v.Description, v.Reason = "账号已停用", "请联系管理员了解详情。", u.SuspensionReason
	case domain.StatusDeletePending:
		v.Heading, v.Description = "账号正在删除", "系统正在撤销凭据，完成后将退出登录。"
	default:
		v.Heading, v.Description = "账号不可用", "请联系管理员。"
	}
	return v
}

func (s *Server) adminStatusCard(u domain.User) webui.StatusCardView {
	v := s.statusCard(u)
	switch u.Status {
	case domain.StatusPending:
		v.Heading, v.Description = "正在等待审批", "请人工核验该用户的手机号和身份后处理申请。"
	case domain.StatusRejected:
		v.Heading, v.Description = "申请未通过", "该用户可以修正资料并重新提交申请。"
	case domain.StatusApproved:
		v.Heading, v.Description = "账号可以正常使用", "该用户可以领取 API Key、查询模型和查看自己的使用量。"
	case domain.StatusSuspended, domain.StatusSuspendedPending:
		v.Heading, v.Description = "账号已停用", "该用户当前无法使用门户功能；核验后可由管理员恢复。"
	case domain.StatusDeletePending:
		v.Heading, v.Description = "账号正在删除", "系统正在撤销该用户的凭据，完成后将永久匿名化账号。"
	default:
		v.Heading, v.Description = "账号不可用", "请检查该用户的账号状态。"
	}
	return v
}

func (s *Server) usageViews(a cpamp.AnalyticsResponse, from, to time.Time) (webui.UsageSummaryView, []webui.UsagePointView, []webui.ModelUsageView, []webui.RequestView) {
	var summary webui.UsageSummaryView
	if a.Summary != nil {
		x := a.Summary
		summary = webui.UsageSummaryView{Requests: compactNumber(x.TotalCalls), Successes: compactNumber(x.SuccessCalls), Failures: compactNumber(x.FailureCalls), InputTokens: compactNumber(x.InputTokens), OutputTokens: compactNumber(x.OutputTokens), CacheTokens: compactNumber(x.CachedTokens + x.CacheReadTokens + x.CacheCreationTokens), ReasoningTokens: compactNumber(x.ReasoningTokens), TotalTokens: compactNumber(x.TotalTokens), WindowLabel: from.In(s.Cfg.TimeZone).Format("01-02") + " 至 " + to.In(s.Cfg.TimeZone).Format("01-02")}
		if x.AverageLatencyMS != nil {
			summary.Latency = fmt.Sprintf("%.0f ms", *x.AverageLatencyMS)
		} else {
			summary.Latency = "—"
		}
	}
	maxCalls := int64(1)
	maxTokens := int64(1)
	for _, p := range a.Timeline {
		if p.Calls > maxCalls {
			maxCalls = p.Calls
		}
		if p.TotalTokens > maxTokens {
			maxTokens = p.TotalTokens
		}
	}
	daily := make([]webui.UsagePointView, 0, len(a.Timeline))
	timelineLayout := "01-02"
	if strings.EqualFold(a.Granularity, "hour") || (to.After(from) && to.Sub(from) <= 48*time.Hour) {
		timelineLayout = "01-02 15:00"
	}
	for _, p := range a.Timeline {
		label := p.Label
		if p.BucketMS > 0 {
			label = time.UnixMilli(p.BucketMS).In(s.Cfg.TimeZone).Format(timelineLayout)
		}
		daily = append(daily, webui.UsagePointView{Date: label, Requests: compactNumber(p.Calls), Tokens: compactNumber(p.TotalTokens), Percent: int(p.Calls * 100 / maxCalls), TokenPercent: int(p.TotalTokens * 100 / maxTokens), RequestValue: p.Calls, TokenValue: p.TotalTokens})
	}
	maxModel := int64(1)
	maxModelTokens := int64(1)
	for _, m := range a.ModelStats {
		if m.Calls > maxModel {
			maxModel = m.Calls
		}
		if m.TotalTokens > maxModelTokens {
			maxModelTokens = m.TotalTokens
		}
	}
	models := make([]webui.ModelUsageView, 0, len(a.ModelStats))
	for _, m := range a.ModelStats {
		models = append(models, webui.ModelUsageView{Model: m.Model, Requests: compactNumber(m.Calls), Tokens: compactNumber(m.TotalTokens), Percent: int(m.Calls * 100 / maxModel), TokenPercent: int(m.TotalTokens * 100 / maxModelTokens), RequestValue: m.Calls, TokenValue: m.TotalTokens})
	}
	var requests []webui.RequestView
	if a.Events != nil {
		for _, e := range a.Events.Items {
			requests = append(requests, s.requestView(e))
		}
	}
	return summary, daily, models, requests
}

func (s *Server) requestView(e cpamp.EventRow) webui.RequestView {
	status, label := "success", "成功"
	if e.Failed {
		status, label = "failed", "失败"
	}
	latency := "—"
	if e.LatencyMS != nil {
		latency = fmt.Sprintf("%d ms", *e.LatencyMS)
	}
	errorFull := requestFailureText(e)
	view := webui.RequestView{At: s.formatTime(time.UnixMilli(e.TimestampMS)), Model: requestModelLabel(e), Status: status, StatusLabel: label, InputTokens: compactNumber(e.InputTokens), OutputTokens: compactNumber(e.OutputTokens), CacheTokens: compactNumber(e.CachedTokens + e.CacheReadTokens + e.CacheCreationTokens), ReasoningTokens: compactNumber(e.ReasoningTokens), ReasoningEffort: reasoningEffortLabel(e.ReasoningEffort), TotalTokens: compactNumber(e.TotalTokens), Latency: latency, Error: errorFull, ErrorFull: errorFull}
	if s.CaptureVault != nil && e.RequestID != "" && e.APIKeyHash != "" {
		if capture, err := s.Store.GatewayCaptureByCPARequest(contextBackground(), strings.ToLower(e.RequestID), strings.ToLower(e.APIKeyHash)); err == nil && capture.CreatedAt.After(time.Now().Add(-7*24*time.Hour)) {
			eventAt := time.UnixMilli(e.TimestampMS)
			if delta := capture.CreatedAt.Sub(eventAt); delta > -10*time.Minute && delta < 10*time.Minute {
				view.CaptureID = capture.ID
			}
		}
	}
	return view
}

// requestModelLabel shows what the client requested and where CPA routed it.
// Older CPAMP records may not have either dedicated field, so fall back to
// their existing display model without inventing a resolved target.
func requestModelLabel(e cpamp.EventRow) string {
	requested := strings.TrimSpace(e.RequestedModel)
	if requested == "" {
		requested = strings.TrimSpace(e.Model)
	}
	resolved := strings.TrimSpace(e.ResolvedModel)
	if requested == "" {
		return resolved
	}
	if resolved == "" || requested == resolved {
		return requested
	}
	return requested + " → " + resolved
}

func requestFailureText(e cpamp.EventRow) string {
	if !e.Failed {
		return ""
	}
	parts := make([]string, 0, 3)
	if e.FailStatusCode != nil && *e.FailStatusCode >= 100 {
		parts = append(parts, fmt.Sprintf("HTTP %d", *e.FailStatusCode))
	}
	summary := strings.TrimSpace(e.FailSummary)
	if summary == "" {
		return strings.Join(parts, " · ")
	}

	code, message, plain := failureSummaryDetails(summary)
	if message != "" {
		parts = appendUnique(parts, message)
	} else if plain != "" {
		parts = appendUnique(parts, plain)
	} else {
		parts = appendUnique(parts, code)
	}
	return strings.Join(parts, " · ")
}

func failureSummaryDetails(summary string) (string, string, string) {
	decoder := json.NewDecoder(strings.NewReader(summary))
	decoder.UseNumber()
	var code, message string
	var consumed int64
	for {
		var payload any
		err := decoder.Decode(&payload)
		if err == io.EOF {
			break
		}
		if err != nil {
			return code, message, plainFailureSummary(summary[consumed:])
		}
		consumed = decoder.InputOffset()
		object, ok := payload.(map[string]any)
		if !ok {
			break
		}
		objectCode, objectMessage := failureJSONDetails(object)
		if code == "" {
			code = objectCode
		}
		if message == "" {
			message = objectMessage
		}
	}
	return code, message, ""
}

func plainFailureSummary(summary string) string {
	lines := strings.FieldsFunc(summary, func(char rune) bool { return char == '\r' || char == '\n' })
	plain := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "{") || strings.HasPrefix(line, "[") || strings.HasPrefix(line, "<") || failureHeaderLine(line) {
			break
		}
		plain = append(plain, line)
	}
	return strings.Join(plain, " ")
}

func failureHeaderLine(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(line))
	for _, prefix := range []string{
		"authorization:", "proxy-authorization:", "cookie:", "set-cookie:",
		"cf-ray:", "cf-cache-status:", "content-type:", "content-length:",
		"date:", "server:", "strict-transport-security:",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func failureJSONDetails(object map[string]any) (string, string) {
	code := firstFailureText(object, "code", "type")
	message := firstFailureText(object, "message", "detail")
	if nested, ok := object["error"].(map[string]any); ok {
		if nestedCode := firstFailureText(nested, "code", "type"); nestedCode != "" {
			code = nestedCode
		}
		if nestedMessage := firstFailureText(nested, "message", "detail"); nestedMessage != "" {
			message = nestedMessage
		}
	} else if message == "" {
		message = failureText(object["error"])
	}
	return code, message
}

func firstFailureText(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := failureText(object[key]); value != "" {
			return value
		}
	}
	return ""
}

func failureText(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}

func appendUnique(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	for _, existing := range values {
		if strings.EqualFold(existing, value) {
			return values
		}
	}
	return append(values, value)
}

func modelUsageCharts(models []webui.ModelUsageView) ([]webui.ModelUsageView, []webui.ModelUsageView) {
	byRequests := append([]webui.ModelUsageView(nil), models...)
	byTokens := append([]webui.ModelUsageView(nil), models...)
	sort.SliceStable(byRequests, func(i, j int) bool {
		if byRequests[i].RequestValue == byRequests[j].RequestValue {
			return strings.ToLower(byRequests[i].Model) < strings.ToLower(byRequests[j].Model)
		}
		return byRequests[i].RequestValue > byRequests[j].RequestValue
	})
	sort.SliceStable(byTokens, func(i, j int) bool {
		if byTokens[i].TokenValue == byTokens[j].TokenValue {
			return strings.ToLower(byTokens[i].Model) < strings.ToLower(byTokens[j].Model)
		}
		return byTokens[i].TokenValue > byTokens[j].TokenValue
	})
	return byRequests, byTokens
}

func userUsageCharts(users []webui.UserUsageView) ([]webui.UserUsageView, []webui.UserUsageView) {
	byRequests := append([]webui.UserUsageView(nil), users...)
	byTokens := append([]webui.UserUsageView(nil), users...)
	sort.SliceStable(byRequests, func(i, j int) bool {
		if byRequests[i].RequestValue == byRequests[j].RequestValue {
			return strings.ToLower(byRequests[i].User.Name) < strings.ToLower(byRequests[j].User.Name)
		}
		return byRequests[i].RequestValue > byRequests[j].RequestValue
	})
	sort.SliceStable(byTokens, func(i, j int) bool {
		if byTokens[i].TokenValue == byTokens[j].TokenValue {
			return strings.ToLower(byTokens[i].User.Name) < strings.ToLower(byTokens[j].User.Name)
		}
		return byTokens[i].TokenValue > byTokens[j].TokenValue
	})
	maxRequests, maxTokens := int64(1), int64(1)
	if len(byRequests) > 0 && byRequests[0].RequestValue > 0 {
		maxRequests = byRequests[0].RequestValue
	}
	if len(byTokens) > 0 && byTokens[0].TokenValue > 0 {
		maxTokens = byTokens[0].TokenValue
	}
	for i := range byRequests {
		byRequests[i].RequestPercent = int(byRequests[i].RequestValue * 100 / maxRequests)
	}
	for i := range byTokens {
		byTokens[i].TokenPercent = int(byTokens[i].TokenValue * 100 / maxTokens)
	}
	return byRequests, byTokens
}

func usageTrend(points []webui.UsagePointView) webui.UsageTrendView {
	trend := webui.UsageTrendView{}
	if len(points) == 0 {
		return trend
	}
	var maxRequests, maxTokens int64
	for _, point := range points {
		if point.RequestValue > maxRequests {
			maxRequests = point.RequestValue
		}
		if point.TokenValue > maxTokens {
			maxTokens = point.TokenValue
		}
	}
	requestScale, tokenScale := maxRequests, maxTokens
	if requestScale <= 0 {
		requestScale = 1
	}
	if tokenScale <= 0 {
		tokenScale = 1
	}
	const left, right, top, bottom = 48, 952, 28, 218
	requestPairs := make([]string, 0, len(points))
	tokenPairs := make([]string, 0, len(points))
	for i, point := range points {
		x := (left + right) / 2
		if len(points) > 1 {
			x = left + i*(right-left)/(len(points)-1)
		}
		requestY := bottom - int(float64(point.RequestValue)/float64(requestScale)*float64(bottom-top))
		tokenY := bottom - int(float64(point.TokenValue)/float64(tokenScale)*float64(bottom-top))
		view := webui.UsageTrendPointView{X: x, RequestY: requestY, TokenY: tokenY, Date: point.Date, Requests: point.Requests, Tokens: point.Tokens, ShowLabel: len(points) <= 14 || i%5 == 0 || i == len(points)-1}
		trend.Points = append(trend.Points, view)
		requestPairs = append(requestPairs, fmt.Sprintf("%d,%d", x, requestY))
		tokenPairs = append(tokenPairs, fmt.Sprintf("%d,%d", x, tokenY))
	}
	trend.RequestPoints = strings.Join(requestPairs, " ")
	trend.TokenPoints = strings.Join(tokenPairs, " ")
	trend.MaxRequests = compactNumber(maxRequests)
	trend.MaxTokens = compactNumber(maxTokens)
	trend.GranularityLabel = "按天"
	if strings.Contains(points[0].Date, ":") {
		trend.GranularityLabel = "按小时"
	}
	return trend
}

func compactNumber(value int64) string {
	abs := value
	if abs < 0 {
		abs = -abs
	}
	unit := ""
	scale := float64(1)
	if abs >= 1_000_000 {
		unit, scale = "M", 1_000_000
	} else if abs >= 1_000 {
		unit, scale = "K", 1_000
	}
	if unit == "" {
		return number(value)
	}
	formatted := fmt.Sprintf("%.1f", float64(value)/scale)
	formatted = strings.TrimSuffix(formatted, ".0")
	return formatted + unit
}

func (s *Server) quotaPoolView(pool service.UpstreamQuotaPool, csrfToken, returnTo string) webui.QuotaPoolView {
	refresh := s.Keys.QuotaRefreshStatus()
	v := webui.QuotaPoolView{
		Show:              pool.TotalAccounts > 0,
		Available:         len(pool.Groups) > 0,
		Provider:          pool.Provider,
		Accounts:          fmt.Sprintf("共 %d 个已启用账号", pool.TotalAccounts),
		AvailabilityLabel: fmt.Sprintf("可用 %d / %d", pool.UsableAccounts, pool.TotalAccounts),
		AvailabilityClass: "success",
		NoUsableAccounts:  pool.UsableAccounts == 0 && len(pool.Groups) > 0,
		UnknownCount:      pool.UnknownCount,
		CSRFToken:         csrfToken,
		ReturnTo:          returnTo,
		RefreshLabel:      "刷新额度",
		RefreshMessage:    refresh.Message,
		RefreshRunning:    refresh.Running,
	}
	if pool.UsableAccounts == 0 {
		v.AvailabilityClass = "danger"
	} else if pool.UsableAccounts < pool.TotalAccounts {
		v.AvailabilityClass = "warning"
	}
	if refresh.Running {
		v.RefreshDisabled = true
		v.RefreshLabel = "正在刷新…"
	} else if !refresh.NextAllowedAt.IsZero() && time.Now().UTC().Before(refresh.NextAllowedAt) {
		v.RefreshDisabled = true
		v.RefreshLabel = "稍后可刷新"
	}
	for _, group := range pool.Groups {
		plan := strings.ToUpper(group.PlanType)
		if group.PlanType == "unknown" {
			plan = "套餐未知"
		}
		period := "周额度"
		switch group.Period {
		case "five_hour":
			period = "5 小时额度"
		case "monthly":
			period = "月额度"
		}
		item := webui.QuotaGroupView{
			Label:            plan + " · " + period,
			RemainingPercent: group.RemainingPercent,
			StatusClass:      "success",
			Accounts:         fmt.Sprintf("%d 个可用 · %d 个已同步", group.AvailableAccounts, group.KnownAccounts),
			ResetAt:          s.formatTime(group.NextResetAt),
			ObservedAt:       s.formatTime(group.ObservedAt),
			Estimated:        group.Estimated,
		}
		if group.RemainingPercent < 20 {
			item.StatusClass = "danger"
		} else if group.RemainingPercent < 50 {
			item.StatusClass = "warning"
		}
		v.Groups = append(v.Groups, item)
	}
	return v
}

func (s *Server) auditViews(ctx context.Context, items []domain.AuditEvent) []webui.AuditView {
	out := make([]webui.AuditView, 0, len(items))
	actors := make(map[string]domain.User)
	for _, e := range items {
		name, phone := auditActorParts(e.ActorLabel)
		if e.ActorUserID != "" && s.Store != nil && (name == "" || phone == "") {
			actor, cached := actors[e.ActorUserID]
			if !cached {
				actor, _ = s.Store.UserByID(ctx, e.ActorUserID)
				actors[e.ActorUserID] = actor
			}
			if name == "" {
				name = actor.Name
			}
			if phone == "" {
				phone = maskedAuditPhone(actor.Phone)
			}
		}
		label := emptyDash(name)
		if phone != "" {
			label += "(" + phone + ")"
		}
		out = append(out, webui.AuditView{At: s.formatTime(e.CreatedAt), Actor: label, ActorName: emptyDash(name), ActorPhone: phone, Action: e.Action, Target: emptyDash(e.TargetLabel), Result: "success", IP: e.IP, Details: e.Detail})
	}
	return out
}

func auditActorParts(label string) (string, string) {
	label = strings.TrimSpace(label)
	open := strings.LastIndex(label, "(")
	if open < 0 || !strings.HasSuffix(label, ")") {
		return label, ""
	}
	phone := label[open+1 : len(label)-1]
	if len(phone) != 11 || phone[3:7] != "****" || !asciiDigits(phone[:3]) || !asciiDigits(phone[7:]) {
		return label, ""
	}
	return strings.TrimSpace(label[:open]), phone
}

func auditActorLabel(name, phone string) string {
	if masked := maskedAuditPhone(phone); masked != "" {
		return name + "(" + masked + ")"
	}
	return name
}

func maskedAuditPhone(phone string) string {
	if len(phone) != 11 || !asciiDigits(phone) {
		return ""
	}
	return phone[:3] + "****" + phone[7:]
}

func asciiDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func (s *Server) formatTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.In(s.Cfg.TimeZone).Format("2006-01-02 15:04")
}
func number(v int64) string { return strconv.FormatInt(v, 10) }

func reasoningEffortLabel(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "none", "disabled", "disable":
		return "无"
	case "minimal":
		return "极低"
	case "low":
		return "低"
	case "medium":
		return "中"
	case "high":
		return "高"
	case "xhigh":
		return "极高"
	case "max":
		return "最高"
	case "":
		return "—"
	default:
		return value
	}
}
func roleLabel(r domain.Role) string {
	if r == domain.RoleAdmin {
		return "管理员"
	}
	return "用户"
}
func userStatusLabel(v domain.UserStatus) string {
	m := map[domain.UserStatus]string{domain.StatusPending: "待审批", domain.StatusRejected: "已拒绝", domain.StatusApproved: "已批准", domain.StatusSuspended: "已停用", domain.StatusSuspendedPending: "停用待同步", domain.StatusDeletePending: "删除待撤销", domain.StatusDeleted: "已删除"}
	if x := m[v]; x != "" {
		return x
	}
	return string(v)
}
func keyStatusLabel(v string) string {
	m := map[string]string{"active": "正常", "revoked": "已撤销", "external_revoked": "已被外部撤销"}
	if x := m[v]; x != "" {
		return x
	}
	return v
}
func initials(name string) string {
	r := []rune(strings.TrimSpace(name))
	if len(r) == 0 {
		return "?"
	}
	if len(r) > 2 {
		r = r[len(r)-2:]
	}
	return string(r)
}
func emptyDash(v string) string {
	if strings.TrimSpace(v) == "" {
		return "—"
	}
	return v
}

// Kept as tiny indirections so helpers remain easy to test without exporting internals.
func contextBackground() context.Context              { return context.Background() }
func securityCSRF(secret []byte, token string) string { return security.CSRFToken(secret, token) }
