package cpamp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testAdminKey = "cpamp-admin-secret"
	testCPAKey   = "cpa_portal_test-key"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewWithConfig(Config{
		BaseURL:  server.URL,
		AdminKey: testAdminKey,
		Timeout:  time.Second,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client, server
}

func TestRefreshCodexQuotaSnapshotWritesMonthlyPartialObservation(t *testing.T) {
	var apiCall map[string]any
	var snapshot map[string]any
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testAdminKey {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case pathAPICall:
			if err := json.NewDecoder(r.Body).Decode(&apiCall); err != nil {
				t.Errorf("decode api call: %v", err)
			}
			_, _ = io.WriteString(w, `{"status_code":200,"body":{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":5,"limit_window_seconds":2592000,"reset_at":1787500000}}}}`)
		case pathQuotaWrite:
			if err := json.NewDecoder(r.Body).Decode(&snapshot); err != nil {
				t.Errorf("decode snapshot: %v", err)
			}
			_, _ = io.WriteString(w, `{"observed_at_ms":1,"items":[]}`)
		default:
			http.NotFound(w, r)
		}
	})
	file := AuthFile{Name: "plus.json", Provider: "codex", AuthIndex: "auth-7", AccountID: "acct-1", AccountSnapshot: "hidden@example.com"}
	observedAt := time.Date(2026, 8, 21, 1, 2, 3, 0, time.UTC)
	refreshed, err := client.RefreshCodexQuotaSnapshot(context.Background(), file, observedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(refreshed) != 2 || refreshed[1].PlanType != "plus" {
		t.Fatalf("refreshed windows = %#v", refreshed)
	}
	if apiCall["authIndex"] != "auth-7" || apiCall["method"] != "GET" {
		t.Fatalf("api call = %#v", apiCall)
	}
	headers, ok := apiCall["header"].(map[string]any)
	if !ok || headers["Chatgpt-Account-Id"] != "acct-1" || headers["Authorization"] != "Bearer $TOKEN$" {
		t.Fatalf("api call headers = %#v", apiCall["header"])
	}
	entries, ok := snapshot["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("snapshot entries = %#v", snapshot["entries"])
	}
	entry := entries[0].(map[string]any)
	observation := entry["observation"].(map[string]any)
	if observation["inventory_mode"] != "partial" || observation["source"] != "api_query" {
		t.Fatalf("observation = %#v", observation)
	}
	windows := entry["windows"].([]any)
	if len(windows) != 2 {
		t.Fatalf("windows = %#v", windows)
	}
	monthly := windows[1].(map[string]any)
	if monthly["provider_window_id"] != "monthly" || monthly["window_kind"] != "monthly" || monthly["remaining_percent"] != float64(95) || monthly["plan_type"] != "plus" {
		t.Fatalf("monthly window = %#v", monthly)
	}
}

func TestQuotaResetEndPrefersLiveCountdownOverExpiredTimestamp(t *testing.T) {
	observedAt := time.Date(2026, 9, 6, 19, 27, 0, 0, time.UTC)
	end, accuracy, ok := quotaResetEnd(map[string]any{
		"reset_at":            float64(observedAt.Add(-5 * 24 * time.Hour).Unix()),
		"reset_after_seconds": float64(15 * 60 * 60),
	}, observedAt)
	if !ok || accuracy != "derived" || end != observedAt.Add(15*time.Hour).UnixMilli() {
		t.Fatalf("quotaResetEnd() = (%d, %q, %v)", end, accuracy, ok)
	}
}

func requireAdmin(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer "+testAdminKey {
		t.Fatalf("authorization = %q", got)
	}
}

func TestNewValidationAndHash(t *testing.T) {
	for name, cfg := range map[string]Config{
		"missing URL":  {AdminKey: testAdminKey},
		"missing key":  {BaseURL: "http://127.0.0.1:18317"},
		"relative URL": {BaseURL: "cpamp:18317", AdminKey: testAdminKey},
		"bad scheme":   {BaseURL: "ftp://cpamp.example", AdminKey: testAdminKey},
		"URL query":    {BaseURL: "http://cpamp.example/?x=1", AdminKey: testAdminKey},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewWithConfig(cfg); err == nil {
				t.Fatal("expected constructor error")
			}
		})
	}
	if got := HashAPIKey(testCPAKey); got != "4580bce1b0c301e7902c56eced312f9478983a42b9323e2e309b6c90ebb6bcfd" {
		t.Fatalf("hash = %q", got)
	}
	client, err := New("http://cpamp.example", "Bearer "+testAdminKey)
	if err != nil {
		t.Fatalf("bearer key constructor: %v", err)
	}
	if client.adminHeader != "Bearer "+testAdminKey {
		t.Fatalf("normalized admin header = %q", client.adminHeader)
	}
}

