package httpserver

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"cliproxy-portal/internal/config"
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/store"
	"cliproxy-portal/internal/webui"
)

func TestRegistrationLoginApprovalAndOneTimeKey(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	var aliases []cpamp.APIKeyAlias
	var aliasActiveHashes []string
	var aliasCleanup bool
	var quotaWrites int
	var modelCalls int
	var modelCatalogReads int
	var lastSeenMS int64
	var lastSeenAnalyticsCalls int
	cpampServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/health" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "service": "cpa-manager-plus"})
		case r.URL.Path == "/v0/management/api-keys" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"api-keys": keys})
		case r.URL.Path == "/v0/management/api-keys" && r.Method == http.MethodPut:
			if err := json.NewDecoder(r.Body).Decode(&keys); err != nil {
				t.Errorf("decode keys: %v", err)
			}
			_, _ = io.WriteString(w, `{"ok":true}`)
		case r.URL.Path == "/v0/management/api-key-aliases" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": aliases})
		case r.URL.Path == "/v0/management/api-key-aliases" && r.Method == http.MethodPut:
			var body struct {
				Items                   []cpamp.APIKeyAlias `json:"items"`
				ActiveAPIKeyHashes      []string            `json:"activeApiKeyHashes"`
				AllowOrphanAliasCleanup bool                `json:"allowOrphanAliasCleanup"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode aliases: %v", err)
			}
			aliases = body.Items
			aliasActiveHashes = body.ActiveAPIKeyHashes
			aliasCleanup = body.AllowOrphanAliasCleanup
			_ = json.NewEncoder(w).Encode(map[string]any{"items": aliases})
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{"name": "plus.json", "provider": "codex", "auth_index": "auth-1"}}})
		case r.URL.Path == "/v0/management/quota-snapshots/query" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]any{"generated_at_ms": time.Now().UnixMilli(), "items": []any{}})
		case r.URL.Path == "/v0/management/api-call" && r.Method == http.MethodPost:
			_, _ = io.WriteString(w, `{"status_code":200,"body":{"plan_type":"plus","rate_limit":{"secondary_window":{"used_percent":5,"limit_window_seconds":2592000,"reset_after_seconds":1000}}}}`)
		case r.URL.Path == "/v0/management/quota-snapshots" && r.Method == http.MethodPost:
			quotaWrites++
			_, _ = io.WriteString(w, `{"observed_at_ms":1,"items":[]}`)
		case r.URL.Path == "/v0/management/monitoring/analytics" && r.Method == http.MethodPost:
			var req cpamp.AnalyticsRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode analytics: %v", err)
			}
			response := map[string]any{"generated_at_ms": time.Now().UnixMilli()}
			if req.Include.APIKeyStats {
				lastSeenAnalyticsCalls++
				stats := make([]map[string]any, 0, len(keys))
				for _, key := range keys {
					stats = append(stats, map[string]any{"api_key_hash": cpamp.HashAPIKey(key), "calls": 2_300, "total_tokens": 5_100_000, "last_seen_ms": lastSeenMS})
				}
				response["api_key_stats"] = stats
			}
			if req.Include.EventsPage != nil {
				response["events"] = map[string]any{"total_count": 1, "items": []map[string]any{{"timestamp_ms": time.Now().UnixMilli(), "model": "gpt-test", "reasoning_effort": "high", "input_tokens": 120, "output_tokens": 30, "cached_tokens": 20, "cache_read_tokens": 40, "cache_creation_tokens": 10, "reasoning_tokens": 25, "total_tokens": 150, "latency_ms": 1_200, "failed": false}}}
			}
			_ = json.NewEncoder(w).Encode(response)
		case r.URL.Path == "/v1/models" && r.Method == http.MethodGet:
			if len(keys) == 0 || r.Header.Get("Authorization") != "Bearer "+keys[0] {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			modelCatalogReads++
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"gpt-test","object":"model","owned_by":"openai"},{"id":"gpt-fail","object":"model","owned_by":"openai"}]}`)
		case r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost:
			if len(keys) == 0 || r.Header.Get("Authorization") != "Bearer "+keys[0] {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			var body struct {
				Model               string `json:"model"`
				MaxCompletionTokens int    `json:"max_completion_tokens"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode model test: %v", err)
			}
			if body.Model != "gpt-test" || body.MaxCompletionTokens != 16 {
				if body.Model != "gpt-fail" || body.MaxCompletionTokens != 16 {
					t.Errorf("unexpected model test body: %#v", body)
				}
			}
			modelCalls++
			if body.Model == "gpt-fail" {
				http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
				return
			}
			_, _ = io.WriteString(w, `{"id":"chatcmpl-test","choices":[{"message":{"role":"assistant","content":"OK"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer cpampServer.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	secret := []byte("01234567890123456789012345678901")
	api, err := cpamp.New(cpampServer.URL, "test-admin-key")
	if err != nil {
		t.Fatal(err)
	}
	accounts := service.NewAccounts(st, secret)
	keysService := service.NewKeys(st, api)
	cfg := config.Config{ListenAddr: ":18080", CPAAPIBaseURL: cpampServer.URL, CPAMPBaseURL: cpampServer.URL, CookieName: "portal_session", TimeZone: time.FixedZone("CST", 8*3600), PendingRetry: 30 * time.Second, ReconcileInterval: 5 * time.Minute}
	server, err := New(cfg, st, accounts, keysService, secret, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	portal := httptest.NewServer(server.Handler())
	defer portal.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	registerPage := getBody(t, client, portal.URL+"/register", http.StatusOK)
	csrf := extract(t, registerPage, `name="csrf_token" value="([^"]+)"`)
	policy := extract(t, registerPage, `name="rules_version" value="([^"]+)"`)
	postForm(t, client, portal.URL+"/register", url.Values{"csrf_token": {csrf}, "phone": {"13800138000"}, "name": {"张三"}, "password": {"long-password-123"}, "confirm_password": {"long-password-123"}, "rule": {"accept"}, "rules_version": {policy}}, http.StatusSeeOther)
	u, err := st.UserByPhone(t.Context(), "13800138000")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := accounts.CreateAdmin(t.Context(), "13900139000", "管理员", "very-long-admin-password", nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.Approve(t.Context(), admin, u, "verified", "test"); err != nil {
		t.Fatal(err)
	}
	// Legacy databases may still contain a future cooldown timestamp. It must
	// no longer prevent a user from claiming or rotating a key.
	if err := st.SetIssueCooldown(t.Context(), u.ID, time.Now().UTC().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	loginPage := getBody(t, client, portal.URL+"/login", http.StatusOK)
	csrf = extract(t, loginPage, `name="csrf_token" value="([^"]+)"`)
	postForm(t, client, portal.URL+"/login", url.Values{"csrf_token": {csrf}, "phone": {"13800138000"}, "password": {"long-password-123"}}, http.StatusSeeOther)
	healthPage := getBody(t, client, portal.URL+"/health", http.StatusOK)
	if !strings.Contains(healthPage, "系统状态") || !strings.Contains(healthPage, "用量与管理服务") {
		t.Fatalf("user health page missing sanitized checks: %s", healthPage)
	}
	dashboard := getBody(t, client, portal.URL+"/dashboard", http.StatusOK)
	if !strings.Contains(dashboard, "刷新额度") {
		t.Fatal("approved user quota refresh button was not rendered")
	}
	csrf = extract(t, dashboard, `name="csrf_token" value="([^"]+)"`)
	postForm(t, client, portal.URL+"/quota/refresh", url.Values{"csrf_token": {csrf}, "next": {"/dashboard"}}, http.StatusSeeOther)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		written := quotaWrites
		mu.Unlock()
		if written == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	written := quotaWrites
	mu.Unlock()
	if written != 1 {
		t.Fatalf("quota snapshot writes = %d", written)
	}
	keyPage := getBody(t, client, portal.URL+"/key", http.StatusOK)
	csrf = extract(t, keyPage, `name="csrf_token" value="([^"]+)"`)
	claimed := postForm(t, client, portal.URL+"/key/claim", url.Values{"csrf_token": {csrf}, "password": {"long-password-123"}}, http.StatusOK)
	fullKey := extract(t, claimed, `(cpa_portal_[A-Za-z0-9_-]+)`)
	if strings.Count(claimed, fullKey) < 1 {
		t.Fatal("one-time key was not rendered")
	}
	stored, err := st.ActiveKey(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Hash != cpamp.HashAPIKey(fullKey) || stored.LastFour == "" {
		t.Fatalf("stored key metadata = %#v", stored)
	}
	usedAt := time.Now().UTC().Truncate(time.Millisecond)
	mu.Lock()
	lastSeenMS = usedAt.UnixMilli()
	mu.Unlock()
	if err := keysService.Reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile last used time: %v", err)
	}
	stored, err = st.ActiveKey(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.LastSeenAt.Equal(usedAt) {
		t.Fatalf("stored last used time = %s, want %s", stored.LastSeenAt, usedAt)
	}
	mu.Lock()
	analyticsCalls := lastSeenAnalyticsCalls
	mu.Unlock()
	if analyticsCalls != 1 {
		t.Fatalf("last-seen analytics calls = %d", analyticsCalls)
	}
	activityPage := getBody(t, client, portal.URL+"/activity", http.StatusOK)
	if !strings.Contains(activityPage, "模型请求日志") || !strings.Contains(activityPage, "gpt-test") || !strings.Contains(activityPage, ">150<") {
		t.Fatalf("user activity page missing model request: %s", activityPage)
	}
	if !strings.Contains(activityPage, "最近 100 条") || strings.Contains(activityPage, "时间范围") || strings.Contains(activityPage, "type=\"date\"") {
		t.Fatalf("user activity page still rendered time filters: %s", activityPage)
	}
	if !strings.Contains(activityPage, "思考强度") || !strings.Contains(activityPage, ">70<") || !strings.Contains(activityPage, ">25<") || !strings.Contains(activityPage, ">高<") {
		t.Fatalf("user activity page missing token breakdown or reasoning effort: %s", activityPage)
	}
	if strings.Contains(activityPage, "账号获批") || strings.Contains(activityPage, "领取 API Key") {
		t.Fatalf("user activity page exposed account or security operations: %s", activityPage)
	}
	usagePage := getBody(t, client, portal.URL+"/usage?range=7d", http.StatusOK)
	if strings.Contains(usagePage, "逐请求记录") {
		t.Fatalf("usage page still rendered request records: %s", usagePage)
	}
	adminJar, _ := cookiejar.New(nil)
	adminClient := &http.Client{Jar: adminJar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	adminLogin := getBody(t, adminClient, portal.URL+"/login", http.StatusOK)
	adminCSRF := extract(t, adminLogin, `name="csrf_token" value="([^"]+)"`)
	postForm(t, adminClient, portal.URL+"/login", url.Values{"csrf_token": {adminCSRF}, "phone": {admin.Phone}, "password": {"very-long-admin-password"}}, http.StatusSeeOther)
	adminUsersPage := getBody(t, adminClient, portal.URL+"/admin/users", http.StatusOK)
	if !strings.Contains(adminUsersPage, "2.3K 请求") || !strings.Contains(adminUsersPage, "5.1M tokens") {
		t.Fatalf("admin user usage was not populated: %s", adminUsersPage)
	}
	if !strings.Contains(adminUsersPage, "最近使用") || !strings.Contains(adminUsersPage, usedAt.In(cfg.TimeZone).Format("2006-01-02 15:04")) {
		t.Fatalf("admin user last-used time was not populated from model requests: %s", adminUsersPage)
	}
	adminUserPage := getBody(t, adminClient, portal.URL+"/admin/users/"+u.ID, http.StatusOK)
	if !strings.Contains(adminUserPage, "最近 100 条模型请求") || !strings.Contains(adminUserPage, "gpt-test") || strings.Contains(adminUserPage, "最近操作") {
		t.Fatalf("admin user detail did not show model request records: %s", adminUserPage)
	}
	adminUsagePage := getBody(t, adminClient, portal.URL+"/admin/usage?range=7d", http.StatusOK)
	if !strings.Contains(adminUsagePage, "按用户") || !strings.Contains(adminUsagePage, "张三") || !strings.Contains(adminUsagePage, "2.3K 请求") || !strings.Contains(adminUsagePage, "5.1M tokens") {
		t.Fatalf("admin global usage did not show per-user statistics: %s", adminUsagePage)
	}
	filteredAdminUsagePage := getBody(t, adminClient, portal.URL+"/admin/usage?user="+u.ID+"&range=7d", http.StatusOK)
	if strings.Contains(filteredAdminUsagePage, "<h2>按用户</h2>") {
		t.Fatalf("per-user usage page rendered the global user chart: %s", filteredAdminUsagePage)
	}
	keyPage = getBody(t, client, portal.URL+"/key", http.StatusOK)
	if strings.Contains(keyPage, fullKey) {
		t.Fatal("full key was displayed after leaving the one-time page")
	}
	if !strings.Contains(keyPage, "测试 Key") {
		t.Fatal("test key action was not rendered")
	}
	if strings.Contains(keyPage, "轮换") || strings.Contains(keyPage, "/key/rotate") {
		t.Fatal("removed key rotation action was still rendered")
	}
	csrf = extract(t, keyPage, `name="csrf_token" value="([^"]+)"`)
	postForm(t, client, portal.URL+"/key/rotate", url.Values{"csrf_token": {csrf}, "password": {"long-password-123"}}, http.StatusMethodNotAllowed)
	tested := postForm(t, client, portal.URL+"/key/test", url.Values{"csrf_token": {csrf}}, http.StatusOK)
	if !strings.Contains(tested, "API Key 可用，已发现 2 个模型") {
		t.Fatalf("unexpected key test response: %s", tested)
	}
	if strings.Contains(tested, "测试模型连接") {
		t.Fatal("model connectivity test was rendered on the API Key page")
	}
	modelsPage := getBody(t, client, portal.URL+"/models", http.StatusOK)
	if !strings.Contains(modelsPage, "gpt-test") || !strings.Contains(modelsPage, "测试连接") {
		t.Fatal("model connectivity action was not rendered on the models page")
	}
	csrf = extract(t, modelsPage, `name="csrf_token" value="([^"]+)"`)
	tested = postFormJSON(t, client, portal.URL+"/models/test", url.Values{"csrf_token": {csrf}, "model": {"gpt-test"}}, http.StatusOK)
	if !strings.Contains(tested, `"status":"success"`) || !strings.Contains(tested, "Key、CPA 与模型上游链路正常") {
		t.Fatalf("unexpected model connectivity response: %s", tested)
	}
	failedTest := postFormJSON(t, client, portal.URL+"/models/test", url.Values{"csrf_token": {csrf}, "model": {"gpt-fail"}}, http.StatusOK)
	if !strings.Contains(failedTest, `"status":"danger"`) || !strings.Contains(failedTest, "模型上游暂时不可用") {
		t.Fatalf("model failure was not rendered on its card: %s", failedTest)
	}
	mu.Lock()
	if len(keys) != 1 || keys[0] != fullKey || len(aliases) != 1 || modelCalls != 2 || modelCatalogReads != 2 {
		mu.Unlock()
		t.Fatalf("CPAMP state keys=%d aliases=%d model_calls=%d catalog_reads=%d", len(keys), len(aliases), modelCalls, modelCatalogReads)
	}
	mu.Unlock()
	modelsPage = getBody(t, client, portal.URL+"/models", http.StatusOK)
	if !strings.Contains(modelsPage, "连接成功") || !strings.Contains(modelsPage, "连接失败") {
		t.Fatal("previous model test results were not retained")
	}
	if strings.Index(modelsPage, "gpt-fail") > strings.Index(modelsPage, "gpt-test") {
		t.Fatal("model order was not stable and case-insensitive")
	}
	mu.Lock()
	keys = nil // Simulate an administrator deleting the key directly in CPA.
	mu.Unlock()

	keyPage = getBody(t, client, portal.URL+"/key", http.StatusOK)
	csrf = extract(t, keyPage, `name="csrf_token" value="([^"]+)"`)
	tested = postForm(t, client, portal.URL+"/key/test", url.Values{"csrf_token": {csrf}}, http.StatusBadRequest)
	if !strings.Contains(tested, "外部撤销") {
		t.Fatalf("external deletion was not synchronized: %s", tested)
	}
	if _, err := st.ActiveKey(t.Context(), u.ID); !store.IsNotFound(err) {
		t.Fatalf("active key still exists after external deletion: %v", err)
	}

	// CPAMP can retain the old alias when a key is removed outside the portal.
	// Reissuing must safely replace that orphan instead of failing with HTTP 400.
	csrf = extract(t, tested, `name="csrf_token" value="([^"]+)"`)
	reissued := postForm(t, client, portal.URL+"/key/claim", url.Values{"csrf_token": {csrf}, "password": {"long-password-123"}}, http.StatusOK)
	newFullKey := extract(t, reissued, `(cpa_portal_[A-Za-z0-9_-]+)`)
	if newFullKey == fullKey {
		t.Fatal("reissued key did not change")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 1 || keys[0] != newFullKey || len(aliases) != 1 || aliases[0].APIKeyHash != cpamp.HashAPIKey(newFullKey) || !aliasCleanup || len(aliasActiveHashes) != 1 || aliasActiveHashes[0] != cpamp.HashAPIKey(newFullKey) {
		t.Fatalf("reissued CPAMP state keys=%d aliases=%#v cleanup=%v active_hashes=%#v", len(keys), aliases, aliasCleanup, aliasActiveHashes)
	}
}

func TestUsageRangeTodayStartsAtLocalMidnight(t *testing.T) {
	zone := time.FixedZone("CST", 8*60*60)
	server := &Server{Cfg: config.Config{TimeZone: zone}}
	request := httptest.NewRequest(http.MethodGet, "/usage?range=today", nil)
	before := time.Now().UTC()
	from, to, name := server.usageRange(request)
	after := time.Now().UTC()
	if name != "today" {
		t.Fatalf("range name = %q", name)
	}
	localFrom := from.In(zone)
	if localFrom.Hour() != 0 || localFrom.Minute() != 0 || localFrom.Second() != 0 || localFrom.Nanosecond() != 0 {
		t.Fatalf("today starts at %s", localFrom)
	}
	localTo := to.In(zone)
	if localFrom.Year() != localTo.Year() || localFrom.YearDay() != localTo.YearDay() {
		t.Fatalf("today crosses local dates: %s to %s", localFrom, localTo)
	}
	if to.Before(before) || to.After(after) {
		t.Fatalf("today end = %s, want between %s and %s", to, before, after)
	}
}

func TestCustomUsageRangeKeepsSelectedEndDateInForm(t *testing.T) {
	zone := time.FixedZone("CST", 8*60*60)
	server := &Server{Cfg: config.Config{TimeZone: zone}}
	request := httptest.NewRequest(http.MethodGet, "/usage?range=custom&from=2026-08-20&to=2026-08-21", nil)
	from, to, name := server.usageRange(request)
	if name != "custom" || to.Sub(from) != 48*time.Hour {
		t.Fatalf("custom range = %s to %s (%s)", from, to, name)
	}
	formFrom, formTo := server.usageRangeFormDates(from, to, name)
	if formFrom != "2026-08-20" || formTo != "2026-08-21" {
		t.Fatalf("custom form dates = %q to %q", formFrom, formTo)
	}
}

func TestUsageTrendScalesRequestsAndTokensIndependently(t *testing.T) {
	trend := usageTrend([]webui.UsagePointView{
		{Date: "08-20", Requests: "10", Tokens: "100", RequestValue: 10, TokenValue: 100},
		{Date: "08-21", Requests: "5", Tokens: "400", RequestValue: 5, TokenValue: 400},
	})
	if len(trend.Points) != 2 {
		t.Fatalf("trend points = %d", len(trend.Points))
	}
	if trend.Points[0].RequestY != 28 {
		t.Fatalf("request peak y = %d", trend.Points[0].RequestY)
	}
	if trend.Points[1].TokenY != 28 {
		t.Fatalf("token peak y = %d", trend.Points[1].TokenY)
	}
	if trend.MaxRequests != "10" || trend.MaxTokens != "400" {
		t.Fatalf("trend maxima = %q requests, %q tokens", trend.MaxRequests, trend.MaxTokens)
	}
	if trend.RequestPoints == "" || trend.TokenPoints == "" {
		t.Fatal("trend SVG points were not generated")
	}
}

func TestCompactNumberUsesKAndMUnits(t *testing.T) {
	tests := map[int64]string{
		999:        "999",
		1_000:      "1K",
		2_300:      "2.3K",
		5_100_000:  "5.1M",
		-1_250_000: "-1.2M",
	}
	for value, want := range tests {
		if got := compactNumber(value); got != want {
			t.Errorf("compactNumber(%d) = %q, want %q", value, got, want)
		}
	}
}

func TestUsageViewsFormatsHourlyTimelineLabels(t *testing.T) {
	zone := time.FixedZone("CST", 8*60*60)
	server := &Server{Cfg: config.Config{TimeZone: zone}}
	bucket := time.Date(2026, 8, 21, 1, 0, 0, 0, zone)
	_, points, _, _ := server.usageViews(cpamp.AnalyticsResponse{
		Granularity: "hour",
		Timeline:    []cpamp.UsageTimelinePoint{{BucketMS: bucket.UnixMilli(), Calls: 2_300, TotalTokens: 5_100_000}},
	}, bucket.Add(-time.Hour), bucket.Add(time.Hour))
	if len(points) != 1 {
		t.Fatalf("hourly points = %d", len(points))
	}
	if points[0].Date != "08-21 01:00" || points[0].Requests != "2.3K" || points[0].Tokens != "5.1M" {
		t.Fatalf("hourly point = %#v", points[0])
	}
}

func TestUsageViewsCompactsSummaryMetrics(t *testing.T) {
	zone := time.FixedZone("CST", 8*60*60)
	server := &Server{Cfg: config.Config{TimeZone: zone}}
	from := time.Date(2026, 8, 20, 0, 0, 0, 0, zone)
	summary, _, _, _ := server.usageViews(cpamp.AnalyticsResponse{Summary: &cpamp.UsageSummary{
		TotalCalls:   1_693,
		SuccessCalls: 1_684,
		FailureCalls: 9,
		TotalTokens:  100_679_131,
	}}, from, from.Add(24*time.Hour))
	if summary.Requests != "1.7K" || summary.Successes != "1.7K" || summary.Failures != "9" || summary.TotalTokens != "100.7M" {
		t.Fatalf("compacted summary = %#v", summary)
	}
}

func TestModelUsageChartsSortEachMetricIndependently(t *testing.T) {
	models := []webui.ModelUsageView{
		{Model: "request-heavy", RequestValue: 20, TokenValue: 100, Percent: 100, TokenPercent: 10},
		{Model: "token-heavy", RequestValue: 5, TokenValue: 1_000, Percent: 25, TokenPercent: 100},
	}
	byRequests, byTokens := modelUsageCharts(models)
	if byRequests[0].Model != "request-heavy" || byTokens[0].Model != "token-heavy" {
		t.Fatalf("model chart order requests=%q tokens=%q", byRequests[0].Model, byTokens[0].Model)
	}
}

func TestUserUsageChartsSortEachMetricIndependently(t *testing.T) {
	users := []webui.UserUsageView{
		{User: webui.UserView{Name: "请求用户"}, RequestValue: 20, TokenValue: 100},
		{User: webui.UserView{Name: "Token 用户"}, RequestValue: 5, TokenValue: 1_000},
	}
	byRequests, byTokens := userUsageCharts(users)
	if byRequests[0].User.Name != "请求用户" || byRequests[0].RequestPercent != 100 || byTokens[0].User.Name != "Token 用户" || byTokens[0].TokenPercent != 100 {
		t.Fatalf("user chart order requests=%#v tokens=%#v", byRequests, byTokens)
	}
}

func TestAdminStatusCardUsesAdministratorPerspective(t *testing.T) {
	server := &Server{}
	approved := server.adminStatusCard(domain.User{Status: domain.StatusApproved})
	if !strings.Contains(approved.Description, "该用户") || strings.Contains(approved.Description, "您可以") {
		t.Fatalf("approved admin status copy = %q", approved.Description)
	}
	suspended := server.adminStatusCard(domain.User{Status: domain.StatusSuspended})
	if !strings.Contains(suspended.Description, "管理员恢复") || strings.Contains(suspended.Description, "请联系管理员") {
		t.Fatalf("suspended admin status copy = %q", suspended.Description)
	}
}

func TestQuotaPoolViewLabelsFiveHourWindow(t *testing.T) {
	server := &Server{Cfg: config.Config{TimeZone: time.UTC}, Keys: &service.Keys{}}
	view := server.quotaPoolView(service.UpstreamQuotaPool{TotalAccounts: 1, UsableAccounts: 1, Provider: "Codex", Groups: []service.UpstreamQuotaGroup{{PlanType: "plus", Period: "five_hour", KnownAccounts: 1, AvailableAccounts: 1, RemainingPercent: 80}}}, "", "/dashboard")
	if len(view.Groups) != 1 || view.Groups[0].Label != "PLUS · 5 小时额度" {
		t.Fatalf("five-hour quota view = %#v", view.Groups)
	}
}

func getBody(t *testing.T, client *http.Client, endpoint string, want int) string {
	t.Helper()
	resp, err := client.Get(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("GET %s status=%d body=%s", endpoint, resp.StatusCode, body)
	}
	return string(body)
}
func postForm(t *testing.T, client *http.Client, endpoint string, form url.Values, want int) string {
	t.Helper()
	resp, err := client.PostForm(endpoint, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("POST %s status=%d body=%s", endpoint, resp.StatusCode, body)
	}
	return string(body)
}

func postFormJSON(t *testing.T, client *http.Client, endpoint string, form url.Values, want int) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("POST %s status=%d body=%s", endpoint, resp.StatusCode, body)
	}
	return string(body)
}
func extract(t *testing.T, body, pattern string) string {
	t.Helper()
	match := regexp.MustCompile(pattern).FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("pattern %q not found", pattern)
	}
	return match[1]
}
