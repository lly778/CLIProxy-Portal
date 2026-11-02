package webui

import (
	"bytes"
	"io/fs"
	"strings"
	"testing"
)

func TestOAuthPresetsShareTheGlobalConfigurationCard(t *testing.T) {
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
	if strings.Count(html, `data-global-oauth-presets`) != 1 || strings.Count(html, `data-preset-editor aria-labelledby=`) != 1 {
		t.Fatal("preset controls duplicated per channel")
	}
	for _, want := range []string{"最多保存 20 个预设", "Codex · gpt", "Antigravity · claude", "预设记录的所有渠道模型"} {
		if !strings.Contains(html, want) {
			t.Fatalf("global preset UI missing %s", want)
		}
	}
	for _, want := range []string{`data-masonry-key="presets"`, `data-masonry-card="daily"`, `type="button" data-masonry-handle hidden`, `data-masonry-status aria-live="polite"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("preset sorting markup missing %s", want)
		}
	}
	for _, unwanted := range []string{"未记录的渠道保持当前配置", `oauth-preset-detail-label">停用模型`, "disabled-test-model"} {
		if strings.Contains(html, unwanted) {
			t.Fatalf("global preset UI contains removed content %s", unwanted)
		}
	}
}

func TestOAuthPresetCardsUseIndependentColumns(t *testing.T) {
	asset, err := fs.ReadFile(Assets(), "style.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(asset)
	for _, want := range []string{
		`.oauth-preset-list { position: relative; display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); align-items: start; gap: 10px; }`,
		`.oauth-preset-list.is-masonry { display: block; }`,
		`.oauth-preset-list.is-masonry > .oauth-preset-row { position: absolute; }`,
	} {
		if !strings.Contains(css, want) {
			t.Fatalf("preset cards must stack independently without splitting: %s", want)
		}
	}
	if !strings.Contains(css, `.oauth-preset-list { grid-template-columns: 1fr; }`) {
		t.Fatal("narrow preset cards must retain their single-column layout")
	}
	if strings.Contains(css, `.oauth-preset-list { column-count:`) {
		t.Fatal("preset cards must not depend on browser column balancing")
	}
}
