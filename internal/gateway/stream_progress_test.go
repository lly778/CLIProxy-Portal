package gateway

import (
	"testing"
	"time"

	"cliproxy-portal/internal/store"
)

func TestModelStreamStartDistinguishesProtocolHandshakesAndActualOutput(t *testing.T) {
	for _, tc := range []struct {
		data   string
		starts bool
	}{
		{`{"type":"response.created"}`, false},
		{`{"type":"response.in_progress"}`, false},
		{`{"type":"response.output_item.added","item":{"type":"message"}}`, false},
		{`{"type":"response.output_item.added","item":{"type":"function_call"}}`, true},
		{`{"type":"response.reasoning_summary_text.delta","delta":"thought"}`, true},
		{`{"choices":[{"delta":{"role":"assistant","content":""}}]}`, false},
		{`{"choices":[{"delta":{"reasoning_content":"thought"}}]}`, true},
		{`{"choices":[{"delta":{"tool_calls":[{"index":0}]}}]}`, true},
		{`{"type":"message_start"}`, false},
		{`{"type":"content_block_start","content_block":{"type":"text","text":""}}`, false},
		{`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"thought"}}`, true},
		{`{"candidates":[{"content":{"parts":[{"text":"hello"}]}}]}`, true},
		{`{"type":"response.failed"}`, true},
		{`[DONE]`, true},
	} {
		if got := modelStreamData([]byte(tc.data)); got != tc.starts {
			t.Errorf("data=%s got=%v want=%v", tc.data, got, tc.starts)
		}
	}
}

func TestModelStreamStartSurvivesSplitFramesAndSkipsHeartbeat(t *testing.T) {
	m := &requestMeasurement{row: store.GatewayRequestTiming{StartedAt: time.Now()}}
	m.modelResponseStarted(false)
	b := &modelStreamBody{measurement: m}
	for _, c := range []byte(": keepalive\n\nevent: response.created\ndata: {\"type\":\"response.created\"}\n\n") {
		b.feed([]byte{c})
	}
	if b.started || m.row.ResponseStartedMS != nil {
		t.Fatal("heartbeat/handshake was counted as model output")
	}
	for _, c := range []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n") {
		b.feed([]byte{c})
	}
	if !b.started || m.row.ResponseStartedMS == nil || b.line != nil {
		t.Fatal("actual output was not detected or parser retained its buffer")
	}
}
