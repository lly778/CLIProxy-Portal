package agy

import (
	"bytes"
	"encoding/json"
)

// Tracker reads the small normalized response envelope while forwarding bytes.
// It does not retain prompts or complete responses and has a bounded line size.
type Tracker struct {
	line                []byte
	jsonBody            []byte
	Stream              bool
	Usage               Usage
	ResponseID, Failure string
	Effort              string
	Completed           bool
	ModelStarted        bool
	Invalid             bool
}

func (t *Tracker) Feed(p []byte) {
	if t.Invalid {
		return
	}
	if !t.Stream {
		if len(t.jsonBody)+len(p) > 16<<20 {
			t.Invalid = true
			return
		}
		t.jsonBody = append(t.jsonBody, p...)
		return
	}
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if len(t.line)+len(p) > 16<<20 {
				t.Invalid = true
				return
			}
			t.line = append(t.line, p...)
			return
		}
		if len(t.line)+i > 16<<20 {
			t.Invalid = true
			return
		}
		t.line = append(t.line, p[:i]...)
		if bytes.HasPrefix(t.line, []byte("data: ")) {
			var f struct {
				Type     string          `json:"type"`
				Response json.RawMessage `json:"response"`
				Delta    string          `json:"delta"`
				Item     struct {
					Type string `json:"type"`
				} `json:"item"`
			}
			if json.Unmarshal(t.line[6:], &f) == nil {
				// Connection handshakes, progress-only envelopes and SSE comments
				// do not mark the first actual model output or tool action.
				if f.Delta != "" || f.Type == "response.output_item.added" && f.Item.Type != "" && f.Item.Type != "message" && f.Item.Type != "reasoning" {
					t.ModelStarted = true
				}
				if f.Type == "response.completed" || f.Type == "response.failed" {
					t.ModelStarted = true
					t.read(f.Response)
				}
			}
		}
		t.line = t.line[:0]
		p = p[i+1:]
	}
}
func (t *Tracker) Finish() {
	if !t.Stream {
		t.read(t.jsonBody)
	}
	t.line = nil
	t.jsonBody = nil
}
func (t *Tracker) read(b []byte) {
	var r struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
		Usage *struct {
			Input  int64 `json:"input_tokens"`
			Output int64 `json:"output_tokens"`
			Total  int64 `json:"total_tokens"`
			In     struct {
				Cache int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			Out struct {
				Reasoning int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &r) != nil {
		return
	}
	t.ResponseID = r.ID
	t.Effort = r.Reasoning.Effort
	t.Completed = r.Status == "completed"
	if r.Status == "failed" || r.Error != nil {
		t.Failure = "agy_generation_failed"
		if r.Error != nil {
			if publicFailureMessage(r.Error.Code) != "" {
				t.Failure = r.Error.Code
			}
		}
	}
	if r.Usage != nil {
		v := r.Usage
		if v.Input >= 0 && v.Output >= 0 && v.Total >= 0 {
			t.Usage = Usage{Input: v.Input, Output: v.Output, Total: v.Total, Cache: max(0, v.In.Cache), Reasoning: max(0, v.Out.Reasoning), Known: true}
		}
	}
}
