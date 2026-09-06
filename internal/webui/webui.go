// Package webui contains the lightweight, server-rendered user interface for
// the CLIProxy account portal.  It intentionally knows nothing about HTTP
// routing, persistence, or the CPA-Manager-Plus API: callers provide the view
// models and use Renderer to execute a named page.
package webui

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed templates/*.html static/*
var assets embed.FS

var pageNames = []string{
	"login", "register", "reset", "resubmit",
	"dashboard", "status", "key", "usage", "models", "activity", "health", "profile", "password",
	"admin-dashboard", "admin-users", "admin-user", "admin-approvals",
	"admin-upstreams", "admin-policy", "admin-audit", "admin-health", "error",
}

// Page constants are accepted by Renderer.Render and Renderer.Execute.
const (
	PageLogin          = "login"
	PageRegister       = "register"
	PageReset          = "reset"
	PageResubmit       = "resubmit"
	PageDashboard      = "dashboard"
	PageStatus         = "status"
	PageKey            = "key"
	PageUsage          = "usage"
	PageModels         = "models"
	PageActivity       = "activity"
	PageHealth         = "health"
	PageProfile        = "profile"
	PagePassword       = "password"
	PageAdminDashboard = "admin-dashboard"
	PageAdminUsers     = "admin-users"
	PageAdminUser      = "admin-user"
	PageAdminApprovals = "admin-approvals"
	PageAdminUpstreams = "admin-upstreams"
	PageAdminPolicy    = "admin-policy"
	PageAdminAudit     = "admin-audit"
	PageAdminHealth    = "admin-health"
	PageError          = "error"
)

// Renderer parses each page together with the shared layout.  Templates are
// parsed once and are safe for concurrent execution.
type Renderer struct {
	mu        sync.RWMutex
	templates map[string]*template.Template
}

// NewRenderer parses all embedded templates and returns a renderer ready for
// concurrent use.
func NewRenderer() (*Renderer, error) {
	r := &Renderer{templates: make(map[string]*template.Template, len(pageNames))}
	for _, page := range pageNames {
		t, err := template.New("base").Funcs(template.FuncMap{
			"dec": func(n int) int {
				if n > 0 {
					return n - 1
				}
				return 0
			},
			"formatNumber": formatNumber,
			"formatTime":   formatTime,
			"inc":          func(n int) int { return n + 1 },
			"join":         strings.Join,
			"statusClass":  statusClass,
			"statusLabel":  statusLabel,
		}).ParseFS(assets, "templates/base.html", "templates/quota-pool.html", "templates/"+page+".html")
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", page, err)
		}
		r.templates[page] = t
	}
	return r, nil
}

var (
	defaultRenderer     *Renderer
	defaultRendererOnce sync.Once
	defaultRendererErr  error
)

// DefaultRenderer returns the process-wide renderer. It panics only if the
// embedded, compile-time templates are invalid.
func DefaultRenderer() *Renderer {
	defaultRendererOnce.Do(func() {
		defaultRenderer, defaultRendererErr = NewRenderer()
	})
	if defaultRendererErr != nil {
		panic(defaultRendererErr)
	}
	return defaultRenderer
}

// Execute renders page into w. page is one of the Page* constants.
func (r *Renderer) Execute(w io.Writer, page string, data any) error {
	if r == nil {
		return fmt.Errorf("webui: nil renderer")
	}
	r.mu.RLock()
	t, ok := r.templates[page]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("webui: unknown page %q", page)
	}
	return t.ExecuteTemplate(w, "base", data)
}

// Render renders page as an HTTP response. The method deliberately does not
// choose status codes or redirects; callers set those before calling it when
// needed. For errors, callers can use ErrorView and set the response status.
func (r *Renderer) Render(w http.ResponseWriter, page string, data any) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	return r.Execute(w, page, data)
}

// Assets returns the embedded static files with the "static/" prefix removed,
// suitable for http.FileServer(http.FS(webui.Assets())).
func Assets() fs.FS {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		// The directory is embedded at compile time; this is unreachable unless
		// the package is changed incorrectly.
		panic(err)
	}
	return sub
}

// TemplateNames returns a sorted copy of the pages available for rendering.
func (r *Renderer) TemplateNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]string, 0, len(r.templates))
	for name := range r.templates {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func formatNumber(v any) string {
	switch n := v.(type) {
	case int:
		return formatInt64(int64(n))
	case int64:
		return formatInt64(n)
	case uint:
		return formatInt64(int64(n))
	case uint64:
		return formatInt64(int64(n))
	case string:
		return n
	default:
		return fmt.Sprint(v)
	}
}

func formatInt64(n int64) string {
	negative := n < 0
	if negative {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if negative {
		return "-" + s
	}
	return s
}

func formatTime(v any) string {
	switch value := v.(type) {
	case time.Time:
		if value.IsZero() {
			return "—"
		}
		return value.In(time.FixedZone("CST", 8*60*60)).Format("2006-01-02 15:04")
	case string:
		if value == "" {
			return "—"
		}
		return value
	default:
		return fmt.Sprint(v)
	}
}

func statusClass(status string) string {
	status = strings.ToLower(strings.TrimSpace(status))
	switch {
	case strings.Contains(status, "approved"), strings.Contains(status, "active"), strings.Contains(status, "success"), strings.Contains(status, "healthy"):
		return "success"
	case strings.Contains(status, "pending"), strings.Contains(status, "warning"), strings.Contains(status, "sync"):
		return "warning"
	case strings.Contains(status, "reject"), strings.Contains(status, "suspend"), strings.Contains(status, "revok"), strings.Contains(status, "error"), strings.Contains(status, "fail"):
		return "danger"
	default:
		return "neutral"
	}
}

func statusLabel(status string) string {
	labels := map[string]string{
		"pending":          "待审批",
		"rejected":         "已拒绝",
		"approved":         "已批准",
		"suspended":        "已停用",
		"pending-sync":     "停用待同步",
		"deletion-pending": "删除待撤销",
		"active":           "正常",
		"healthy":          "正常",
		"warning":          "需关注",
		"error":            "异常",
		"revoked":          "已撤销",
		"unlinked":         "未关联",
	}
	if label, ok := labels[strings.ToLower(status)]; ok {
		return label
	}
	if status == "" {
		return "未知"
	}
	return status
}
