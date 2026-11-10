package agy

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAGYQuotaUsesOnlyReturnedWindowsAndNeverInventsZero(t *testing.T) {
	groups, err := parseQuota([]byte(`{"command":{"data":{"groups":[{"name":"Gemini Models","buckets":[{"id":"weekly","window":"weekly","remaining_fraction":0.815,"reset_time":"2026-11-15T15:15:16Z"},{"id":"missing","window":"five_hour"}]},{"name":"Claude and GPT models","buckets":[{"id":"short","window":"five_hour","remaining_fraction":0}]}]}}}`))
	if err != nil || len(groups) != 2 || len(groups[0].Windows) != 1 || groups[0].Label != "Gemini" || groups[0].Windows[0].Label != "7D" || groups[0].Windows[0].RemainingPercent != 82 || groups[1].Windows[0].RemainingPercent != 0 {
		t.Fatalf("incorrect quota projection: %+v %v", groups, err)
	}
	if groups[0].Windows[0].ResetAt.IsZero() {
		t.Fatal("lost reset timestamp")
	}
	if _, err = parseQuota([]byte(`{"command":{"data":{}}}`)); err == nil {
		t.Fatal("missing quota groups treated as zero quota")
	}
}
func TestAGYQuotaCacheCoalescesAndRetainsLastGoodSnapshot(t *testing.T) {
	var calls atomic.Int32
	gate := make(chan struct{})
	m := &Manager{cfg: RuntimeConfig{Home: t.TempDir()}, quotaReadTest: func(context.Context) (QuotaSnapshot, error) {
		calls.Add(1)
		<-gate
		return QuotaSnapshot{Groups: []QuotaGroup{{Label: "Gemini", Windows: []QuotaWindow{{Label: "7D", RemainingPercent: 81}}}}, UpdatedAt: time.Now()}, nil
	}}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := m.Quota(t.Context(), false)
			if err != nil || len(v.Groups) != 1 {
				t.Error("concurrent quota fetch failed")
			}
		}()
	}
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	close(gate)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("concurrent quota readers spawned repeated CLI queries")
	}
	m.quotaReadTest = func(context.Context) (QuotaSnapshot, error) {
		calls.Add(1)
		return QuotaSnapshot{}, errors.New("provider error with sensitive details")
	}
	v, err := m.Quota(t.Context(), true)
	if err != nil || !v.Stale || v.Error != "官方额度暂时无法读取" || v.Groups[0].Windows[0].RemainingPercent != 81 {
		t.Fatal("quota failure lost cached data or exposed raw diagnostics")
	}
	_, _ = m.Quota(t.Context(), false)
	if calls.Load() != 2 {
		t.Fatal("failed quota reads were not throttled")
	}
}
func TestAGYQuotaAccountSwitchDoesNotPublishPreviousAccount(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	m := &Manager{quotaReadTest: func(context.Context) (QuotaSnapshot, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			return QuotaSnapshot{Account: Account{LoggedIn: true, Email: "old@example.com"}}, nil
		}
		return QuotaSnapshot{Account: Account{LoggedIn: true, Email: "new@example.com"}}, nil
	}}
	done := make(chan QuotaSnapshot, 1)
	go func() { v, _ := m.Quota(t.Context(), false); done <- v }()
	<-started
	m.invalidateQuota()
	close(release)
	if v := <-done; v.Account.Email != "new@example.com" {
		t.Fatal("in-flight quota republished a logged-out account")
	}
}
