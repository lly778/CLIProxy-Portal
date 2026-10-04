package webui

import (
	"bytes"
	"io/fs"
	"strings"
	"testing"
)

func TestRendererParsesAllPages(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer() error = %v", err)
	}
	if got := len(r.TemplateNames()); got != len(pageNames) {
		t.Fatalf("TemplateNames() = %d, want %d", got, len(pageNames))
	}
}

func TestUpstreamChannelsHavePreloadedContentsAndNoScriptFallback(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	for _, channel := range []string{"codex", "antigravity", "gemini"} {
		var out bytes.Buffer
		view := AdminUpstreamsView{
			LayoutView: LayoutView{ActiveNav: "admin-upstreams", CSRFToken: "test-token"},
			Channel:    channel, ChannelLabel: channel,
			Channels: []OAuthChannelView{{Value: channel, Label: channel, Selected: true}},
			Accounts: []UpstreamAccountView{{ID: "account"}},
		}
		if err := r.Execute(&out, PageAdminUpstreams, view); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			`data-upstream-channel-page data-channel="` + channel + `"`,
			`/static/upstream-channels.js?v=20261004-6`,
			`data-upstream-channel-panel data-channel="` + channel + `"`,
			`method="get" action="/admin/upstreams"`,
			`name="channel" value="` + channel + `"`,
			`name="csrf_token" value="test-token"`,
			`type="submit">切换渠道</button>`,
		} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("%s page missing %q", channel, want)
			}
		}
	}
}

func TestUpstreamChannelPanelsHaveUniqueFormsAndSelectedVisibility(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	v := AdminUpstreamsView{Channel: "antigravity"}
	for _, channel := range []string{"codex", "antigravity", "gemini"} {
		v.ChannelPanels = append(v.ChannelPanels, AdminUpstreamsView{
			Channel: channel, AliasReady: true, ReasoningReady: true, SupportsReasoning: true,
			AliasModels:     []OAuthModelView{{ID: channel + "-model"}},
			ReasoningModels: []ReasoningCapModelView{{ID: channel + "-model"}},
		})
	}
	var out bytes.Buffer
	if err := r.Execute(&out, PageAdminUpstreams, v); err != nil {
		t.Fatal(err)
	}
	for _, channel := range []string{"codex", "antigravity", "gemini"} {
		panel := `data-upstream-channel-panel data-channel="` + channel + `" `
		if channel != v.Channel {
			panel += "hidden"
		}
		if !strings.Contains(out.String(), panel+">") {
			t.Fatalf("wrong panel visibility: %s", channel)
		}
		for _, id := range []string{"oauth-alias-form-", "reasoning-cap-form-"} {
			if channel == "gemini" && id == "reasoning-cap-form-" {
				continue
			}
			if strings.Count(out.String(), `id="`+id+channel+`"`) != 1 || !strings.Contains(out.String(), `form="`+id+channel+`"`) {
				t.Fatalf("missing unique form/button association: %s%s", id, channel)
			}
		}
	}
}

func TestUpstreamQuotasUseCompactRowsAndSeparatePlan(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	view := AdminUpstreamsView{
		Channel: "codex", ChannelLabel: "Codex",
		Accounts: []UpstreamAccountView{{Account: "test@example.com", StatusLabel: "已启用", Quotas: []UpstreamAccountQuotaView{
			{Label: "5H", Plan: "PLUS", RemainingPercent: 0, StatusClass: "danger", ResetAt: "2026-10-04 21:11"},
			{Label: "7D", Plan: "PLUS", RemainingPercent: 68, StatusClass: "success", ResetAt: "2026-10-07 17:33"},
		}}},
	}
	var out bytes.Buffer
	if err := r.Execute(&out, PageAdminUpstreams, view); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{`class="upstream-row upstream-row-codex"`, `<span class="badge neutral">5H</span><strong>0%</strong><small class="muted">PLUS</small>`, `<span class="badge neutral">7D</span><strong>68%</strong><small class="muted">PLUS</small>`, `aria-valuenow="0"`, `aria-valuenow="68"`, `重置 2026-10-07 17:33`, `/static/style.css?v=20261005-5`} {
		if !strings.Contains(html, want) {
			t.Fatalf("missing compact quota row markup: %s", want)
		}
	}
	if strings.Index(html, `class="upstream-account-quotas"`) > strings.Index(html, `class="upstream-actions"`) {
		t.Fatal("quotas must precede actions in the account row")
	}
}

func TestAntigravityQuotaGroupsRenderTwoShortPeriodRows(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	view := AdminUpstreamsView{Channel: "antigravity", Accounts: []UpstreamAccountView{{QuotaGroups: []UpstreamAccountQuotaGroupView{
		{Label: "Claude / GPT", Quotas: []UpstreamAccountQuotaView{{Label: "5H", RemainingPercent: 99}, {Label: "7D", RemainingPercent: 89}}},
		{Label: "Gemini", Quotas: []UpstreamAccountQuotaView{{Label: "5H", RemainingPercent: 73}, {Label: "7D", RemainingPercent: 65}}},
	}}}}
	var out bytes.Buffer
	if err := r.Execute(&out, PageAdminUpstreams, view); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	if strings.Count(html, `class="upstream-quota-group"`) != 2 || strings.Count(html, `class="badge neutral">5H</span>`) != 2 || strings.Count(html, `class="badge neutral">7D</span>`) != 2 || !strings.Contains(html, `class="upstream-row upstream-row-grouped"`) {
		t.Fatal("expected two groups with two abbreviated periods each")
	}
	for _, name := range []string{"Claude / GPT", "Gemini"} {
		if strings.Count(html, `class="upstream-quota-group-label">`+name+`</strong>`) != 1 {
			t.Fatal("model group title must appear once")
		}
	}
}

func TestTokenSummaryLabelsMatchTrendLegend(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range []struct {
		name string
		view any
	}{
		{PageUsage, UsageView{Summary: UsageSummaryView{TotalTokens: "225M"}}},
		{PageAdminUser, AdminUserDetailView{Usage: UsageSummaryView{TotalTokens: "225M"}}},
		{PageAdminDashboard, AdminDashboardView{Usage: UsageSummaryView{TotalTokens: "225M"}}},
	} {
		var out bytes.Buffer
		if err := r.Execute(&out, page.name, page.view); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), `<span>Token 数</span><strong>225M</strong>`) || strings.Contains(out.String(), "总 Tokens") {
			t.Fatalf("inconsistent Token summary label on %s", page.name)
		}
	}
}

