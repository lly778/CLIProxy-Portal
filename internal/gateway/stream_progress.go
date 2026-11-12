package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// Inspect only one bounded SSE line until the first model output. Comments and
// empty protocol handshakes keep the network alive but do not shorten the model
// wait stage. Bytes are forwarded untouched; no response or usage is retained.
type modelStreamBody struct {
	io.ReadCloser
	measurement *requestMeasurement
	line        []byte
	started     bool
}

func (b *modelStreamBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if !b.started && n > 0 {
		b.feed(p[:n])
	}
	return n, err
}

func (b *modelStreamBody) feed(p []byte) {
	for len(p) > 0 && !b.started {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			i = len(p)
		}
		if len(b.line)+i > 64<<10 {
			// A large actual model/image frame need not be buffered to time it.
			b.markStarted()
			return
		}
		b.line = append(b.line, p[:i]...)
		if i == len(p) {
			return
		}
		if bytes.HasPrefix(b.line, []byte("data:")) && modelStreamData(bytes.TrimSpace(b.line[5:])) {
			b.markStarted()
			return
		}
		b.line = b.line[:0]
		p = p[i+1:]
	}
}

func (b *modelStreamBody) markStarted() {
	b.started, b.line = true, nil
	b.measurement.modelResponseStarted(true)
}

func modelStreamData(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		return true
	}
	var v map[string]any
	if json.Unmarshal(data, &v) != nil {
		return true // Preserve timing for unknown providers' non-JSON SSE data.
	}
	kind, _ := v["type"].(string)
	switch kind {
	case "ping", "keepalive", "message_start", "response.created", "response.in_progress", "response.queued", "response.content_part.added", "response.reasoning_summary_part.added":
		return false
	case "response.output_item.added":
		item, _ := v["item"].(map[string]any)
		t, _ := item["type"].(string)
		return t != "" && t != "message" && t != "reasoning"
	case "content_block_start":
		block, _ := v["content_block"].(map[string]any)
		return block["type"] == "tool_use" || hasModelValue(block, "text", "thinking")
	}
	if strings.HasPrefix(kind, "response.") {
		return kind == "response.completed" || kind == "response.failed" || hasModelValue(v, "delta", "text", "arguments")
	}
	if choices, ok := v["choices"].([]any); ok {
		for _, raw := range choices {
			choice, _ := raw.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if hasModelValue(delta, "content", "reasoning", "reasoning_content", "tool_calls", "function_call") || choice["finish_reason"] != nil {
				return true
			}
		}
		return false
	}
	if kind == "content_block_delta" {
		delta, _ := v["delta"].(map[string]any)
		return hasModelValue(delta, "text", "thinking", "partial_json", "signature")
	}
	if candidates, ok := v["candidates"].([]any); ok {
		for _, raw := range candidates {
			candidate, _ := raw.(map[string]any)
			content, _ := candidate["content"].(map[string]any)
			if hasModelValue(content, "parts") || candidate["finishReason"] != nil {
				return true
			}
		}
		return false
	}
	return true
}

func hasModelValue(v map[string]any, keys ...string) bool {
	for _, key := range keys {
		switch x := v[key].(type) {
		case string:
			if x != "" {
				return true
			}
		case []any:
			if len(x) > 0 {
				return true
			}
		case map[string]any:
			if len(x) > 0 {
				return true
			}
		}
	}
	return false
}
