package service

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"cliproxy-portal/internal/cpamp"
)

// A mutable upstream with keyset pagination, including records older than the
// recent overlap. It lets regressions distinguish delta reads from full scans.
type incrementalModelCPAMP struct {
	cpamp.API
	rows     []cpamp.EventRow
	requests []cpamp.AnalyticsRequest
	archived bool
}

func modelTestID(row cpamp.EventRow) int64 {
	id, _ := strconv.ParseInt(row.EventHash, 10, 64)
	return id
}

func (f *incrementalModelCPAMP) Analytics(_ context.Context, req cpamp.AnalyticsRequest) (cpamp.AnalyticsResponse, error) {
	f.requests = append(f.requests, req)
	var rows []cpamp.EventRow
	for _, row := range f.rows {
		if row.TimestampMS >= req.FromMS && row.TimestampMS < req.ToMS {
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].TimestampMS != rows[j].TimestampMS {
			return rows[i].TimestampMS > rows[j].TimestampMS
		}
		return modelTestID(rows[i]) > modelTestID(rows[j])
	})
	value := cpamp.AnalyticsResponse{}
	if f.archived {
		value.Coverage = &cpamp.AnalyticsCoverage{RawDeletedEventCount: 1}
	}
	if req.Include.Summary {
		value.Summary = &cpamp.UsageSummary{TotalCalls: int64(len(rows))}
		for _, row := range rows {
			value.Summary.TotalTokens += row.TotalTokens
		}
	}
	if page := req.Include.EventsPage; page != nil {
		out := &cpamp.EventsResponse{TotalCount: int64(len(rows))}
		for _, row := range rows {
			if page.BeforeMS != nil && (row.TimestampMS > *page.BeforeMS || row.TimestampMS == *page.BeforeMS && page.BeforeID != nil && modelTestID(row) >= *page.BeforeID) {
				continue
			}
			if len(out.Items) == page.Limit {
				out.HasMore = true
				break
			}
			out.Items = append(out.Items, row)
		}
		if len(out.Items) > 0 {
			last := out.Items[len(out.Items)-1]
			out.NextBeforeMS, out.NextBeforeID = last.TimestampMS, modelTestID(last)
		}
		value.Events = out
	}
	return value, nil
}

func modelHistoryFixture(t *testing.T) (*Keys, *incrementalModelCPAMP, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	fake := &incrementalModelCPAMP{}
	from := now.Add(-7 * 24 * time.Hour)
	for i := 1; i <= 400; i++ {
		fake.rows = append(fake.rows, cpamp.EventRow{EventHash: fmt.Sprintf("%08d", i), TimestampMS: from.Add(24*time.Hour + time.Duration(i)*10*time.Minute).UnixMilli(), ResolvedModel: "history", TotalTokens: 10})
	}
	fake.rows = append(fake.rows,
		cpamp.EventRow{EventHash: "00000401", TimestampMS: from.Add(30 * time.Second).UnixMilli(), ResolvedModel: "expired", TotalTokens: 7},
		cpamp.EventRow{EventHash: "00000402", TimestampMS: now.Add(-time.Minute).UnixMilli(), ResolvedModel: "recent", TotalTokens: 10})
	k := NewKeys(nil, fake)
	k.Now = func() time.Time { return now }
	return k, fake, &now
}

func queryModelHistory(t *testing.T, k *Keys, now time.Time, refresh bool) cpamp.AnalyticsResponse {
	t.Helper()
	var value cpamp.AnalyticsResponse
	var err error
	if refresh {
		value, err = k.RefreshGlobalUsage(t.Context(), now.Add(-7*24*time.Hour), now, 0)
	} else {
		value, err = k.GlobalUsage(t.Context(), now.Add(-7*24*time.Hour), now, 0)
	}
	if err != nil || value.ModelStatsNote != "" {
		t.Fatal("complete model statistics unavailable", err, value.ModelStatsNote)
	}
	return value
}

func modelTotals(value cpamp.AnalyticsResponse) map[string]int64 {
	result := map[string]int64{}
	for _, stat := range value.ModelStats {
		result[stat.Model] = stat.TotalTokens
	}
	return result
}

