package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

type dialogueMessage struct {
	Role string
	Text string
}

// requestDialogue extracts only user and assistant message text. System and
// developer instructions, tools, tool results, images and other payload fields
// never enter the persisted dialogue capture.
func requestDialogue(body []byte) string {
	return formatDialogue(requestDialogueMessages(body))
}

func requestDialogueMessages(body []byte) []dialogueMessage {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return nil
	}
	var messages []dialogueMessage
	if input, ok := payload["input"]; ok {
		messages = append(messages, inputMessages(input)...)
	} else if raw, ok := payload["messages"]; ok {
		messages = append(messages, inputMessages(raw)...)
	} else if raw, ok := payload["contents"]; ok {
		messages = append(messages, geminiMessages(raw)...)
	}
	return messages
}

func geminiMessages(value any) []dialogueMessage {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	var result []dialogueMessage
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok || (object["role"] != "user" && object["role"] != "model" && object["role"] != nil && object["role"] != "") {
			continue
		}
		label := "用户"
		if object["role"] == "model" {
			label = "助手（请求中的历史对话）"
		}
		if text := geminiPartsText(object["parts"]); strings.TrimSpace(text) != "" {
			result = append(result, dialogueMessage{Role: label, Text: text})
		}
	}
	return result
}

func geminiPartsText(value any) string {
	parts, ok := value.([]any)
	if !ok {
		return ""
	}
	var texts []string
	for _, part := range parts {
		object, ok := part.(map[string]any)
		if !ok || object["functionCall"] != nil || object["functionResponse"] != nil || object["inlineData"] != nil || object["fileData"] != nil || object["executableCode"] != nil || object["codeExecutionResult"] != nil || object["thought"] == true {
			continue
		}
		if text, ok := object["text"].(string); ok && strings.TrimSpace(text) != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}

func inputMessages(input any) []dialogueMessage {
	if text, ok := input.(string); ok {
		if strings.TrimSpace(text) != "" {
			return []dialogueMessage{{Role: "用户", Text: text}}
		}
		return nil
	}
	items, ok := input.([]any)
	if !ok {
		return nil
	}
	result := make([]dialogueMessage, 0, len(items))
	for _, item := range items {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role := message["role"]
		if role != "user" && role != "assistant" {
			continue
		}
		kind, _ := message["type"].(string)
		if kind != "" && kind != "message" {
			continue
		}
		text := contentText(message["content"])
		if strings.TrimSpace(text) == "" {
			continue
		}
		label := "用户"
		if role == "assistant" {
			label = "助手（请求中的历史对话）"
		}
		result = append(result, dialogueMessage{Role: label, Text: text})
	}
	return result
}

func contentText(content any) string {
	if text, ok := content.(string); ok {
		return text
	}
	parts, ok := content.([]any)
	if !ok {
		return ""
	}
	var out []string
	for _, part := range parts {
		object, ok := part.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := object["type"].(string)
		if kind != "text" && kind != "input_text" && kind != "output_text" {
			continue
		}
		if text, ok := object["text"].(string); ok && strings.TrimSpace(text) != "" {
			out = append(out, text)
		}
	}
	return strings.Join(out, "\n")
}

func responseDialogue(body []byte, contentType string) string {
	return formatDialogue(responseDialogueMessages(body, contentType))
}

func responseDialogueMessages(body []byte, contentType string) []dialogueMessage {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return streamDialogueMessages(body)
	}
	return jsonResponseDialogueMessages(body)
}

func jsonResponseDialogue(body []byte) string {
	return formatDialogue(jsonResponseDialogueMessages(body))
}

func jsonResponseDialogueMessages(body []byte) []dialogueMessage {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		var list []map[string]any
		if json.Unmarshal(body, &list) != nil {
			return nil
		}
		var messages []dialogueMessage
		for _, item := range list {
			messages = append(messages, responseObjectMessages(item)...)
		}
		return messages
	}
	return responseObjectMessages(payload)
}

func responseObjectDialogue(payload map[string]any) string {
	return formatDialogue(responseObjectMessages(payload))
}

func responseObjectMessages(payload map[string]any) []dialogueMessage {
	var messages []dialogueMessage
	if output, ok := payload["output"].([]any); ok {
		for _, item := range output {
			object, ok := item.(map[string]any)
			if !ok || object["type"] != "message" || object["role"] != "assistant" {
				continue
			}
			if text := contentText(object["content"]); strings.TrimSpace(text) != "" {
				messages = append(messages, dialogueMessage{Role: "助手", Text: text})
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
			if !ok || message["role"] != "assistant" {
				continue
			}
			if text := contentText(message["content"]); strings.TrimSpace(text) != "" {
				messages = append(messages, dialogueMessage{Role: "助手", Text: text})
			}
		}
	}
	if payload["type"] == "message" && payload["role"] == "assistant" {
		if text := contentText(payload["content"]); strings.TrimSpace(text) != "" {
			messages = append(messages, dialogueMessage{Role: "助手", Text: text})
		}
	}
	if candidates, ok := payload["candidates"].([]any); ok {
		for _, candidate := range candidates {
			object, ok := candidate.(map[string]any)
			if !ok {
				continue
			}
			content, ok := object["content"].(map[string]any)
			if !ok {
				continue
			}
			if text := geminiPartsText(content["parts"]); strings.TrimSpace(text) != "" {
				messages = append(messages, dialogueMessage{Role: "助手", Text: text})
			}
		}
	}
	return messages
}

func streamDialogue(body []byte) string {
	return formatDialogue(streamDialogueMessages(body))
}

func streamDialogueMessages(body []byte) []dialogueMessage {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), CaptureLimit+1)
	var output strings.Builder
	var data []string
	var finalResponse []dialogueMessage
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
		if event["type"] == "response.output_text.delta" {
			if delta, ok := event["delta"].(string); ok {
				output.WriteString(delta)
			}
			return
		}
		if event["type"] == "content_block_delta" {
			if delta, ok := event["delta"].(map[string]any); ok && delta["type"] == "text_delta" {
				if text, ok := delta["text"].(string); ok {
					output.WriteString(text)
				}
			}
			return
		}
		if candidates, ok := event["candidates"].([]any); ok {
			for _, candidate := range candidates {
				object, ok := candidate.(map[string]any)
				if !ok {
					continue
				}
				content, ok := object["content"].(map[string]any)
				if ok {
					output.WriteString(geminiPartsText(content["parts"]))
				}
			}
			return
		}
		if choices, ok := event["choices"].([]any); ok {
			for _, choice := range choices {
				object, ok := choice.(map[string]any)
				if !ok {
					continue
				}
				if delta, ok := object["delta"].(map[string]any); ok {
					if text, ok := delta["content"].(string); ok {
						output.WriteString(text)
					}
				}
			}
			return
		}
		if event["type"] == "response.completed" {
			if response, ok := event["response"].(map[string]any); ok {
				finalResponse = responseObjectMessages(response)
			}
		}
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			process()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	process()
	if strings.TrimSpace(output.String()) != "" {
		return []dialogueMessage{{Role: "助手", Text: output.String()}}
	}
	return finalResponse
}

func formatDialogue(messages []dialogueMessage) string {
	var out strings.Builder
	for _, message := range messages {
		if out.Len() > 0 {
			out.WriteString("\n\n")
		}
		out.WriteString(message.Role)
		out.WriteString("：\n")
		out.WriteString(message.Text)
	}
	return out.String()
}
