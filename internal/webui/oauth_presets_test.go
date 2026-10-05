package webui

import (
	"bytes"
	"strings"
	"testing"
)

func TestOAuthPresetIsGlobalAndOutsideChannelPanels(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	panel := AdminUpstreamsView{Channel: "codex", ChannelLabel: "Codex"}
	other := AdminUpstreamsView{Channel: "antigravity", ChannelLabel: "Antigravity"}
	view := AdminUpstreamsView{LayoutView: LayoutView{CSRFToken: "csrf"}, Channel: "codex", ChannelPanels: []AdminUpstreamsView{panel, other}, Presets: []OAuthPresetView{{ID: "daily", Name: "全渠道日常", EnabledModels: []string{"Codex · gpt", "Antigravity · claude"}, DisabledModels: []string{"Codex · disabled-test-model"}}}}
	var out bytes.Buffer
	if err := r.Execute(&out, PageAdminUpstreams, view); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	if strings.Count(html, `data-global-oauth-presets`) != 1 || strings.Count(html, `action="/admin/upstreams/presets/save"`) != 1 {
		t.Fatal("preset controls duplicated per channel")
	}
	global := strings.Index(html, `data-global-oauth-presets`)
	panelIndex := strings.Index(html, `data-upstream-channel-panel`)
	if global < 0 || global > panelIndex {
		t.Fatal("shared presets nested in channel panel")
	}
	for _, want := range []string{"共用最多 20 个预设", "Codex · gpt", "Antigravity · claude", "预设记录的所有渠道模型"} {
		if !strings.Contains(html, want) {
			t.Fatalf("global preset UI missing %s", want)
		}
	}
	for _, unwanted := range []string{"未记录的渠道保持当前配置", `oauth-preset-detail-label">停用模型`, "disabled-test-model"} {
		if strings.Contains(html, unwanted) {
			t.Fatalf("global preset UI contains removed content %s", unwanted)
		}
	}
}
