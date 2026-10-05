package webui

import (
	"bytes"
	"io/fs"
	"strings"
	"testing"
)

func TestBackupSectionHasTwoActionsAndSettingsInsideDialog(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	for _, busy := range []bool{false, true} {
		var out bytes.Buffer
		v := AdminSystemView{LayoutView: LayoutView{ActiveNav: "admin-system", CSRFToken: "test-csrf"}, Backup: BackupView{Available: true, Configured: true, KeyReady: true, Running: busy, LastSuccess: "2026-10-05 21:00", Size: "3.6 MB", Message: "备份成功", RestoreMessage: "门户数据库已恢复", RestoreRollback: "restore-rollback-test"}}
		if err := r.Execute(&out, PageAdminSystem, v); err != nil {
			t.Fatal(err)
		}
		html := out.String()
		start := strings.Index(html, `id="backup"`)
		storage := strings.Index(html, `id="storage"`)
		if storage < 0 || start < 0 {
			t.Fatal("storage or backup overview missing")
		}
		storageEnd := strings.Index(html[storage:], `</section>`) + storage
		if storage < 0 || start <= storage || start >= storageEnd {
			t.Fatal("backup actions and text must be below the cards within storage")
		}
		end := strings.Index(html[start:], `<dialog`)
		main := html[start : start+end]
		if strings.Count(main, `<button`) != 2 {
			t.Fatal("backup overview must have exactly two buttons")
		}
		if !strings.Contains(main, `<dl class="backup-status detail-grid">`) || strings.Count(main, `<dt>`) != 3 {
			t.Fatal("backup must display three plain text details")
		}
		if strings.Contains(main, `class="card`) || strings.Contains(main, `<article`) || strings.Contains(main, `backup-summary-card`) {
			t.Fatal("backup overview and restore result must not use cards")
		}
		if strings.Contains(main, "轻量灾备") || strings.Contains(main, `<h2`) {
			t.Fatal("backup overview must not have a separate heading")
		}
		for _, want := range []string{`>备份</button>`, `>恢复</button>`, "上次成功", "下次定时", "2026-10-05 21:00 · 3.6 MB", "最近恢复", "门户数据库已恢复", "restore-rollback-test"} {
			if !strings.Contains(main, want) {
				t.Fatalf("missing overview %q", want)
			}
		}
		for _, hidden := range []string{`name="repository"`, `name="github_token"`, `/backup/settings`, `下载备份恢复密钥`} {
			if strings.Contains(main, hidden) {
				t.Fatalf("settings escaped backup dialog %q", hidden)
			}
		}
		for _, want := range []string{`enctype="multipart/form-data"`, `name="confirm" value="replace-current-data"`, `name="backup_file"`, `name="restore_mode"`, `value="replace-portal-database"`, `value="rebuild-and-restore"`, "仅门户数据库", "需要重新登录", `class="card backup-section"`, `/static/backup-controls.js?v=20261006-1`, `name="repository"`, `name="github_token"`, `action="/admin/system/backup/settings"`, `data-backup-download`, `name="key_saved"`, "下载备份恢复密钥", `name="enabled"`, `name="weekday"`, `name="backup_time"`, `type="time"`, `step="60"`, "启用每周自动备份"} {
			if !strings.Contains(html, want) {
				t.Fatalf("missing restore workflow %q", want)
			}
		}
		if strings.Contains(html, "恢复范围不包含 usage-archives 归档文件") {
			t.Fatal("removed restore wording retained")
		}
		if strings.Contains(html, "恢复密钥用于解密备份，与 GitHub 令牌不同。") {
			t.Fatal("removed recovery key explanation retained")
		}
		if strings.Contains(html, "保存不会发起手动备份；保留份数沿用后台设置，默认 3 份。") {
			t.Fatal("removed backup settings explanation retained")
		}
		if !strings.Contains(html, "</div>\n        <p class=\"field-help\" id=\"backup-schedule-help\">北京时间，每周执行一次。</p>") {
			t.Fatal("schedule help must follow both controls and align to the left")
		}
		if busy && strings.Count(main, `disabled`) != 2 {
			t.Fatal("running jobs must disable both actions")
		}
	}
}

func TestBackupCanBeConfiguredBeforeFirstBackup(t *testing.T) {
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = r.Execute(&out, PageAdminSystem, AdminSystemView{Backup: BackupView{Available: true}}); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	if strings.Contains(html, `data-backup-open="backup-run-dialog" disabled`) {
		t.Fatal("unconfigured backup button must allow opening settings")
	}
	if !strings.Contains(html, `class="backup-settings" open`) || !strings.Contains(html, `>确认备份</button>`) {
		t.Fatal("first-time setup missing")
	}
	if !strings.Contains(html, `type="submit" disabled>确认备份`) {
		t.Fatal("unconfigured backup must not be submitted")
	}
}

func TestBackupSummaryUsesPlainResponsiveDetails(t *testing.T) {
	css, err := fs.ReadFile(Assets(), "style.css")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(css, []byte(".backup-status.detail-grid { flex: 1 1 560px; min-width: 0; grid-template-columns: minmax(0, 1.4fr) repeat(2, minmax(0, 1fr)); gap: 18px 24px; margin: 0; }")) {
		t.Fatal("wide backup status must be moderately wider than the two timestamp columns")
	}
	if !bytes.Contains(css, []byte(".backup-overview { display: flex; flex-wrap: wrap; align-items: center; gap: 20px 24px; }")) {
		t.Fatal("wide backup actions must be vertically centered alongside the details")
	}
	if bytes.Contains(css, []byte("backup-summary-value")) || bytes.Contains(css, []byte("#backup .system-health-grid")) {
		t.Fatal("obsolete backup card rules retained")
	}
}

