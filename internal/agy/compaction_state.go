package agy

import (
	"context"
	"encoding/json"
	"strings"
)

func (m *Manager) finishCodexCheckpoint(ctx context.Context, r request, out *response) error {
	if !isCodexCompactionRequest(r) {
		return nil
	}
	source := append([]map[string]any(nil), r.Items[:len(r.Items)-1]...)
	// A parked, already completed remote result can be published while the
	// original session produces its checkpoint. Seal that actual evidence too;
	// approval alone must never stand in for execution.
	seen := map[string]bool{}
	for _, item := range source {
		if item["type"] == "mcp_call" {
			seen[stringField(item, "id")] = true
		}
	}
	for _, item := range out.output {
		id := stringField(item, "id")
		if item["type"] == "mcp_call" && id != "" && !seen[id] {
			source = append(source, item)
			seen[id] = true
		}
	}
	history, err := inspectToolHistory(source)
	if err != nil {
		return err
	}
	if len(history.Open) > 0 {
		return fail(409, "compaction_tools_pending", "工具结果尚未完整回传，未提交新摘要")
	}
	proof, err := proofFor(source, source, nil, "codex_summary")
	if err != nil {
		return err
	}
	note, archive, err := m.prepareToolRecordNote(ctx, r.owner, r.Model, source, history.Records, proof.SourceHash)
	if err != nil {
		return err
	}
	if len(out.textValue)+len(note) > MaxText {
		return fail(413, "compaction_failed", "摘要与工具事实索引超过安全大小，未提交新摘要")
	}
	if archive != nil {
		if err := m.saveToolRecords(ctx, *archive); err != nil {
			return fail(503, "tool_record_unavailable", "工具事实索引保存失败，未提交新摘要")
		}
		for _, ref := range archive.Parents {
			cap, _ := m.parseToolRecordReference(r.owner, r.Model, ref)
			proof.ParentRecords = append(proof.ParentRecords, cap.ID)
		}
	}
	if note != "" {
		if err := out.text(note, false); err != nil {
			return err
		}
	}
	proof.SummaryHash, proof.OutputBytes, proof.ToolRecords = digest(out.textValue), len(out.textValue), len(history.Records)
	out.trace.emit(Observation{Kind: "compaction", State: "completed", Compaction: proof})
	return nil
}

// Proof describes coverage, not the semantic quality of a generated summary.
type compactionProof struct {
	ID, Source, SourceHash, CoveredHash, TailHash, SummaryHash           string
	ParentRecords                                                        []string `json:",omitempty"`
	InputItems, CoveredItems, TailItems, Images, InputBytes, OutputBytes int
	ToolRecords                                                          int
}

type toolHistoryRecord struct {
	CallID, Tool, Kind, Execution, Outcome, ResultHash string
	Provenance                                         string
	CallIndex, ResultIndex                             int
	Call, Result                                       map[string]any `json:",omitempty"`
}

type toolHistory struct {
	Records []toolHistoryRecord
	Open    []int
}

