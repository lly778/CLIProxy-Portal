package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"cliproxy-portal/internal/cpamp"
)

func TestQuotaRefreshIsSharedAndEnforcesCooldown(t *testing.T) {
	var apiCalls, writes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{"name": "plus.json", "provider": "codex", "auth_index": "auth-1"}}})
		case "/v0/management/api-call":
			apiCalls++
			_, _ = w.Write([]byte(`{"status_code":200,"body":{"plan_type":"plus","rate_limit":{"secondary_window":{"used_percent":5,"limit_window_seconds":2592000,"reset_after_seconds":1000}}}}`))
		case "/v0/management/quota-snapshots":
			writes++
			_, _ = w.Write([]byte(`{"observed_at_ms":1,"items":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := cpamp.New(server.URL, "admin-key")
	if err != nil {
		t.Fatal(err)
	}
	keys := NewKeys(nil, client)
	keys.RefreshCooldown = time.Minute
	keys.RefreshTimeout = time.Second
	if _, err := keys.StartQuotaRefresh(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for keys.QuotaRefreshStatus().Running && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	status := keys.QuotaRefreshStatus()
	if status.Running || status.Succeeded != 1 || status.Failed != 0 {
		t.Fatalf("refresh status = %#v", status)
	}
	if apiCalls != 1 || writes != 1 {
		t.Fatalf("api calls = %d writes = %d", apiCalls, writes)
	}
	if _, err := keys.StartQuotaRefresh(); !errors.Is(err, ErrQuotaRefreshCooldown) {
		t.Fatalf("second refresh error = %v", err)
	}
}

func TestUpstreamAccountStatusIsVerifiedAndInvalidatesQuota(t *testing.T) {
	disabled := false
	patches := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{"id": "runtime-1", "physicalName": "one.json", "provider": "codex", "auth_index": "auth-1", "account": "one@example.com", "disabled": disabled}}})
		case r.URL.Path == "/v0/management/auth-files/status" && r.Method == http.MethodPatch:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["name"] != "runtime-1" || body["cpamp_physical_name"] != "one.json" {
				t.Fatalf("status payload = %#v", body)
			}
			disabled, _ = body["disabled"].(bool)
			patches++
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := cpamp.New(server.URL, "admin-key")
	if err != nil {
		t.Fatal(err)
	}
	keys := NewKeys(nil, client)
	accounts, err := keys.UpstreamAccounts(context.Background())
	if err != nil || len(accounts) != 1 || accounts[0].Disabled {
		t.Fatalf("accounts = %#v, err = %v", accounts, err)
	}
	updated, err := keys.SetUpstreamAccountDisabled(context.Background(), accounts[0].ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Disabled || !disabled || patches != 1 {
		t.Fatalf("updated = %#v disabled=%v patches=%d", updated, disabled, patches)
	}
}

func TestUpstreamAccountQuotasMatchOpaqueAccountIdentity(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{"id": "runtime-1", "physicalName": "one.json", "provider": "codex", "auth_index": "auth-1", "account": "one@example.com"}}})
		case "/v0/management/quota-snapshots/query":
			shortEnd := now.Add(2 * time.Hour).UnixMilli()
			weekEnd := now.Add(3 * 24 * time.Hour).UnixMilli()
			fiveHour, weekly := 20.0, 40.0
			_ = json.NewEncoder(w).Encode(cpamp.QuotaSnapshotQueryResponse{Items: []cpamp.QuotaSnapshotItem{{
				RowKey: "one.json\x00auth-1", Provider: "codex", Windows: []cpamp.QuotaSnapshotWindow{
					{WindowKind: "five_hour", ModelScopeKind: "all", ObservedAtMS: now.UnixMilli(), CycleEndMS: &shortEnd, UsedPercent: &fiveHour, PlanType: "plus", Availability: "active"},
					{WindowKind: "weekly", ModelScopeKind: "all", ObservedAtMS: now.UnixMilli(), CycleEndMS: &weekEnd, UsedPercent: &weekly, PlanType: "plus", Availability: "active"},
				},
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := cpamp.New(server.URL, "admin-key")
	if err != nil {
		t.Fatal(err)
	}
	keys := NewKeys(nil, client)
	keys.Now = func() time.Time { return now }
	accounts, err := keys.UpstreamAccounts(context.Background())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts=%#v err=%v", accounts, err)
	}
	quotas, err := keys.UpstreamAccountQuotas(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	quota, ok := quotas[accounts[0].ID]
	if !ok || len(quota.Windows) != 2 {
		t.Fatalf("account quota=%#v all=%#v", quota, quotas)
	}
	if quota.Windows[0].Period != "five_hour" || quota.Windows[0].RemainingPercent != 80 || quota.Windows[1].Period != "weekly" || quota.Windows[1].RemainingPercent != 60 {
		t.Fatalf("quota windows=%#v", quota.Windows)
	}
}

func TestOAuthModelRuleMatchingDistinguishesExactAndWildcard(t *testing.T) {
	matched, wildcard := excludedModelRule("gpt-5.6-sol", []string{"gpt-old", "gpt-5.*"})
	if matched != "gpt-5.*" || wildcard != "gpt-5.*" {
		t.Fatalf("wildcard match = %q, %q", matched, wildcard)
	}
	matched, wildcard = excludedModelRule("GPT-OLD", []string{"gpt-old"})
	if matched != "gpt-old" || wildcard != "" {
		t.Fatalf("exact match = %q, %q", matched, wildcard)
	}
	if wildcardModelMatch("gpt-*-preview", "gpt-5-preview") != true || wildcardModelMatch("gpt-*-preview", "gpt-5") {
		t.Fatal("wildcard matcher returned an unexpected result")
	}
}

func TestUpstreamQuotaKeepsPlansSeparate(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{
				{"name": "plus.json", "provider": "codex", "auth_index": "plus-1"},
				{"name": "pro.json", "provider": "codex", "auth_index": "pro-1"},
				{"name": "disabled.json", "provider": "codex", "auth_index": "off-1", "disabled": true},
			}})
		case "/v0/management/quota-snapshots/query":
			shortEnd := now.Add(time.Hour).UnixMilli()
			longEnd := now.Add(4 * 24 * time.Hour).UnixMilli()
			usedPlusShort, usedPlusLong := 20.0, 5.0
			remainingProShort, remainingProLong := 60.0, 30.0
			_ = json.NewEncoder(w).Encode(cpamp.QuotaSnapshotQueryResponse{Items: []cpamp.QuotaSnapshotItem{
				{RowKey: "plus.json\x00plus-1", Provider: "codex", Windows: []cpamp.QuotaSnapshotWindow{
					{ProviderWindowID: "five-hour", WindowKind: "five_hour", ModelScopeKind: "all", ObservedAtMS: now.UnixMilli(), CycleEndMS: &shortEnd, UsedPercent: &usedPlusShort, PlanType: "plus", Availability: "active"},
					{ProviderWindowID: "weekly", WindowKind: "weekly", ModelScopeKind: "all", ObservedAtMS: now.UnixMilli(), CycleEndMS: &longEnd, UsedPercent: &usedPlusLong, PlanType: "plus", Availability: "active"},
				}},
				{RowKey: "pro.json\x00pro-1", Provider: "codex", Windows: []cpamp.QuotaSnapshotWindow{
					{ProviderWindowID: "primary", ModelScopeKind: "all", ObservedAtMS: now.UnixMilli(), CycleEndMS: &shortEnd, RemainingPercent: &remainingProShort, PlanType: "pro", Availability: "active"},
					{ProviderWindowID: "weekly", WindowKind: "weekly", ModelScopeKind: "all", ObservedAtMS: now.UnixMilli(), CycleEndMS: &longEnd, RemainingPercent: &remainingProLong, PlanType: "pro", Availability: "active"},
				}},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := cpamp.New(server.URL, "admin-key")
	if err != nil {
		t.Fatal(err)
	}
	keys := NewKeys(nil, client)
	keys.Now = func() time.Time { return now }
	keys.CacheTTL = 0
	pool, err := keys.UpstreamQuota(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pool.TotalAccounts != 2 || pool.UsableAccounts != 2 || pool.UnknownCount != 0 || len(pool.Groups) != 4 {
		t.Fatalf("pool = %#v", pool)
	}
	remaining := map[string]int{}
	for _, group := range pool.Groups {
		remaining[group.PlanType+"/"+group.Period] = group.RemainingPercent
	}
	if remaining["plus/five_hour"] != 80 || remaining["plus/weekly"] != 95 || remaining["pro/five_hour"] != 60 || remaining["pro/weekly"] != 30 {
		t.Fatalf("remaining groups = %#v", remaining)
	}
}

func TestUpstreamQuotaExcludesWeeklyExhaustedAccountFromFiveHourAverage(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{
				{"name": "one.json", "provider": "codex", "auth_index": "one"},
				{"name": "two.json", "provider": "codex", "auth_index": "two"},
			}})
		case "/v0/management/quota-snapshots/query":
			end := now.Add(time.Hour).UnixMilli()
			exhausted, firstAvailable, secondFiveHour, secondWeekly := 100.0, 10.0, 20.0, 30.0
			_ = json.NewEncoder(w).Encode(cpamp.QuotaSnapshotQueryResponse{Items: []cpamp.QuotaSnapshotItem{
				{RowKey: "one.json\x00one", Provider: "codex", Windows: []cpamp.QuotaSnapshotWindow{
					{WindowKind: "five_hour", ModelScopeKind: "all", ObservedAtMS: now.UnixMilli(), CycleEndMS: &end, UsedPercent: &firstAvailable, PlanType: "plus", Availability: "active"},
					{WindowKind: "weekly", ModelScopeKind: "all", ObservedAtMS: now.UnixMilli(), CycleEndMS: &end, UsedPercent: &exhausted, PlanType: "plus", Availability: "active"},
				}},
				{RowKey: "two.json\x00two", Provider: "codex", Windows: []cpamp.QuotaSnapshotWindow{
					{WindowKind: "five_hour", ModelScopeKind: "all", ObservedAtMS: now.UnixMilli(), CycleEndMS: &end, UsedPercent: &secondFiveHour, PlanType: "plus", Availability: "active"},
					{WindowKind: "weekly", ModelScopeKind: "all", ObservedAtMS: now.UnixMilli(), CycleEndMS: &end, UsedPercent: &secondWeekly, PlanType: "plus", Availability: "active"},
				}},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := cpamp.New(server.URL, "admin-key")
	if err != nil {
		t.Fatal(err)
	}
	keys := NewKeys(nil, client)
	keys.Now = func() time.Time { return now }
	keys.CacheTTL = 0
	pool, err := keys.UpstreamQuota(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pool.TotalAccounts != 2 || pool.UsableAccounts != 1 || len(pool.Groups) != 2 {
		t.Fatalf("pool = %#v", pool)
	}
	want := map[string]int{"five_hour": 80, "weekly": 35}
	for _, group := range pool.Groups {
		if group.RemainingPercent != want[group.Period] || group.AvailableAccounts != 1 || group.KnownAccounts != 2 {
			t.Fatalf("group = %#v", group)
		}
	}
}

func TestFiveHourExhaustionDoesNotExcludeWeeklyQuota(t *testing.T) {
	exhausted, available := 100.0, 20.0
	windows := []quotaWindowSelection{
		{Period: "five_hour", Window: cpamp.QuotaSnapshotWindow{UsedPercent: &exhausted}},
		{Period: "weekly", Window: cpamp.QuotaSnapshotWindow{UsedPercent: &available}},
	}
	if !quotaWindowIncludedInAverage("weekly", windows) {
		t.Fatal("weekly quota was excluded by exhausted five-hour quota")
	}
	if !quotaWindowIncludedInAverage("five_hour", windows) {
		t.Fatal("five-hour quota should remain in its own average as a zero value")
	}
}

func TestWeeklyExhaustionExcludesFiveHourQuota(t *testing.T) {
	exhausted, available := 100.0, 20.0
	windows := []quotaWindowSelection{
		{Period: "five_hour", Window: cpamp.QuotaSnapshotWindow{UsedPercent: &available}},
		{Period: "weekly", Window: cpamp.QuotaSnapshotWindow{UsedPercent: &exhausted}},
	}
	if quotaWindowIncludedInAverage("five_hour", windows) {
		t.Fatal("five-hour quota remained eligible after weekly quota was exhausted")
	}
	if !quotaWindowIncludedInAverage("weekly", windows) {
		t.Fatal("weekly quota excluded itself from its own average")
	}
}

func TestFiveHourEffectiveResetWaitsForExhaustedWeeklyWindow(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	fiveHourEnd := now.Add(time.Hour).UnixMilli()
	weeklyEnd := now.Add(12 * time.Hour).UnixMilli()
	available, exhausted := 20.0, 100.0
	fiveHour := cpamp.QuotaSnapshotWindow{CycleEndMS: &fiveHourEnd, UsedPercent: &available}
	windows := []quotaWindowSelection{
		{Period: "five_hour", Window: fiveHour},
		{Period: "weekly", Window: cpamp.QuotaSnapshotWindow{CycleEndMS: &weeklyEnd, UsedPercent: &exhausted}},
	}
	if got := quotaWindowEffectiveReset("five_hour", fiveHour, windows, now); !got.Equal(time.UnixMilli(weeklyEnd)) {
		t.Fatalf("five-hour effective reset = %v, want %v", got, time.UnixMilli(weeklyEnd))
	}
}

func TestWeeklyEffectiveResetDoesNotWaitForExhaustedFiveHourWindow(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	fiveHourEnd := now.Add(12 * time.Hour).UnixMilli()
	weeklyEnd := now.Add(time.Hour).UnixMilli()
	available, exhausted := 20.0, 100.0
	weekly := cpamp.QuotaSnapshotWindow{CycleEndMS: &weeklyEnd, UsedPercent: &available}
	windows := []quotaWindowSelection{
		{Period: "five_hour", Window: cpamp.QuotaSnapshotWindow{CycleEndMS: &fiveHourEnd, UsedPercent: &exhausted}},
		{Period: "weekly", Window: weekly},
	}
	if got := quotaWindowEffectiveReset("weekly", weekly, windows, now); !got.Equal(time.UnixMilli(weeklyEnd)) {
		t.Fatalf("weekly effective reset = %v, want %v", got, time.UnixMilli(weeklyEnd))
	}
}

func TestCurrentQuotaWindowsKeepsFreshestWindowPerPeriod(t *testing.T) {
	oldUsed, newUsed, weeklyUsed, modelUsed := 80.0, 20.0, 5.0, 1.0
	windows := currentQuotaWindows([]cpamp.QuotaSnapshotWindow{
		{WindowKind: "five_hour", ModelScopeKind: "all", ObservedAtMS: 100, UsedPercent: &oldUsed, Availability: "active"},
		{WindowKind: "five-hour", ModelScopeKind: "all", ObservedAtMS: 200, UsedPercent: &newUsed, Availability: "active"},
		{WindowKind: "weekly", ModelScopeKind: "all", ObservedAtMS: 150, UsedPercent: &weeklyUsed, Availability: "active"},
		{WindowKind: "five_hour", ModelScopeKind: "model", ObservedAtMS: 300, UsedPercent: &modelUsed, Availability: "active"},
	}, time.UnixMilli(400))
	if len(windows) != 2 || windows[0].Period != "five_hour" || windows[1].Period != "weekly" {
		t.Fatalf("windows = %#v", windows)
	}
	if got := quotaRemaining(windows[0].Window); got != 80 {
		t.Fatalf("five-hour remaining = %v", got)
	}
}

func TestCurrentQuotaWindowsAcceptsFreshValueWithObsoleteBoundary(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	used := 93.0
	weeklyDuration := int64(7 * 24 * 60 * 60)
	oldEnd := now.Add(-5 * 24 * time.Hour).UnixMilli()
	windows := currentQuotaWindows([]cpamp.QuotaSnapshotWindow{{
		ProviderWindowID: "weekly",
		WindowKind:       "weekly",
		ModelScopeKind:   "all",
		ObservedAtMS:     now.Add(-time.Minute).UnixMilli(),
		CycleEndMS:       &oldEnd,
		DurationSeconds:  &weeklyDuration,
		UsedPercent:      &used,
		PlanType:         "plus",
		Stale:            true,
		Availability:     "active",
	}}, now)
	if len(windows) != 1 || windows[0].Period != "weekly" || quotaRemaining(windows[0].Window) != 7 {
		t.Fatalf("windows = %#v", windows)
	}
}

func TestCurrentQuotaWindowsPrefersLiveRefreshAtSameObservationTime(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	used := 64.0
	duration := int64(7 * 24 * 60 * 60)
	oldEnd := now.Add(-5 * 24 * time.Hour).UnixMilli()
	newEnd := now.Add(23 * time.Hour).UnixMilli()
	windows := currentQuotaWindows([]cpamp.QuotaSnapshotWindow{
		{ProviderWindowID: "weekly", WindowKind: "weekly", ModelScopeKind: "all", ObservedAtMS: now.UnixMilli(), CycleEndMS: &oldEnd, DurationSeconds: &duration, UsedPercent: &used, Stale: true, Availability: "active"},
		{ProviderWindowID: "weekly", WindowKind: "weekly", ModelScopeKind: "all", ObservedAtMS: now.UnixMilli(), CycleEndMS: &newEnd, DurationSeconds: &duration, UsedPercent: &used, Availability: "active"},
	}, now)
	if len(windows) != 1 || windows[0].Window.CycleEndMS == nil || *windows[0].Window.CycleEndMS != newEnd {
		t.Fatalf("windows = %#v", windows)
	}
}

func TestCurrentQuotaWindowsRejectsActuallyExpiredStaleValue(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	used := 93.0
	weeklyDuration := int64(7 * 24 * 60 * 60)
	oldEnd := now.Add(-time.Hour).UnixMilli()
	for name, observedAt := range map[string]int64{
		"observation belongs to old cycle": now.Add(-2 * time.Hour).UnixMilli(),
		"observation is too old":           now.Add(-8 * 24 * time.Hour).UnixMilli(),
	} {
		t.Run(name, func(t *testing.T) {
			windows := currentQuotaWindows([]cpamp.QuotaSnapshotWindow{{
				ProviderWindowID: "weekly",
				WindowKind:       "weekly",
				ModelScopeKind:   "all",
				ObservedAtMS:     observedAt,
				CycleEndMS:       &oldEnd,
				DurationSeconds:  &weeklyDuration,
				UsedPercent:      &used,
				Stale:            true,
				Availability:     "active",
			}}, now)
			if len(windows) != 0 {
				t.Fatalf("windows = %#v", windows)
			}
		})
	}
}

func TestQuotaRemainingClampsProviderValues(t *testing.T) {
	used := 140.0
	if got := quotaRemaining(cpamp.QuotaSnapshotWindow{UsedPercent: &used}); got != 0 {
		t.Fatalf("remaining = %v", got)
	}
	remaining := 120.0
	if got := quotaRemaining(cpamp.QuotaSnapshotWindow{RemainingPercent: &remaining}); got != 100 {
		t.Fatalf("remaining = %v", got)
	}
}
