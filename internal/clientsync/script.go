// Package clientsync embeds the client configuration synchronizer for download.
package clientsync

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"strings"
)

//go:embed Sync-Codex.ps1
var Script []byte

//go:embed Sync-Codex.bat
var batchLauncher string

// ScriptForPortal embeds the configured public URL as a PowerShell literal.
func ScriptForPortal(portalURL string) []byte {
	value := strings.ReplaceAll(portalURL, "'", "''")
	return []byte(strings.Replace(string(Script), "[string]$PortalUrl = '', # PORTAL_URL_DEFAULT", "[string]$PortalUrl = '"+value+"', # PORTAL_URL_DEFAULT", 1))
}

// BatchForPortal is a single ASCII/CRLF download. Its embedded PowerShell
// payload has a UTF-8 BOM so Windows PowerShell 5.1 preserves Chinese prompts.
// The launcher extracts only its bundled code, forwards arguments via -File,
// and removes the temporary script after the synchronizer exits.
func BatchForPortal(portalURL string) []byte {
	bom := []byte{0xef, 0xbb, 0xbf}
	script := ScriptForPortal(portalURL)
	for bytes.HasPrefix(script, bom) {
		script = bytes.TrimPrefix(script, bom)
	}
	payload := append(append([]byte(nil), bom...), script...)
	encoded := base64.StdEncoding.EncodeToString(payload)
	header := strings.ReplaceAll(batchLauncher, "\r\n", "\n")
	var result strings.Builder
	result.WriteString(strings.ReplaceAll(strings.TrimRight(header, "\n"), "\n", "\r\n"))
	result.WriteString("\r\n")
	for len(encoded) > 0 {
		n := min(len(encoded), 76)
		result.WriteString(encoded[:n])
		result.WriteString("\r\n")
		encoded = encoded[n:]
	}
	return []byte(result.String())
}
