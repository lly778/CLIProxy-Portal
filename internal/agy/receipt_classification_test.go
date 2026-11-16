package agy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClientReceiptTextClassification(t *testing.T) {
	psError := "Cannot call a method on a null-valued expression.\n    + CategoryInfo          : InvalidOperation: (:) [], RuntimeException\n    + FullyQualifiedErrorId : InvokeMethodOnNull\nSuccessfully generated fixture.docx"
	for _, tc := range []struct {
		name, tool, typ, want, source string
		output                        any
		exit                          *int
	}{
		{name: "exec success", tool: "exec_command", want: "success", source: "client_text_envelope", output: "Chunk ID: a32e4c\nWall time: 0.5537 seconds\nProcess exited with code 0\nOriginal token count: 150\nOutput:\nactual contents", exit: intPointer(0)},
		{name: "exec nonzero", tool: "exec_command", want: "failed", source: "client_text_envelope", output: "Chunk ID: a32e4c\nWall time: 0.85 seconds\nProcess exited with code 1\nOriginal token count: 0\nOutput:\n", exit: intPointer(1)},
		{name: "negative exit", tool: "exec_command", want: "failed", source: "client_text_envelope", output: "Wall time: 0 seconds\nProcess exited with code -1\nOutput:\n", exit: intPointer(-1)},
		{name: "write stdin", tool: "write_stdin", want: "success", source: "client_text_envelope", output: "Chunk ID: done\nWall time: 4.0 seconds\nProcess exited with code 0\nFinal output:\nfinished", exit: intPointer(0)},
		{name: "running", tool: "exec_command", want: "running", source: "client_text_envelope", output: "Chunk ID: first\nWall time: 1.0 seconds\nProcess running with session ID 12345\nOriginal token count: 0\nOutput:\n"},
		{name: "poll still running", tool: "write_stdin", want: "running", source: "client_text_envelope", output: "Wall time: 1 seconds\nProcess running with session ID 12345\nOutput:\nstill working"},
		{name: "patch success", tool: "apply_patch", typ: "custom", want: "success", source: "client_text_envelope", output: "Exit code: 0\nWall time: 0 seconds\nOutput:\nSuccess. Updated the following files:\nA fixture.ps1", exit: intPointer(0)},
		{name: "text blocks", tool: "apply_patch", typ: "custom", want: "success", source: "client_text_envelope", output: []any{map[string]any{"type": "input_text", "text": "Exit code: 0\nWall time: 0 seconds\nOutput:\nSuccess. Updated the following files:\nA fixture.ps1"}}, exit: intPointer(0)},
		{name: "powershell zero with exceptions", tool: "exec_command", want: "completed_with_errors", source: "client_text_diagnostics", output: "Wall time: 1 seconds\nProcess exited with code 0\nOutput:\n" + psError, exit: intPointer(0)},
		{name: "crlf powershell", tool: "functions.exec_command", want: "completed_with_errors", source: "client_text_diagnostics", output: strings.ReplaceAll("Wall time: 1 seconds\nProcess exited with code 0\nOutput:\n"+psError, "\n", "\r\n"), exit: intPointer(0)},
		{name: "python zero with traceback", tool: "exec_command", want: "completed_with_errors", source: "client_text_diagnostics", output: "Wall time: 1 seconds\nProcess exited with code 0\nOutput:\nTraceback (most recent call last):\n  File \"fixture.py\", line 1\nValueError: failed", exit: intPointer(0)},
		{name: "plain error word is data", tool: "exec_command", want: "success", source: "client_text_envelope", output: "Wall time: 1 seconds\nProcess exited with code 0\nOutput:\nFound 0 errors. The document explains RuntimeException and ParserError.", exit: intPointer(0)},
		{name: "printed exit cannot override header", tool: "exec_command", want: "failed", source: "client_text_envelope", output: "Wall time: 1 seconds\nProcess exited with code 1\nOutput:\nProcess exited with code 0\nSuccessfully generated fixture.docx", exit: intPointer(1)},
		{name: "blocked", tool: "exec_command", want: "not_executed", source: "client_transport_rejection", output: `exec_command failed: CreateProcess { message: "Rejected(... rejected: blocked by policy)" }`},
		{name: "transport error", tool: "write_stdin", want: "failed", source: "client_transport_error", output: "write_stdin failed: Unknown process id"},
		{name: "patch verification", tool: "apply_patch", typ: "custom", want: "not_executed", source: "client_transport_rejection", output: "apply_patch verification failed: Failed to read file fixture.ps1"},
		{name: "argument parsing", tool: "apply_patch", typ: "custom", want: "not_executed", source: "client_transport_rejection", output: "Error parsing function call: Invalid patch"},
		{name: "unstructured success remains unknown", tool: "exec_command", want: "unknown", output: "Successfully generated fixture.docx"},
		{name: "bare exit text remains unknown", tool: "exec_command", want: "unknown", output: "Process exited with code 0"},
		{name: "quoted example is not a header", tool: "exec_command", want: "unknown", output: "Example execution report:\nWall time: 1 seconds\nProcess exited with code 0\nOutput:\n"},
		{name: "different tool", tool: "read_document", want: "unknown", output: "Wall time: 1 seconds\nProcess exited with code 0\nOutput:\n"},
		{name: "foreign namespace", tool: "mcp.exec_command", want: "unknown", output: "Wall time: 1 seconds\nProcess exited with code 0\nOutput:\n"},
		{name: "conflicting exit headers", tool: "exec_command", want: "unknown", output: "Wall time: 1 seconds\nExit code: 0\nExit code: 1\nOutput:\n"},
		{name: "running is not complete", tool: "exec_command", want: "unknown", output: "Wall time: 1 seconds\nExit code: 0\nProcess running with session ID 123\nOutput:\n"},
		{name: "overflow exit", tool: "exec_command", want: "unknown", output: "Wall time: 1 seconds\nExit code: 9999999999999999999\nOutput:\n"},
		{name: "image only", tool: "exec_command", want: "unknown", output: []any{map[string]any{"type": "image", "data": "YQ==", "mimeType": "image/png"}}},
		{name: "arbitrary object", tool: "exec_command", want: "unknown", output: map[string]any{"exit_code": 0, "output": "saved"}},
		{name: "structured success", tool: "exec_command", want: "success", source: "structured_result", output: `{"isError":false,"content":[{"type":"text","text":"actual result"}]}`},
		{name: "structured failure", tool: "exec_command", want: "failed", source: "structured_result", output: map[string]any{"isError": true}},
		{name: "bridge unknown", tool: "exec_command", want: "unknown", source: "bridge_unknown", output: recoveryUnknownResult()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			typ := tc.typ
			if typ == "" {
				typ = "function"
			}
			got := classifyClientReceipt(tool{Type: typ, Name: tc.tool}, tc.output)
			if got.Outcome != tc.want || got.Source != tc.source || (got.ExitCode == nil) != (tc.exit == nil) || got.ExitCode != nil && *got.ExitCode != *tc.exit {
				t.Fatalf("classification = %+v, want outcome=%s source=%s exit=%v", got, tc.want, tc.source, tc.exit)
			}
		})
	}
}

