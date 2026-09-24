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

func TestUsageTrendRendersBothSeries(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	view := UsageView{Trend: UsageTrendView{
		Points: []UsageTrendPointView{
			{X: 48, RequestY: 28, TokenY: 180, Date: "08-20", Requests: "12", Tokens: "1,234", ShowLabel: true},
			{X: 952, RequestY: 123, TokenY: 28, Date: "08-21", Requests: "6", Tokens: "4,567", ShowLabel: true},
		},
		RequestPoints:    "48,28 952,123",
		TokenPoints:      "48,180 952,28",
		MaxRequests:      "12",
		MaxTokens:        "4,567",
		GranularityLabel: "按小时",
	}}
	var out bytes.Buffer
	if err := r.Execute(&out, PageUsage, view); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{`points="48,28 952,123"`, `points="48,180 952,28"`, "请求峰值 12", "Token 峰值 4,567", "按小时", "data-trend-tooltip", `data-requests="12"`} {
		if !strings.Contains(got, want) {
			t.Errorf("usage trend output does not contain %q", want)
		}
	}
	if strings.Contains(got, "预估成本") {
		t.Fatal("usage trend unexpectedly renders cost")
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
