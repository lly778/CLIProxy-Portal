package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const identityRewriteLimit = 2 << 20

var identityOpening = regexp.MustCompile(`(?i)^[\s\p{Zs}\x{FEFF}]*(?:you[\s\p{Zs}]+are|you['’]re|i[\s\p{Zs}]+am|i['’]m|(?:codex|this[\s\p{Zs}]+(?:assistant|agent|model)|the[\s\p{Zs}]+(?:assistant|agent|model))[\s\p{Zs}]+is)[\s\p{Zs}]+`)
var identityQualifier = regexp.MustCompile(`(?i)[\s\p{Zs}]+based[\s\p{Zs}]+on[\s\p{Zs}]+GPT-5`)
var identityParagraphEnd = regexp.MustCompile(`\r?\n[\t\r\p{Zs}]*\n`)

// Match the opening identity sentence rather than two fixed templates. Keep
// punctuation, everything outside that sentence, and other GPT model versions.
func withoutGPT5Identity(text string) (string, bool) {
	opening := identityOpening.FindStringIndex(text)
	if opening == nil {
		return text, false
	}
	end := len(text)
	if paragraph := identityParagraphEnd.FindStringIndex(text[opening[1]:]); paragraph != nil {
		end = opening[1] + paragraph[0]
	}
	for offset, char := range text[opening[1]:end] {
		if !strings.ContainsRune(".!?。！？", char) {
			continue
		}
		at := opening[1] + offset
		if char == '.' && at+1 < end {
			next, _ := utf8.DecodeRuneInString(text[at+1:])
			if unicode.IsLetter(next) || unicode.IsDigit(next) {
				continue // Dots within version numbers and names are not sentence ends.
			}
		}
		end = at
		break
	}
	matches := identityQualifier.FindAllStringIndex(text[:end], -1)
	changed := false
	for i := len(matches) - 1; i >= 0; i-- {
		match := matches[i]
		if match[1] < len(text) {
			next, size := utf8.DecodeRuneInString(text[match[1]:])
			if unicode.IsLetter(next) || unicode.IsDigit(next) || next == '_' || next == '-' {
				continue
			}
			if next == '.' && match[1]+size < len(text) {
				after, _ := utf8.DecodeRuneInString(text[match[1]+size:])
				if unicode.IsLetter(after) || unicode.IsDigit(after) {
					continue // GPT-5.1/GPT-5.6 are distinct identifiers.
				}
			}
		}
		before := text[:match[0]]
		if identityInsideQuotation(before) {
			continue // Do not change a quoted example inside the identity sentence.
		}
		text = before + text[match[1]:]
		changed = true
	}
	return text, changed
}

func identityInsideQuotation(text string) bool {
	var quote rune
	for at, char := range text {
		if char == '\'' {
			previous, _ := utf8.DecodeLastRuneInString(text[:at])
			next, _ := utf8.DecodeRuneInString(text[at+1:])
			if unicode.IsLetter(previous) && unicode.IsLetter(next) {
				continue // Contractions and possessives inside words are not quotes.
			}
			if quote == 0 && unicode.IsLetter(previous) && (unicode.IsSpace(next) || next == utf8.RuneError) {
				continue
			}
		}
		if quote != 0 {
			if char == quote {
				quote = 0
			}
			continue
		}
		switch char {
		case '"', '\'', '`':
			quote = char
		case '“':
			quote = '”'
		case '‘':
			quote = '’'
		}
	}
	return quote != 0
}

func rewriteIdentityText(raw json.RawMessage) (json.RawMessage, bool) {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return raw, false
	}
	text, changed := withoutGPT5Identity(text)
	if !changed {
		return raw, false
	}
	encoded, _ := json.Marshal(text)
	return encoded, true
}