func TestHealthTrendLegendSharesUsageHeaderPosition(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := r.Execute(&out, PageUsage, UsageView{IsAdmin: true, Summary: UsageSummaryView{AverageDuration: "4000 ms"}, HealthTrend: HealthTrendView{Points: []HealthTrendPointView{{HasRequests: true, HasTiming: true}}}}); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	if strings.Contains(html, "data-bar-radius") {
		t.Fatal("health bars must share the usage chart corner calculation")
	}
	start := strings.Index(html, "<h2>请求健康趋势</h2>")
	if start < 0 {
		t.Fatal("health trend heading missing")
	}
	chart := strings.Index(html, `data-health-trend`)
	legend := strings.Index(html[start:], `class="trend-legend"`) + start
	if start < 0 || chart < start || legend < start || legend > chart {
		t.Fatal("health legend must be in header before chart")
	}
	if strings.Count(html, `class="usage-trend-meta"`) != 2 {
		t.Fatal("trend headers must share the same layout")
	}
	caption := "成功率、失败率与平均耗时在同一时间范围内联动展示。"
	if strings.Contains(html, "平均总耗时") {
		t.Fatal("duration labels must consistently use 平均耗时")
	}
	if strings.Contains(html, "计时请求数") || strings.Contains(html, `data-trend-value="samples"`) {
		t.Fatal("health tooltip must not display the timing request count")
	}
	if strings.Contains(html, "总耗时仅统计已记录的门户请求") {
		t.Fatal("removed timing note must not appear in the page")
	}
	if strings.Contains(html, "柱状图按接入及上传") || strings.Contains(html, "缺少分段记录的区间") {
		t.Fatal("removed stage explanation must not appear in the page")
	}
	if at := strings.Index(html, caption); at < start || at > chart || strings.Count(html, caption) != 1 {
		t.Fatal("timing description must appear only below the heading, not in the tooltip")
	}
	if !strings.Contains(html, "平均耗时") || strings.Contains(html, "平均延迟") || !strings.Contains(html, "4000 ms") {
		t.Fatal("duration metric must use total timing")
	}
}

func TestUserUsageSectionOmitsKeyAggregationCaption(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := r.Execute(&out, PageUsage, UsageView{IsAdmin: true, ShowUserStats: true}); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	if !strings.Contains(html, "<h2>按用户</h2>") {
		t.Fatal("user usage section must remain visible")
	}
	for _, heading := range []struct {
		title, caption string
	}{
		{"按用户", "展示所选时间范围内各用户的请求数与 Token 数，分别按用量排序。"},
		{"按模型", "按实际使用的模型汇总请求数与 Token 数，分别按用量排序。"},
	} {
		if !strings.Contains(html, "<h2>"+heading.title+`</h2><p class="muted small">`+heading.caption+"</p>") {
			t.Fatalf("ranking explanation must appear below %s", heading.title)
		}
	}
	if strings.Contains(html, "同一用户的历次 API Key 用量合并统计") {
		t.Fatal("removed key aggregation caption must not appear")
	}
}

func TestUserUsageRankingKeepsTwoColumnsOnNarrowScreens(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	users := []UserUsageView{{User: UserView{Name: "测试用户"}, UsageURL: "/admin/usage?user=u1", Requests: "2.5K", Tokens: "378.2M", RequestPercent: 100, TokenPercent: 100}}
	if err := r.Execute(&out, PageUsage, UsageView{IsAdmin: true, ShowUserStats: true, ByUserRequests: users, ByUserTokens: users}); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{`class="model-usage-grid user-usage-grid"`, `<h3>按请求数</h3>`, `<h3>按 Token 数</h3>`, `2.5K 请求`, `378.2M tokens`} {
		if !strings.Contains(html, want) {
			t.Fatalf("user rankings missing %s", want)
		}
	}
	if strings.Count(html, `href="/admin/usage?user=u1"`) != 2 {
		t.Fatal("both user rankings must retain their detail links")
	}
	css, err := fs.ReadFile(Assets(), "style.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{
		".model-usage-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 20px; }",
		".model-usage-grid > .model-usage-chart { container: usage-ranking / inline-size; }",
		".model-usage-grid .usage-main > :first-child { min-width: 0; overflow-wrap: anywhere; }",
		"@container usage-ranking (max-width: 220px)",
	} {
		if !bytes.Contains(css, []byte(rule)) {
			t.Fatalf("missing narrow user ranking layout: %s", rule)
		}
	}
}

func TestModelUsageRankingSharesNarrowTwoColumnLayout(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	models := []ModelUsageView{{Model: "claude-opus-4-6-thinking-long-model-name", Requests: "2.5K", Tokens: "378.2M", Percent: 100, TokenPercent: 100}}
	for _, isAdmin := range []bool{false, true} {
		var out bytes.Buffer
		if err := r.Execute(&out, PageUsage, UsageView{IsAdmin: isAdmin, ByModelRequests: models, ByModelTokens: models}); err != nil {
			t.Fatal(err)
		}
		html := out.String()
		for _, want := range []string{`class="model-usage-grid"`, `<h3>按请求数</h3>`, `<h3>按 Token 数</h3>`, `2.5K 请求`, `378.2M tokens`} {
			if !strings.Contains(html, want) {
				t.Fatalf("model rankings missing %s (admin=%t)", want, isAdmin)
			}
		}
		if strings.Count(html, `<span class="mono">`+models[0].Model+`</span>`) != 2 {
			t.Fatal("both rankings must preserve the complete model name")
		}
	}
	css, err := fs.ReadFile(Assets(), "style.css")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(css, []byte(".model-grid, .model-usage-grid, .approval-grid")) {
		t.Fatal("model rankings must not collapse with unrelated single-column card grids")
	}
	for _, rule := range []string{
		".model-usage-grid { gap: 14px; }",
		".model-usage-grid .usage-main > :last-child { flex-shrink: 0; }",
		".usage-main { flex-direction: column; align-items: flex-start; gap: 2px; }",
	} {
		if !bytes.Contains(css, []byte(rule)) {
			t.Fatalf("missing shared model ranking overflow protection: %s", rule)
		}
	}
}

