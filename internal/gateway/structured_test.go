package gateway

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestStructuredRequestParsersKeepToolsButNotDefinitionsOrPrivateRoles(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []RecordEvent
	}{
		{
			name: "responses",
			body: `{"model":"gpt-test","instructions":"private instructions","tools":[{"name":"unused schema"}],"input":[{"role":"developer","content":"private developer"},{"type":"reasoning","summary":"private reasoning"},{"role":"user","content":[{"type":"input_text","text":"请检查"},{"type":"input_image","image_url":"data:image/png;base64,secret"}]},{"type":"function_call","name":"shell","call_id":"call-1","arguments":"{\"command\":\"pwd\"}"},{"type":"function_call_output","call_id":"call-1","output":"/work"}]}`,
			want: []RecordEvent{
				{Type: "message", Role: "user", Content: "请检查"},
				{Type: "tool_call", Role: "assistant", ToolName: "shell", ToolCallID: "call-1", Arguments: `{"command":"pwd"}`},
				{Type: "tool_result", Role: "tool", ToolName: "shell", ToolCallID: "call-1", Result: "/work"},
			},
		},
		{
			name: "chat completions",
			body: `{"tools":[{"function":{"name":"unused schema"}}],"messages":[{"role":"system","content":"private system"},{"role":"developer","content":"private developer"},{"role":"user","content":"hello"},{"role":"assistant","content":"working","tool_calls":[{"id":"tool-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"abc\"}"}}]},{"role":"tool","tool_call_id":"tool-1","content":"found"}]}`,
			want: []RecordEvent{
				{Type: "message", Role: "user", Content: "hello"},
				{Type: "message", Role: "assistant", Content: "working"},
				{Type: "tool_call", Role: "assistant", ToolName: "lookup", ToolCallID: "tool-1", Arguments: `{"q":"abc"}`},
				{Type: "tool_result", Role: "tool", ToolName: "lookup", ToolCallID: "tool-1", Result: "found"},
			},
		},
		{
			name: "anthropic messages",
			body: `{"system":"private system","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]},{"role":"assistant","content":[{"type":"tool_use","id":"tool-2","name":"shell","input":{"command":"pwd"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-2","content":"/work"}]}]}`,
			want: []RecordEvent{
				{Type: "message", Role: "user", Content: "hello"},
				{Type: "tool_call", Role: "assistant", ToolName: "shell", ToolCallID: "tool-2", Arguments: `{"command":"pwd"}`},
				{Type: "tool_result", Role: "tool", ToolName: "shell", ToolCallID: "tool-2", Result: "/work"},
			},
		},
		{
			name: "gemini",
			body: `{"systemInstruction":{"parts":[{"text":"private system"}]},"tools":[{"functionDeclarations":[{"name":"unused schema"}]}],"contents":[{"role":"user","parts":[{"text":"hello"},{"inlineData":{"mimeType":"image/png","data":"secret"}}]},{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"q":"abc"}}},{"text":"private thought","thought":true}]},{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"answer":"found"}}}]}]}`,
			want: []RecordEvent{
				{Type: "message", Role: "user", Content: "hello"},
				{Type: "tool_call", Role: "assistant", ToolName: "lookup", Arguments: `{"q":"abc"}`},
				{Type: "tool_result", Role: "tool", ToolName: "lookup", Result: `{"answer":"found"}`},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := requestRecordEvents([]byte(tc.body))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("events = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestStructuredResponseParsersKeepOnlyActualOutput(t *testing.T) {
	const response = `{"output":[{"type":"reasoning","summary":"private thought"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]},{"type":"function_call","name":"shell","call_id":"call-1","arguments":"{\"command\":\"pwd\"}"}],"tools":[{"name":"unused schema"}]}`
	want := []RecordEvent{
		{Type: "message", Role: "assistant", Content: "done"},
		{Type: "tool_call", Role: "assistant", ToolName: "shell", ToolCallID: "call-1", Arguments: `{"command":"pwd"}`},
	}
	if got := responseRecordEvents([]byte(response), "application/json"); !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON events = %#v, want %#v", got, want)
	}
	stream := "data: {\"type\":\"response.created\",\"response\":{\"tools\":[{\"name\":\"unused schema\"}]}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"done\"}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":1,\"delta\":\"secret fallback\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
	if got := responseRecordEvents([]byte(stream), "text/event-stream"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Responses stream events = %#v, want %#v", got, want)
	}
	fallback := "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"done\"}\n\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"type\":\"function_call\",\"name\":\"shell\",\"call_id\":\"call-1\"}}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":1,\"delta\":\"{\\\"command\\\":\\\"pwd\\\"}\"}\n\n"
	if got := responseRecordEvents([]byte(fallback), "text/event-stream"); !reflect.DeepEqual(got, want) {
		t.Fatalf("partial Responses stream events = %#v, want %#v", got, want)
	}
}

func TestStructuredToolStreamsAcrossProtocols(t *testing.T) {
	tests := []struct {
		name, body string
		want       []RecordEvent
	}{
		{
			"chat",
			"data: {\"choices\":[{\"delta\":{\"content\":\"checking\",\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"function\":{\"name\":\"shell\",\"arguments\":\"{\\\"command\\\":\"}}]}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"pwd\\\"}\"}}]}}]}\n\n",
			[]RecordEvent{{Type: "message", Role: "assistant", Content: "checking"}, {Type: "tool_call", Role: "assistant", ToolName: "shell", ToolCallID: "call-1", Arguments: `{"command":"pwd"}`}},
		},
		{
			"anthropic",
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"checking\"}}\n\n" +
				"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call-1\",\"name\":\"shell\",\"input\":{}}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"command\\\":\\\"pwd\\\"}\"}}\n\n",
			[]RecordEvent{{Type: "message", Role: "assistant", Content: "checking"}, {Type: "tool_call", Role: "assistant", ToolName: "shell", ToolCallID: "call-1", Arguments: `{"command":"pwd"}`}},
		},
		{
			"gemini",
			"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"checking\"},{\"functionCall\":{\"name\":\"shell\",\"args\":{\"command\":\"pwd\"}}}]}}]}\n\n",
			[]RecordEvent{{Type: "message", Role: "assistant", Content: "checking"}, {Type: "tool_call", Role: "assistant", ToolName: "shell", Arguments: `{"command":"pwd"}`}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := responseRecordEvents([]byte(tc.body), "text/event-stream"); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("stream events = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestStructuredEventsLimitAndOmitMedia(t *testing.T) {
	value := safeToolValue(map[string]any{"command": "pwd", "image": "secret-image", "source": map[string]any{"type": "base64", "data": "secret-base64"}})
	if strings.Contains(value, "secret") || !strings.Contains(value, "pwd") {
		t.Fatalf("tool value retained media: %q", value)
	}
	events, truncated := limitRecordEvents([]RecordEvent{{Type: "message", Role: "user", Content: strings.Repeat("中", 200)}}, 200)
	if !truncated || len(events) != 1 || events[0].Content == "" || !utf8.ValidString(events[0].Content) {
		t.Fatalf("limited events = %#v, truncated=%t", events, truncated)
	}
	encoded, err := json.Marshal(events[0])
	if err != nil || len(encoded) > 200 {
		t.Fatalf("limited event encoding = %d, %v", len(encoded), err)
	}
}
