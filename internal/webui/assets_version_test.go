package webui

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

func TestStaticAssetURLsUseContentVersions(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	var html bytes.Buffer
	if err := r.Execute(&html, PageAdminUpstreams, AdminUpstreamsView{LayoutView: LayoutView{ActiveNav: "admin-upstreams"}, Channel: "codex"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Execute(&html, PageAdminSystem, AdminSystemView{LayoutView: LayoutView{ActiveNav: "admin-system"}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"app.js", "style.css", "quota-carousel.js", "masonry-sort.js", "upstream-channels.js", "codex-identity-toggle.js", "backup-controls.js"} {
		data, err := fs.ReadFile(Assets(), name)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data)
		want := fmt.Sprintf("/static/%s?v=%x", name, digest[:8])
		if !strings.Contains(html.String(), want) {
			t.Errorf("missing content version: %s", want)
		}
	}
	if strings.Contains(html.String(), "?v=20261005-") || strings.Contains(html.String(), "?v=20261008-") {
		t.Fatal("stale hard-coded asset version remained")
	}
}