func TestHealthTrendRendersStageBarsAndTooltipWithoutChangingLegacyBars(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	view := UsageView{IsAdmin: true, HealthTrend: HealthTrendView{Points: []HealthTrendPointView{
		{UsageTrendPointView: UsageTrendPointView{BarX: 100, BarWidth: 8, TokenY: 100, BarHeight: 118}, HasTiming: true, HasStages: true, AverageUpload: "200 ms", AverageWait: "1 s", AverageResponse: "2 s", Stages: []TrendBarSegmentView{
			{Class: "duration-upload", Y: 200, Height: 18, Square: true},
			{Class: "duration-wait", Y: 180, Height: 20, Square: true},
			{Class: "duration-response", Y: 100, Height: 80},
		}},
		{HasTiming: true},
	}}}
	if err := r.Execute(&out, PageUsage, view); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{`class="trend-bar duration-upload"`, `class="trend-bar duration-wait"`, `class="trend-bar duration-response"`, `data-bar-square="true"`, `data-bar-square="false"`, `data-average-upload="200 ms"`, `data-average-wait="1 s"`, `data-average-response="2 s"`, `data-trend-value="averageUpload"`, `data-trend-value="averageWait"`, `data-trend-value="averageResponse"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("stage chart missing %s", want)
		}
	}
	if strings.Count(html, `class="trend-bar duration"`) != 1 {
		t.Fatal("legacy total-only bar must remain")
	}
	for _, want := range []string{`data-trend-bar-stack`, `data-trend-bar-stack-target`, `data-trend-bar-clip clipPathUnits="userSpaceOnUse"`, `data-trend-bar-outline data-bar-x="100" data-bar-y="100" data-bar-width="8" data-bar-height="118"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("health stack missing total-column corner clip: %s", want)
		}
	}
}

func TestTrendSegmentsUseTheirLegendColorFamiliesAndMatchingTooltipSwatches(t *testing.T) {
	css, err := fs.ReadFile(Assets(), "style.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []struct{ class, color string }{
		{"duration-upload", "#58a0e7"},
		{"duration-wait", "#8dbded"},
		{"duration-response", "#357dc4"},
	} {
		rule := ".trend-bar." + part.class + ", .trend-tooltip ." + part.class + " i { fill: " + part.color + "; background: " + part.color + "; }"
		if !bytes.Contains(css, []byte(rule)) {
			t.Fatalf("missing coordinated segment and tooltip palette: %s", rule)
		}
	}
	for _, rule := range []string{
		".trend-bar { fill: #5ab38e; }",
		".trend-tooltip .tokens i { background: #5ab38e; }",
		".trend-legend .tokens i { background: #5ab38e; border-radius: 1px; }",
		".trend-bar.duration, .trend-legend .duration i, .trend-tooltip .duration i { fill: #58a0e7; background: #58a0e7; }",
	} {
		if !bytes.Contains(css, []byte(rule)) {
			t.Fatalf("legend base color changed: %s", rule)
		}
	}
}

