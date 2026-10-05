package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
)

type userRequestsCPAMP struct {
	cpamp.API
	requests []cpamp.AnalyticsRequest
	value    cpamp.AnalyticsResponse
	err      error
}

func (f *userRequestsCPAMP) Analytics(_ context.Context, req cpamp.AnalyticsRequest) (cpamp.AnalyticsResponse, error) {
	f.requests = append(f.requests, req)
	return f.value, f.err
}

func TestUserRequestsOnlyReadsOwnedRecentEventsAndCachesAcrossMinuteBoundary(t *testing.T) {
	accounts, st := accountsForTest(t)
	u, err := accounts.CreateAdmin(t.Context(), "13900139000", "管理员", "very-long-admin-password", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	now := from.Add(35*24*time.Hour + 59*time.Second)
	for i, status := range []string{"revoked", "active"} {
		if err := st.CreateKey(t.Context(), domain.APIKey{ID: status, UserID: u.ID, Hash: strings.Repeat(string(rune('a'+i)), 64), Status: status, IssuedAt: from}); err != nil {
			t.Fatal(err)
		}
	}
	oldHash, newHash := strings.Repeat("a", 64), strings.Repeat("b", 64)
	fake := &userRequestsCPAMP{value: cpamp.AnalyticsResponse{Events: &cpamp.EventsResponse{HasMore: true, TotalCount: 2000, Items: []cpamp.EventRow{{TimestampMS: 1, APIKeyHash: oldHash}, {TimestampMS: 3, APIKeyHash: newHash}, {TimestampMS: 2, APIKeyHash: oldHash}}}}}
	k := NewKeys(st, fake)
	k.Now = func() time.Time { return now }
	page, err := k.UserRequests(t.Context(), u.ID, from, 2)
	if err != nil || len(page.Items) != 2 || page.Items[0].TimestampMS != 3 || page.Items[1].TimestampMS != 2 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	want := cpamp.AnalyticsRequest{FromMS: from.UnixMilli(), ToMS: now.UnixMilli(), NowMS: now.UnixMilli(), TimeZone: "Asia/Shanghai", Filters: cpamp.AnalyticsFilters{APIKeyHashes: []string{oldHash, newHash}}, Include: cpamp.AnalyticsInclude{EventsPage: &cpamp.EventsPage{Limit: 2}}}
	if len(fake.requests) != 1 || !reflect.DeepEqual(fake.requests[0], want) {
		t.Fatalf("must request only one owned events page: %+v", fake.requests)
	}
	now = now.Add(2 * time.Second)
	if _, err := k.UserRequests(t.Context(), u.ID, from, 2); err != nil || len(fake.requests) != 1 {
		t.Fatalf("cache invalidated at minute boundary: %v", err)
	}
	fake.value.Events.Items[1].TimestampMS = 4
	if page, err := k.RefreshUserRequests(t.Context(), u.ID, from, 2); err != nil || page.Items[0].TimestampMS != 4 || len(fake.requests) != 2 {
		t.Fatalf("refresh failed: %+v %v", page, err)
	}
	if _, err := k.UserRequests(t.Context(), u.ID, from, 2); err != nil || len(fake.requests) != 2 {
		t.Fatal("refresh did not replace cache")
	}
	now = now.Add(k.CacheTTL + time.Second)
	if _, err := k.UserRequests(t.Context(), u.ID, from, 2); err != nil || len(fake.requests) != 3 {
		t.Fatal("expired cache not refreshed")
	}
	if err := st.SetKeyStatus(t.Context(), newHash, "revoked"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateKey(t.Context(), domain.APIKey{ID: "rotated", UserID: u.ID, Hash: strings.Repeat("c", 64), Status: "active", IssuedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.UserRequests(t.Context(), u.ID, from, 2); err != nil || len(fake.requests) != 4 || len(fake.requests[3].Filters.APIKeyHashes) != 3 {
		t.Fatal("new key did not invalidate cache")
	}
}

func TestUserRequestsNoKeysNeverFallsBackToGlobalEvents(t *testing.T) {
	_, st := accountsForTest(t)
	fake := &userRequestsCPAMP{}
	k := NewKeys(st, fake)
	for _, refresh := range []bool{false, true} {
		page, err := k.userRequests(t.Context(), "no-keys", time.Time{}, 100, refresh)
		if err != nil || len(page.Items) != 0 || len(fake.requests) != 0 {
			t.Fatalf("unfiltered request for user without keys: %+v %v", page, err)
		}
	}
}

func TestUserRequestsFailuresAreNotCachedAndForeignEventsAreRejected(t *testing.T) {
	accounts, st := accountsForTest(t)
	u, err := accounts.CreateAdmin(t.Context(), "13900139000", "管理员", "very-long-admin-password", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("a", 64)
	if err := st.CreateKey(t.Context(), domain.APIKey{ID: "key", UserID: u.ID, Hash: hash, Status: "active", IssuedAt: u.CreatedAt}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		value cpamp.AnalyticsResponse
		err   error
	}{
		{"upstream failure", cpamp.AnalyticsResponse{}, errors.New("timeout")},
		{"missing events", cpamp.AnalyticsResponse{}, nil},
		{"foreign events", cpamp.AnalyticsResponse{Events: &cpamp.EventsResponse{Items: []cpamp.EventRow{{APIKeyHash: strings.Repeat("b", 64)}}}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &userRequestsCPAMP{value: tc.value, err: tc.err}
			k := NewKeys(st, fake)
			if page, err := k.UserRequests(t.Context(), u.ID, u.CreatedAt, 100); err == nil || len(page.Items) != 0 {
				t.Fatalf("failure exposed events: %+v %v", page, err)
			}
			fake.value, fake.err = cpamp.AnalyticsResponse{Events: &cpamp.EventsResponse{Items: []cpamp.EventRow{{APIKeyHash: hash}}}}, nil
			if page, err := k.UserRequests(t.Context(), u.ID, u.CreatedAt, 100); err != nil || len(page.Items) != 1 || len(fake.requests) != 2 {
				t.Fatal("failed query was cached")
			}
		})
	}
	for _, limit := range []int{-1, 0, 101, 5000} {
		fake := &userRequestsCPAMP{value: cpamp.AnalyticsResponse{Events: &cpamp.EventsResponse{}}}
		k := NewKeys(st, fake)
		if _, err := k.UserRequests(t.Context(), u.ID, time.Time{}, limit); err != nil {
			t.Fatal(err)
		}
		if got := fake.requests[0].Include.EventsPage.Limit; got != max(1, min(100, limit)) {
			t.Fatalf("limit=%d", got)
		}
	}
}
