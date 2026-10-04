package httpserver

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/config"
	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/webui"
)

func TestSharedQuotaProvidersRenderTogether(t *testing.T) {
	s := &Server{Keys: service.NewKeys(nil, nil), Cfg: config.Config{TimeZone: time.UTC}}
	pools := []service.UpstreamQuotaPool{
		{Provider: "Codex", TotalAccounts: 2, UsableAccounts: 2, Groups: []service.UpstreamQuotaGroup{{PlanType: "plus", Period: "five_hour", RemainingPercent: 80, KnownAccounts: 2, AvailableAccounts: 2}}},
		{Provider: "Antigravity", TotalAccounts: 1, UsableAccounts: 1, Groups: []service.UpstreamQuotaGroup{{Label: "Claude", Period: "claude-gpt", RemainingPercent: 60, KnownAccounts: 1, AvailableAccounts: 1}, {Label: "Gemini", Period: "gemini", RemainingPercent: 90, KnownAccounts: 1, AvailableAccounts: 1}}},
	}
	v := s.quotaPoolsView(pools, "csrf", "/dashboard")
	if len(v.Providers) != 2 || v.Providers[1].Groups[0].Label != "Claude" {
		t.Fatalf("view %#v", v)
	}
	ui, err := webui.NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	output := httptest.NewRecorder()
	if err := ui.Render(output, webui.PageDashboard, webui.DashboardView{Quota: v}); err != nil {
		t.Fatal(err)
	}
	page := output.Body.String()
	if strings.Count(page, "data-quota-pool") != 1 {
		t.Fatal("refresh must replace all providers as one unit")
	}
	for _, want := range []string{"Codex", "Antigravity", "PLUS · 5 小时额度", "Claude", "Gemini", "60%", "90%"} {
		if !strings.Contains(page, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if strings.Contains(page, "Claude · 周额度") || strings.Contains(page, "Gemini · 周额度") {
		t.Fatal("model groups mislabeled as Codex periods")
	}
}

func TestSharedQuotaNativeWindowLabelsAreLocalized(t *testing.T) {
	s := &Server{Keys: service.NewKeys(nil, nil), Cfg: config.Config{TimeZone: time.UTC}}
	v := s.quotaPoolsView([]service.UpstreamQuotaPool{{Provider: "Antigravity", TotalAccounts: 1,
		Groups: []service.UpstreamQuotaGroup{
			{Label: "Claude and GPT models · Five Hour Limit Remaining", Period: "provider-id-1", RemainingPercent: 100},
			{Label: "Gemini Models · Weekly Limit Remaining", Period: "provider-id-2", RemainingPercent: 100},
		},
	}}, "csrf", "/dashboard")
	if got := v.Providers[0].Groups; got[0].Label != "Claude / GPT · 5 小时额度" || got[1].Label != "Gemini · 周额度" {
		t.Fatalf("native labels not localized: %#v", got)
	}
	ui, err := webui.NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	output := httptest.NewRecorder()
	if err := ui.Render(output, webui.PageDashboard, webui.DashboardView{Quota: v}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.Body.String(), "Limit Remaining") || !strings.Contains(output.Body.String(), "Claude / GPT · 5 小时额度") {
		t.Fatal("shared quota template retained raw English labels")
	}
}
