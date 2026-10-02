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

type actualModelCPAMP struct {
	cpamp.API
	requests []cpamp.AnalyticsRequest
	pages    []cpamp.AnalyticsResponse
	err      error
}

func (f *actualModelCPAMP) Analytics(_ context.Context, req cpamp.AnalyticsRequest) (cpamp.AnalyticsResponse, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		return cpamp.AnalyticsResponse{}, f.err
	}
	if len(f.pages) == 0 {
		return cpamp.AnalyticsResponse{}, errors.New("unexpected page")
	}
	v := f.pages[0]
	f.pages = f.pages[1:]
	return v, nil
}

func TestActualModelUsagePaginatesAliasesAndPreservesAnalytics(t *testing.T) {
	initial := cpamp.AnalyticsResponse{
		Summary:     &cpamp.UsageSummary{TotalCalls: 4, TotalTokens: 100, FailureCalls: 1},
		ModelStats:  []cpamp.ModelUsageStat{{Model: "gpt-6-sol", Calls: 4, TotalTokens: 100}},
		Timeline:    []cpamp.UsageTimelinePoint{{Calls: 4, Tokens: 100}},
		APIKeyStats: []cpamp.APIKeyUsageStat{{Calls: 4, TotalTokens: 100}},
		Events: &cpamp.EventsResponse{TotalCount: 4, HasMore: true, NextBeforeMS: 100, NextBeforeID: 2, Items: []cpamp.EventRow{
			{RequestedModel: "gpt-6-sol", ResolvedModel: "gpt-6.1-sol", TotalTokens: 40},
			{RequestedModel: "gpt-6.1-sol", ResolvedModel: "gpt-6.1-sol", TotalTokens: 30},
		}},
	}
	fake := &actualModelCPAMP{pages: []cpamp.AnalyticsResponse{{Events: &cpamp.EventsResponse{Items: []cpamp.EventRow{
		{RequestedModel: "alias", ResponseModel: "gpt-6.1-sol", TotalTokens: 20},
		{RequestedModel: "gpt-6-sol", Model: "gpt-6-sol", TotalTokens: 10, Failed: true},
	}}}}}
	req := cpamp.AnalyticsRequest{FromMS: 1, ToMS: 200, NowMS: 200, TimeZone: "Asia/Shanghai", Filters: cpamp.AnalyticsFilters{APIKeyHashes: []string{"historical", "active"}}}
	got := NewKeys(nil, fake).actualModelUsage(t.Context(), req, initial, 1)
	want := []cpamp.ModelUsageStat{
		{Model: "gpt-6.1-sol", Calls: 3, SuccessCalls: 3, SuccessRate: 1, TotalTokens: 90},
		{Model: "未记录实际模型", Calls: 1, FailureCalls: 1, TotalTokens: 10},
	}
	if got.ModelStatsNote != "" || !reflect.DeepEqual(got.ModelStats, want) {
		t.Fatalf("actual stats: %+v note=%s", got.ModelStats, got.ModelStatsNote)
	}
	if !reflect.DeepEqual(got.Summary, initial.Summary) || !reflect.DeepEqual(got.Timeline, initial.Timeline) || !reflect.DeepEqual(got.APIKeyStats, initial.APIKeyStats) {
		t.Fatal("unrelated aggregates changed")
	}
	if len(got.Events.Items) != 1 || len(initial.Events.Items) != 2 {
		t.Fatal("preview limit or source events changed")
	}
	if len(fake.requests) != 1 {
		t.Fatalf("requests=%d", len(fake.requests))
	}
	next := fake.requests[0]
	if !reflect.DeepEqual(next.Filters, req.Filters) || next.FromMS != req.FromMS || next.ToMS != req.ToMS || next.NowMS != req.NowMS || next.TimeZone != req.TimeZone || *next.Include.EventsPage.BeforeMS != 100 || *next.Include.EventsPage.BeforeID != 2 || next.Include.Summary {
		t.Fatalf("pagination scope changed: %+v", next)
	}
	if initial.ModelStats[0].Model != "gpt-6-sol" {
		t.Fatal("source aggregate mutated")
	}
}

