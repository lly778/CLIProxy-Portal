package agy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func evidenceTranscript() []map[string]any {
	return []map[string]any{
		{"role": "user", "content": "save the requested file once"},
		{"type": "function_call", "call_id": "save-once", "name": "save", "arguments": `{"path":"fixture.docx"}`},
		{"type": "function_call_output", "call_id": "save-once", "output": map[string]any{"isError": false, "structuredContent": map[string]any{"record": json.Number("9007199254740993")}, "content": []any{map[string]any{"type": "text", "text": "retained-actual-save-result"}}}},
		{"role": "assistant", "content": strings.Repeat("history ", 22000)},
		{"role": "user", "content": "continue without repeating the save"},
	}
}

func TestCompactionKeepsDeterministicFactsWhenSummaryOmitsAction(t *testing.T) {
	log := &observationLog{}
	m := &Manager{cfg: RuntimeConfig{Secret: strings.Repeat("s", 64), Observe: log.save}, summarizeTest: func(context.Context, string, []any, func()) (string, error) {
		return "summary without the execution details", nil
	}}
	trace := &requestTrace{m: m, owner: "owner:key", model: "agy/test", request: "compact"}
	ctx := context.WithValue(t.Context(), traceKey{}, trace)
	items := evidenceTranscript()
	before := digest(items)
	result, err := m.compactWithProof(ctx, request{owner: "owner:key", Model: "agy/test", Items: items}, nil)
	if err != nil {
		t.Fatal(err)
	}
	text := result.Items[0]["content"].(string)
	if !strings.Contains(text, "save-once") || !strings.Contains(text, `"Outcome":"success"`) || digest(items) != before {
		t.Fatal("summary replaced verified evidence or mutated source", text)
	}
	refs, err := m.previousToolRecords("owner:key", "agy/test", result.Items)
	if err != nil || len(refs) != 1 {
		t.Fatal("missing signed result index", err)
	}
	read := m.readToolRecord(ctx, "owner:key", "agy/test", map[string]any{"reference": refs[0], "call_id": "save-once"})
	b, _ := json.Marshal(read)
	if !strings.Contains(string(b), "retained-actual-save-result") || !strings.Contains(string(b), "9007199254740993") {
		t.Fatal("actual result lost", string(b))
	}
	p := result.Proof
	if p.InputItems != len(items) || p.CoveredItems+p.TailItems != len(items) || p.ToolRecords != 1 || p.SourceHash == "" || p.SummaryHash != digest(strings.TrimPrefix(text, checkpointPrefix)) {
		t.Fatal("invalid coverage", p)
	}
	token, err := m.sealCheckpoint("owner:key", "agy/test", strings.TrimPrefix(text, checkpointPrefix), p)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := m.openCheckpoint("owner:key", "agy/test", token)
	if err != nil || opened != strings.TrimPrefix(text, checkpointPrefix) {
		t.Fatal("v2 checkpoint lost proof/facts", err)
	}
	for _, owner := range []string{"other:key", "owner:other"} {
		if !toolResultFailed(m.readToolRecord(ctx, owner, "agy/test", map[string]any{"reference": refs[0]})) {
			t.Fatal("cross-owner result disclosure")
		}
	}
	changed := copyItems(result.Items)
	changed[0]["content"] = strings.Replace(text, `"Outcome":"success"`, `"Outcome":"failed"`, 1)
	if _, err := m.previousToolRecords("owner:key", "agy/test", changed); err == nil {
		t.Fatal("rewritten facts accepted with old reference")
	}
	if len(log.snapshot()) == 0 {
		t.Fatal("coverage not observable")
	}
}

func TestCompactionHistoryRejectsBrokenIdentityBeforeSummarizing(t *testing.T) {
	for _, change := range []func([]map[string]any) []map[string]any{
		func(v []map[string]any) []map[string]any { v[2]["call_id"] = "foreign"; return v },
		func(v []map[string]any) []map[string]any { v[2]["type"] = "custom_tool_call_output"; return v },
		func(v []map[string]any) []map[string]any { return append(v, v[1]) },
		func(v []map[string]any) []map[string]any { delete(v[2], "output"); return v },
		func(v []map[string]any) []map[string]any { v[1]["execution"] = "server"; return v },
		func(v []map[string]any) []map[string]any { v[2]["execution"] = "server"; return v },
	} {
		calls := 0
		m := &Manager{summarizeTest: func(context.Context, string, []any, func()) (string, error) { calls++; return "bad", nil }}
		items := change(evidenceTranscript())
		before := digest(items)
		if _, err := m.compactItems(t.Context(), request{Model: "agy/test", Items: items}, nil); err == nil || calls != 0 || digest(items) != before {
			t.Fatal("invalid history reached summarizer or changed source")
		}
	}
}

