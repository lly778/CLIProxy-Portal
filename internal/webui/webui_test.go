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
	if strings.Contains(html, "总耗时仅统计已记录的门户请求") {
		t.Fatal("removed timing note must not appear in the page")
	}
	if at := strings.Index(html, caption); at < start || at > chart || strings.Count(html, caption) != 1 {
		t.Fatal("timing description must appear only below the heading, not in the tooltip")
	}
	if !strings.Contains(html, "平均耗时") || strings.Contains(html, "平均延迟") || !strings.Contains(html, "4000 ms") {
		t.Fatal("duration metric must use total timing")
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
	if !strings.Contains(got, `<p class="quota-pool-note">这是所有用户共享的上游账号池状态，不是个人限额。</p>`) || strings.Contains(got, "不同套餐和不同周期不会混合计算") || strings.Contains(got, "长周期额度耗尽的账号仍计入") {
		t.Fatal("quota pool note should contain only the shared pool explanation")
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
			result := strings.Index(got, `class="muted small quota-refresh-result" role="status">已刷新</span>`)
			button := strings.Index(got, `class="button secondary quota-refresh-button"`)
			if completed && (result < 0 || button < result) || !completed && result >= 0 {
				t.Fatalf("page %s completed=%v should show success only before the refresh button", page, completed)
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
}
