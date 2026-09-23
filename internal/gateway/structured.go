package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"
)

// StructuredCaptureContentType marks captures whose shared encrypted blobs
// contain RecordEvent JSON.
const StructuredCaptureContentType = "application/vnd.cliproxy.interaction+json"
const StructuredMessageRole = "structured-v1"

// RecordEvent is intentionally narrower than an API payload. In particular,
// tool definitions, system/developer instructions, reasoning, wire headers and
// media are never copied into persistent records.
type RecordEvent struct {
	Type       string `json:"type"` // message, tool_call, or tool_result
	Role       string `json:"role"` // user, assistant, or tool
	Content    string `json:"content,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	Arguments  string `json:"arguments,omitempty"`
	Result     string `json:"result,omitempty"`
}

func requestRecordEvents(body []byte) []RecordEvent {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return nil
	}
	if input, ok := payload["input"]; ok {
		return inputRecordEvents(input)
	}
	if messages, ok := payload["messages"]; ok {
		return inputRecordEvents(messages)
	}
	if contents, ok := payload["contents"]; ok {
		return geminiRecordEvents(contents)
	}
	return nil
}

func inputRecordEvents(value any) []RecordEvent {
	if text, ok := value.(string); ok {
		return textRecord("user", text)
	}
	items, ok := value.([]any)
	if !ok {
		if item, ok := value.(map[string]any); ok {
			items = []any{item}
		} else {
			return nil
		}
	}
	var events []RecordEvent
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		events = append(events, itemRecordEvents(object)...)
	}
	fillToolNames(events)
	return events
}

func itemRecordEvents(item map[string]any) []RecordEvent {
	kind := stringField(item, "type")
	switch kind {
	case "reasoning":
		return nil
	case "function_call", "custom_tool_call":
		return toolCallRecord(stringField(item, "name"), stringField(item, "call_id"), firstValue(item, "arguments", "input"))
	case "function_call_output", "custom_tool_call_output":
		return toolResultRecord(stringField(item, "name"), stringField(item, "call_id"), item["output"])
	}
	if strings.HasSuffix(kind, "_call") && kind != "" {
		return toolCallRecord(kind, stringField(item, "call_id"), firstValue(item, "arguments", "input", "action", "query"))
	}
	role := stringField(item, "role")
	if role == "tool" {
		return toolResultRecord(stringField(item, "name"), stringField(item, "tool_call_id"), item["content"])
	}
	if role != "user" && role != "assistant" {
		return nil
	}
	if kind != "" && kind != "message" {
		return nil
	}
	events := contentRecordEvents(role, item["content"])
	if role == "assistant" {
		if calls, ok := item["tool_calls"].([]any); ok {
			for _, call := range calls {
				object, ok := call.(map[string]any)
				if !ok {
					continue
				}
				function, _ := object["function"].(map[string]any)
				events = append(events, toolCallRecord(stringField(function, "name"), stringField(object, "id"), function["arguments"])...)
			}
		}
		if function, ok := item["function_call"].(map[string]any); ok {
			events = append(events, toolCallRecord(stringField(function, "name"), "", function["arguments"])...)
		}
	}
	return events
}

func contentRecordEvents(role string, value any) []RecordEvent {
	if text, ok := value.(string); ok {
		return textRecord(role, text)
	}
	parts, ok := value.([]any)
	if !ok {
		return nil
	}
	var events []RecordEvent
	for _, part := range parts {
		object, ok := part.(map[string]any)
		if !ok {
			continue
		}
		switch stringField(object, "type") {
		case "text", "input_text", "output_text":
			events = append(events, textRecord(role, stringField(object, "text"))...)
		case "tool_use":
			if role == "assistant" {
				events = append(events, toolCallRecord(stringField(object, "name"), stringField(object, "id"), object["input"])...)
			}
		case "tool_result":
			if role == "user" {
				events = append(events, toolResultRecord("", stringField(object, "tool_use_id"), object["content"])...)
			}
		}
	}
	return events
}

func geminiRecordEvents(value any) []RecordEvent {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	var events []RecordEvent
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role := stringField(object, "role")
		if role == "model" {
			role = "assistant"
		} else if role == "" {
			role = "user"
		}
		if role != "user" && role != "assistant" {
			continue
		}
		parts, _ := object["parts"].([]any)
		for _, part := range parts {
			piece, ok := part.(map[string]any)
			if !ok || piece["thought"] == true {
				continue
			}
			if text := stringField(piece, "text"); text != "" {
				events = append(events, textRecord(role, text)...)
			}
			if call, ok := piece["functionCall"].(map[string]any); ok && role == "assistant" {
				events = append(events, toolCallRecord(stringField(call, "name"), stringField(call, "id"), call["args"])...)
			}
			if result, ok := piece["functionResponse"].(map[string]any); ok {
				events = append(events, toolResultRecord(stringField(result, "name"), stringField(result, "id"), result["response"])...)
			}
		}
	}
	return events
}

func responseRecordEvents(body []byte, contentType string) []RecordEvent {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return streamRecordEvents(body)
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) == nil {
		return responseObjectRecordEvents(payload)
	}
	var list []map[string]any
	if json.Unmarshal(body, &list) != nil {
		return nil
	}
	var events []RecordEvent
	for _, item := range list {
		events = append(events, responseObjectRecordEvents(item)...)
	}
	return events
}

func responseObjectRecordEvents(payload map[string]any) []RecordEvent {
	var events []RecordEvent
	if output, ok := payload["output"].([]any); ok {
		for _, item := range output {
			object, ok := item.(map[string]any)
			if ok {
				events = append(events, itemRecordEvents(object)...)
			}
		}
	}
	if choices, ok := payload["choices"].([]any); ok {
		for _, choice := range choices {
			object, ok := choice.(map[string]any)
			if !ok {
				continue
			}
			message, ok := object["message"].(map[string]any)
			if ok {
				events = append(events, itemRecordEvents(message)...)
			}
		}
	}
	if payload["type"] == "message" && payload["role"] == "assistant" {
		events = append(events, itemRecordEvents(payload)...)
	}
	if candidates, ok := payload["candidates"].([]any); ok {
		for _, candidate := range candidates {
			object, ok := candidate.(map[string]any)
			if !ok {
				continue
			}
			content, ok := object["content"].(map[string]any)
			if ok {
				events = append(events, geminiRecordEvents([]any{content})...)
			}
		}
	}
	return events
}

func textRecord(role, value string) []RecordEvent {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return []RecordEvent{{Type: "message", Role: role, Content: value}}
}

func toolCallRecord(name, id string, args any) []RecordEvent {
	arguments := safeToolValue(args)
	if name == "" && id == "" && arguments == "" {
		return nil
	}
	return []RecordEvent{{Type: "tool_call", Role: "assistant", ToolName: name, ToolCallID: id, Arguments: arguments}}
}

func toolResultRecord(name, id string, output any) []RecordEvent {
	result := safeToolValue(output)
	if name == "" && id == "" && result == "" {
		return nil
	}
	return []RecordEvent{{Type: "tool_result", Role: "tool", ToolName: name, ToolCallID: id, Result: result}}
}

func safeToolValue(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		if mediaData(text) {
			return "[非文本内容已省略]"
		}
		return text
	}
	cleaned := scrubMedia(value)
	if cleaned == nil {
		return ""
	}
	encoded, err := json.Marshal(cleaned)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func scrubMedia(value any) any {
	switch v := value.(type) {
	case string:
		if mediaData(v) {
			return "[非文本内容已省略]"
		}
		return v
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, scrubMedia(item))
		}
		return out
	case map[string]any:
		if kind := strings.ToLower(stringField(v, "type")); kind == "base64" || strings.Contains(kind, "image") || strings.Contains(kind, "audio") || strings.Contains(kind, "video") {
			return "[非文本内容已省略]"
		}
		out := make(map[string]any, len(v))
		for key, item := range v {
			switch strings.ToLower(key) {
			case "b64_json", "base64", "inline_data", "inlinedata", "file_data", "filedata", "image_url", "audio", "image", "video", "bytes":
				out[key] = "[非文本内容已省略]"
			default:
				out[key] = scrubMedia(item)
			}
		}
		return out
	default:
		return value
	}
}

func mediaData(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(lower, "data:image/") || strings.HasPrefix(lower, "data:audio/") || strings.HasPrefix(lower, "data:video/")
}

func stringField(object map[string]any, key string) string {
	value, _ := object[key].(string)
	return value
}

func firstValue(object map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := object[key]; ok {
			return value
		}
	}
	return nil
}

func fillToolNames(events []RecordEvent) {
	names := make(map[string]string)
	for _, event := range events {
		if event.Type == "tool_call" && event.ToolCallID != "" && event.ToolName != "" {
			names[event.ToolCallID] = event.ToolName
		}
	}
	for i := range events {
		if events[i].Type == "tool_result" && events[i].ToolName == "" {
			events[i].ToolName = names[events[i].ToolCallID]
		}
	}
}

type streamItem struct {
	item map[string]any
	text strings.Builder
	args strings.Builder
}

func streamRecordEvents(body []byte) []RecordEvent {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), CaptureLimit+1)
	var data []string
	items := make(map[int]*streamItem)
	blocks := make(map[int]*streamItem)
	chatCalls := make(map[int]*streamItem)
	var chatText strings.Builder
	var gemini []RecordEvent
	var final []RecordEvent
	process := func() {
		if len(data) == 0 {
			return
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		if payload == "[DONE]" {
			return
		}
		var event map[string]any
		if json.Unmarshal([]byte(payload), &event) != nil {
			return
		}
		kind := stringField(event, "type")
		switch kind {
		case "response.completed":
			if response, ok := event["response"].(map[string]any); ok {
				final = responseObjectRecordEvents(response)
			}
		case "response.output_item.added", "response.output_item.done":
			index := intField(event, "output_index")
			state := ensureStreamItem(items, index)
			if item, ok := event["item"].(map[string]any); ok {
				state.item = item
			}
		case "response.output_text.delta":
			ensureStreamItem(items, intField(event, "output_index")).text.WriteString(stringField(event, "delta"))
		case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
			ensureStreamItem(items, intField(event, "output_index")).args.WriteString(stringField(event, "delta"))
		case "content_block_start":
			state := ensureStreamItem(blocks, intField(event, "index"))
			if block, ok := event["content_block"].(map[string]any); ok {
				state.item = block
			}
		case "content_block_delta":
			state := ensureStreamItem(blocks, intField(event, "index"))
			if delta, ok := event["delta"].(map[string]any); ok {
				switch stringField(delta, "type") {
				case "text_delta":
					state.text.WriteString(stringField(delta, "text"))
				case "input_json_delta":
					state.args.WriteString(stringField(delta, "partial_json"))
				}
			}
		}
		if candidates, ok := event["candidates"].([]any); ok {
			gemini = append(gemini, responseObjectRecordEvents(map[string]any{"candidates": candidates})...)
		}
		if choices, ok := event["choices"].([]any); ok {
			for _, choice := range choices {
				object, ok := choice.(map[string]any)
				if !ok {
					continue
				}
				delta, ok := object["delta"].(map[string]any)
				if !ok {
					continue
				}
				chatText.WriteString(stringField(delta, "content"))
				if calls, ok := delta["tool_calls"].([]any); ok {
					for _, call := range calls {
						part, ok := call.(map[string]any)
						if !ok {
							continue
						}
						state := ensureStreamItem(chatCalls, intField(part, "index"))
						if state.item == nil {
							state.item = make(map[string]any)
						}
						if id := stringField(part, "id"); id != "" {
							state.item["id"] = id
						}
						if function, ok := part["function"].(map[string]any); ok {
							if name := stringField(function, "name"); name != "" {
								state.item["name"] = name
							}
							state.args.WriteString(stringField(function, "arguments"))
						}
					}
				}
			}
		}
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			process()
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	process()
	if len(final) > 0 {
		return final
	}
	if len(items) > 0 {
		var events []RecordEvent
		for _, index := range sortedStreamKeys(items) {
			state := items[index]
			if state.item != nil {
				if state.args.Len() > 0 && stringField(state.item, "arguments") == "" && stringField(state.item, "input") == "" {
					state.item["arguments"] = state.args.String()
				}
				complete := itemRecordEvents(state.item)
				if len(complete) > 0 {
					events = append(events, complete...)
					continue
				}
				if stringField(state.item, "type") == "function_call" {
					events = append(events, toolCallRecord(stringField(state.item, "name"), stringField(state.item, "call_id"), state.args.String())...)
					continue
				}
			}
			events = append(events, textRecord("assistant", state.text.String())...)
		}
		return events
	}
	if len(blocks) > 0 {
		var events []RecordEvent
		for _, index := range sortedStreamKeys(blocks) {
			state := blocks[index]
			if state.item == nil {
				continue
			}
			switch stringField(state.item, "type") {
			case "text":
				text := state.text.String()
				if text == "" {
					text = stringField(state.item, "text")
				}
				events = append(events, textRecord("assistant", text)...)
			case "tool_use":
				args := any(state.args.String())
				if state.args.Len() == 0 {
					args = state.item["input"]
				}
				events = append(events, toolCallRecord(stringField(state.item, "name"), stringField(state.item, "id"), args)...)
			}
		}
		return events
	}
	if chatText.Len() > 0 || len(chatCalls) > 0 {
		events := textRecord("assistant", chatText.String())
		for _, index := range sortedStreamKeys(chatCalls) {
			state := chatCalls[index]
			events = append(events, toolCallRecord(stringField(state.item, "name"), stringField(state.item, "id"), state.args.String())...)
		}
		return events
	}
	return gemini
}

func ensureStreamItem(items map[int]*streamItem, index int) *streamItem {
	if item := items[index]; item != nil {
		return item
	}
	item := &streamItem{}
	items[index] = item
	return item
}

func sortedStreamKeys(items map[int]*streamItem) []int {
	keys := make([]int, 0, len(items))
	for index := range items {
		keys = append(keys, index)
	}
	sort.Ints(keys)
	return keys
}

func intField(object map[string]any, key string) int {
	value, _ := object[key].(float64)
	return int(value)
}

// Keep the beginning of a structured record when the selected text exceeds
// the per-side limit. This avoids replacing a nearly full tool result with an
// empty download while preserving valid UTF-8 and JSON.
func limitRecordEvents(events []RecordEvent, limit int) ([]RecordEvent, bool) {
	result := make([]RecordEvent, 0, len(events))
	used := 0
	for _, event := range events {
		encoded, err := json.Marshal(event)
		if err != nil {
			continue
		}
		remaining := limit - used
		if len(encoded)+1 <= remaining {
			result = append(result, event)
			used += len(encoded) + 1
			continue
		}
		var field *string
		switch event.Type {
		case "message":
			field = &event.Content
		case "tool_call":
			field = &event.Arguments
		case "tool_result":
			field = &event.Result
		}
		if field == nil || *field == "" || remaining < 128 {
			return result, true
		}
		original := *field
		low, high := 0, len(original)
		best := ""
		for low <= high {
			middle := low + (high-low)/2
			prefix := original[:middle]
			for len(prefix) > 0 && !utf8.ValidString(prefix) {
				prefix = prefix[:len(prefix)-1]
			}
			*field = prefix
			candidate, _ := json.Marshal(event)
			if len(candidate)+1 <= remaining {
				best = prefix
				low = middle + 1
			} else {
				high = middle - 1
			}
		}
		if best != "" {
			*field = best
			result = append(result, event)
		}
		return result, true
	}
	return result, false
}