func TestActualModelDeltaExpiresRowsAndCorrectsRecentModels(t *testing.T) {
	k, fake, now := modelHistoryFixture(t)
	queryModelHistory(t, k, *now, false)
	if len(fake.requests) != 1 || fake.requests[0].Include.EventsPage.Limit != actualModelPageSize {
		t.Fatal("first aggregation was not a complete read")
	}
	// Correct a recent row without changing its identity, expire an old row,
	// and add a new row. Replacing the overlap must neither lose nor double it.
	fake.rows[len(fake.rows)-1].ResolvedModel = "corrected"
	fake.rows[len(fake.rows)-1].TotalTokens = 15
	fake.rows = append(fake.rows, cpamp.EventRow{EventHash: "00000403", TimestampMS: now.Add(time.Minute).UnixMilli(), ResolvedModel: "new", TotalTokens: 20})
	*now = now.Add(k.CacheTTL + time.Second)
	value := queryModelHistory(t, k, *now, false)
	if !reflect.DeepEqual(modelTotals(value), map[string]int64{"history": 4000, "corrected": 15, "new": 20}) || value.Summary.TotalCalls != 402 {
		t.Fatal("delta totals differ after expiry/correction", modelTotals(value))
	}
	if len(fake.requests) != 2 || fake.requests[1].Include.EventsPage.Limit != actualModelDeltaPageSize {
		t.Fatal("incremental update reread the complete historical range")
	}
	// Reading the next small delta must not mutate the previously published
	// checkpoint or duplicate records already seen in the overlap.
	*now = now.Add(k.CacheTTL + time.Second)
	second := queryModelHistory(t, k, *now, false)
	if !reflect.DeepEqual(modelTotals(second), modelTotals(value)) || len(fake.requests) != 3 {
		t.Fatal("overlap records accumulated twice")
	}
}

func TestActualModelLateImportsAndDeletionsFallBackToFullRead(t *testing.T) {
	for _, change := range []string{"late import", "old deletion", "old token correction"} {
		t.Run(change, func(t *testing.T) {
			k, fake, now := modelHistoryFixture(t)
			queryModelHistory(t, k, *now, false)
			want := map[string]int64{"history": 4000, "recent": 10}
			switch change {
			case "late import":
				fake.rows = append(fake.rows, cpamp.EventRow{EventHash: "00000403", TimestampMS: fake.rows[0].TimestampMS, ResolvedModel: "imported", TotalTokens: 30})
				want["imported"] = 30
			case "old deletion":
				fake.rows = fake.rows[1:]
				want["history"] -= 10
			case "old token correction":
				fake.rows[0].TotalTokens = 50
				want["history"] += 40
			}
			*now = now.Add(k.CacheTTL + time.Second)
			value := queryModelHistory(t, k, *now, false)
			if !reflect.DeepEqual(modelTotals(value), want) {
				t.Fatal("out-of-overlap change was not reconciled", modelTotals(value))
			}
			if len(fake.requests) != 3 || fake.requests[1].Include.EventsPage.Limit != actualModelDeltaPageSize || fake.requests[2].Include.EventsPage.Limit != actualModelPageSize || fake.requests[2].FromMS != fake.requests[1].FromMS {
				t.Fatal("inconsistent delta did not fall back to a scoped full read")
			}
		})
	}
}

func TestActualModelDeltaRemovesDeletedOverlapRecords(t *testing.T) {
	k, fake, now := modelHistoryFixture(t)
	queryModelHistory(t, k, *now, false)
	fake.rows = fake.rows[:len(fake.rows)-1]
	*now = now.Add(k.CacheTTL + time.Second)
	value := queryModelHistory(t, k, *now, false)
	if !reflect.DeepEqual(modelTotals(value), map[string]int64{"history": 4000}) || len(fake.requests) != 2 || fake.requests[1].Include.EventsPage.Limit != actualModelDeltaPageSize {
		t.Fatal("deleted recent rows survived the delta or caused an unnecessary full scan")
	}
}

