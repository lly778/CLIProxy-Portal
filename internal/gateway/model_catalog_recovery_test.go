package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/service"
)

type catalogRecoveryGatewayCPA struct {
	gatewayCPAMP
	cooldown time.Time
}

func (c *catalogRecoveryGatewayCPA) ListAuthFiles(context.Context) ([]cpamp.AuthFile, error) {
	file := cpamp.AuthFile{Name: "account.json", Provider: "antigravity", AuthIndex: "opaque", Status: "active", AvailabilityKnown: true}
	if !c.cooldown.IsZero() {
		file.Cooldowns = []cpamp.AuthCooldown{{Scope: "model", ModelKey: "gemini-model", RetryAt: c.cooldown}}
	}
	return []cpamp.AuthFile{file}, nil
}

func (c *catalogRecoveryGatewayCPA) ListAuthFileModels(context.Context, cpamp.AuthFile) ([]cpamp.Model, error) {
	return []cpamp.Model{{ID: "gemini-model", OwnedBy: "antigravity"}}, nil
}

func TestGatewayRecoversExpiredCatalogButKeepsOriginalMetadata(t *testing.T) {
	now := time.Now().UTC()
	for _, cooling := range []bool{false, true} {
		client := &catalogRecoveryGatewayCPA{}
		if cooling {
			client.cooldown = now.Add(time.Minute)
		}
		keys := service.NewKeys(nil, client)
		keys.Now = func() time.Time { return now }
		g := &Gateway{keys: keys}
		response := &http.Response{Header: http.Header{}, Request: httptest.NewRequest(http.MethodGet, "/v1/models", nil), Body: io.NopCloser(strings.NewReader(`{"object":"list","extension":"preserved","data":[{"id":"gpt-6-luna","thinking":{"levels":["high"]}},{"id":"gpt-5.6-luna"}]}`))}
		if err := g.filterModelList(response); err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(response.Body)
		var parsed struct {
			Extension string
			Data      []map[string]any
		}
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatal(err)
		}
		want := 2
		if cooling {
			want = 1
		}
		if parsed.Extension != "preserved" || len(parsed.Data) != want || parsed.Data[0]["thinking"] == nil || strings.Contains(string(data), "gpt-5.6-luna") {
			t.Fatalf("cooling=%v catalog=%s", cooling, data)
		}
		if !cooling && parsed.Data[1]["id"] != "gemini-model" {
			t.Fatalf("missing restored model: %s", data)
		}
	}
}