// Inspect the wire transcript independently of resident pending state: imported
// or archived history must obey the same call/result identity rules.
func inspectToolHistory(items []map[string]any) (toolHistory, error) {
	h := toolHistory{}
	pending, seen := map[string]int{}, map[string]bool{}
	invalid := func() (toolHistory, error) {
		return h, fail(400, "invalid_compaction_history", "压缩历史中的工具调用、结果或执行来源无法完整匹配，原历史未被替换")
	}
	for i, item := range items {
		kind := stringField(item, "type")
		if kind == "mcp_call" {
			id := stringField(item, "id")
			if id == "" || len(id) > 256 || seen[id] {
				return invalid()
			}
			seen[id] = true
			name := stringField(item, "server_label") + "." + stringField(item, "name")
			if len(name) > 256 {
				return invalid()
			}
			record := toolHistoryRecord{CallID: id, Tool: name, Kind: kind, Execution: "server", Provenance: "client_history", CallIndex: i, ResultIndex: i, Call: item, Result: item, ResultHash: itemFingerprint(item), Outcome: receiptOutcome(item["output"])}
			if item["error"] != nil {
				record.Outcome = "failed"
				if item["error"] == "approval_denied" || item["error"] == "authorization_changed" || item["error"] == "remote_capacity" {
					record.Outcome = "not_executed"
				}
				if item["error"] == "execution_unknown" || item["error"] == "result_unrepresentable" {
					record.Outcome = "unknown"
				}
			}
			if item["status"] == "in_progress" && item["output"] == nil && item["error"] == nil {
				record.ResultIndex = -1
				record.Result = nil
				record.ResultHash = ""
				record.Outcome = "not_received"
			}
			h.Records = append(h.Records, record)
			continue
		}
		approval := kind == "mcp_approval_request"
		if !approval && !isCallItem(kind) && !isToolResultItem(kind) {
			continue
		}
		id := resultCallID(item)
		if id == "" || len(id) > 256 {
			return invalid()
		}
		if approval || isCallItem(kind) {
			if seen[id] {
				return invalid()
			}
			seen[id] = true
			execution := stringField(item, "execution")
			if approval {
				execution = "server"
			} else if kind == "tool_search_call" {
				if execution != "client" && execution != "server" {
					return invalid()
				}
			} else {
				if execution != "" && execution != "client" {
					return invalid()
				}
				execution = "client"
			}
			name := stringField(item, "name")
			if namespace := stringField(item, "namespace"); namespace != "" {
				name = namespace + "." + name
			}
			if name == "" {
				name = strings.TrimSuffix(kind, "_call")
			}
			if approval {
				name = stringField(item, "server_label") + "." + name
			}
			if len(name) > 256 {
				return invalid()
			}
			pending[id] = len(h.Records)
			h.Records = append(h.Records, toolHistoryRecord{CallID: id, Tool: name, Kind: kind, Execution: execution, Outcome: "not_received", Provenance: "client_history", CallIndex: i, ResultIndex: -1, Call: item})
			continue
		}
		index, ok := pending[id]
		if !ok {
			return invalid()
		}
		record := &h.Records[index]
		if execution := stringField(item, "execution"); execution != "" && execution != record.Execution {
			return invalid()
		}
		expected := record.Kind + "_output"
		if record.Kind == "mcp_approval_request" {
			expected = "mcp_approval_response"
		}
		if record.Kind == "tool_search_call" {
			expected = "tool_search_output"
		}
		if kind != expected {
			return invalid()
		}
		if record.Kind == "mcp_approval_request" {
			approved, ok := item["approve"].(bool)
			if !ok {
				return invalid()
			}
			record.ResultIndex, record.Result, record.ResultHash = i, item, itemFingerprint(item)
			record.Outcome = "approval_denied"
			if approved {
				record.Outcome = "approval_granted"
			}
			delete(pending, id)
			continue
		}
		if record.Kind == "tool_search_call" {
			if stringField(item, "execution") != record.Execution {
				return invalid()
			}
			if _, ok := item["tools"].([]any); !ok {
				return invalid()
			}
		} else if _, present := item["output"]; !present {
			return invalid()
		}
		if _, err := promptBlocksLimit([]map[string]any{item}, MaxBody); err != nil {
			return h, err
		}
		record.ResultIndex, record.Result, record.ResultHash = i, item, itemFingerprint(item)
		c := &pendingTool{Tool: tool{Type: strings.TrimSuffix(record.Kind, "_call")}, Output: item["output"]}
		if record.Kind == "apply_patch_call" || record.Kind == "shell_call" || record.Kind == "local_shell_call" {
			c.Output = item
		}
		record.Outcome = c.outcome()
		delete(pending, id)
	}
	for _, record := range h.Records {
		if record.ResultIndex < 0 {
			h.Open = append(h.Open, record.CallIndex)
		}
	}
	return h, nil
}

func safeCompactSplit(items []map[string]any, h toolHistory) ([]map[string]any, []map[string]any, error) {
	head, _, err := splitCompactTail(items)
	if err != nil {
		return nil, nil, err
	}
	cut := len(head)
	for i := len(h.Records) - 1; i >= 0; i-- {
		record := h.Records[i]
		if record.CallIndex < cut && (record.ResultIndex < 0 || record.ResultIndex >= cut) {
			cut = record.CallIndex
			for cut > 0 && items[cut]["role"] != "user" {
				cut--
			}
		}
	}
	if _, err := promptBlocks(items[cut:]); err != nil {
		if len(h.Open) > 0 {
			return nil, nil, fail(409, "compaction_tools_pending", "未完成工具调用与最新输入无法完整保留，请先回传工具结果再压缩")
		}
		return nil, nil, err
	}
	return items[:cut], items[cut:], nil
}

func proofFor(items, head, tail []map[string]any, source string) (*compactionProof, error) {
	blocks, err := promptBlocksLimit(items, MaxBody)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	proof := &compactionProof{ID: newID("coverage_"), Source: source, SourceHash: digest(requestFingerprints(items)), CoveredHash: digest(requestFingerprints(head)), TailHash: digest(requestFingerprints(tail)), InputItems: len(items), CoveredItems: len(head), TailItems: len(tail), InputBytes: len(data)}
	for _, raw := range blocks {
		if b, ok := raw.(map[string]string); ok && b["type"] == "image" {
			proof.Images++
		}
	}
	return proof, nil
}
