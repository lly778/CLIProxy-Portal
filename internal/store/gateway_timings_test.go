package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestTimingRetentionDoesNotDependOnDialogueRecords(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	start := time.Now().Add(-8 * 24 * time.Hour)
	for i := 0; i < 101; i++ {
		row := GatewayRequestTiming{ID: fmt.Sprintf("timing-%03d", i), UserID: "a", APIKeyHash: "hash-a", CPARequestID: fmt.Sprintf("request-%03d", i), StartedAt: start.Add(time.Duration(i) * time.Second), EndedAt: start.Add(time.Duration(i+1) * time.Second), TotalMS: 1000, Completed: true, StartSource: "connection_accepted"}
		if err := s.SaveGatewayRequestTiming(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.GatewayTimingByCPARequest(ctx, "request-000", "hash-a"); err != ErrNotFound {
		t.Fatalf("oldest timing not pruned: %v", err)
	}
	row, err := s.GatewayTimingByCPARequest(ctx, "request-001", "hash-a")
	if err != nil || row.TotalMS != 1000 || row.RequestReadMS != nil || !row.Completed {
		t.Fatalf("retained timing/null stages = %+v, %v", row, err)
	}
	if _, err := s.GatewayTimingByCPARequest(ctx, "request-001", "hash-b"); err != ErrNotFound {
		t.Fatalf("cross-key timing lookup: %v", err)
	}
}
