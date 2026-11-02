// Package clientsync embeds the client configuration synchronizer for download.
package clientsync

import (
	_ "embed"
	"strings"
)

//go:embed Sync-Codex.ps1
var Script []byte

// ScriptForPortal embeds the configured public URL as a PowerShell literal.
func ScriptForPortal(portalURL string) []byte {
	value := strings.ReplaceAll(portalURL, "'", "''")
	return []byte(strings.Replace(string(Script), "[string]$PortalUrl = '', # PORTAL_URL_DEFAULT", "[string]$PortalUrl = '"+value+"', # PORTAL_URL_DEFAULT", 1))
}