func TestUserManagementTableKeepsAlignedAvatarsAndUserDetails(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := r.Execute(&out, PageAdminUsers, AdminUsersView{Users: []UserRowView{
		{User: UserView{ID: "u1", Name: "测试用户", Phone: "13800138000", Initials: "头像缩写", Role: "admin"}},
	}}); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{`class="avatar small-avatar" aria-hidden="true"`, "头像缩写", "测试用户", "13800138000", `class="badge role-admin"`, `href="/admin/users/u1"`, `class="user-cell"`, `class="user-name-line"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("user details or centered layout removed: %s", want)
		}
	}
}

func TestUserManagementTableContentsAreCentered(t *testing.T) {
	css, err := fs.ReadFile(Assets(), "style.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{
		".user-management-card th, .user-management-card td { text-align: center; }",
		".user-management-card .user-cell, .user-management-card .user-name-line, .user-management-card .user-row-actions { justify-content: center; }",
		".user-management-card .user-cell { position: relative; padding: 0 37px; }",
		".user-management-card .user-cell > .avatar { position: absolute; left: 0; top: 50%; transform: translateY(-50%); }",
		".user-management-card .user-name-line { position: relative; justify-self: center; }",
		".user-management-card .user-name-line .role-admin { position: absolute; left: 100%; top: 50%; margin-left: 7px; transform: translateY(-50%); }",
	} {
		if !bytes.Contains(css, []byte(rule)) {
			t.Fatalf("missing scoped user table centering: %s", rule)
		}
	}
}

func TestRequestLogColumnsBalanceModelsNumbersAndFailureDetails(t *testing.T) {
	css, err := fs.ReadFile(Assets(), "style.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{
		".request-log-table table { min-width: 1100px; table-layout: fixed; }",
		".request-log-table th:nth-child(1) { width: 9.5%; }",
		".request-log-table th:nth-child(2) { width: 11.5%; }",
		".request-log-table th:nth-child(3) { width: 22%; }",
		".request-log-table th:nth-child(n+4):nth-child(-n+8) { width: 5.5%; }",
		".request-log-table th:nth-child(9) { width: 7%; }",
		".request-log-table th:nth-child(10) { width: 7.5%; }",
		".request-log-table th:nth-child(11) { width: 9%; }",
		".request-log-table th:nth-child(12) { width: 6%; }",
		".request-log-table table { min-width: 1120px; table-layout: auto; }",
		".request-log-table .request-model-cell { min-width: 250px; }",
	} {
		if !bytes.Contains(css, []byte(rule)) {
			t.Fatalf("missing balanced request table column rule: %s", rule)
		}
	}
}

func TestLogTablesCenterContentsWithAuditTextColumnExceptions(t *testing.T) {
	css, err := fs.ReadFile(Assets(), "style.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{
		".request-log-table th, .request-log-table td, .request-detail-table th, .request-detail-table td { text-align: center; }",
		".audit-table th, .audit-table td { text-align: center; }",
		".audit-table .audit-target-cell, .audit-table td.audit-detail-cell { text-align: left; }",
	} {
		if !bytes.Contains(css, []byte(rule)) {
			t.Fatalf("missing log table alignment: %s", rule)
		}
	}
	if bytes.Contains(css, []byte(".request-log-table td:nth-child(1), .request-log-table td:nth-child(2)")) {
		t.Fatal("request time and user columns must no longer be left-aligned")
	}
	if bytes.Contains(css, []byte(".request-log-table td.request-model-cell, .request-detail-table td.request-model-cell { text-align: left; }")) {
		t.Fatal("request model columns must also be centered")
	}
	if bytes.Contains(css, []byte(".audit-table .audit-action-cell, .audit-table .audit-target-cell")) {
		t.Fatal("audit action column must also be centered")
	}
}

func TestBrandFaviconIsEmbeddedAndLinked(t *testing.T) {
	icon, err := fs.ReadFile(Assets(), "favicon.svg")
	if err != nil {
		t.Fatalf("read embedded favicon: %v", err)
	}
	if !bytes.Contains(icon, []byte("<svg")) {
		t.Fatal("embedded favicon is not SVG")
	}
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := r.Execute(&out, PageLogin, LoginView{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `href="/static/favicon.svg"`) {
		t.Fatal("favicon link missing from page head")
	}
}

func TestRepresentativePagesRenderEscapedViewData(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer() error = %v", err)
	}
	view := LayoutView{
		Title:      "登录 <测试>",
		Brand:      "门户 & 测试",
		CSRFToken:  "csrf-token",
		Error:      "错误 <内容>",
		IsLoggedIn: false,
	}
	var out bytes.Buffer
	if err := r.Execute(&out, PageLogin, LoginView{LayoutView: view, Phone: "13800138000"}); err != nil {
		t.Fatalf("login render error = %v", err)
	}
	got := out.String()
	for _, want := range []string{"手机号", "csrf-token", "13800138000"} {
		if !strings.Contains(got, want) {
			t.Errorf("login output does not contain %q", want)
		}
	}
	if strings.Contains(got, "<测试>") || strings.Contains(got, "错误 <内容>") {
		t.Error("unescaped data found in HTML output")
	}

	out.Reset()
	user := &UserView{ID: "u1", Name: "张三", Phone: "13800138000", Role: "admin", RoleLabel: "管理员"}
	if err := r.Execute(&out, PageAdminDashboard, AdminDashboardView{
		LayoutView: LayoutView{Brand: "CLIProxy 账号门户", IsLoggedIn: true, User: user},
		Counts:     []CountView{{Label: "用户", Value: "1"}},
	}); err != nil {
		t.Fatalf("admin render error = %v", err)
	}
	if !strings.Contains(out.String(), "管理概览") || !strings.Contains(out.String(), "用户") {
		t.Error("admin output missing representative content")
	}
}

func TestRequestFailureRendersFullTextTooltip(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	view := ActivityView{Requests: []RequestView{{
		Status:      "failed",
		StatusLabel: "失败",
		Error:       "HTTP 503 · shortened…",
		ErrorFull:   `HTTP 503 · full "detail" <safe>`,
	}}}
	var out bytes.Buffer
	if err := r.Execute(&out, PageActivity, view); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, `title="HTTP 503 · full &#34;detail&#34; &lt;safe&gt;"`) {
		t.Fatalf("request failure tooltip missing or unescaped: %s", got)
	}
	if !strings.Contains(got, ">HTTP 503 · shortened…</div>") {
		t.Fatalf("request failure summary missing: %s", got)
	}
}

func TestRequestDurationDisplaysOnlyTotalAndEscapesStageTooltip(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	request := RequestView{TotalLatency: "473000 ms", Latency: "8000 ms", LatencyDetail: "服务器耗时：473000 ms\n模型调用耗时：8000 ms\n接入及请求上传：465000 ms\n<safe> \"detail\""}
	for _, test := range []struct {
		page string
		view any
	}{
		{PageActivity, ActivityView{Requests: []RequestView{request}}},
		{PageDashboard, DashboardView{RecentUsage: []RequestView{request}}},
		{PageAdminRequests, AdminRequestsView{Requests: []GlobalRequestView{{RequestView: request}}}},
		{PageAdminUser, AdminUserDetailView{Requests: []RequestView{request}}},
	} {
		var out bytes.Buffer
		if err := r.Execute(&out, test.page, test.view); err != nil {
			t.Fatal(err)
		}
		got := out.String()
		if !strings.Contains(got, "<th>耗时</th>") || strings.Contains(got, "总耗时") || !strings.Contains(got, "服务器耗时：473000 ms") || !strings.Contains(got, ">473000 ms</span>") || strings.Contains(got, ">8000 ms<") || !strings.Contains(got, "模型调用耗时：8000 ms") || !strings.Contains(got, "&lt;safe&gt; &#34;detail&#34;") {
			t.Fatalf("total-only display or escaped tooltip missing on %s: %s", test.page, got)
		}
	}
}

func TestAuditCellsDoNotHaveUnconditionalTooltips(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	view := AdminSystemView{Entries: []AuditView{{Action: "下载交互记录", Target: "交互记录", Details: "完整显示的详情", Result: "success"}}}
	var out bytes.Buffer
	if err := r.Execute(&out, PageAdminSystem, view); err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		`class="audit-action-text">下载交互记录`,
		`class="audit-target-text">交互记录`,
		`class="audit-detail-text">完整显示的详情`,
	} {
		if !strings.Contains(out.String(), fragment) {
			t.Fatalf("audit cell should not have a default tooltip: %s", fragment)
		}
	}
}

func TestSystemCheckButtonsAreBesideTheirSections(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := r.Execute(&out, PageAdminSystem, AdminSystemView{}); err != nil {
		t.Fatal(err)
	}
	page := out.String()
	storage := strings.Index(page, `<section class="section" id="storage">`)
	health := strings.Index(page, `<section class="section" id="health">`)
	audit := strings.Index(page, `<section class="section section-gap" id="audit">`)
	storageButton := strings.Index(page, `action="/admin/system/check#storage"`)
	healthButton := strings.Index(page, `action="/admin/system/check#health"`)
	if storage < 0 || health < 0 || audit < 0 || storageButton <= storage || storageButton >= health || healthButton <= health || healthButton >= audit || strings.Count(page, `action="/admin/system/check#`) != 2 || strings.Count(page, `name="check_scope" value="storage"`) != 1 || strings.Count(page, `name="check_scope" value="health"`) != 1 {
		t.Fatalf("check buttons should appear once beside each section: %s", page)
	}
}

func TestStorageCardsHideLatencyButHealthCardsKeepIt(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	view := AdminSystemView{
		Storage: []HealthCheckView{{Component: "CPAMP SQLite", CheckedAt: "检查时间", Latency: "2ms"}},
		Checks:  []HealthCheckView{{Component: "模型网关", CheckedAt: "检查时间", Latency: "3ms"}},
	}
	var out bytes.Buffer
	if err := r.Execute(&out, PageAdminSystem, view); err != nil {
		t.Fatal(err)
	}
	page := out.String()
	storage, health, found := strings.Cut(page, `<section class="section" id="health">`)
	if !found || !strings.Contains(storage, "CPAMP SQLite") || strings.Contains(storage, "延迟") || !strings.Contains(health, "延迟 3ms") {
		t.Fatalf("storage latency should be hidden and health latency retained: %s", page)
	}
}

func TestDialogueDownloadHasAdjacentTableColumn(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	request := RequestView{Status: "success", StatusLabel: "成功", CaptureID: "capture123"}
	for _, tc := range []struct {
		name string
		page string
		view any
		href string
	}{
		{"user activity", PageActivity, ActivityView{Requests: []RequestView{request}}, "/logs/dialogue/capture123"},
		{"admin requests", PageAdminRequests, AdminRequestsView{Requests: []GlobalRequestView{{RequestView: request}}}, "/admin/logs/dialogue/capture123"},
		{"admin user", PageAdminUser, AdminUserDetailView{Requests: []RequestView{request}}, "/admin/logs/dialogue/capture123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := r.Execute(&out, tc.page, tc.view); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			if !strings.Contains(got, `<th class="request-status-cell">状态</th><th class="request-download-cell">交互记录</th>`) ||
				!strings.Contains(got, `class="request-download-cell"><a class="request-status-download" href="`+tc.href+`"`) {
				t.Fatalf("download link does not have a column beside status: %s", got)
			}
			if strings.Contains(got, "可下载的对话</h2>") {
				t.Fatal("separate dialogue download card remains")
			}
		})
	}
}