func TestHealthAndStatus(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requireAdmin(t, r)
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s", r.Method)
		}
		switch r.URL.Path {
		case pathHealth:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true,"service":"manager-server"}`)
		case pathStatus:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"service":"manager-server","dbPath":"/data/usage.sqlite","events":4,"deadLetters":1,"collector":{"collector":"running","upstream":"http://cpa:8317","mode":"full","transport":"http","queue":"usage","lastConsumedAt":11,"lastInsertedAt":12,"totalInserted":4,"totalSkipped":0,"deadLetters":1},"dataMigration":{"name":"usage-cache-accounting","status":"complete","lastEventId":4,"targetEventId":4,"processedRows":2,"changedRows":1,"appliedRows":1,"updatedAtMs":13}}`)
		default:
			http.NotFound(w, r)
		}
	})
	health, err := client.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if !health.OK || health.Service != "manager-server" {
		t.Fatalf("health = %#v", health)
	}
	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Events != 4 || status.DeadLetters != 1 || status.Collector.Collector != "running" || status.DataMigration.Status != "complete" {
		t.Fatalf("status = %#v", status)
	}
}

func TestAPIKeyListReplaceDeleteAndReadMerge(t *testing.T) {
	var mu sync.Mutex
	keys := []string{"external-key", "portal-old"}
	var putPayload []string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requireAdmin(t, r)
		if r.URL.Path != pathAPIKeys {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"api-keys": keys})
		case http.MethodPut:
			if err := json.NewDecoder(r.Body).Decode(&putPayload); err != nil {
				t.Fatalf("decode put: %v", err)
			}
			keys = append([]string(nil), putPayload...)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case http.MethodDelete:
			query := r.URL.Query()
			value := query.Get("value")
			if value == "" {
				t.Fatalf("delete query = %q", r.URL.RawQuery)
			}
			filtered := keys[:0]
			for _, key := range keys {
				if key != value {
					filtered = append(filtered, key)
				}
			}
			keys = filtered
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	})

	got, err := client.ListAPIKeys(context.Background())
	if err != nil || len(got) != 2 {
		t.Fatalf("list = %#v, err = %v", got, err)
	}
	if err := client.AddAPIKey(context.Background(), testCPAKey); err != nil {
		t.Fatalf("add: %v", err)
	}
	if len(putPayload) != 3 || putPayload[0] != "external-key" || putPayload[2] != testCPAKey {
		t.Fatalf("read-merge put payload = %#v", putPayload)
	}
	if err := client.RemoveAPIKey(context.Background(), "portal-old"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := client.DeleteAPIKey(context.Background(), testCPAKey); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestAliasesUseSHA256Hashes(t *testing.T) {
	hash := HashAPIKey(testCPAKey)
	var put struct {
		Items              []APIKeyAlias `json:"items"`
		ActiveAPIKeyHashes []string      `json:"activeApiKeyHashes"`
	}
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requireAdmin(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == pathAliases:
			_ = json.NewEncoder(w).Encode(AliasesResponse{Items: []APIKeyAlias{{APIKeyHash: hash, Alias: "old"}}})
		case r.Method == http.MethodPut && r.URL.Path == pathAliases:
			if err := json.NewDecoder(r.Body).Decode(&put); err != nil {
				t.Fatalf("decode aliases PUT: %v", err)
			}
			_ = json.NewEncoder(w).Encode(AliasesResponse{Items: put.Items})
		case r.Method == http.MethodDelete && r.URL.Path == pathAliases+"/"+hash:
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		default:
			http.NotFound(w, r)
		}
	})
	aliases, err := client.UpsertAliasForKey(context.Background(), testCPAKey, "portal-user")
	if err != nil {
		t.Fatalf("upsert alias: %v", err)
	}
	if len(aliases) != 1 || aliases[0].APIKeyHash != hash || aliases[0].Alias != "portal-user" {
		t.Fatalf("aliases = %#v", aliases)
	}
	if len(put.Items) != 1 || put.Items[0].APIKeyHash != hash || put.Items[0].Alias != "portal-user" {
		t.Fatalf("put = %#v", put)
	}
	if err := client.DeleteAliasForKey(context.Background(), testCPAKey); err != nil {
		t.Fatalf("delete alias: %v", err)
	}

	if _, err := client.ReplaceAliases(context.Background(), []APIKeyAlias{{APIKeyHash: "not-a-hash", Alias: "bad"}}, nil, false); err == nil {
		t.Fatal("expected invalid hash error")
	}
}

