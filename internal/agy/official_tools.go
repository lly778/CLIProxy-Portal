package agy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const officialToolsVersion = 2

const officialAgentDefinition = `---
name: portal-bridge
description: Client tool bridge with official Google web search and image generation
tools: [search_web, read_url_content, generate_image]
excludeDefaultComponents: true
mainAgent: true
subagent: false
---
Use only the official search_web tool for Google web search, read_url_content for public web pages, and generate_image for image generation. Client search and image-generation tools are excluded. Cite real search sources in your answer. Do not repeatedly search for the same fact once useful sources have been returned; report uncertainty instead.
All commands, local file operations, and computer actions belong to the remote client. Use only the portal MCP tools for those operations. Do not read server files. Generate images without ImagePaths or other local reference files. ImageName must be a short filename stem using only letters, digits, underscores or hyphens. The portal returns generated images to the client; do not expose server paths in your answer. Follow the supplied task instructions and this request's tool policy.
`

func isOfficialTool(name string) bool {
	return name == "search_web" || name == "read_url_content" || name == "generate_image"
}

func (p toolPolicy) permitsOfficial(name string) bool {
	return isOfficialTool(name) && p.Mode != "none" && (p.Names == nil || p.Names["google."+name])
}

// The official CLI runs this hook before executing a built-in tool. It checks
// the live owning session and policy, including internal summary workers.
func officialHooks() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	command := "'" + strings.ReplaceAll(exe, "'", "'\"'\"'") + "' --official-tool-policy"
	if runtime.GOOS == "windows" {
		command = "\"" + exe + "\" --official-tool-policy"
	}
	hooks := map[string]any{"portal-google-tools": map[string]any{"PreToolUse": []any{map[string]any{
		"matcher": "^(search_web|read_url_content|generate_image)$",
		"hooks":   []any{map[string]any{"type": "command", "command": command, "timeout": 5}},
	}}}}
	b, err := json.Marshal(hooks)
	return string(b), err
}

// RunOfficialToolPolicy is a small, fail-closed hook process, not a model tool.
// It never prints credentials or the prompt, and never executes a command.
func RunOfficialToolPolicy(input io.Reader, output io.Writer) error {
	deny := map[string]string{"decision": "deny", "reason": "Official tool policy could not validate this session."}
	body, err := io.ReadAll(io.LimitReader(input, 64<<10+1))
	if err != nil || len(body) > 64<<10 || !json.Valid(body) {
		return json.NewEncoder(output).Encode(deny)
	}
	secretPath := os.Getenv("PORTAL_AGY_SECRET_FILE")
	if secretPath == "" {
		secretPath = "/run/secrets/portal_agy_secret"
	}
	secret, err := os.ReadFile(secretPath)
	if err != nil {
		return json.NewEncoder(output).Encode(deny)
	}
	ctx, done := context.WithTimeout(context.Background(), 3*time.Second)
	defer done()
	r, err := http.NewRequestWithContext(ctx, "POST", "http://"+embeddedAddress+"/internal/official-tool-policy", bytes.NewReader(body))
	if err != nil {
		return json.NewEncoder(output).Encode(deny)
	}
	r.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(secret)))
	r.Header.Set("Content-Type", "application/json")
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	resp, err := client.Do(r)
	if err != nil {
		return json.NewEncoder(output).Encode(deny)
	}
	defer resp.Body.Close()
	var decision map[string]string
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 2048)).Decode(&decision) != nil || decision["decision"] != "allow" {
		return json.NewEncoder(output).Encode(deny)
	}
	return json.NewEncoder(output).Encode(decision)
}

type officialPolicyRequest struct {
	SessionID string `json:"conversationId"`
	Call      struct {
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
	} `json:"toolCall"`
}

