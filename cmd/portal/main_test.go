package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/gateway"
	"cliproxy-portal/internal/store"
)

func TestGatewayMaintenanceKeepsOldStructuredCapture(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	vault, err := gateway.NewVault(t.TempDir(), []byte(strings.Repeat("x", 32)))
	if err != nil {
		t.Fatal(err)
	}
	messageID, _, err := vault.SaveSharedMessage("owner", gateway.StructuredMessageRole, `{"type":"message","role":"user","content":"old"}`)
	if err != nil {
		t.Fatal(err)
	}
	const captureID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	row := store.GatewayCapture{ID: captureID, UserID: "owner", CreatedAt: time.Now().UTC().Add(-8 * 24 * time.Hour), Method: "POST", Path: "/v1/responses", RequestContentType: gateway.StructuredCaptureContentType}
	if err := st.SaveGatewayCaptureWithMessages(ctx, row, []store.GatewayCaptureMessage{{Side: "request", ID: messageID, Role: gateway.StructuredMessageRole}}); err != nil {
		t.Fatal(err)
	}
	cleanupGatewayCaptures(ctx, st, vault, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := st.GatewayCaptureByID(ctx, captureID); err != nil {
		t.Fatalf("old capture was removed by maintenance: %v", err)
	}
	if _, err := vault.ReadSharedMessage("owner", gateway.StructuredMessageRole, messageID); err != nil {
		t.Fatalf("old capture's shared message was removed by maintenance: %v", err)
	}
}