func TestAnalyticsFiltersByAPIKeyHash(t *testing.T) {
	wantHash := HashAPIKey(testCPAKey)
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requireAdmin(t, r)
		if r.Method != http.MethodPost || r.URL.Path != pathAnalytics {
			http.NotFound(w, r)
			return
		}
		var req AnalyticsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode analytics: %v", err)
		}
		if req.Filters.APIKeyHashes[0] != wantHash || !req.Include.Summary || !req.Include.Timeline {
			t.Fatalf("analytics request = %#v", req)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"generated_at_ms": 100,
			"granularity":     "day",
			"summary":         map[string]any{"total_calls": 3, "total_tokens": 42},
			"timeline":        []map[string]any{{"bucket_ms": 1, "calls": 3, "tokens": 42}},
		})
	})
	response, err := client.Analytics(context.Background(), AnalyticsRequest{
		FromMS:  1,
		ToMS:    2,
		Filters: AnalyticsFilters{APIKeyHashes: []string{strings.ToUpper(wantHash)}},
		Include: AnalyticsInclude{Summary: true, Timeline: true},
	})
	if err != nil {
		t.Fatalf("analytics: %v", err)
	}
	if response.Summary == nil || response.Summary.TotalCalls != 3 || len(response.Timeline) != 1 {
		t.Fatalf("analytics response = %#v", response)
	}
	if _, err := client.Analytics(context.Background(), AnalyticsRequest{FromMS: 2, ToMS: 1}); err == nil {
		t.Fatal("expected invalid time range")
	}
}

func TestListModelsUsesCPAAPIKey(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathModelCatalog {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testCPAKey {
			t.Fatalf("model authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []map[string]string{{"id": "gpt-test", "owned_by": "portal"}}})
	})
	models, err := client.ListModelsWithAPIKey(context.Background(), testCPAKey)
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if len(models.Data) != 1 || models.Data[0].ID != "gpt-test" {
		t.Fatalf("models = %#v", models)
	}
}

func TestQuotaSnapshotMetadataFlow(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requireAdmin(t, r)
		switch r.URL.Path {
		case pathAuthFiles:
			_, _ = io.WriteString(w, `{"files":[{"id":"runtime-7","physicalName":"codex.json","provider":"codex","auth_index":7,"account":"hidden@example.com","metadata":{"chatgpt_account_id":"acct-nested"}},{"name":"off.json","type":"codex","authIndex":"8","disabled":true}]}`)
		case pathQuotaQuery:
			var request QuotaSnapshotQueryRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode quota query: %v", err)
			}
			if len(request.Accounts) != 1 || request.Accounts[0].Account.AuthIndex != "7" {
				t.Fatalf("quota query = %#v", request)
			}
			_, _ = io.WriteString(w, `{"generated_at_ms":1000,"items":[{"row_key":"row-1","provider":"codex","windows":[{"provider_window_id":"weekly","window_kind":"weekly","model_scope_kind":"all","observed_at_ms":900,"cycle_end_ms":2000,"used_percent":5,"plan_type":"plus","stale":false,"availability":"active"}]}]}`)
		default:
			http.NotFound(w, r)
		}
	})
	files, err := client.ListAuthFiles(context.Background())
	if err != nil {
		t.Fatalf("list auth files: %v", err)
	}
	if len(files) != 2 || files[0].RuntimeID != "runtime-7" || files[0].AuthIndex != "7" || files[0].AccountSnapshot != "hidden@example.com" || files[0].AccountID != "acct-nested" || !files[1].Disabled {
		t.Fatalf("auth files = %#v", files)
	}
	result, err := client.QueryQuotaSnapshots(context.Background(), QuotaSnapshotQueryRequest{Accounts: []QuotaQueryAccount{{RowKey: "row-1", Provider: "codex", Account: QuotaAccountTarget{AuthFileSnapshot: files[0].Name, AuthIndex: files[0].AuthIndex}}}})
	if err != nil {
		t.Fatalf("query quota snapshots: %v", err)
	}
	if len(result.Items) != 1 || len(result.Items[0].Windows) != 1 || result.Items[0].Windows[0].UsedPercent == nil || *result.Items[0].Windows[0].UsedPercent != 5 {
		t.Fatalf("quota result = %#v", result)
	}
}

