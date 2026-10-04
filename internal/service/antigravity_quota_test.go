package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"cliproxy-portal/internal/cpamp"
)

type antigravityQuotaAPI struct {
	cpamp.API
	mu    sync.Mutex
	files []cpamp.AuthFile
	calls int
	fail  bool
}

func (f *antigravityQuotaAPI) ListAuthFiles(context.Context) ([]cpamp.AuthFile, error) {
	return f.files, nil
}
func (f *antigravityQuotaAPI) FetchAntigravityQuota(_ context.Context, file cpamp.AuthFile, now time.Time) ([]cpamp.AntigravityQuotaWindow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail {
		return nil, errors.New("upstream unavailable")
	}
	percent := 80.0
	if file.AuthIndex == "second" {
		percent = 40
	}
	return []cpamp.AntigravityQuotaWindow{{ID: "claude", Label: "Claude", RemainingPercent: 0, ObservedAt: now, ResetAt: now.Add(time.Hour)}, {ID: "gemini", Label: "Gemini", RemainingPercent: percent, ObservedAt: now, ResetAt: now.Add(2 * time.Hour)}}, nil
}

func TestAntigravityQuotaIsolationCacheAndAggregation(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	fake := &antigravityQuotaAPI{files: []cpamp.AuthFile{{Provider: "codex", Name: "same.json", AuthIndex: "first"}, {Provider: "antigravity", Name: "same.json", AuthIndex: "first"}, {Provider: "antigravity", Name: "two.json", AuthIndex: "second"}, {Provider: "antigravity", Name: "disabled.json", Disabled: true}}}
	keys := NewKeys(nil, fake)
	keys.Now = func() time.Time { return now }
	pools := keys.UpstreamQuotas(t.Context())
	if len(pools) != 1 || pools[0].Provider != "Antigravity" || pools[0].TotalAccounts != 2 || pools[0].UsableAccounts != 2 || len(pools[0].Groups) != 2 || pools[0].Groups[1].RemainingPercent != 60 {
		t.Fatalf("pools %#v", pools)
	}
	quotas, err := keys.UpstreamAccountQuotas(t.Context(), "antigravity")
	if err != nil || len(quotas) != 2 || fake.calls != 2 {
		t.Fatalf("quotas=%#v calls=%d err=%v", quotas, fake.calls, err)
	}
	if _, exists := quotas[upstreamAccountID(fake.files[0])]; exists {
		t.Fatal("foreign Codex identity in Antigravity quota")
	}
	now = now.Add(31 * time.Minute)
	fake.fail = true
	quotas, err = keys.UpstreamAccountQuotas(t.Context(), "antigravity")
	if err == nil || len(quotas) != 0 || fake.calls != 4 {
		t.Fatalf("expired quotas=%#v calls=%d err=%v", quotas, fake.calls, err)
	}
	_, _ = keys.UpstreamAccountQuotas(t.Context(), "antigravity")
	if fake.calls != 4 {
		t.Fatal("failed queries should be throttled")
	}
}

func TestAntigravityManualRefreshSelectedChannel(t *testing.T) {
	fake := &antigravityQuotaAPI{files: []cpamp.AuthFile{{Provider: "codex", Name: "codex.json"}, {Provider: "antigravity", Name: "anti.json", AuthIndex: "first"}}}
	keys := NewKeys(nil, fake)
	if _, err := keys.StartQuotaRefresh("antigravity"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for keys.QuotaRefreshStatus().Running && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	status := keys.QuotaRefreshStatus()
	if status.Running || status.Succeeded != 1 || status.Failed != 0 {
		t.Fatalf("status %#v", status)
	}
	if _, err := keys.StartQuotaRefresh("claude"); err == nil {
		t.Fatal("unsupported quota channel accepted")
	}
}

func TestAntigravityNativeWindowConstraints(t *testing.T) {
	now := time.Now().UTC()
	file := cpamp.AuthFile{Provider: "antigravity", Name: "anti.json", AuthIndex: "first"}
	fake := &antigravityQuotaAPI{files: []cpamp.AuthFile{file}}
	keys := NewKeys(nil, fake)
	keys.Now = func() time.Time { return now }
	keys.antigravityQuota = map[string][]cpamp.AntigravityQuotaWindow{upstreamAccountID(file): {
		{ID: "daily", GroupID: "gemini", Label: "Gemini Daily", RemainingPercent: 0, ObservedAt: now},
		{ID: "weekly", GroupID: "gemini", Label: "Gemini Weekly", RemainingPercent: 80, ObservedAt: now},
	}}
	pools := keys.UpstreamQuotas(t.Context())
	if len(pools) != 1 || pools[0].UsableAccounts != 0 || pools[0].Groups[1].AvailableAccounts != 0 || pools[0].Groups[1].RemainingPercent != 80 {
		t.Fatalf("daily exhaustion did not constrain shared window: %#v", pools)
	}
	keys.antigravityQuota[upstreamAccountID(file)] = append(keys.antigravityQuota[upstreamAccountID(file)], cpamp.AntigravityQuotaWindow{ID: "claude", GroupID: "claude", Label: "Claude", RemainingPercent: 10, ObservedAt: now})
	pools = keys.UpstreamQuotas(t.Context())
	if pools[0].UsableAccounts != 1 {
		t.Fatalf("independent available group was blocked: %#v", pools)
	}
}
