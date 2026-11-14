package agy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

type remoteMCPBinding struct {
	Server, Name, Specification, Credential string
	ApprovalRequired                        bool
}

func (m *Manager) remoteCredentialIdentity(owner string, spec remoteMCPSpec) string {
	h := hmac.New(sha256.New, []byte(m.cfg.Secret))
	h.Write([]byte("remote-mcp-credential-v1\x00" + owner + "\x00" + spec.authorizationIdentity()))
	return hex.EncodeToString(h.Sum(nil))
}

func (m *Manager) prepareRemoteTools(ctx context.Context, owner string, r *request) error {
	if len(r.remoteSpecs) == 0 {
		return nil
	}
	tools := append([]tool{}, r.Tools...)
	seen := map[string]bool{}
	for _, t := range tools {
		seen[t.key()] = true
	}
	connections := map[string]*remoteMCPClient{}
	for _, spec := range r.remoteSpecs {
		client := newRemoteMCPClient(spec, m.remoteTransport)
		if err := client.initialize(ctx); err != nil {
			return fail(502, "remote_mcp_unavailable", "远程 MCP 初始化失败，未执行工具")
		}
		catalog, err := client.list(ctx)
		if err != nil {
			return fail(502, "remote_mcp_unavailable", "远程 MCP 目录读取失败，未执行工具")
		}
		connections[spec.Label] = client
		traceFrom(ctx).emit(Observation{Kind: "bridge", Category: "remote_mcp_catalog", State: "fetched", Tool: spec.Label, DefinitionHash: spec.identity()})
		wireTools := []any{}
		for _, entry := range catalog {
			if seen[entry.key()] || len(tools) >= 256 {
				return fail(400, "invalid_remote_mcp", "远程 MCP 与当前工具的身份冲突或目录超过上限")
			}
			seen[entry.key()] = true
			entry.remoteBinding = &remoteMCPBinding{Server: spec.Label, Name: entry.Name, Specification: spec.identity(), Credential: m.remoteCredentialIdentity(owner, spec), ApprovalRequired: spec.Approval != "never"}
			entry.RemoteIdentity = digest([]any{spec.identity(), m.remoteCredentialIdentity(owner, spec), entry.Parameters, entry.Description})
			tools = append(tools, entry)
			wireTools = append(wireTools, map[string]any{"name": entry.Name, "description": entry.Description, "input_schema": entry.Parameters})
		}
		r.remoteCatalogItems = append(r.remoteCatalogItems, map[string]any{"id": newID("mcpl_"), "type": "mcp_list_tools", "server_label": spec.Label, "tools": wireTools, "error": nil})
	}
	r.Tools, r.remoteConnections = tools, connections
	return nil
}

// Called only when committing an already validated request. Credentials from
// an earlier HTTP request are never inherited by a new request.
func commitRemoteConnections(s *session, r *request) {
	s.remoteConnections = r.remoteConnections
	s.remoteLease++
	r.remoteLease = s.remoteLease
}

func savedRemoteBindings(tools []tool) map[string]remoteMCPBinding {
	out := map[string]remoteMCPBinding{}
	for _, t := range tools {
		if t.remoteBinding != nil {
			out[t.key()] = *t.remoteBinding
		}
	}
	return out
}

func restoreRemoteBindings(tools []tool, bindings map[string]remoteMCPBinding) {
	for i := range tools {
		if binding, ok := bindings[tools[i].key()]; ok {
			copy := binding
			tools[i].remoteBinding = &copy
		}
	}
}
