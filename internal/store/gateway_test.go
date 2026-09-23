package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestGatewayCaptureRetentionPerUserAndAge(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 0; i < 101; i++ {
		for _, user := range []string{"a", "b"} {
			id := fmt.Sprintf("%s-%03d", user, i)
			if err := s.SaveGatewayCapture(ctx, GatewayCapture{ID: id, UserID: user, CreatedAt: now.Add(time.Duration(i) * time.Second), Method: "POST", Path: "/v1/responses", StatusCode: 200}); err != nil {
				t.Fatal(err)
			}
		}
	}
	removed, err := s.DeleteExcessGatewayCaptures(ctx, "a", 100)
	if err != nil || len(removed.CaptureIDs) != 1 || removed.CaptureIDs[0] != "a-000" {
		t.Fatalf("per-user pruning = %v, %v", removed, err)
	}
	a, _ := s.ListGatewayCaptures(ctx, "a", 200)
	b, _ := s.ListGatewayCaptures(ctx, "b", 200)
	if len(a) != 100 || len(b) != 101 {
		t.Fatalf("remaining: a=%d b=%d", len(a), len(b))
	}
	if err := s.SaveGatewayCapture(ctx, GatewayCapture{ID: "expired", UserID: "a", CreatedAt: now.Add(-8 * 24 * time.Hour), Method: "POST", Path: "/v1/responses", StatusCode: 200}); err != nil {
		t.Fatal(err)
	}
	removed, err = s.DeleteExpiredGatewayCaptures(ctx, now.Add(-7*24*time.Hour))
	if err != nil || len(removed.CaptureIDs) != 1 || removed.CaptureIDs[0] != "expired" {
		t.Fatalf("age pruning = %v, %v", removed, err)
	}
}

func TestSharedGatewayMessagesRemainUntilLastCaptureIsDeleted(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	const shared = "shared-message-id"
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("capture-%d", i)
		err := s.SaveGatewayCaptureWithMessages(ctx, GatewayCapture{ID: id, UserID: "owner", CreatedAt: now.Add(time.Duration(i) * time.Second), Method: "POST", Path: "/v1/responses"}, []GatewayCaptureMessage{{Side: "request", ID: shared, Role: "用户"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	removed, err := s.DeleteExcessGatewayCaptures(ctx, "owner", 1)
	if err != nil || len(removed.CaptureIDs) != 1 || removed.CaptureIDs[0] != "capture-0" || len(removed.MessageIDs) != 0 {
		t.Fatalf("first cleanup = %+v, %v", removed, err)
	}
	parts, err := s.GatewayCaptureMessages(ctx, "capture-1", "request")
	if err != nil || len(parts) != 1 || parts[0].ID != shared {
		t.Fatalf("remaining references = %+v, %v", parts, err)
	}
	removed, err = s.DeleteExcessGatewayCaptures(ctx, "owner", 0)
	if err != nil || len(removed.CaptureIDs) != 1 || removed.CaptureIDs[0] != "capture-1" || len(removed.MessageIDs) != 1 || removed.MessageIDs[0] != shared {
		t.Fatalf("last cleanup = %+v, %v", removed, err)
	}
}