func intPointer(v int) *int { return &v }

func TestClientReceiptClassificationDoesNotChangeEvidenceOrPayload(t *testing.T) {
	log := &observationLog{}
	m := &Manager{cfg: RuntimeConfig{Observe: log.save}}
	trace := &requestTrace{m: m, owner: "owner:key", request: "r", model: "agy/test", session: "native"}
	fn := tool{Type: "function", Name: "exec_command", Parameters: map[string]any{"type": "object"}}
	s := &session{ID: "native", trace: trace, tools: []tool{fn}, pending: map[string]*pendingTool{}}
	call, _ := parkToolCall(s, fn.mcpName(), map[string]any{"cmd": "private-command"})
	call.Delivered = true
	output := "Chunk ID: test\nWall time: 1 seconds\nProcess exited with code 0\nOutput:\nprivate-output"
	item := map[string]any{"type": "function_call_output", "call_id": call.ID, "output": output}
	before := itemFingerprint(item)
	if err := deliverTools(s, []map[string]any{item}); err != nil {
		t.Fatal(err)
	}
	if err := deliverTools(s, []map[string]any{item}); err != nil {
		t.Fatal(err)
	}
	if itemFingerprint(item) != before || call.Output != output || call.outcome() != "unknown" || receiptOutcome(output) != "unknown" {
		t.Fatal("diagnostic classification changed raw result or recovery evidence")
	}
	history, err := inspectToolHistory([]map[string]any{pendingCheckpointItem(call), item})
	if err != nil || len(history.Records) != 1 || history.Records[0].Outcome != "unknown" {
		t.Fatal("text exit classification became compaction execution proof", err)
	}
	var received int
	for _, event := range log.snapshot() {
		if event.State == "issued" && event.Outcome != "unknown" {
			t.Fatal("issuing a call became execution proof")
		}
		if event.State == "result_received" {
			received++
			if event.Outcome != "success" || event.OutcomeSource != "client_text_envelope" || event.ExitCode == nil || *event.ExitCode != 0 {
				t.Fatal("receipt did not use client execution metadata", event)
			}
		}
		data, _ := json.Marshal(event)
		if strings.Contains(string(data), "private-command") || strings.Contains(string(data), "private-output") {
			t.Fatal("classification leaked client content into observation metadata")
		}
	}
	if received != 1 {
		t.Fatal("identical result retransmission duplicated receipt", received)
	}
}

