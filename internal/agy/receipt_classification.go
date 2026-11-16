package agy

import (
	"regexp"
	"strconv"
	"strings"
)

// Diagnostic classification is separate from the conservative evidence used
// by recovery and compaction. A process exit code is not proof of a saved file
// or an external side effect, and must never authorize an automatic retry.
type receiptClassification struct {
	Outcome, Source string
	ExitCode        *int
}

var receiptExitLine = regexp.MustCompile(`^(?:Process exited with code |Exit code: )(-?[0-9]+)$`)
var receiptWallLine = regexp.MustCompile(`^Wall time: [0-9]+(?:\.[0-9]+)?(?: seconds)?$`)
var receiptChunkLine = regexp.MustCompile(`^Chunk ID: [[:alnum:]_-]+$`)
var receiptTokenLine = regexp.MustCompile(`^(?:Original|Final) token count: [0-9]+$`)
var receiptRunningLine = regexp.MustCompile(`^(?:Process|Script) running with (?:session|cell) ID [[:alnum:]_:-]+(?:[.].*)?$`)
var receiptPowerShellCategory = regexp.MustCompile(`(?m)^\s*\+\s*CategoryInfo\s*:`)
var receiptPowerShellErrorID = regexp.MustCompile(`(?m)^\s*\+\s*FullyQualifiedErrorId\s*:`)
var receiptPythonTraceback = regexp.MustCompile(`(?m)^Traceback \(most recent call last\):\s*$`)

func classifyClientReceipt(t tool, output any) receiptClassification {
	result := receiptClassification{Outcome: (&pendingTool{Tool: t, Output: output}).outcome()}
	if isRecoveryUnknown(output) {
		result.Source = "bridge_unknown"
		return result
	}
	if result.Outcome != "unknown" {
		result.Source = "structured_result"
		return result
	}
	// Apply text adapters only to known client executors, never to arbitrary
	// tools/remote MCP services whose data could resemble an execution report.
	name := t.key()
	name = strings.TrimPrefix(name, "functions.")
	if t.Type != "function" && t.Type != "custom" || name != "exec_command" && name != "write_stdin" && name != "apply_patch" {
		return result
	}
	text, ok := clientReceiptText(output)
	if !ok {
		return result
	}
	text = strings.TrimSpace(strings.TrimPrefix(text, "\ufeff"))
	if strings.HasPrefix(text, name+" failed:") {
		result.Outcome, result.Source = "failed", "client_transport_error"
		if strings.Contains(strings.ToLower(text), "blocked by policy") {
			result.Outcome, result.Source = "not_executed", "client_transport_rejection"
		}
		return result
	}
	if name == "apply_patch" && (strings.HasPrefix(text, "apply_patch verification failed:") || strings.HasPrefix(text, "Invalid patch:")) || strings.HasPrefix(text, "Error parsing function call:") {
		return receiptClassification{Outcome: "not_executed", Source: "client_transport_rejection"}
	}
	// Only parse transport metadata before Output:. Lines printed by the
	// command itself (including another "exit code") cannot override it.
	header, body, found := strings.Cut(text, "\nOutput:")
	if !found {
		header, body, found = strings.Cut(text, "\nFinal output:")
	}
	if !found {
		return result
	}
	var exit *int
	wall, running, chunk := false, false, false
	for _, line := range strings.Split(header, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case receiptChunkLine.MatchString(line):
			if chunk {
				return result
			}
			chunk = true
		case receiptWallLine.MatchString(line):
			if wall {
				return result
			}
			wall = true
		case receiptTokenLine.MatchString(line):
		case receiptRunningLine.MatchString(line):
			if running {
				return result
			}
			running = true
		default:
			m := receiptExitLine.FindStringSubmatch(line)
			if m == nil || exit != nil {
				return result
			}
			code, err := strconv.ParseInt(m[1], 10, 32)
			if err != nil {
				return result
			}
			value := int(code)
			exit = &value
		}
	}
	if !wall || exit == nil && !running || exit != nil && running {
		return result
	}
	result.Source = "client_text_envelope"
	if running {
		result.Outcome = "running"
		return result
	}
	result.ExitCode = exit
	result.Outcome = "failed"
	if *exit == 0 {
		result.Outcome = "success"
		if receiptPowerShellCategory.MatchString(body) && receiptPowerShellErrorID.MatchString(body) || receiptPythonTraceback.MatchString(body) {
			// Diagnostics can themselves be command output (e.g. reading a log).
			// Report a distinct warning state, not confirmed execution failure.
			result.Outcome, result.Source = "completed_with_errors", "client_text_diagnostics"
		}
	}
	return result
}

// Accept a string or an all-text Responses/MCP block list. Do not stringify
// arbitrary objects or inspect image data to manufacture an execution status.
func clientReceiptText(output any) (string, bool) {
	if text, ok := output.(string); ok {
		return strings.ReplaceAll(text, "\r\n", "\n"), true
	}
	blocks, ok := output.([]any)
	if !ok || len(blocks) == 0 {
		return "", false
	}
	var parts []string
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok || block["type"] != "text" && block["type"] != "input_text" && block["type"] != "output_text" {
			return "", false
		}
		text, ok := block["text"].(string)
		if !ok {
			return "", false
		}
		parts = append(parts, text)
	}
	return strings.ReplaceAll(strings.Join(parts, "\n"), "\r\n", "\n"), true
}

func classifyLateClientReceipt(s *session, actual map[string]any) (tool, receiptClassification) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range s.items {
		if resultCallID(item) != resultCallID(actual) || item["type"] != "function_call" && item["type"] != "custom_tool_call" {
			continue
		}
		t := tool{Type: strings.TrimSuffix(stringField(item, "type"), "_call"), Name: stringField(item, "name"), Namespace: stringField(item, "namespace")}
		return t, classifyClientReceipt(t, actual["output"])
	}
	return tool{}, classifyClientReceipt(tool{}, actual["output"])
}
