package store

import (
	"fmt"
	"testing"
	"time"

	"cliproxy-portal/internal/domain"
)

func TestRequestFailuresSurviveDetailRetentionAndFilterUsers(t *testing.T) {
	s := testStore(t)
	now := time.Now().UTC()
	for i := range 120 {
		row := GatewayRequestTiming{ID: fmt.Sprint(i), UserID: "a", APIKeyHash: "key", StartedAt: now.Add(time.Duration(i) * time.Second), TotalMS: 100, StatusCode: 504, FailureReason: "idle_timeout"}
		if err := s.SaveGatewayRequestTiming(t.Context(), row); err != nil {
			t.Fatal(err)
		}
	}
	var retained int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM gateway_request_timings`).Scan(&retained); err != nil || retained != 100 {
		t.Fatalf("detail retention=%d err=%v", retained, err)
	}
	data, err := s.RequestLogData(t.Context(), []string{"a"}, false, now.UnixMilli(), now.Add(time.Hour).UnixMilli(), true)
	if err != nil || len(data.Failures) != 120 {
		t.Fatalf("history lost with pruned details: %d %v", len(data.Failures), err)
	}
	other, err := s.RequestLogData(t.Context(), []string{"b"}, false, now.UnixMilli(), now.Add(time.Hour).UnixMilli(), true)
	if err != nil || len(other.Failures) != 0 {
		t.Fatal("other user's failures returned")
	}
	page, total, err := s.GatewayUntracedFailures(t.Context(), now, now.Add(time.Hour), "a", 10)
	if err != nil || len(page) != 10 || total != 120 {
		t.Fatalf("log count/limit=%d/%d %v", len(page), total, err)
	}
}

func TestRequestOutcomeBackfillIsIdempotentAndObservationsAreNotFailures(t *testing.T) {
	s := testStore(t)
	now := time.Now().UTC()
	row := GatewayRequestTiming{ID: "legacy", UserID: "a", APIKeyHash: "key", CPARequestID: "request", StartedAt: now, EndedAt: now.Add(time.Second), TotalMS: 1000, StatusCode: 200, Completed: false}
	if err := s.SaveGatewayRequestTiming(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM gateway_request_failures`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.migrateRequestOutcomes(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ResourceAuditBatch(t.Context(), []domain.AuditEvent{{Action: "resource.rejected", TargetID: "a", CreatedAt: now}, {Action: "resource.observed", TargetID: "a", CreatedAt: now}, {Action: "resource.rejected", TargetID: "b", CreatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	data, err := s.RequestLogData(t.Context(), []string{"a"}, false, now.Add(-time.Minute).UnixMilli(), now.Add(time.Minute).UnixMilli(), false)
	if err != nil || len(data.Failures) != 1 || data.Failures[0].Reason != "connection_interrupted" || len(data.Rejections) != 1 || data.Rejections[0].Calls != 1 {
		t.Fatalf("scope/backfill=%+v err=%v", data, err)
	}
	reasons, err := s.FailedRequestReasons(t.Context(), [][2]string{{"request", "key"}, {"request", "other"}})
	if err != nil || len(reasons) != 1 || reasons[[2]string{"request", "key"}] != "connection_interrupted" {
		t.Fatal("exact request/key scope lost")
	}
}
