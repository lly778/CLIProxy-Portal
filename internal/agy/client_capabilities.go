package agy

import (
	"sort"
	"strings"
)

// These labels describe provided executors, never grant access to a hosted
// service. Qualified identities and original schemas remain unchanged.
func clientCapabilities(tools []tool) map[string][]string {
	out := map[string][]string{}
	for _, t := range tools {
		if !t.Deferred && isClientSearch(t) {
			out["tool_discovery"] = append(out["tool_discovery"], t.key())
			continue
		}
		if t.Deferred || t.Type != "function" && t.Type != "custom" && !isNativeClientTool(t.Type) {
			continue
		}
		name := strings.ToLower(t.Name)
		if strings.HasPrefix(name, "mcp__") {
			if at := strings.LastIndex(name, "__"); at >= 0 {
				name = name[at+2:]
			}
		}
		kind := ""
		switch {
		case name == "file_search", name == "search_files", name == "document_search", name == "search_documents", name == "retrieve_documents":
			kind = "file_search"
		case name == "code_interpreter", name == "execute_python", name == "run_python", name == "python", name == "run_code":
			kind = "code_execution"
		case t.Type == "shell" || t.Type == "local_shell" || name == "exec_command" || name == "shell_command" || name == "execute_command":
			kind = "client_shell"
		}
		if kind != "" {
			out[kind] = append(out[kind], t.key())
		}
	}
	return out
}

func clientCapabilityText(tools []tool) string {
	caps := clientCapabilities(tools)
	kinds := []string{}
	for kind := range caps {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	lines := []string{}
	for _, kind := range kinds {
		names := caps[kind]
		sort.Strings(names)
		lines = append(lines, kind+" is available through these client tools: "+strings.Join(names, ", ")+". Use their declared argument schemas and return their real results.")
	}
	search := false
	for _, t := range tools {
		search = search || !t.Deferred && isClientSearch(t)
	}
	if search {
		lines = append(lines, "If a requested client capability is not currently listed, use the provided client tool-discovery tool to find it. Tool discovery searches tool definitions, not the web. Only call a discovered tool after its definition has been returned by the client.")
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n\n[Client-provided capabilities]\n" + strings.Join(lines, "\n") + "\nUse official Google tools for web search, public page reads and image generation. Other client tools retain their declared executors. Do not invent tools or claim success without the actual result.\n"
}