func TestActualModelDeltaPaginationReadsOnlyRecentOverlap(t *testing.T) {
	k, fake, now := modelHistoryFixture(t)
	initial := *now
	for i := 1; i <= 150; i++ {
		fake.rows = append(fake.rows, cpamp.EventRow{EventHash: fmt.Sprintf("%08d", 1000+i), TimestampMS: now.Add(-time.Duration(i) * time.Second).UnixMilli(), ResolvedModel: "burst", TotalTokens: 1})
	}
	queryModelHistory(t, k, *now, false)
	*now = now.Add(k.CacheTTL + time.Second)
	value := queryModelHistory(t, k, *now, false)
	if !reflect.DeepEqual(modelTotals(value), map[string]int64{"history": 4000, "recent": 10, "burst": 150}) {
		t.Fatal("multi-page delta lost or duplicated recent rows", modelTotals(value))
	}
	if len(fake.requests) != 3 {
		t.Fatal("delta did not paginate independently of old history")
	}
	last := fake.requests[2]
	if last.FromMS != initial.Add(-actualModelOverlap).UnixMilli() || last.Include.EventsPage.Limit != actualModelDeltaPageSize || last.Include.Summary {
		t.Fatal("delta pagination recomputed historical statistics or changed the overlap scope")
	}
}

func TestActualModelCheckpointCountIsBounded(t *testing.T) {
	k := NewKeys(nil, nil)
	now := k.Now()
	for i := 0; i <= actualModelMaxScopes; i++ {
		snapshot := &actualModelSnapshot{updatedAt: now.Add(time.Duration(i) * time.Second), calls: 1, rows: map[string]actualModelEvent{"row": {model: "model"}}}
		k.putActualModelSnapshot(fmt.Sprint(i), snapshot)
	}
	if len(k.actualModels) != actualModelMaxScopes || k.actualModels["0"] != nil {
		t.Fatal("incremental checkpoints grew without bound or did not evict the oldest scope")
	}
}

func TestActualModelPeriodicAndManualReconciliation(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(fmt.Sprint(manual), func(t *testing.T) {
			k, fake, now := modelHistoryFixture(t)
			initial := *now
			queryModelHistory(t, k, *now, false)
			fake.rows[0].ResolvedModel = "corrected-history"
			if manual {
				*now = now.Add(time.Second)
			} else {
				*now = initial.Add(11 * time.Minute)
				value := queryModelHistory(t, k, *now, false)
				if modelTotals(value)["corrected-history"] != 0 || fake.requests[len(fake.requests)-1].Include.EventsPage.Limit != actualModelDeltaPageSize {
					t.Fatal("hourly reconciliation performed a full read after only eleven minutes")
				}
				// Refresh just before the reconciliation deadline. A fresh
				// two-minute page cache must not postpone the full read.
				*now = initial.Add(actualModelReconcile - time.Second)
				queryModelHistory(t, k, *now, false)
				*now = initial.Add(actualModelReconcile)
			}
			value := queryModelHistory(t, k, *now, manual)
			if modelTotals(value)["corrected-history"] != 10 || fake.requests[len(fake.requests)-1].Include.EventsPage.Limit != actualModelPageSize {
				t.Fatal("manual/periodic reconciliation failed to detect model-only history correction")
			}
		})
	}
}

func TestActualModelCheckpointsRequireStableIdentityAndKeepScopesSeparate(t *testing.T) {
	k, fake, now := modelHistoryFixture(t)
	fake.rows[0].EventHash = ""
	queryModelHistory(t, k, *now, false)
	*now = now.Add(k.CacheTTL + time.Second)
	queryModelHistory(t, k, *now, false)
	if fake.requests[len(fake.requests)-1].Include.EventsPage.Limit != actualModelPageSize {
		t.Fatal("unstable event identities created an incremental checkpoint")
	}
	req := cpamp.AnalyticsRequest{FromMS: now.Add(-7 * 24 * time.Hour).UnixMilli(), ToMS: now.UnixMilli(), TimeZone: "Asia/Shanghai"}
	global := k.actualModelScopeKey(req)
	req.Filters.APIKeyHashes = []string{"user-a-key"}
	firstUser := k.actualModelScopeKey(req)
	req.Filters.APIKeyHashes = []string{"user-b-key"}
	if global == firstUser || firstUser == k.actualModelScopeKey(req) {
		t.Fatal("incremental checkpoint crossed user/global key scope")
	}
	fake.archived = true
	*now = now.Add(k.CacheTTL + time.Second)
	value, err := k.GlobalUsage(t.Context(), now.Add(-7*24*time.Hour), *now, 0)
	if err != nil || value.ModelStatsNote == "" || len(value.ModelStats) != 0 {
		t.Fatal("archived raw history was replaced with a partial ranking")
	}
}
