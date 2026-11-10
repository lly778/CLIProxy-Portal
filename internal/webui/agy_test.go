package webui

import (
	"bytes"
	"strings"
	"testing"
)

func TestAGYAccountCardShowsOfficialQuotaAndOnlyAccountEntry(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	v := AdminUpstreamsView{LayoutView: LayoutView{CSRFToken: "csrf"}, AGYConfigured: true, AGYLoggedIn: true, AGYAccount: UpstreamAccountView{Account: "account@example.com", QuotaGroups: []UpstreamAccountQuotaGroupView{{Label: "Gemini", Quotas: []UpstreamAccountQuotaView{{Label: "7D", RemainingPercent: 81, StatusClass: "success", ResetAt: "2026-11-15"}}}}}}
	var out bytes.Buffer
	if err = r.Execute(&out, PageAdminUpstreams, v); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{"account@example.com", "Gemini", `aria-valuenow="81"`, "重置 2026-11-15", "账号管理", "退出当前账号"} {
		if !strings.Contains(html, want) {
			t.Fatalf("missing account card state %s", want)
		}
	}
	for _, old := range []string{"接入设置", "仅管理员试用", "停止全部会话", "读取账号模型", "data-agy-dialog", "agy-controls.js"} {
		if strings.Contains(html, old) {
			t.Fatalf("removed AGY control remains: %s", old)
		}
	}
}