func TestStorageSummaryUsesThreeMatchingCardsAndOneBackupCard(t *testing.T) {
	v := AdminSystemView{Storage: []HealthCheckView{
		{Component: "门户 SQLite", Metric: "30.5 MB", Status: "healthy", StatusLabel: "正常"},
		{Component: "CPAMP SQLite", Metric: "612.8 MB", Status: "healthy", StatusLabel: "正常"},
		{Component: "其他存储", Metric: "21.6 MB", Status: "healthy", StatusLabel: "正常", Message: "交互记录、CPA 与容器日志合计"},
	}}
	r, err := NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := r.Execute(&out, PageAdminSystem, v); err != nil {
		t.Fatal(err)
	}
	storage, _, found := strings.Cut(out.String(), `<section class="section" id="health">`)
	if !found || strings.Count(storage, `class="card health-card system-health-card`) != 3 || strings.Count(storage, `class="card backup-section"`) != 1 {
		t.Fatal("storage must contain three top cards and one backup card")
	}
	for _, item := range v.Storage {
		if strings.Count(storage, item.Component) != 1 || !strings.Contains(storage, item.Metric) || !strings.Contains(storage, item.Message) {
			t.Fatalf("lost storage detail: %#v", item)
		}
	}
	if strings.Contains(storage, "storage-other-list") {
		t.Fatal("other storage must not split into individual items")
	}
	css, err := fs.ReadFile(Assets(), "style.css")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(css, []byte("#storage .system-health-grid > .system-health-card:last-child { grid-column: 1 / -1; }")) {
		t.Fatal("third storage card must span the narrow page")
	}
}

func TestBackupNarrowOverviewKeepsDetailsAndActionsTogether(t *testing.T) {
	css, err := fs.ReadFile(Assets(), "style.css")
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(css, []byte("@media (max-width: 760px) {"))
	if start < 0 {
		t.Fatal("narrow breakpoint missing")
	}
	narrow := string(css[start:])
	if end := strings.Index(narrow[1:], "@media"); end >= 0 {
		narrow = narrow[:end+1]
	}
	for _, rule := range []string{
		".backup-overview { display: block; }",
		".backup-status.detail-grid { grid-template-columns: minmax(0, 1fr); gap: 12px; }",
		".backup-status.detail-grid > div { display: grid; grid-template-columns: 88px minmax(0, 1fr); align-items: baseline; gap: 12px; }",
		".backup-status.detail-grid dt { margin: 0; }",
		".backup-overview > .button-row { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 10px; margin: 16px 0 0; }",
		".backup-overview > .button-row > .button { min-width: 0; width: 100%; }",
	} {
		if !strings.Contains(narrow, rule) {
			t.Fatalf("missing narrow-only backup layout: %s", rule)
		}
		count := 1
		if strings.HasPrefix(rule, ".backup-status.detail-grid { grid-template-columns:") {
			count = 2 // Compact details shared by medium cards and phone screens.
		}
		if strings.Count(string(css), rule) != count {
			t.Fatalf("narrow backup rule must not affect other breakpoints: %s", rule)
		}
	}
}

func TestBackupMediumOverviewUsesCardWidthAndCenteredActions(t *testing.T) {
	css, err := fs.ReadFile(Assets(), "style.css")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(css, []byte("container: backup-card / inline-size;")) {
		t.Fatal("backup must adapt to its actual card width, including sidebar layouts")
	}
	start := bytes.Index(css, []byte("@container backup-card (max-width: 900px) {"))
	if start < 0 {
		t.Fatal("medium card breakpoint missing")
	}
	end := bytes.Index(css[start:], []byte("\n}"))
	if end < 0 {
		t.Fatal("medium breakpoint incomplete")
	}
	medium := string(css[start : start+end])
	for _, rule := range []string{
		".backup-overview { display: grid; grid-template-columns: minmax(0, 1fr) auto; align-items: center; gap: 24px; }",
		".backup-status.detail-grid { grid-template-columns: minmax(0, 1fr); gap: 12px; }",
		".backup-status.detail-grid > div { display: grid; grid-template-columns: 88px minmax(0, 1fr); align-items: baseline; gap: 16px; }",
		".backup-status.detail-grid dt { margin-bottom: 0; }",
		".backup-overview > .button-row { flex-wrap: nowrap; margin-left: 0; }",
	} {
		if !strings.Contains(medium, rule) {
			t.Fatalf("missing medium layout: %s", rule)
		}
	}
}

func TestRequestDownloadColumnHasScopedRightEdgeInset(t *testing.T) {
	css, err := fs.ReadFile(Assets(), "style.css")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(css, []byte(".request-log-table th.request-download-cell, .request-log-table td.request-download-cell, .request-detail-table th.request-download-cell, .request-detail-table td.request-download-cell { padding-left: 0; padding-right: 16px; }")) {
		t.Fatal("request download header and link need the same right inset")
	}
}
