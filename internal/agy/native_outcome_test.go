package agy

import (
	"context"
	"database/sql"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAGYNativeTerminalErrorOverridesEndTurnAndUnknownUsage(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "agy", "conversations")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	const id = "native-error-fixture"
	db, err := sql.Open("sqlite", filepath.ToSlash(filepath.Join(dir, id+".db")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE steps(idx INTEGER PRIMARY KEY,step_type INTEGER,status INTEGER,metadata BLOB,step_payload BLOB)`); err != nil {
		t.Fatal(err)
	}
	add := func(idx, kind int, metadata, payload []byte) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO steps VALUES(?,?,3,?,?)`, idx, kind, metadata, payload); err != nil {
			t.Fatal(err)
		}
	}
	add(1, 17, nil, []byte("INVALID_ARGUMENT historical failure"))
	m := &Manager{cfg: RuntimeConfig{Home: home}}
	s := &session{ID: id, life: context.Background()}
	m.startUsage(s)
	add(2, 15, usageProto(2, 100, 3, 5), nil)
	if u, err := m.consumeOutcome(s); err != nil || u.Total != 105 {
		t.Fatalf("old failure contaminated new turn: %+v %v", u, err)
	}
	// Even a failed generation with no usage must not hide the error step.
	add(3, 15, nil, nil)
	add(4, 17, nil, []byte("\x0a\x00INVALID_ARGUMENT tools.parameters.allowAsync.enum[0]: cannot be empty; Authorization: Bearer private-fixture-secret"))
	u, err := m.consumeOutcome(s)
	var terminal *Error
	if !errors.As(err, &terminal) || terminal.Status != 502 || terminal.Code != "upstream_invalid_argument" || u.Known || strings.Contains(err.Error(), "private-fixture-secret") {
		t.Fatalf("terminal error lost or leaked: %+v %v", u, err)
	}
	w := httptest.NewRecorder()
	out := newResponse(w, true, "agy/gemini")
	if err := out.text("Agent execution terminated due to error.", false); err != nil {
		t.Fatal(err)
	}
	out.fail(terminal)
	tracker := &Tracker{Stream: true}
	tracker.Feed(w.Body.Bytes())
	tracker.Finish()
	if tracker.Completed || tracker.Failure != "upstream_invalid_argument" || !strings.Contains(w.Body.String(), "response.failed") || strings.Contains(w.Body.String(), "response.completed") {
		t.Fatalf("terminal error marked successful: %+v %s", tracker, w.Body.String())
	}
	// Missing telemetry stays unknown, rather than inventing a failure or zero usage.
	s.ID = "missing-native-db"
	if u, err := m.consumeOutcome(s); err != nil || u.Known {
		t.Fatalf("missing telemetry misclassified: %+v %v", u, err)
	}
}

func TestAGYNativeErrorDiagnosticsAreBoundedAndSanitized(t *testing.T) {
	for _, tc := range []struct {
		marker, code string
		status       int
	}{
		{"INVALID_ARGUMENT", "upstream_invalid_argument", 502},
		{"RESOURCE_EXHAUSTED", "upstream_resource_exhausted", 429},
		{"PERMISSION_DENIED", "upstream_access_denied", 403},
		{"UNAUTHENTICATED", "upstream_access_denied", 403},
		{"DEADLINE_EXCEEDED", "upstream_deadline_exceeded", 504},
		{"unknown", "agent_execution_failed", 502},
	} {
		err := nativeAgentFailure([]byte(tc.marker + " private-fixture-secret"))
		var terminal *Error
		if !errors.As(err, &terminal) || terminal.Code != tc.code || terminal.Status != tc.status || strings.Contains(err.Error(), "private-fixture-secret") {
			t.Fatalf("wrong diagnostic: %v", err)
		}
	}
}

func TestAGYNativeRecoveredErrorDoesNotFailCompletedTurn(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "agy", "conversations")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	const id = "recovered-empty-output"
	db, err := sql.Open("sqlite", filepath.ToSlash(filepath.Join(dir, id+".db")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE steps(idx INTEGER PRIMARY KEY,step_type INTEGER,status INTEGER,metadata BLOB,step_payload BLOB)`); err != nil {
		t.Fatal(err)
	}
	add := func(idx, kind int, metadata, payload []byte) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO steps VALUES(?,?,3,?,?)`, idx, kind, metadata, payload); err != nil {
			t.Fatal(err)
		}
	}
	m := &Manager{cfg: RuntimeConfig{Home: home}}
	s := &session{ID: id, life: context.Background()}
	m.startUsage(s)
	// Reproduce the actual failure: all 342 output tokens were thinking. The
	// official agent recorded an empty-output error, then recovered with text.
	add(1, 15, usageProto(2, 100, 3, 342, 9, 342), nil)
	add(2, 17, nil, []byte("model output must contain either output text or tool calls"))
	add(3, 15, usageProto(2, 200, 3, 344, 9, 300), nil)
	u, err := m.consumeOutcome(s)
	if err != nil || u != (Usage{Known: true, Input: 300, Output: 686, Reasoning: 642, Total: 986}) || s.usageCursor != 3 {
		t.Fatalf("recovered turn failed or lost usage: %+v cursor=%d err=%v", u, s.usageCursor, err)
	}
	// Completing the recovered turn clears its duplicate-request guard normally.
	m.attempts = map[string]promptAttempt{"recovered-request": {State: "unknown"}}
	s.attempts = map[string]bool{"recovered-request": true}
	m.settleAttempts(s, true)
	if len(m.attempts) != 0 {
		t.Fatal("recovered completion left a duplicate-request tombstone")
	}
	if u, err = m.consumeOutcome(s); err != nil || u.Known {
		t.Fatalf("consumed steps repeated: %+v %v", u, err)
	}
	// Later thinking or missing telemetry must not hide a new terminal error.
	add(4, 17, nil, []byte("INVALID_ARGUMENT terminal error"))
	add(5, 15, usageProto(2, 20, 3, 10, 9, 10), nil)
	add(6, 15, nil, nil)
	u, err = m.consumeOutcome(s)
	var terminal *Error
	if !errors.As(err, &terminal) || terminal.Code != "upstream_invalid_argument" || u.Known || s.usageCursor != 3 {
		t.Fatalf("unrecovered error hidden by thinking/missing usage: %+v %v", u, err)
	}
	add(7, 15, usageProto(2, 30, 3, 12, 9, 2), nil)
	add(8, 17, nil, []byte("RESOURCE_EXHAUSTED later failure"))
	if _, err = m.consumeOutcome(s); !errors.As(err, &terminal) || terminal.Code != "upstream_resource_exhausted" {
		t.Fatalf("latest terminal error did not override intermediate recovery: %v", err)
	}
}