func TestCompactionSplitPreservesCrossingAndOpenToolSpans(t *testing.T) {
	for _, closed := range []bool{false, true} {
		items := []map[string]any{{"role": "user", "content": "old"}, {"role": "assistant", "content": strings.Repeat("x", MaxText+1)}, {"role": "user", "content": "tool task"}, {"type": "function_call", "name": "write", "call_id": "pending", "arguments": "{}"}, {"role": "user", "content": "new steering"}}
		if closed {
			items = append(items, map[string]any{"type": "function_call_output", "call_id": "pending", "output": "actual result"})
		}
		h, err := inspectToolHistory(items)
		if err != nil {
			t.Fatal(err)
		}
		head, tail, err := safeCompactSplit(items, h)
		if err != nil || len(head) != 2 || len(tail) != len(items)-2 || tail[1]["call_id"] != "pending" {
			t.Fatal("tool pair split by user boundary", err)
		}
	}
}

func TestToolRecordArchiveIsEncryptedPersistentAndExpires(t *testing.T) {
	home, secret := t.TempDir(), strings.Repeat("s", 64)
	disk, err := openSessionDiskCache(home, secret)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{cfg: RuntimeConfig{Secret: secret}, disk: disk}
	items := evidenceTranscript()
	h, err := inspectToolHistory(items)
	if err != nil {
		t.Fatal(err)
	}
	note, archive, err := m.prepareToolRecordNote(t.Context(), "owner:key", "agy/test", items, h.Records, digest(items))
	if err != nil {
		t.Fatal(err)
	}
	if err = m.saveToolRecords(t.Context(), *archive); err != nil {
		t.Fatal(err)
	}
	var sealed []byte
	if err = disk.db.QueryRow(`SELECT payload FROM tool_record_archives WHERE id=?`, archive.ID).Scan(&sealed); err != nil || strings.Contains(string(sealed), "retained-actual-save-result") {
		t.Fatal("raw result was not encrypted", err)
	}
	disk.db.Close()
	disk, err = openSessionDiskCache(home, secret)
	if err != nil {
		t.Fatal(err)
	}
	defer disk.db.Close()
	m.disk = disk
	refs, err := m.previousToolRecords("owner:key", "agy/test", []map[string]any{{"content": note}})
	if err != nil {
		t.Fatal(err)
	}
	if toolResultFailed(m.readToolRecord(t.Context(), "owner:key", "agy/test", map[string]any{"reference": refs[0], "call_id": "save-once"})) {
		t.Fatal("archive lost across restart")
	}
	archive.Expires = time.Now().Add(-time.Second).UnixMilli()
	expired, _ := m.toolRecordReference(*archive)
	if !toolResultFailed(m.readToolRecord(t.Context(), "owner:key", "agy/test", map[string]any{"reference": expired})) {
		t.Fatal("expired record returned as available")
	}
}