func TestSetAuthFileDisabledSendsIdentityPreconditions(t *testing.T) {
	var payload map[string]any
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requireAdmin(t, r)
		if r.URL.Path != pathAuthFiles+"/status" || r.Method != http.MethodPatch {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode status request: %v", err)
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	file := AuthFile{Name: "codex.json", RuntimeID: "runtime-7", Provider: "codex", AuthIndex: "auth-7", AccountID: "acct-7", AccountSnapshot: "hidden@example.com"}
	if err := client.SetAuthFileDisabled(context.Background(), file, true); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"name": "runtime-7", "disabled": true, "auth_index": "auth-7",
		"cpamp_physical_name": "codex.json", "cpamp_runtime_id": "runtime-7",
		"cpamp_provider": "codex", "cpamp_account_id": "acct-7",
		"cpamp_account_snapshot": "hidden@example.com",
	}
	for key, value := range want {
		if payload[key] != value {
			t.Errorf("payload[%q] = %#v, want %#v", key, payload[key], value)
		}
	}
}

func TestOAuthExcludedModelsAndDefinitions(t *testing.T) {
	rules := []string{"GPT-Z", "gpt-a", "gpt-a"}
	patches, deletes := 0, 0
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requireAdmin(t, r)
		switch {
		case r.URL.Path == pathOAuthExcluded && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"oauth-excluded-models": map[string]any{"codex": rules}})
		case r.URL.Path == pathOAuthExcluded && r.Method == http.MethodPatch:
			var body struct {
				Provider string   `json:"provider"`
				Models   []string `json:"models"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Provider != "codex" {
				t.Fatalf("provider = %q", body.Provider)
			}
			rules = body.Models
			patches++
			_, _ = io.WriteString(w, `{"ok":true}`)
		case r.URL.Path == pathOAuthExcluded && r.Method == http.MethodDelete:
			if r.URL.Query().Get("provider") != "codex" {
				t.Fatalf("delete provider = %q", r.URL.Query().Get("provider"))
			}
			rules = nil
			deletes++
			_, _ = io.WriteString(w, `{"ok":true}`)
		case r.URL.Path == pathModelDefinitions+"/codex" && r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"models":[{"id":"gpt-z","display_name":"GPT Z"},{"id":"gpt-a"},{"id":"GPT-A"},{"display_name":"missing"}]}`)
		default:
			http.NotFound(w, r)
		}
	})
	gotRules, err := client.ListOAuthExcludedModels(context.Background(), "Codex")
	if err != nil || len(gotRules) != 2 || gotRules[0] != "gpt-a" || gotRules[1] != "GPT-Z" {
		t.Fatalf("rules = %#v, err = %v", gotRules, err)
	}
	models, err := client.ListOAuthModelDefinitions(context.Background(), "codex")
	if err != nil || len(models) != 2 || models[0].DisplayName != "GPT Z" {
		t.Fatalf("models = %#v, err = %v", models, err)
	}
	if err := client.SetOAuthExcludedModels(context.Background(), "codex", []string{"gpt-b", "GPT-A", "gpt-a"}); err != nil {
		t.Fatal(err)
	}
	if patches != 1 || len(rules) != 2 || rules[0] != "GPT-A" || rules[1] != "gpt-b" {
		t.Fatalf("patched rules = %#v patches=%d", rules, patches)
	}
	if err := client.SetOAuthExcludedModels(context.Background(), "codex", nil); err != nil {
		t.Fatal(err)
	}
	if deletes != 1 || len(rules) != 0 {
		t.Fatalf("deleted rules = %#v deletes=%d", rules, deletes)
	}
}

func TestHTTPErrorDoesNotExposeSecret(t *testing.T) {
	secret := "secret-api-key-value"
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requireAdmin(t, r)
		http.Error(w, "upstream leaked "+secret, http.StatusBadGateway)
	})
	_, err := client.ListAPIKeys(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), testAdminKey) {
		t.Fatalf("error exposed secret: %v", err)
	}
	var statusErr *HTTPError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("error = %T %v", err, err)
	}
	if err := client.DeleteAPIKey(context.Background(), secret); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("delete error exposed key: %v", err)
	}
}

func TestContextTimeout(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := client.ListAPIKeys(ctx)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if strings.Contains(err.Error(), testAdminKey) {
		t.Fatalf("timeout error exposed admin key: %v", err)
	}
}

func TestRequestURLKeepsBasePath(t *testing.T) {
	client, err := New("http://example.test/manager/", testAdminKey)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	u, err := client.requestURL(pathAliases + "?x=" + url.QueryEscape("a b"))
	if err != nil {
		t.Fatalf("request url: %v", err)
	}
	if !strings.Contains(u, "http://example.test/manager/v0/management/api-key-aliases?") {
		t.Fatalf("url = %q", u)
	}
}