func TestAllPagesExecuteWithZeroViews(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer() error = %v", err)
	}
	views := map[string]any{
		PageLogin:          LoginView{},
		PageRegister:       RegisterView{},
		PageReset:          ResetView{},
		PageResubmit:       ResubmitView{},
		PageDashboard:      DashboardView{},
		PageStatus:         StatusView{},
		PageKey:            KeyPageView{},
		PageUsage:          UsageView{},
		PageModels:         ModelsView{},
		PageActivity:       ActivityView{},
		PageHealth:         HealthView{},
		PageProfile:        ProfileView{},
		PagePassword:       PasswordView{},
		PageAdminDashboard: AdminDashboardView{},
		PageAdminUsers:     AdminUsersView{},
		PageAdminUser:      AdminUserDetailView{},
		PageAdminApprovals: AdminApprovalsView{},
		PageAdminUpstreams: AdminUpstreamsView{},
		PageAdminPolicy:    AdminPolicyView{},
		PageAdminRequests:  AdminRequestsView{},
		PageAdminSystem:    AdminSystemView{},
		PageError:          ErrorView{},
	}
	for page, view := range views {
		var out bytes.Buffer
		if err := r.Execute(&out, page, view); err != nil {
			t.Errorf("page %q execute error = %v", page, err)
		}
	}
}

func TestUpstreamEmailObfuscationOptOutSurvivesRendering(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, account, filename, escapedAccount, escapedFilename string
	}{
		{"email and credential filename", "test@example.com", "codex-test@example.com-plus.json", "test@example.com", "codex-test@example.com-plus.json"},
		{"escape HTML and opt-out injection", `<script>alert("x")</script>@example.com`, `<!--/email_off--><img src=x onerror=alert(1)>@example.com`, `&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;@example.com`, `&lt;!--/email_off--&gt;&lt;img src=x onerror=alert(1)&gt;@example.com`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			view := AdminUpstreamsView{Accounts: []UpstreamAccountView{{Account: tc.account, Name: tc.filename}}}
			if err := r.Execute(&out, PageAdminUpstreams, view); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			for _, want := range []string{
				`<strong><!--email_off-->` + tc.escapedAccount + `<!--/email_off--></strong>`,
				`<span class="muted mono"><!--email_off-->` + tc.escapedFilename + `<!--/email_off--></span>`,
			} {
				if !strings.Contains(got, want) {
					t.Errorf("rendered upstream credentials missing %q", want)
				}
			}
			if strings.Count(got, "<!--email_off-->") != 2 || strings.Count(got, "<!--/email_off-->") != 2 {
				t.Fatal("both credential fields must have intact opt-out boundaries")
			}
		})
	}
}

func TestUsageTrendRendersBothSeries(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	view := UsageView{Trend: UsageTrendView{
		ShowSymbols: true,
		Points: []UsageTrendPointView{
			{X: 290, RequestY: 28, TokenY: 180, BarX: 272, BarWidth: 36, BarHeight: 38, Date: "08-20", Requests: "12", Tokens: "1,234", ShowLabel: true},
			{X: 710, RequestY: 123, TokenY: 28, BarX: 692, BarWidth: 36, BarHeight: 190, Date: "08-21", Requests: "6", Tokens: "4,567", ShowLabel: true},
		},
		RequestPath:      "M290,28 L710,123",
		RequestAreaPath:  "M290,28 L710,123 L710,218 L290,218 Z",
		AxisTicks:        []UsageTrendAxisTickView{{Y: 218, Requests: "0", Tokens: "0"}, {Y: 28, Requests: "15", Tokens: "5K"}},
		MaxRequests:      "12",
		MaxTokens:        "4,567",
		GranularityLabel: "按小时",
	}}
	for _, page := range []struct {
		name string
		view any
	}{
		{PageUsage, view},
		{PageAdminUser, AdminUserDetailView{Target: UserView{ID: "test-user"}, Trend: view.Trend}},
	} {
		t.Run(page.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := r.Execute(&out, page.name, page.view); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			if !strings.Contains(got, `<div><span class="tokens"><i></i>Token 数</span><b data-trend-tokens>`) {
				t.Fatal("chart tooltip must use the same Token label as the legend")
			}
			for _, want := range []string{`<path class="trend-line requests" d="M290,28 L710,123">`, `<path class="trend-bar tokens" data-bar-x="272" data-bar-y="180" data-bar-width="36" data-bar-height="38" d="M272,180 h36 v38 h-36 Z">`, `class="trend-area requests"`, `class="trend-axis-label tokens"`, `class="trend-axis-label requests"`, `<div class="trend-legend"><span class="requests"><i></i>请求数</span><span class="tokens"><i></i>Token 数</span></div>`, `aria-label="请求数平滑曲线（左轴）与 Token 柱状图（右轴）"`, "data-trend-tooltip", `data-requests="12"`, "请求数与 Token 数在同一时间范围内联动展示。"} {
				if !strings.Contains(got, want) {
					t.Errorf("usage trend output does not contain %q", want)
				}
			}
			for _, unwanted := range []string{"usage-trend-scales", "trend-granularity", "请求峰值", "Token 峰值", "每 6 小时", "最近 24 小时按小时", "预估成本", "在图表上移动鼠标可查看详细数值", `class="trend-line tokens"`, `class="bar-chart`} {
				if strings.Contains(got, unwanted) {
					t.Errorf("usage trend unexpectedly renders removed content %q", unwanted)
				}
			}
			if page.name == PageAdminUser && !strings.Contains(got, `href="/admin/usage?user=test-user&amp;range=7d"`) {
				t.Fatal("user detail chart lost its detailed usage link")
			}
		})
	}
}