func validOfficialArguments(name string, args map[string]any) bool {
	text := func(key string, limit int) bool {
		s, ok := args[key].(string)
		return ok && strings.TrimSpace(s) != "" && len(s) <= limit
	}
	switch name {
	case "search_web":
		return text("query", 8192)
	case "read_url_content":
		if !text("Url", 8192) {
			return false
		}
		u, err := url.Parse(args["Url"].(string))
		if err != nil || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
			return false
		}
		host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
		if host == "localhost" || strings.HasSuffix(host, ".localhost") || !strings.Contains(host, ".") {
			return false
		}
		if ip := net.ParseIP(host); ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast()) {
			return false
		}
		return true
	case "generate_image":
		if !text("Prompt", 16384) || !text("ImageName", 80) || !validImageStem(stringField(args, "ImageName")) {
			return false
		}
		for key, value := range args {
			switch key {
			case "Prompt", "ImageName", "AspectRatio":
			case "ImagePaths":
				paths, ok := value.([]any)
				if !ok || len(paths) != 0 {
					return false
				}
			default:
				return false
			}
		}
		return true
	}
	return false
}

func validImageStem(name string) bool {
	if name == "" || len(name) > 80 {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func (m *Manager) officialPolicyHTTP(w http.ResponseWriter, r *http.Request) {
	decision := map[string]string{"decision": "deny", "reason": "This official tool is not allowed for the current request. Local references must use client tools."}
	var p officialPolicyRequest
	if json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&p) == nil && validOfficialArguments(p.Call.Name, p.Call.Args) {
		m.mu.Lock()
		for _, s := range m.sessions {
			if s.ID != p.SessionID {
				continue
			}
			s.mu.Lock()
			allowed := nativePromptActive(s) && !s.internal && s.policy.permitsOfficial(p.Call.Name)
			if s.policy.Serial {
				allowed = allowed && len(s.officialActive) <= 1
				for _, call := range s.pending {
					select {
					case <-call.Ready:
					default:
						allowed = false
					}
				}
			}
			if allowed {
				s.policy.Used = true
				decision = map[string]string{"decision": "allow"}
			}
			s.mu.Unlock()
			break
		}
		m.mu.Unlock()
	}
	_ = json.NewEncoder(w).Encode(decision)
}

type officialToolEvent struct {
	ID, Name, Status string
	Args             map[string]any
}

func (m *Manager) sendOfficialEvent(s *session, event officialToolEvent, epochs ...uint64) {
	s.mu.Lock()
	epoch := s.epoch
	if len(epochs) > 0 && epochs[0] != epoch {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	select {
	case s.events <- agentEvent{Epoch: epoch, Official: &event}:
	case <-s.life.Done():
	default:
		s.cancel()
	}
}

type officialResponseCall struct {
	Index   int
	Started time.Time
	Done    bool
}

func (o *response) official(home string, s *session, event officialToolEvent) error {
	if event.ID == "" || !isOfficialTool(event.Name) {
		return nil
	}
	if o.officialCalls == nil {
		o.officialCalls = map[string]*officialResponseCall{}
	}
	o.trace.emit(Observation{ID: digest([]string{s.owner, s.ID, event.ID, event.Status}), SessionID: s.ID, Kind: "official_tool", State: event.Status, Category: "google_official", Tool: event.Name, CallID: event.ID})
	call := o.officialCalls[event.ID]
	if call == nil {
		call = &officialResponseCall{Index: len(o.output), Started: time.Now()}
		o.officialCalls[event.ID] = call
		item := map[string]any{"id": newID("ws_"), "type": "web_search_call", "status": "in_progress"}
		if event.Name == "generate_image" {
			item["id"], item["type"], item["result"] = newID("ig_"), "image_generation_call", ""
		} else if event.Name == "search_web" {
			item["action"] = map[string]any{"type": "search", "query": stringField(event.Args, "query")}
		} else {
			item["action"] = map[string]any{"type": "open_page", "url": stringField(event.Args, "Url")}
		}
		o.output = append(o.output, item)
		if err := o.event("response.output_item.added", map[string]any{"output_index": call.Index, "item": item}); err != nil {
			return err
		}
	}
	if call.Done {
		return nil
	}
	item := o.output[call.Index]
	if event.Status != "completed" && event.Status != "failed" {
		return o.event("response."+item["type"].(string)+".in_progress", map[string]any{"item_id": item["id"], "output_index": call.Index})
	}
	call.Done = true
	item["status"] = event.Status
	if event.Name == "generate_image" && event.Status == "completed" {
		data, _, err := officialGeneratedImage(home, s.ID, stringField(event.Args, "ImageName"), call.Started)
		if err != nil {
			return err
		}
		data, size, err := imagePNG(data)
		if err != nil {
			return err
		}
		preview, err := imagePreview(data)
		if err != nil {
			return err
		}
		imageURL, err := storePublicImage(home, o.imageBase, o.imageSecret, s.owner, s.ID, data)
		if err != nil {
			return err
		}
		if imageURL != "" {
			item["result_url"] = imageURL
		}
		o.imageRefs = append(o.imageRefs, preview)
		o.imageLinks = append(o.imageLinks, imageURL)
		o.imageBytes += len(data) * 4 / 3
		if o.imageBytes+o.bytes > 12<<20 {
			return fail(502, "output_limit", "AGY 图片与文字输出超过限制")
		}
		item["result"] = base64.StdEncoding.EncodeToString(data)
		item["output_format"], item["size"], item["action"] = "png", size, "generate"
		if err := o.event("response.image_generation_call.completed", map[string]any{"item_id": item["id"], "output_index": call.Index}); err != nil {
			return err
		}
	}
	return o.event("response.output_item.done", map[string]any{"output_index": call.Index, "item": item})
}

// OpenRoot prevents symlinks or path traversal from escaping the owning native
// session. Only newly generated top-level images with the exact tool stem are
// returned; arbitrary server paths and another user's artifacts are never read.
func officialGeneratedImage(home, id, stem string, started time.Time) ([]byte, string, error) {
	bad := fail(502, "image_result_unavailable", "Google 生图已结束，但未获得当前会话可回传的图片")
	if !safeNativeSessionID(id) || !validImageStem(stem) {
		return nil, "", bad
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return nil, "", bad
	}
	defer root.Close()
	brain, err := root.OpenRoot(filepath.Join("agy", "brain", id))
	if err != nil {
		return nil, "", bad
	}
	defer brain.Close()
	dir, err := brain.Open(".")
	if err != nil {
		return nil, "", bad
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return nil, "", bad
	}
	var newest string
	var at time.Time
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !strings.HasPrefix(entry.Name(), stem+"_") {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext != ".png" && ext != ".jpg" && ext != ".jpeg" && ext != ".webp" {
			continue
		}
		info, err := entry.Info()
		if err == nil && info.Mode().IsRegular() && info.Size() > 0 && info.Size() <= 4<<20 && !info.ModTime().Before(started.Add(-time.Second)) && info.ModTime().After(at) {
			newest, at = entry.Name(), info.ModTime()
		}
	}
	if newest == "" {
		return nil, "", bad
	}
	f, err := brain.Open(newest)
	if err != nil {
		return nil, "", bad
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4<<20+1))
	if err != nil || len(data) > 4<<20 {
		return nil, "", bad
	}
	mime := http.DetectContentType(data)
	if mime != "image/png" && mime != "image/jpeg" && mime != "image/webp" {
		return nil, "", bad
	}
	return data, mime, nil
}

// Keep directory removal inside open roots, including on Go 1.24 where
// os.Root has no RemoveAll. Symlinks are unlinked and never traversed.
func removeOfficialArtifacts(home, id string) error {
	if !safeNativeSessionID(id) {
		return fail(500, "invalid_session", "会话目录无效")
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return err
	}
	defer root.Close()
	var remove func(*os.Root, string) error
	remove = func(parent *os.Root, name string) error {
		info, err := parent.Lstat(name)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return parent.Remove(name)
		}
		dir, err := parent.OpenRoot(name)
		if err != nil {
			return err
		}
		f, err := dir.Open(".")
		if err != nil {
			_ = dir.Close()
			return err
		}
		entries, err := f.ReadDir(-1)
		_ = f.Close()
		if err == nil {
			for _, entry := range entries {
				if err = remove(dir, entry.Name()); err != nil {
					break
				}
			}
		}
		_ = dir.Close()
		if err != nil {
			return err
		}
		return parent.Remove(name)
	}
	return remove(root, filepath.Join("agy", "brain", id))
}
