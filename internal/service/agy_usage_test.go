package service

import (
	"path/filepath"
	"testing"
	"time"

	"cliproxy-portal/internal/agy"
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/store"
)

func TestAGYUsageLogsAndRollupsShareCountsWithoutDoublingCache(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC().Truncate(time.Minute)
	for i, id := range []string{"known", "unknown"} {
		timing := store.GatewayRequestTiming{ID: id, UserID: "owner", APIKeyHash: "key", CPARequestID: "agy:" + id, StartedAt: now, EndedAt: now.Add(time.Second), TotalMS: 1000, Completed: true, StatusCode: 200, AGY: &store.AGYUsage{Model: "agy/test", Requested: "agy/test", Effort: "high"}}
		if i == 0 {
			timing.AGY.Usage = agy.Usage{Known: true, Input: 310, Output: 40, Cache: 210, Reasoning: 30, Total: 350}
		}
		if err = st.SaveGatewayRequestTiming(t.Context(), timing); err != nil {
			t.Fatal(err)
		}
	}
	k := &Keys{Store: st}
	req := cpamp.AnalyticsRequest{FromMS: now.UnixMilli(), ToMS: now.Add(time.Minute).UnixMilli()}
	req.Include.Summary = true
	req.Include.EventsPage = &cpamp.EventsPage{Limit: 100}
	got, err := k.readAGYUsage(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary.TotalTokens != 350 || got.Summary.CachedTokens+got.Summary.CacheReadTokens+got.Summary.CacheCreationTokens != 210 || got.Summary.TotalCalls != 2 {
		t.Fatalf("rollup double counted: %+v", got.Summary)
	}
	if got.AGYUsageNote == "" || len(got.Events.Items) != 2 {
		t.Fatal("unknown usage note/events missing")
	}
	for _, v := range got.Events.Items {
		if v.PortalUsageKnown == nil || v.ReasoningEffort != "high" {
			t.Fatalf("usage certainty / effort lost: %+v", v)
		}
		if *v.PortalUsageKnown && (v.TotalTokens != 350 || v.CachedTokens+v.CacheReadTokens+v.CacheCreationTokens != 210) {
			t.Fatal("log and rollup disagree")
		}
	}
}
