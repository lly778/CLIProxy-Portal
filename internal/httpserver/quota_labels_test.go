package httpserver

import (
	"cliproxy-portal/internal/webui"
	"reflect"
	"testing"
)

func TestPoolQuotaColumnsPreserveIndependentWindows(t *testing.T) {
	quotas := []webui.QuotaGroupView{
		{Label: "Claude / GPT · 5 小时额度", RemainingPercent: 34, StatusClass: "warning", ResetAt: "claude-short", Accounts: "1 个可用 · 2 个已同步"},
		{Label: "Gemini · 5 小时额度", RemainingPercent: 96, StatusClass: "success", ResetAt: "gemini-short"},
		{Label: "Claude / GPT · 周额度", RemainingPercent: 63, ResetAt: "claude-week"},
		{Label: "Gemini · 周额度", RemainingPercent: 99, ResetAt: "gemini-week"},
	}
	columns := groupPoolQuotaViews(quotas)
	want := [][]webui.QuotaGroupView{{quotas[0], quotas[2]}, {quotas[1], quotas[3]}}
	if !reflect.DeepEqual(columns, want) {
		t.Fatalf("columns changed independent quota data: %#v", columns)
	}
	for _, rows := range [][]webui.QuotaGroupView{
		quotas[:2],
		{{Label: "Custom"}, quotas[1], quotas[2]},
		{{Label: "Custom · Unknown"}, quotas[1], quotas[2]},
		{{Label: " · 周额度"}, quotas[1], quotas[2]},
		{quotas[0], quotas[2], {Label: "Claude / GPT · 月额度"}},
	} {
		if groupPoolQuotaViews(rows) != nil {
			t.Fatal("single family or unknown labels should retain the original flat layout")
		}
	}
}

func TestUpstreamAccountQuotaLabel(t *testing.T) {
	for _, tt := range []struct{ channel, label, period, want string }{
		{"codex", "PLUS · 5 小时额度", "five_hour", "5H"},
		{"codex", "PLUS · 周额度", "weekly", "7D"},
		{"codex", "", "monthly", "月"},
		{"antigravity", "Claude and GPT models · Five Hour Limit Remaining", "five_hour", "Claude / GPT · 5H"},
		{"antigravity", "Gemini Models · Weekly Limit Remaining", "weekly", "Gemini · 7D"},
	} {
		if got := upstreamAccountQuotaLabel(tt.channel, tt.label, tt.period); got != tt.want {
			t.Errorf("%s/%s => %q, want %q", tt.channel, tt.period, got, tt.want)
		}
	}
}

func TestAccountQuotaGroupsPreserveEachWindow(t *testing.T) {
	quotas := []webui.UpstreamAccountQuotaView{
		{Label: "Claude / GPT · 5H", RemainingPercent: 99, ResetAt: "first"},
		{Label: "Gemini · 5H", RemainingPercent: 73, ResetAt: "third"},
		{Label: "Claude / GPT · 7D", RemainingPercent: 89, ResetAt: "second"},
		{Label: "Gemini · 7D", RemainingPercent: 65, ResetAt: "fourth"},
	}
	groups := groupAccountQuotaViews(quotas)
	if len(groups) != 2 || groups[0].Label != "Claude / GPT" || groups[1].Label != "Gemini" {
		t.Fatalf("groups=%v", groups)
	}
	for i, group := range groups {
		if len(group.Quotas) != 2 || group.Quotas[0].Label != "5H" || group.Quotas[1].Label != "7D" {
			t.Fatalf("windows=%v", group.Quotas)
		}
		for j, index := range []int{i, i + 2} {
			if group.Quotas[j].RemainingPercent != quotas[index].RemainingPercent || group.Quotas[j].ResetAt != quotas[index].ResetAt {
				t.Fatal("window values mixed")
			}
		}
	}
	if quotas[0].Label != "Claude / GPT · 5H" {
		t.Fatal("source rows mutated")
	}
	for _, rows := range [][]webui.UpstreamAccountQuotaView{
		{{Label: "Gemini"}}, {{Label: "Custom · Unknown"}}, {{Label: " · 5H"}},
	} {
		if groupAccountQuotaViews(rows) != nil {
			t.Fatal("unknown period inferred")
		}
	}
}

func TestQuotaDisplayLabel(t *testing.T) {
	for _, tt := range []struct{ label, plan, period, want string }{
		{"Claude and GPT models · Five Hour Limit Remaining", "", "", "Claude / GPT · 5 小时额度"},
		{"Claude and GPT models · Weekly Limit Remaining", "", "", "Claude / GPT · 周额度"},
		{"Gemini Models · Five Hour Limit Remaining", "", "", "Gemini · 5 小时额度"},
		{"Gemini Models · Weekly Limit Remaining", "", "", "Gemini · 周额度"},
		{"", "plus", "five_hour", "PLUS · 5 小时额度"},
		{"", "plus", "weekly", "PLUS · 周额度"},
		{"", "unknown", "monthly", "月额度"},
		{"Custom group · Custom window", "", "", "Custom group · Custom window"},
	} {
		if got := quotaDisplayLabel(tt.label, tt.plan, tt.period); got != tt.want {
			t.Errorf("%q => %q, want %q", tt.label, got, tt.want)
		}
	}
}