func TestDenseUsageTrendHidesOnlySymbols(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	trend := UsageTrendView{RequestPath: "M100,218 L900,28"}
	for i := 0; i < 168; i++ {
		trend.Points = append(trend.Points, UsageTrendPointView{X: 100 + i*4, Date: "10-02 12:00", Requests: "2", Tokens: "1234"})
	}
	var out bytes.Buffer
	if err := r.Execute(&out, PageUsage, UsageView{Trend: trend}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Count(got, "data-trend-point ") != 168 || strings.Count(got, "data-trend-label ") != 168 || strings.Count(got, `class="trend-bar tokens"`) != 168 || strings.Count(got, `class="trend-dot requests"`) != 168 || strings.Count(got, `r="3" hidden`) != 168 {
		t.Fatal("dense chart must retain every bar, point, tooltip and hidden circle for zooming")
	}
	for _, want := range []string{"data-trend-clip", "data-trend-plot", `data-requests="2"`, `data-tokens="1234"`, `d="M100,218 L900,28"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing dense chart content %q", want)
		}
	}
}

func TestUsageTrendRendersOnlyTotalTokenBarsAndTooltip(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	view := UsageView{Trend: UsageTrendView{Points: []UsageTrendPointView{
		{BarX: 100, BarWidth: 8, TokenY: 158, BarHeight: 60, Tokens: "140", Requests: "2"},
		{BarX: 120, BarWidth: 8, TokenY: 218, BarHeight: 0, Tokens: "0", Requests: "0"},
	}}}
	var out bytes.Buffer
	if err := r.Execute(&out, PageUsage, view); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{`data-bar-x="100" data-bar-y="158" data-bar-width="8" data-bar-height="60"`, `data-tokens="140"`, `data-tokens="0"`, `data-trend-tokens`, `data-trend-requests`} {
		if !strings.Contains(html, want) {
			t.Fatalf("missing total-only chart value: %s", want)
		}
	}
	for _, unwanted := range []string{"tokens-input", "tokens-cache", "tokens-output", "tokens-reasoning", "data-input-tokens", "data-cache-tokens", "data-output-tokens", "data-trend-value", "data-trend-bar-stack", "data-bar-square", "Token 柱按输入"} {
		if strings.Contains(html, unwanted) {
			t.Fatalf("removed Token breakdown still rendered: %s", unwanted)
		}
	}
	if strings.Count(html, `class="trend-bar tokens"`) != 2 {
		t.Fatal("each interval must render exactly one total Token bar")
	}
}

func TestQuotaPoolRendersAsyncRefreshMarkers(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	view := DashboardView{Quota: QuotaPoolView{
		Show: true, Available: true, Provider: "Codex", Accounts: "共 2 个已启用账号",
		AvailabilityLabel: "可用 0 / 2", AvailabilityClass: "danger", NoUsableAccounts: true,
		CSRFToken: "csrf-token", ReturnTo: "/dashboard", RefreshRunning: true,
		Groups: []QuotaGroupView{{Label: "PLUS · 周额度", RemainingPercent: 50}},
	}}
	var out bytes.Buffer
	if err := r.Execute(&out, PageDashboard, view); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{`data-quota-pool`, `data-refresh-running="true"`, `data-quota-refresh-form`, `最早可恢复时间`, `可用 0 / 2`, `当前无可用上游账号`} {
		if !strings.Contains(got, want) {
			t.Errorf("quota pool output does not contain %q", want)
		}
	}
	if strings.Contains(got, "查看完整用量") {
		t.Fatal("dashboard should not render the full usage button")
	}
	if strings.Contains(got, "quota-refresh-status") {
		t.Fatal("quota pool should not render refresh status text")
	}
	if !strings.Contains(got, `<p class="quota-pool-note"><span data-quota-text>这是所有用户共享的上游账号池状态，不是个人限额。</span></p>`) || strings.Contains(got, "不同套餐和不同周期不会混合计算") || strings.Contains(got, "长周期额度耗尽的账号仍计入") {
		t.Fatal("quota pool note should contain only the shared pool explanation")
	}
}

func TestTextCursorPolicyPreservesSelectionAndInteractiveControls(t *testing.T) {
	css, err := fs.ReadFile(Assets(), "style.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{
		`:where(a[href], button, summary, [role="button"], label) { cursor: pointer; }`,
		`.quota-carousel-viewport:not(.is-drag-pending):not(.is-dragging) [data-quota-text] { cursor: text; user-select: text; }`,
		`.quota-carousel-viewport:is(.is-drag-pending, .is-dragging), .quota-carousel-viewport:is(.is-drag-pending, .is-dragging) * { -webkit-user-select: none; user-select: none; }`,
		`.quota-carousel-viewport.is-drag-pending [data-quota-text] { cursor: grab; }`,
		`.quota-carousel-viewport.is-dragging [data-quota-text] { cursor: grabbing; user-select: none; }`,
		`.trend-axis-label, .trend-x-label { cursor: text; user-select: text; }`,
		`.trend-interactive { touch-action: pan-y; user-select: none; cursor: grab; }`,
		`.trend-dragging, .trend-dragging svg { cursor: grabbing; }`,
	} {
		if !bytes.Contains(css, []byte(rule)) {
			t.Fatalf("missing text cursor policy: %s", rule)
		}
	}
	if bytes.Contains(css, []byte("cursor: crosshair")) {
		t.Fatal("chart pointer capture must not switch to a crosshair cursor")
	}
	if bytes.Contains(css, []byte(":where(h1,")) {
		t.Fatal("text cursor must not cover entire headings, paragraphs or table cells")
	}
	if !bytes.Contains(css, []byte(".quota-pool-value > strong, .quota-pool-value > span { display: block; }")) {
		t.Fatal("quota layout must not stretch the nested inline text hit areas")
	}
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := r.Execute(&out, PageDashboard, DashboardView{Quota: QuotaPoolView{Show: true, Provider: "Codex", Available: true, Groups: []QuotaGroupView{{Label: "PLUS · 5 小时额度", RemainingPercent: 50, Accounts: "2 个可用", ResetAt: "2026-10-05 01:03", ObservedAt: "2026-10-05 00:22"}}}}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{`<h2><span data-quota-text>上游额度池</span></h2>`, `<span><span data-quota-text>PLUS · 5 小时额度</span></span>`, `<dt><span data-quota-text>账号状态</span></dt>`, `<dd><span data-quota-text>2 个可用</span></dd>`, `<dd><span data-quota-text>2026-10-05 01:03</span></dd>`, `<dd><span data-quota-text>2026-10-05 00:22</span></dd>`} {
		if !strings.Contains(out.String(), text) {
			t.Fatalf("quota text must support native selection: %s", text)
		}
	}
	if strings.Count(out.String(), "data-quota-text") != strings.Count(out.String(), "<span data-quota-text>") {
		t.Fatal("only tight inline text spans may block carousel dragging")
	}
}

func TestQuotaPoolsRenderOneChannelCarouselWithPreloadedProviders(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range []string{PageDashboard, PageUsage} {
		quota := QuotaPoolView{Show: true, Providers: []QuotaPoolView{
			{Provider: "Codex", Available: true, Groups: []QuotaGroupView{{Label: "PLUS · 周额度", RemainingPercent: 50}}},
			{Provider: "Antigravity", Available: true, Groups: []QuotaGroupView{{Label: "Gemini · 周额度", RemainingPercent: 75}}},
		}}
		var view any = DashboardView{Quota: quota}
		if page == PageUsage {
			view = UsageView{Quota: quota}
		}
		var out bytes.Buffer
		if err := r.Execute(&out, page, view); err != nil {
			t.Fatal(err)
		}
		html := out.String()
		for _, want := range []string{`class="quota-carousel"`, `data-quota-viewport`, `data-quota-track`, `data-quota-provider="Codex"`, `data-quota-provider="Antigravity"`, `data-quota-controls hidden`, `aria-label="下一张额度卡片"`, `/static/quota-carousel.js?v=20261005-2`} {
			if !strings.Contains(html, want) {
				t.Fatalf("%s missing carousel markup: %s", page, want)
			}
		}
		if strings.Count(html, `data-quota-pool data-refresh-running=`) != 1 {
			t.Fatal("providers must share one replaceable carousel root")
		}
		if strings.Contains(html, `quota-carousel-channels`) || strings.Contains(html, `data-quota-select=`) {
			t.Fatal("carousel must not render a separate bottom channel row")
		}
	}
	var out bytes.Buffer
	if err := r.Execute(&out, PageDashboard, DashboardView{Quota: QuotaPoolView{Show: true, Provider: "Codex"}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), `data-quota-controls`) {
		t.Fatal("single-provider pool must not show channel navigation")
	}
}

func TestQuotaPoolColumnsRenderAllWindowsWithoutFourFullWidthRows(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	quota := QuotaPoolView{Show: true, Available: true, Provider: "Antigravity", Columns: [][]QuotaGroupView{
		{{Label: "Claude / GPT · 5 小时额度", RemainingPercent: 34}, {Label: "Claude / GPT · 周额度", RemainingPercent: 63}},
		{{Label: "Gemini · 5 小时额度", RemainingPercent: 96}, {Label: "Gemini · 周额度", RemainingPercent: 99}},
	}}
	var out bytes.Buffer
	if err := r.Execute(&out, PageDashboard, DashboardView{LayoutView: LayoutView{IsLoggedIn: true}, Quota: quota}); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	if !strings.Contains(html, `class="quota-pool-columns"`) || strings.Count(html, `class="quota-group-list"`) != 4 || strings.Count(html, `class="quota-group"`) != 8 {
		t.Fatal("expected desktop columns and two preloaded narrow cards")
	}
	for _, column := range quota.Columns {
		for _, window := range column {
			if strings.Count(html, window.Label+`</span>`) != 2 {
				t.Fatalf("window must appear in desktop and narrow layout: %s", window.Label)
			}
		}
	}
	for _, identity := range []string{`data-quota-provider="Antigravity"`, `data-quota-provider="Antigravity · Claude / GPT"`, `data-quota-provider="Antigravity · Gemini"`} {
		if !strings.Contains(html, identity) {
			t.Fatalf("missing independent card identity: %s", identity)
		}
	}
	if strings.Contains(html, "<select") {
		t.Fatal("quota model families must use cards, not dropdowns")
	}
	if !strings.Contains(html, `<svg width="16" height="16"`) {
		t.Fatal("navigation arrows must use centered SVG icons")
	}
}

func TestAllQuotaChannelsUseOnePercentageLayoutAndEstimateTooltip(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	quota := QuotaPoolView{Show: true, Providers: []QuotaPoolView{
		{Available: true, Provider: "Codex", Groups: []QuotaGroupView{{Label: "PLUS · 5 小时额度", RemainingPercent: 22}}},
		{Available: true, Provider: "Antigravity", Columns: [][]QuotaGroupView{{{Label: "Gemini · 5 小时额度", RemainingPercent: 96}}}},
	}}
	var out bytes.Buffer
	if err := r.Execute(&out, PageDashboard, DashboardView{Quota: quota}); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{`<div class="quota-pool-value"><span><span data-quota-text>PLUS · 5 小时额度</span></span><strong title="额度剩余（按账号等权估算）"><span data-quota-text>22%</span></strong></div>`, `<div class="quota-pool-value"><span><span data-quota-text>Gemini · 5 小时额度</span></span><strong title="额度剩余（按账号等权估算）"><span data-quota-text>96%</span></strong></div>`} {
		if !strings.Contains(html, want) {
			t.Fatalf("channel must share the title/percentage markup: %s", want)
		}
	}
	if strings.Count(html, `title="额度剩余（按账号等权估算）"`) != 2 || strings.Count(html, `<p class="quota-pool-note"><span data-quota-text>这是所有用户共享的上游账号池状态，不是个人限额。</span></p>`) != 2 {
		t.Fatal("estimate must remain available in percentage tooltips without adding visible explanation rows")
	}
}

func TestQuotaPoolHeaderOmitsDuplicateAccountTotal(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	view := DashboardView{Quota: QuotaPoolView{Show: true, Available: true, Provider: "Codex", Accounts: "共 2 个已启用账号", AvailabilityLabel: "可用 2 / 2", RefreshLabel: "刷新额度", Groups: []QuotaGroupView{{Label: "PLUS · 周额度", Accounts: "2 个可用 · 2 个已同步"}}}}
	var out bytes.Buffer
	if err := r.Execute(&out, PageDashboard, view); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	if strings.Contains(html, "共 2 个已启用账号") {
		t.Fatal("quota header should not repeat the availability denominator")
	}
	for _, want := range []string{"可用 2 / 2", "刷新额度", "2 个可用 · 2 个已同步"} {
		if !strings.Contains(html, want) {
			t.Fatalf("existing availability, refresh or period detail missing: %s", want)
		}
	}
}

func TestRecentAuditUsesResponsiveCardWithoutTruncatingTargets(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	const target = "上游账号 (d1711e2f9e0cf4cb7fa94b89bedabb29a1cffe94501a6648021122299abcdef)"
	var out bytes.Buffer
	view := AdminDashboardView{RecentAudit: []AuditView{{Action: "启用上游账号", Actor: "管理员", Target: target, At: "2026-10-04 19:32"}}}
	if err := r.Execute(&out, PageAdminDashboard, view); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`class="card audit-mini-card"`, target, `<time class="muted small">2026-10-04 19:32</time>`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing responsive audit markup or full data: %s", want)
		}
	}
}

func TestDashboardSummaryUsesScopedResponsiveLayoutAndPreservesActions(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := r.Execute(&out, PageDashboard, DashboardView{}); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{`class="grid grid-3 dashboard-summary-grid"`, `href="/status"`, `href="/key"`, `href="/usage?range=7d"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("missing responsive summary layout or existing action: %s", want)
		}
	}
	if strings.Count(html, `class="card summary-card"`) != 3 {
		t.Fatal("summary must retain all three cards")
	}
}

