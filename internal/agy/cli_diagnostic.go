package agy

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"
)

const maxCLIDiagnosticBytes = 64 << 10
const diagnosticTruncation = "\n[AGY CLI diagnostic truncated: exceeded 65536 bytes]\n"

var diagnosticHeader = regexp.MustCompile(`(?im)^([ \t]*(?:authorization|proxy-authorization|cookie|set-cookie|x-api-key|x-goog-api-key)[ \t]*:[ \t]*)[^\r\n]*`)
var diagnosticBearer = regexp.MustCompile(`(?i)\b(Bearer|Basic)[ \t]+[A-Za-z0-9._~+/=-]+`)
var diagnosticAssignment = regexp.MustCompile(`(?i)((?:["']|\\")?(?:access[_-]?token|refresh[_-]?token|id[_-]?token|api[_-]?key|client[_-]?secret|password|passwd|authorization|cookie|set-cookie|x-api-key|x-goog-api-key|token|(?:x-amz|x-goog)-(?:credential|signature|security-token))(?:["']|\\")?\s*[:=]\s*)(?:\[redacted\]|"(?:\\.|[^"\\])*"|'[^']*'|[^\s,;&}\]]+)`)
var diagnosticQuerySecret = regexp.MustCompile(`(?i)([?&#](?:code|sig|signature|credential|secret|password|auth|key|token)\s*=)[^&\s"<>]*`)
var diagnosticURLCredentials = regexp.MustCompile(`(?i)(https?://)[^/\s:@]+:[^/\s@]+@`)
var diagnosticOpaqueToken = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{8,}|AIza[A-Za-z0-9_-]{16,}|ya29\.[A-Za-z0-9._-]+|token_[a-f0-9]{16,}|eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+)\b`)

// Preserve the CLI's diagnostic text, not its credentials. These values are
// excluded from client error messages and generic error serialization.
func sanitizeCLIDiagnostic(text string) string {
	text = strings.ToValidUTF8(text, "\ufffd")
	text = diagnosticHeader.ReplaceAllString(text, "${1}[redacted]")
	text = diagnosticBearer.ReplaceAllString(text, "${1} [redacted]")
	text = diagnosticURLCredentials.ReplaceAllString(text, "${1}[redacted]@")
	text = diagnosticAssignment.ReplaceAllString(text, "${1}[redacted]")
	text = diagnosticQuerySecret.ReplaceAllString(text, "${1}[redacted]")
	text = diagnosticOpaqueToken.ReplaceAllString(text, "[redacted]")
	return limitCLIDiagnostic(text)
}

func limitCLIDiagnostic(text string) string {
	if len(text) <= maxCLIDiagnosticBytes {
		return text
	}
	available := maxCLIDiagnosticBytes - len(diagnosticTruncation)
	head, tail := available*3/4, len(text)-(available-available*3/4)
	for head > 0 && !utf8.RuneStart(text[head]) {
		head--
	}
	for tail < len(text) && !utf8.RuneStart(text[tail]) {
		tail++
	}
	return text[:head] + diagnosticTruncation + text[tail:]
}

func withCLIDiagnostic(e *Error, text string) *Error {
	copy := *e
	copy.Diagnostic = sanitizeCLIDiagnostic(text)
	return &copy
}

func mergeCLIDiagnostics(chosen, previous, next error) error {
	var target, prior, current *Error
	if !errors.As(chosen, &target) {
		return chosen
	}
	var parts []string
	if errors.As(previous, &prior) && prior.Diagnostic != "" {
		parts = append(parts, prior.Diagnostic)
	}
	if errors.As(next, &current) && current.Diagnostic != "" {
		parts = append(parts, current.Diagnostic)
	}
	diagnostic := limitCLIDiagnostic(strings.Join(parts, "\n"))
	if diagnostic == "" || diagnostic == target.Diagnostic {
		return chosen
	}
	copy := *target
	copy.Diagnostic = diagnostic
	return &copy
}

// Diagnostics travel only over the authenticated worker-to-portal response.
// Trailers keep the public JSON/SSE body identical to the previous protocol.
const cliDiagnosticTrailer = "X-Portal-AGY-CLI-Diagnostic"

func writeCLIDiagnostic(w http.ResponseWriter, err error) {
	var e *Error
	if errors.As(err, &e) && e.Diagnostic != "" {
		w.Header().Set(http.TrailerPrefix+cliDiagnosticTrailer, base64.StdEncoding.EncodeToString([]byte(sanitizeCLIDiagnostic(e.Diagnostic))))
	}
}

func writeWorkerError(w http.ResponseWriter, err error) {
	writeCLIDiagnostic(w, err)
	WriteError(w, err)
}

// ResponseFailureSummary is read after the worker body reaches EOF. The
// gateway never forwards the private trailer to the external client.
func ResponseFailureSummary(code string, resp *http.Response) string {
	encoded := resp.Trailer.Get(cliDiagnosticTrailer)
	if len(encoded) > base64.StdEncoding.EncodedLen(maxCLIDiagnosticBytes) {
		return FailureSummary(code)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) == 0 {
		return FailureSummary(code)
	}
	b, _ := json.Marshal(map[string]string{
		"code":           code,
		"message":        publicFailureMessage(code),
		"cli_diagnostic": sanitizeCLIDiagnostic(string(raw)),
	})
	return string(b)
}