func rewriteIdentityContent(raw json.RawMessage) (json.RawMessage, bool) {
	if value, changed := rewriteIdentityText(raw); changed {
		return value, true
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return raw, false
	}
	changed := false
	for i, part := range parts {
		var fields map[string]json.RawMessage
		if json.Unmarshal(part, &fields) != nil {
			continue
		}
		var kind string
		if value, exists := fields["type"]; exists {
			if json.Unmarshal(value, &kind) != nil || (kind != "text" && kind != "input_text") {
				continue
			}
		}
		if value, ok := rewriteIdentityText(fields["text"]); ok {
			fields["text"] = value
			parts[i], _ = json.Marshal(fields)
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	encoded, _ := json.Marshal(parts)
	return encoded, true
}

func rewriteIdentityMessages(raw json.RawMessage) (json.RawMessage, bool) {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return raw, false
	}
	changed := false
	for i, item := range items {
		var message map[string]json.RawMessage
		if json.Unmarshal(item, &message) != nil {
			continue
		}
		var role, kind string
		_ = json.Unmarshal(message["role"], &role)
		if role != "system" && role != "developer" {
			continue
		}
		if value, exists := message["type"]; exists {
			if json.Unmarshal(value, &kind) != nil || kind != "message" {
				continue
			}
		}
		if value, ok := rewriteIdentityContent(message["content"]); ok {
			message["content"] = value
			items[i], _ = json.Marshal(message)
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	encoded, _ := json.Marshal(items)
	return encoded, true
}

func rewriteIdentityPayload(body []byte, path string) ([]byte, string, bool) {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		return body, "", false
	}
	var model string
	_ = json.Unmarshal(payload["model"], &model)
	changed := false
	apply := func(field string, rewrite func(json.RawMessage) (json.RawMessage, bool)) {
		if value, ok := rewrite(payload[field]); ok {
			payload[field] = value
			changed = true
		}
	}
	switch {
	case strings.HasSuffix(path, "/responses"):
		apply("instructions", rewriteIdentityText)
		apply("input", rewriteIdentityMessages)
	case strings.HasSuffix(path, "/chat/completions"):
		apply("messages", rewriteIdentityMessages)
	case strings.HasSuffix(path, "/messages"):
		apply("system", rewriteIdentityContent)
	case strings.HasPrefix(path, "/v1beta/models/"):
		if model == "" {
			model = strings.TrimPrefix(path, "/v1beta/models/")
			model, _, _ = strings.Cut(model, ":")
		}
		var instruction map[string]json.RawMessage
		if json.Unmarshal(payload["systemInstruction"], &instruction) == nil && instruction != nil {
			if value, ok := rewriteIdentityContent(instruction["parts"]); ok {
				instruction["parts"] = value
				payload["systemInstruction"], _ = json.Marshal(instruction)
				changed = true
			}
		}
	}
	if !changed {
		return body, model, false
	}
	// RawMessage preserves unknown fields and numeric precision (including
	// opaque tool schemas and large integer extension values).
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body, model, false
	}
	return encoded, model, true
}

func (g *Gateway) applyCodexIdentityCompatibility(r *http.Request) {
	if r.Body == nil || r.Body == http.NoBody || !isDialoguePath(r.Method, r.URL.Path) {
		return
	}
	enabled, err := g.store.EnabledCodexIdentityChannels(r.Context())
	if err != nil || len(enabled) == 0 {
		return
	}
	if r.ContentLength > identityRewriteLimit {
		return
	}
	encoding := r.Header.Get("Content-Encoding")
	for _, value := range strings.Split(encoding, ",") {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "", "identity", "gzip", "x-gzip", "deflate":
		default:
			return
		}
	}
	originalBody := r.Body
	wire, readErr := io.ReadAll(io.LimitReader(originalBody, identityRewriteLimit+1))
	// Replay every byte if no change is applicable, including oversized and
	// malformed payloads. The original reader still supplies unread bytes.
	r.Body = &teeReadCloser{Reader: io.MultiReader(bytes.NewReader(wire), originalBody), Closer: originalBody}
	if readErr != nil || len(wire) > identityRewriteLimit {
		return
	}
	body, truncated, err := decodeCaptureBody(wire, encoding)
	if err != nil || truncated || len(body) > identityRewriteLimit {
		return
	}
	encoded, model, changed := rewriteIdentityPayload(body, r.URL.Path)
	if !changed {
		return
	}
	allowed, err := g.keys.ShouldRemoveGPT5Identity(r.Context(), model, enabled)
	if err != nil {
		g.logger.Warn("Codex identity compatibility routing unavailable", "path", r.URL.Path)
		return
	}
	if !allowed {
		return
	}
	_ = originalBody.Close()
	r.Body = io.NopCloser(bytes.NewReader(encoded))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(encoded)), nil }
	r.ContentLength = int64(len(encoded))
	r.TransferEncoding = nil
	r.Header.Set("Content-Length", strconv.Itoa(len(encoded)))
	for _, name := range []string{"Content-Encoding", "Content-MD5", "Digest", "Content-Digest", "Transfer-Encoding"} {
		r.Header.Del(name)
	}
	g.logger.Info("Codex GPT-5 identity description removed", "model", model, "path", r.URL.Path)
}