func TestResponsiveCardGroupsKeepRequestedColumnPolicies(t *testing.T) {
	css, err := fs.ReadFile(Assets(), "style.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{
		`.dashboard-summary-grid, #storage .system-health-grid, #health .system-health-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 12px; }`,
		`.oauth-model-list, .oauth-alias-list, .reasoning-cap-list { grid-template-columns: repeat(auto-fit, minmax(min(100%, 260px), 1fr)); }`,
		`.key-actions { display: grid; width: 100%; grid-template-columns: repeat(auto-fit, minmax(260px, 1fr)); gap: 14px; }`,
		`.dashboard-summary-grid > .summary-card:last-child,`,
		`#health .system-health-grid > .system-health-card:last-child { grid-column: 1 / -1; }`,
		`.dashboard-summary-grid > .summary-card, .system-health-grid > .system-health-card { min-width: 0; padding: 16px; overflow-wrap: anywhere; }`,
		`.dashboard-summary-grid .key-mask { padding: 5px 6px; font-size: 13px; letter-spacing: .04em; }`,
		`.oauth-model-row .upstream-main { grid-column: 1 / -1; }`,
		`.oauth-model-row form { grid-column: 2; grid-row: 2; justify-self: end; }`,
		`.reasoning-cap-row { grid-template-columns: minmax(0, 1fr); padding: 12px; }`,
		`.reasoning-cap-row { display: grid; grid-template-columns: minmax(0, 1fr); align-content: start;`,
	} {
		if !bytes.Contains(css, []byte(rule)) {
			t.Fatalf("missing available-width card layout policy: %s", rule)
		}
	}
	if bytes.Contains(css, []byte("@media (min-width: 761px) and (max-width: 1080px)")) {
		t.Fatal("third summary and health cards must also span both columns on phones")
	}
	if bytes.Contains(css, []byte("grid-template-columns: minmax(0, 1fr) minmax(145px, 180px)")) {
		t.Fatal("reasoning selects must not squeeze model IDs into a vertical column")
	}
}