func TestRepeatedCompactionKeepsPreviousIndexAndOldReferenceValid(t *testing.T) {
	m := &Manager{cfg: RuntimeConfig{Secret: strings.Repeat("s", 64)}, summarizeTest: func(context.Context, string, []any, func()) (string, error) { return "checkpoint", nil }}
	first, err := m.compactWithProof(t.Context(), request{owner: "owner:key", Model: "agy/test", Items: evidenceTranscript()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldRefs, err := m.previousToolRecords("owner:key", "agy/test", first.Items)
	if err != nil {
		t.Fatal(err)
	}
	next := append(copyItems(first.Items), map[string]any{"role": "assistant", "content": strings.Repeat("y", MaxText+1)}, map[string]any{"role": "user", "content": "continue"})
	second, err := m.compactWithProof(t.Context(), request{owner: "owner:key", Model: "agy/test", Items: next}, nil)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := m.previousToolRecords("owner:key", "agy/test", second.Items)
	if err != nil || len(refs) != 1 {
		t.Fatal(err)
	}
	cap, _ := m.parseToolRecordReference("owner:key", "agy/test", refs[0])
	archive, err := m.getToolRecords(t.Context(), cap)
	if err != nil || len(archive.Parents) != 1 || archive.Parents[0] != oldRefs[0] {
		t.Fatal("previous evidence index lost", err)
	}
	if toolResultFailed(m.readToolRecord(t.Context(), "owner:key", "agy/test", map[string]any{"reference": oldRefs[0], "call_id": "save-once"})) {
		t.Fatal("new checkpoint invalidated prior evidence")
	}
}

func TestToolRecordLookupPreservesFailureImagesAndPolicyWithoutExecution(t *testing.T) {
	m, s := recoveryFixture(t, func(*session, frame) any { t.Error("record read invoked a model"); return nil })
	m.cfg.Secret = strings.Repeat("s", 64)
	s.secret = "fixture-secret"
	items := evidenceTranscript()[:3]
	items[2]["output"] = map[string]any{"isError": true, "structuredContent": map[string]any{"value": json.Number("9007199254740993")}, "content": []any{map[string]any{"type": "image", "mimeType": "image/png", "data": "YQ=="}, map[string]any{"type": "text", "text": "failed save evidence"}}}
	h, err := inspectToolHistory(items)
	if err != nil {
		t.Fatal(err)
	}
	note, archive, err := m.prepareToolRecordNote(t.Context(), s.owner, s.model, items, h.Records, digest(items))
	if err != nil {
		t.Fatal(err)
	}
	if err = m.saveToolRecords(t.Context(), *archive); err != nil {
		t.Fatal(err)
	}
	refs, err := m.previousToolRecords(s.owner, s.model, []map[string]any{{"content": note}})
	if err != nil {
		t.Fatal(err)
	}
	params := map[string]any{"name": readToolRecordName, "arguments": map[string]any{"reference": refs[0], "call_id": "save-once"}}
	result := localMCPCall(t, m, s, "tools/call", params)
	encoded, _ := json.Marshal(result)
	for _, want := range []string{"image/png", "YQ==", "failed save evidence", "9007199254740993"} {
		if !strings.Contains(string(encoded), want) {
			t.Fatal("lookup lost result detail", want)
		}
	}
	if !toolResultFailed(result) || len(s.pending) != 0 {
		t.Fatal("failure became success or an execution")
	}
	s.policy.Mode = "none"
	denied := localMCPCall(t, m, s, "tools/call", params)
	encoded, _ = json.Marshal(denied)
	if !toolResultFailed(denied) || strings.Contains(string(encoded), "failed save evidence") {
		t.Fatal("none policy permitted record read")
	}
	if !toolResultFailed(m.readToolRecord(t.Context(), s.owner, "agy/other", map[string]any{"reference": refs[0]})) {
		t.Fatal("cross-model archive disclosed")
	}
	proof, err := proofFor(items, items, nil, "codex_summary")
	if err != nil || proof.Images != 1 {
		t.Fatal("coverage lost image", err)
	}
}

func TestToolRecordExpiryNeverMovesBackwardsOnOverlappingWrites(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		m := &Manager{cfg: RuntimeConfig{Secret: strings.Repeat("s", 64)}}
		if persistent {
			d, err := openSessionDiskCache(t.TempDir(), m.cfg.Secret)
			if err != nil {
				t.Fatal(err)
			}
			defer d.db.Close()
			m.disk = d
		}
		items := evidenceTranscript()[:3]
		h, _ := inspectToolHistory(items)
		_, archive, err := m.prepareToolRecordNote(t.Context(), "owner", "agy/test", items, h.Records, digest(items))
		if err != nil {
			t.Fatal(err)
		}
		short := *archive
		long := short
		long.Expires += 1000
		shortRef, _ := m.toolRecordReference(short)
		longRef, _ := m.toolRecordReference(long)
		if err = m.saveToolRecords(t.Context(), long); err != nil {
			t.Fatal(err)
		}
		if err = m.saveToolRecords(t.Context(), short); err != nil {
			t.Fatal(err)
		}
		for _, ref := range []string{shortRef, longRef} {
			if toolResultFailed(m.readToolRecord(t.Context(), "owner", "agy/test", map[string]any{"reference": ref, "call_id": "save-once"})) {
				t.Fatal("overlapping archive write invalidated reference", persistent)
			}
		}
	}
}

func TestCheckpointRejectsMismatchedProofBeforeSealing(t *testing.T) {
	m := &Manager{cfg: RuntimeConfig{Secret: strings.Repeat("s", 64)}}
	if _, err := m.sealCheckpoint("owner", "agy/test", "summary", &compactionProof{SummaryHash: digest("other summary")}); err == nil {
		t.Fatal("sealed mismatched summary proof")
	}
	if _, err := m.sealCheckpoint("owner", "agy/test", strings.Repeat("<", compactSummary)); err == nil {
		t.Fatal("oversized encoded checkpoint committed")
	}
}
