package service

import (
	"testing"

	"cliproxy-portal/internal/cpamp"
)

func TestReconcileGlobalCallStatus(t *testing.T) {
	value := cpamp.AnalyticsResponse{
		Summary: &cpamp.UsageSummary{TotalCalls: 6, SuccessCalls: 5, FailureCalls: 1},
		APIKeyStats: []cpamp.APIKeyUsageStat{
			{Calls: 4, SuccessCalls: 2, FailureCalls: 2},
			{Calls: 2, SuccessCalls: 1, FailureCalls: 1},
		},
	}
	reconcileGlobalCallStatus(&value)
	if value.Summary.SuccessCalls != 3 || value.Summary.FailureCalls != 3 || value.Summary.TotalCalls != 6 {
		t.Fatalf("global call status was not reconciled: %+v", value.Summary)
	}
}

func TestReconcileGlobalCallStatusKeepsIncompleteStats(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stats []cpamp.APIKeyUsageStat
	}{
		{"missing keys", nil},
		{"fewer calls", []cpamp.APIKeyUsageStat{{Calls: 5, SuccessCalls: 2, FailureCalls: 3}}},
		{"unclassified calls", []cpamp.APIKeyUsageStat{{Calls: 6, SuccessCalls: 2, FailureCalls: 3}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := cpamp.AnalyticsResponse{Summary: &cpamp.UsageSummary{TotalCalls: 6, SuccessCalls: 5, FailureCalls: 1}, APIKeyStats: tc.stats}
			reconcileGlobalCallStatus(&value)
			if value.Summary.SuccessCalls != 5 || value.Summary.FailureCalls != 1 {
				t.Fatalf("incomplete stats changed summary: %+v", value.Summary)
			}
		})
	}
}
