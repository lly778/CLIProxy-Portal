package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/store"
)

type gatewayCPAMP struct{ cpamp.API }

func (*gatewayCPAMP) ListOAuthModelAliases(context.Context, string) ([]cpamp.OAuthModelAlias, error) {
	return []cpamp.OAuthModelAlias{
		{Name: "gpt-6-luna", Alias: "gpt-5.6-luna", Fork: true},
		{Name: "gpt-6-luna", Alias: "gpt-6-sol", Fork: true}, // An enabled real name must remain visible.
	}, nil
}
func (*gatewayCPAMP) ListOAuthModelDefinitions(context.Context, string) ([]cpamp.OAuthModelDefinition, error) {
	return []cpamp.OAuthModelDefinition{{ID: "gpt-6-luna"}, {ID: "gpt-6-sol"}}, nil
}
func (*gatewayCPAMP) ListOAuthExcludedModels(context.Context, string) ([]string, error) {
	return nil, nil
}

func TestGatewayFiltersListButPassesAliasAndSavesOnlyDialogue(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	user := domain.User{ID: "usr_gateway", Phone: "13800138000", Name: "测试用户", PasswordHash: "hash", Role: domain.RoleUser, Status: domain.StatusApproved, CreatedAt: now, UpdatedAt: now}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	const key = "sk-gateway-test"
	if err := st.CreateKey(ctx, domain.APIKey{ID: "key_gateway", UserID: user.ID, Hash: cpamp.HashAPIKey(key), LastFour: "test", Alias: "test", Status: "active", IssuedAt: now}); err != nil {
		t.Fatal(err)
	}
	stream := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"你好\"}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"secret-tool-argument\"}\n\n" +
		"data: {\"type\":\"response.completed\"}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("Proxy-Authorization") != "" {
			t.Errorf("private gateway headers reached CPA")
		}
		w.Header().Set("Set-Cookie", "should-not-reach-client=1")
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-6-luna"},{"id":"gpt-5.6-luna"},{"id":"gpt-6-sol"}]}`))
			return
		}
		if r.URL.Path != "/v1/responses" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"model":"gpt-5.6-luna"`)) {
			t.Errorf("alias request was rewritten: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-CPA-TRACE-ID", "20260923120000-auth-1234abcd")
		_, _ = w.Write([]byte(stream))
	}))
	defer upstream.Close()
	vault, err := NewVault(filepath.Join(t.TempDir(), "captures"), []byte(strings.Repeat("s", 32)))
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := New(upstream.URL, service.NewKeys(st, &gatewayCPAMP{}), st, vault, nil)
	if err != nil {
		t.Fatal(err)
	}
	listReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	listReq.Header.Set("Authorization", "Bearer "+key)
	listReq.Header.Set("Cookie", "portal_session=private")
	listReq.Header.Set("Proxy-Authorization", "secret")
	listResponse := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(listResponse, listReq)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("model list status=%d body=%s", listResponse.Code, listResponse.Body.String())
	}
	if listResponse.Header().Get("Set-Cookie") != "" {
		t.Fatal("upstream cookie reached the client")
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listResponse.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 2 || list.Data[0].ID != "gpt-6-luna" || list.Data[1].ID != "gpt-6-sol" {
		t.Fatalf("filtered list = %+v", list.Data)
	}
	requestJSON := `{"model":"gpt-5.6-luna","input":[{"role":"user","content":[{"type":"input_text","text":"你好"}]},{"type":"function_call_output","output":"secret-tool-output"}],"tools":[{"name":"secret-tool-definition"}],"stream":true}`
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(requestJSON))
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != stream {
		t.Fatalf("proxied response = %d %q", response.Code, response.Body.String())
	}
	captures, err := st.ListGatewayCaptures(ctx, user.ID, 100)
	if err != nil || len(captures) != 1 {
		t.Fatalf("captures = %+v, %v", captures, err)
	}
	if captures[0].CPARequestID != "1234abcd" {
		t.Fatalf("CPA request id = %q", captures[0].CPARequestID)
	}
	requestParts, err := st.GatewayCaptureMessages(ctx, captures[0].ID, "request")
	if err != nil || len(requestParts) != 1 || requestParts[0].Role != "用户" {
		t.Fatalf("saved request message references = %+v, %v", requestParts, err)
	}
	requestText, err := vault.ReadSharedMessage(user.ID, requestParts[0].Role, requestParts[0].ID)
	if err != nil || requestText != "你好" {
		t.Fatalf("saved request message = %q, %v", requestText, err)
	}
	responseParts, err := st.GatewayCaptureMessages(ctx, captures[0].ID, "response")
	if err != nil || len(responseParts) != 1 || responseParts[0].Role != "助手" {
		t.Fatalf("saved response message references = %+v, %v", responseParts, err)
	}
	responseText, err := vault.ReadSharedMessage(user.ID, responseParts[0].Role, responseParts[0].ID)
	if err != nil || responseText != "你好" {
		t.Fatalf("saved response message = %q, %v", responseText, err)
	}
	if _, err := os.Stat(filepath.Join(vault.dir, captures[0].ID+".rawrequest.enc")); !os.IsNotExist(err) {
		t.Fatalf("raw request capture retained: %v", err)
	}
	secondJSON := `{"model":"gpt-5.6-luna","input":[{"role":"user","content":[{"type":"input_text","text":"你好"}]},{"role":"assistant","content":[{"type":"output_text","text":"你好"}]},{"role":"user","content":[{"type":"input_text","text":"第二轮"}]}]}`
	second := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(secondJSON))
	second.Header.Set("Authorization", "Bearer "+key)
	secondResponse := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(secondResponse, second)
	if secondResponse.Code != http.StatusOK {
		t.Fatalf("second response status = %d", secondResponse.Code)
	}
	captures, err = st.ListGatewayCaptures(ctx, user.ID, 100)
	if err != nil || len(captures) != 2 {
		t.Fatalf("two captures = %+v, %v", captures, err)
	}
	var secondParts []store.GatewayCaptureMessage
	for _, capture := range captures {
		parts, partErr := st.GatewayCaptureMessages(ctx, capture.ID, "request")
		if partErr != nil {
			t.Fatal(partErr)
		}
		if len(parts) == 3 {
			secondParts = parts
		}
	}
	if len(secondParts) != 3 || secondParts[0].ID != requestParts[0].ID || secondParts[1].ID != responseParts[0].ID || secondParts[2].ID == secondParts[0].ID {
		t.Fatalf("history was not shared between requests: %+v", secondParts)
	}
	files, err := filepath.Glob(filepath.Join(vault.sharedDir, "*.enc"))
	if err != nil || len(files) != 3 {
		t.Fatalf("shared message files = %v, %v", files, err)
	}
}

func TestSharedMessagesDeduplicatePerUserAndRejectOtherOwners(t *testing.T) {
	vault, err := NewVault(t.TempDir(), []byte(strings.Repeat("s", 32)))
	if err != nil {
		t.Fatal(err)
	}
	id, created, err := vault.SaveSharedMessage("user-a", "用户", "相同的历史消息")
	if err != nil || !created {
		t.Fatalf("first save: id=%q created=%t err=%v", id, created, err)
	}
	again, created, err := vault.SaveSharedMessage("user-a", "用户", "相同的历史消息")
	if err != nil || created || again != id {
		t.Fatalf("deduplicated save: id=%q created=%t err=%v", again, created, err)
	}
	otherID, created, err := vault.SaveSharedMessage("user-b", "用户", "相同的历史消息")
	if err != nil || !created || otherID == id {
		t.Fatalf("other owner's save: id=%q created=%t err=%v", otherID, created, err)
	}
	if _, err := vault.ReadSharedMessage("user-b", "用户", id); err == nil {
		t.Fatal("other user could read shared message")
	}
	if text, err := vault.ReadSharedMessage("user-a", "用户", id); err != nil || text != "相同的历史消息" {
		t.Fatalf("owner read = %q, %v", text, err)
	}
}

func TestVaultIntegrityAndLimit(t *testing.T) {
	vault, err := NewVault(t.TempDir(), []byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	const id = "0123456789abcdef0123456789abcdef"
	writer, err := vault.Create(id, "request")
	if err != nil {
		t.Fatal(err)
	}
	input := bytes.Repeat([]byte("a"), CaptureLimit+1)
	if n, err := writer.Write(input); n != len(input) || err != nil {
		t.Fatalf("write = %d, %v", n, err)
	}
	if truncated, err := writer.Close(); !truncated || err != nil {
		t.Fatalf("close = %t, %v", truncated, err)
	}
	plain, err := vault.Read(id, "request")
	if err != nil || len(plain) != CaptureLimit {
		t.Fatalf("read length = %d, %v", len(plain), err)
	}
	path, _ := vault.filePath(id, "request")
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	var flipped [1]byte
	if _, err := file.ReadAt(flipped[:], 20); err != nil {
		t.Fatal(err)
	}
	flipped[0] ^= 0xff
	if _, err := file.WriteAt(flipped[:], 20); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if _, err := vault.Read(id, "request"); err == nil {
		t.Fatal("tampered capture decrypted")
	}
}

func TestDialogueParsersOmitToolsAndReasoningAcrossProtocols(t *testing.T) {
	request := requestDialogue([]byte(`{"messages":[{"role":"system","content":"hidden system"},{"role":"user","content":"hello"},{"role":"tool","content":"hidden tool"},{"role":"assistant","content":"past reply"}]}`))
	if strings.Contains(request, "hidden") || !strings.Contains(request, "用户：\nhello") || !strings.Contains(request, "past reply") {
		t.Fatalf("chat request dialogue = %q", request)
	}
	anthropic := responseDialogue([]byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":"answer"},{"type":"tool_use","name":"shell","input":{"command":"hidden shell"}}]}`), "application/json")
	if !strings.Contains(anthropic, "answer") || strings.Contains(anthropic, "hidden") {
		t.Fatalf("Anthropic dialogue = %q", anthropic)
	}
	geminiRequest := requestDialogue([]byte(`{"contents":[{"role":"user","parts":[{"text":"hello"},{"functionResponse":{"name":"shell","response":"hidden"}}]}]}`))
	if !strings.Contains(geminiRequest, "hello") || strings.Contains(geminiRequest, "hidden") {
		t.Fatalf("Gemini request = %q", geminiRequest)
	}
	geminiResponse := responseDialogue([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"answer"},{"text":"hidden thought","thought":true},{"functionCall":{"name":"shell"}}]}}]}`), "application/json")
	if !strings.Contains(geminiResponse, "answer") || strings.Contains(geminiResponse, "hidden") {
		t.Fatalf("Gemini response = %q", geminiResponse)
	}
	streamed := responseDialogue([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"answer\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"hidden\"}}\n\n"), "text/event-stream")
	if !strings.Contains(streamed, "answer") || strings.Contains(streamed, "hidden") {
		t.Fatalf("Anthropic stream = %q", streamed)
	}
}