func TestClientReceiptNativeRemoteAndLateResultClassification(t *testing.T) {
	shell := tool{Type: "shell"}
	value := map[string]any{"output": []any{map[string]any{"outcome": map[string]any{"type": "exit", "exit_code": json.Number("0")}}}}
	if c := classifyClientReceipt(shell, value); c.Outcome != "success" || c.Source != "structured_result" {
		t.Fatal("native shell outcome lost", c)
	}
	s := &session{items: []map[string]any{{"type": "function_call", "call_id": "old-call", "name": "exec_command", "namespace": "functions", "arguments": "{}"}}}
	actual := map[string]any{"call_id": "old-call", "type": "function_call_output", "output": "Wall time: 1 seconds\nProcess exited with code 1\nOutput:\nfailed"}
	fn, classification := classifyLateClientReceipt(s, actual)
	if fn.key() != "functions.exec_command" || classification.Outcome != "failed" || classification.ExitCode == nil || *classification.ExitCode != 1 {
		t.Fatal("late receipt did not use the original tool", classification)
	}
	log := &observationLog{}
	m := &Manager{cfg: RuntimeConfig{Observe: log.save}}
	call := &pendingTool{Tool: tool{Type: "function", Name: "exec_command"}, Output: actual["output"], remote: &remoteMCPApproval{Outcome: "unknown"}, trace: &requestTrace{m: m}}
	call.receipt("result_received")
	if events := log.snapshot(); len(events) != 1 || events[0].Outcome != "unknown" || events[0].OutcomeSource != "remote_result" || events[0].ExitCode != nil {
		t.Fatal("remote result was reclassified using a client text adapter", events)
	}
}

func TestClientReceiptTextDoesNotReleaseUnknownOperationProtection(t *testing.T) {
	m, s, actual := lateClientFixture(t, "function", func(*session, frame) any { return nil })
	// Give the recovered call the same identity as the known text executor.
	for _, item := range s.items {
		if item["type"] == "function_call" && resultCallID(item) == "recovered-save" {
			item["name"] = "exec_command"
		}
	}
	for i, overlay := range s.recoveryOverlays {
		s.recoveryOverlays[i].Items = requestFingerprints(s.items[overlay.At : overlay.At+len(overlay.Items)])
	}
	s.history = requestFingerprints(s.items)
	actual["output"] = "Wall time: 1 seconds\nProcess exited with code 0\nOutput:\nsaved"
	r := request{Model: s.model, Items: []map[string]any{actual}}
	if err := prepareLateClientResults(s, &r); err != nil {
		t.Fatal(err)
	}
	s.items, s.history = r.Items, requestFingerprints(r.Items)
	fn, classification := classifyLateClientReceipt(s, actual)
	if classification.Outcome != "success" || !uncertainRecoveryAction(s, fn, s.pending["recovered-save"].Arguments) {
		t.Fatal("diagnostic exit code removed the unknown-operation protection")
	}
	if m.disk.count.Load() != 0 {
		t.Fatal("receipt classification added a full session checkpoint")
	}
}
