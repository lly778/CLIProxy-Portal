package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cliproxy-portal/internal/agy"
)

func TestAGYSharedPoolKeepsGroupsIndependentAndPrivate(t *testing.T) {
	stale := false
	loggedIn := true
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(agy.QuotaSnapshot{Account: agy.Account{LoggedIn: loggedIn, Email: "private@example.com"}, UpdatedAt: time.Now(), Stale: stale, Groups: []agy.QuotaGroup{
			{Label: "Gemini", Windows: []agy.QuotaWindow{{ID: "a", Label: "7D", RemainingPercent: 82}, {ID: "b", Label: "5H", RemainingPercent: 0}}},
			{Label: "Claude / GPT", Windows: []agy.QuotaWindow{{ID: "c", Label: "7D", RemainingPercent: 100}}},
		}})
	}))
	defer worker.Close()
	k := NewKeys(nil, &antigravityQuotaAPI{})
	k.AGY, _ = agy.NewClient(worker.URL, strings.Repeat("s", 64))
	pools := k.UpstreamQuotas(t.Context())
	if len(pools) != 1 || pools[0].Provider != "Antigravity CLI（ACP）" || pools[0].TotalAccounts != 1 || pools[0].UsableAccounts != 1 || len(pools[0].Groups) != 3 {
		t.Fatalf("unexpected AGY pool: %+v", pools)
	}
	if pools[0].Groups[0].AvailableAccounts != 0 || pools[0].Groups[0].RemainingPercent != 0 || pools[0].Groups[0].Period != "five_hour" || pools[0].Groups[1].Period != "weekly" || pools[0].Groups[1].RemainingPercent != 82 || pools[0].Groups[2].AvailableAccounts != 1 {
		t.Fatal("one exhausted window did not constrain its group independently")
	}
	encoded, _ := json.Marshal(pools)
	if strings.Contains(string(encoded), "private@example.com") {
		t.Fatal("shared pool exposed account identity")
	}
	stale = true
	pool := k.agyQuotaPool(t.Context())
	if pool.UnknownCount != 1 || pool.UsableAccounts != 0 {
		t.Fatal("stale quota was declared usable")
	}
	loggedIn = false
	if pool = k.agyQuotaPool(t.Context()); pool.TotalAccounts != 0 {
		t.Fatal("logged-out account remained in shared quota pool")
	}
}

func TestAGYManualRefreshWorksWithoutCPAMPAndUsesSharedThrottle(t *testing.T) {
	var fresh atomic.Int32
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/account":
			_ = json.NewEncoder(w).Encode(agy.Account{LoggedIn: true})
		case "/internal/quota":
			if r.URL.Query().Get("fresh") == "1" {
				fresh.Add(1)
			}
			_ = json.NewEncoder(w).Encode(agy.QuotaSnapshot{Account: agy.Account{LoggedIn: true}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer worker.Close()
	k := NewKeys(nil, nil)
	k.AGY, _ = agy.NewClient(worker.URL, strings.Repeat("s", 64))
	if _, err := k.StartQuotaRefresh("agy"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for k.QuotaRefreshStatus().Running && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	v := k.QuotaRefreshStatus()
	if v.Running || v.Succeeded != 1 || v.Failed != 0 || fresh.Load() != 1 {
		t.Fatalf("AGY manual refresh failed: %+v calls %d", v, fresh.Load())
	}
	if _, err := k.StartQuotaRefresh("agy"); err == nil || fresh.Load() != 1 {
		t.Fatal("shared refresh cooldown bypassed")
	}
}