func TestActualModelUsageNeverShowsPartialOrRequestedRankings(t *testing.T) {
	for _, name := range []string{"missing events", "archived", "partial", "token mismatch", "failed page", "stuck cursor"} {
		t.Run(name, func(t *testing.T) {
			initial := cpamp.AnalyticsResponse{Summary: &cpamp.UsageSummary{TotalCalls: 1, TotalTokens: 10}, ModelStats: []cpamp.ModelUsageStat{{Model: "requested"}}, Events: &cpamp.EventsResponse{Items: []cpamp.EventRow{{ResolvedModel: "actual", TotalTokens: 10}}}}
			fake := &actualModelCPAMP{}
			switch name {
			case "missing events":
				initial.Events = nil
			case "archived":
				initial.Coverage = &cpamp.AnalyticsCoverage{RawDeletedEventCount: 1}
			case "partial":
				initial.Summary.TotalCalls = 2
			case "token mismatch":
				initial.Summary.TotalTokens = 20
			case "failed page":
				initial.Events.HasMore = true
				initial.Events.NextBeforeMS = 100
				initial.Events.NextBeforeID = 2
				fake.err = errors.New("upstream")
			case "stuck cursor":
				initial.Events.HasMore = true
				initial.Events.NextBeforeMS = 100
				initial.Events.NextBeforeID = 2
				fake.pages = []cpamp.AnalyticsResponse{{Events: initial.Events}}
			}
			got := NewKeys(nil, fake).actualModelUsage(t.Context(), cpamp.AnalyticsRequest{}, initial, 0)
			if len(got.ModelStats) != 0 || got.ModelStatsNote == "" || got.Events != nil {
				t.Fatalf("partial ranking leaked: %+v", got)
			}
			if !reflect.DeepEqual(got.Summary, initial.Summary) {
				t.Fatal("summary changed")
			}
		})
	}
}

func TestActualModelUsageCompleteEmptyRange(t *testing.T) {
	got := NewKeys(nil, &actualModelCPAMP{}).actualModelUsage(t.Context(), cpamp.AnalyticsRequest{}, cpamp.AnalyticsResponse{Summary: &cpamp.UsageSummary{}, Events: &cpamp.EventsResponse{}}, 0)
	if got.ModelStatsNote != "" || len(got.ModelStats) != 0 {
		t.Fatalf("empty range: %+v", got)
	}
}

func TestUsageScopesCacheActualModelStats(t *testing.T) {
	for _, scope := range []string{"global", "user"} {
		t.Run(scope, func(t *testing.T) {
			accounts, st := accountsForTest(t)
			u, err := accounts.CreateAdmin(t.Context(), "13900139000", "管理员", "very-long-admin-password", nil, "")
			if err != nil {
				t.Fatal(err)
			}
			from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			for i, status := range []string{"active", "revoked"} {
				if err := st.CreateKey(t.Context(), domain.APIKey{ID: status, UserID: u.ID, Hash: strings.Repeat(string(rune('a'+i)), 64), Status: status, IssuedAt: from}); err != nil {
					t.Fatal(err)
				}
			}
			fake := &actualModelCPAMP{pages: []cpamp.AnalyticsResponse{{
				Summary:    &cpamp.UsageSummary{TotalCalls: 1, TotalTokens: 147416},
				ModelStats: []cpamp.ModelUsageStat{{Model: "gpt-6-sol", Calls: 1, TotalTokens: 147416}},
				Events:     &cpamp.EventsResponse{TotalCount: 1, Items: []cpamp.EventRow{{RequestedModel: "gpt-6-sol", ResolvedModel: "gpt-6.1-sol", TotalTokens: 147416}}},
			}}}
			keys := NewKeys(st, fake)
			for i := 0; i < 2; i++ {
				var got cpamp.AnalyticsResponse
				if scope == "global" {
					got, err = keys.GlobalUsage(t.Context(), from, from.Add(24*time.Hour), 0)
				} else {
					got, err = keys.Usage(t.Context(), u.ID, from, from.Add(24*time.Hour), 0)
				}
				if err != nil || len(got.ModelStats) != 1 || got.ModelStats[0].Model != "gpt-6.1-sol" || got.ModelStats[0].TotalTokens != 147416 || got.ModelStatsNote != "" || got.Events != nil {
					t.Fatalf("scope=%s result=%+v err=%v", scope, got, err)
				}
			}
			if len(fake.requests) != 1 || fake.requests[0].Include.EventsPage.Limit != actualModelPageSize {
				t.Fatal("complete model stats were not cached")
			}
			if scope == "user" && len(fake.requests[0].Filters.APIKeyHashes) != 2 {
				t.Fatal("historical user keys omitted")
			}
			if scope == "global" && len(fake.requests[0].Filters.APIKeyHashes) != 0 {
				t.Fatal("global scope narrowed")
			}
		})
	}
}