func TestAdminUserHeroPreservesActionsAndGroupsIdentity(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"approved", "pending", "suspended", "readonly"} {
		view := AdminUserDetailView{LayoutView: LayoutView{CSRFToken: "fixture-csrf"}, Target: UserView{ID: "test-user", Initials: "测试", JoinedAt: "注册时间", LastLoginAt: "登录时间"}, CanSuspend: state == "approved", CanApprove: state == "pending", CanReject: state == "pending", CanUnsuspend: state == "suspended"}
		var out bytes.Buffer
		if err := r.Execute(&out, PageAdminUser, view); err != nil {
			t.Fatal(err)
		}
		hero, rest, found := strings.Cut(out.String(), `<div class="grid grid-2 section-gap admin-user-overview">`)
		if !found || rest == "" || !strings.Contains(hero, `class="detail-hero-identity"`) || !strings.Contains(hero, `class="detail-hero-meta muted small"`) {
			t.Fatal("avatar, status and account dates must stay grouped together")
		}
		if got := strings.Contains(hero, `class="stack-actions detail-hero-actions"`); got != (state != "readonly") {
			t.Fatalf("state %s renders an incorrect or empty action panel", state)
		}
		for _, action := range []string{"approve", "reject", "suspend", "unsuspend"} {
			want := state == "pending" && (action == "approve" || action == "reject") || state == "approved" && action == "suspend" || state == "suspended" && action == "unsuspend"
			start := `method="post" action="/admin/users/test-user/` + action + `"`
			if strings.Contains(hero, start) != want {
				t.Fatalf("state %s changes authorization for %s", state, action)
			}
			if want {
				_, form, _ := strings.Cut(hero, start)
				form, _, _ = strings.Cut(form, `</form>`)
				if !strings.Contains(form, `name="csrf_token" value="fixture-csrf"`) {
					t.Fatal("redesigned action must preserve CSRF protection")
				}
				if action == "reject" && !strings.Contains(form, `name="reason" placeholder="填写拒绝原因" required`) {
					t.Fatal("rejection reason must remain required")
				}
				if action == "suspend" && (!strings.Contains(form, `data-confirm="停用会立即撤销 Key，确定继续吗？"`) || !strings.Contains(form, `name="reason" placeholder="可选，填写停用原因"`) || strings.Contains(form, "required")) {
					t.Fatal("suspension confirmation and optional reason must be preserved")
				}
			}
		}
	}
	css, err := fs.ReadFile(Assets(), "style.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{`.admin-action-list { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(100%, 260px), 1fr)); gap: 12px; }`, `@container account-hero (max-width: 620px)`, `.detail-hero-main { flex: 1; min-width: 0; }`, `.detail-hero-layout.has-account-actions { grid-template-columns: minmax(0, 1fr) 240px; }`} {
		if !bytes.Contains(css, []byte(rule)) {
			t.Fatalf("missing responsive user-detail layout: %s", rule)
		}
	}
}

func TestQuotaRefreshCompletedTextPrecedesButton(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	for _, completed := range []bool{false, true} {
		for _, page := range []string{PageDashboard, PageAdminUpstreams} {
			var view any = DashboardView{Quota: QuotaPoolView{Show: true, RefreshCompleted: completed, RefreshDisabled: true, RefreshLabel: "稍后可刷新"}}
			if page == PageAdminUpstreams {
				view = AdminUpstreamsView{RefreshCompleted: completed, RefreshDisabled: true, RefreshLabel: "稍后可刷新"}
			}
			var out bytes.Buffer
			if err := r.Execute(&out, page, view); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			result := strings.Index(got, `class="muted small quota-refresh-result" role="status">`)
			button := strings.Index(got, `class="button secondary quota-refresh-button"`)
			if completed && (result < 0 || button < result) || !completed && result >= 0 {
				t.Fatalf("page %s completed=%v should show success only before the refresh button", page, completed)
			}
			if completed && !strings.Contains(got[result:button], "已刷新") {
				t.Fatal("refresh status must keep its success message")
			}
		}
	}
}

func TestAdminUsageNavigationIsRenderedAndActive(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	admin := &UserView{Role: "admin", RoleLabel: "管理员"}
	var out bytes.Buffer
	if err := r.Execute(&out, PageUsage, UsageView{LayoutView: LayoutView{IsLoggedIn: true, User: admin, ActiveNav: "admin-usage"}, IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, `class="nav-item active" href="/admin/usage"`) || !strings.Contains(got, "全局用量") {
		t.Fatal("admin usage navigation was not rendered as active")
	}
	if !strings.Contains(got, `<nav data-mobile-nav>`) {
		t.Fatal("mobile navigation scroll restoration marker was not rendered")
	}
	for _, want := range []string{`data-nav-controls`, `data-nav-previous aria-label="向左浏览导航" hidden`, `data-nav-next aria-label="向右浏览导航" hidden`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing narrow navigation control: %s", want)
		}
	}
}
