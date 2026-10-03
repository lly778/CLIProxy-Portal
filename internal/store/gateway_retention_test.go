package store

import (
	"fmt"
	"testing"
	"time"
)

func TestUnidentifiedRequestsDoNotEvictLinkedGatewayDetails(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 10, 3, 14, 0, 0, 0, time.UTC)
	read, response := int64(100), int64(300)
	save := func(id, requestID string, at time.Time, status int) {
		t.Helper()
		if err := s.SaveGatewayRequestTiming(t.Context(), GatewayRequestTiming{
			ID: id, UserID: "owner", APIKeyHash: "hash", CPARequestID: requestID,
			StartedAt: at, EndedAt: at.Add(time.Second), TotalMS: 1000,
			RequestReadMS: &read, ResponseStartedMS: &response, Completed: true,
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveGatewayCapture(t.Context(), GatewayCapture{
			ID: id, UserID: "owner", APIKeyHash: "hash", CPARequestID: requestID,
			CreatedAt: at, Method: "POST", Path: "/v1/responses", StatusCode: status,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DeleteExcessGatewayCaptures(t.Context(), "owner", 100); err != nil {
			t.Fatal(err)
		}
	}
	check := func(id, requestID string, present bool) {
		t.Helper()
		_, timingErr := s.GatewayTimingByCPARequest(t.Context(), requestID, "hash")
		_, captureErr := s.GatewayCaptureByID(t.Context(), id)
		if present && (timingErr != nil || captureErr != nil) {
			t.Fatalf("retained %s: timing=%v capture=%v", id, timingErr, captureErr)
		}
		if !present && (timingErr != ErrNotFound || captureErr != ErrNotFound) {
			t.Fatalf("pruned %s: timing=%v capture=%v", id, timingErr, captureErr)
		}
	}
	// The latest 100 CPAMP rows include failures with a valid trace ID too.
	for i := 0; i < 100; i++ {
		status := 200
		if i == 0 {
			status = 429
		}
		save(fmt.Sprintf("linked-%03d", i), fmt.Sprintf("request-%03d", i), start.Add(time.Duration(i)*time.Second), status)
	}
	// Reproduce the reported boundary: one newer 429 without a trace ID.
	save("unlinked-000", "", start.Add(100*time.Second), 429)
	check("linked-000", "request-000", true)
	// A burst of unidentified requests remains bounded without consuming slots.
	for i := 1; i < 101; i++ {
		requestID := ""
		if i%2 == 0 {
			requestID = " "
		}
		save(fmt.Sprintf("unlinked-%03d", i), requestID, start.Add(time.Duration(100+i)*time.Second), 429)
	}
	check("linked-000", "request-000", true)
	if _, err := s.GatewayCaptureByID(t.Context(), "unlinked-000"); err != ErrNotFound {
		t.Fatalf("unidentified capture not bounded: %v", err)
	}
	for _, table := range []string{"gateway_request_timings", "gateway_captures"} {
		var count int
		if err := s.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table+" WHERE user_id=?", "owner").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 200 {
			t.Fatalf("%s retained %d, want 100 linked + 100 unidentified", table, count)
		}
	}
	// Only a newer linkable request moves the linked retention boundary.
	save("linked-100", "request-100", start.Add(201*time.Second), 200)
	check("linked-000", "request-000", false)
	check("linked-001", "request-001", true)
	check("linked-100", "request-100", true)
	total, err := s.GatewayTimingSummary(t.Context(), start, start.Add(time.Hour), "owner")
	if err != nil || total.Samples != 202 || total.TotalMS != 202000 {
		t.Fatalf("persistent timing samples changed: %+v %v", total, err)
	}

	// An explicit zero limit still removes both classes (and shared-message
	// cleanup continues through the existing deletion transaction).
	deleted, err := s.DeleteExcessGatewayCaptures(t.Context(), "owner", 0)
	if err != nil || len(deleted.CaptureIDs) != 200 {
		t.Fatalf("zero retention: %+v %v", deleted, err)
	}
}

func TestGatewayDetailRetentionClassTiesAreDeterministicAndUserIsolated(t *testing.T) {
	s := testStore(t)
	at := time.Now().UTC()
	for _, user := range []string{"a", "b"} {
		for i := 0; i < 101; i++ {
			id := fmt.Sprintf("%s-%03d", user, i)
			if err := s.SaveGatewayCapture(t.Context(), GatewayCapture{ID: id, UserID: user, CPARequestID: id, CreatedAt: at}); err != nil {
				t.Fatal(err)
			}
			if err := s.SaveGatewayRequestTiming(t.Context(), GatewayRequestTiming{ID: id, UserID: user, CPARequestID: id, APIKeyHash: user, StartedAt: at, TotalMS: 1}); err != nil {
				t.Fatal(err)
			}
		}
	}
	deleted, err := s.DeleteExcessGatewayCaptures(t.Context(), "a", 100)
	if err != nil || len(deleted.CaptureIDs) != 1 || deleted.CaptureIDs[0] != "a-000" {
		t.Fatalf("tie cleanup: %+v %v", deleted, err)
	}
	if _, err := s.GatewayTimingByCPARequest(t.Context(), "a-000", "a"); err != ErrNotFound {
		t.Fatalf("timing tie: %v", err)
	}
	if _, err := s.GatewayTimingByCPARequest(t.Context(), "a-001", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GatewayCaptureByID(t.Context(), "b-000"); err != nil {
		t.Fatal("another user's captures changed", err)
	}
	if _, err := s.GatewayTimingByCPARequest(t.Context(), "b-001", "b"); err != nil {
		t.Fatal("another user's timing changed", err)
	}
}
