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
		PageAdminAdmins:    AdminAdminsView{},
		PageAdminPolicy:    AdminPolicyView{},
		PageAdminAudit:     AdminAuditView{},
		PageAdminHealth:    AdminHealthView{},
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
