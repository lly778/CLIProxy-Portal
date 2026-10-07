package gateway

import (
	"context"
	"encoding/json"
	"errors"
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

type multiChannelCatalogCPA struct {
	gatewayCPAMP
	failure string
}

func (c *multiChannelCatalogCPA) ListOAuthChannels(context.Context) ([]string, error) {
	if c.failure == "discovery" {
		return nil, errors.New("unavailable")
	}
	return []string{"antigravity", "claude"}, nil
}

func (c *multiChannelCatalogCPA) ListAuthFiles(context.Context) ([]cpamp.AuthFile, error) {
	return []cpamp.AuthFile{{Provider: "antigravity", AuthIndex: "opaque", Status: "active", AvailabilityKnown: true}}, nil
}

func (c *multiChannelCatalogCPA) ListAuthFileModels(context.Context, cpamp.AuthFile) ([]cpamp.Model, error) {
	return []cpamp.Model{{ID: "anti-alias"}, {ID: "gemini-real", OwnedBy: "antigravity"}}, nil
}

func (c *multiChannelCatalogCPA) ListOAuthModelAliases(ctx context.Context, channel string) ([]cpamp.OAuthModelAlias, error) {
	if c.failure == "aliases" && channel == "antigravity" {
		return nil, errors.New("unavailable")
	}
	switch channel {
	case "antigravity":
		return []cpamp.OAuthModelAlias{{Name: "gemini-real", Alias: "anti-alias", Fork: true}, {Name: "gemini-real", Alias: "gpt-6-sol", Fork: true}}, nil
	case "claude":
		return []cpamp.OAuthModelAlias{{Name: "claude-real", Alias: "claude-alias", Fork: true}}, nil
	default:
		return c.gatewayCPAMP.ListOAuthModelAliases(ctx, channel)
	}
}

func (c *multiChannelCatalogCPA) ListOAuthModelDefinitions(ctx context.Context, channel string) ([]cpamp.OAuthModelDefinition, error) {
	switch channel {
	case "antigravity":
		return []cpamp.OAuthModelDefinition{{ID: "gemini-real"}}, nil
	case "claude":
		return []cpamp.OAuthModelDefinition{{ID: "claude-real"}}, nil
	default:
		return c.gatewayCPAMP.ListOAuthModelDefinitions(ctx, channel)
	}
}

func TestGatewayFiltersAllChannelsAndRecoveredAliases(t *testing.T) {
	for _, failure := range []string{"", "discovery", "aliases"} {
		t.Run(failure, func(t *testing.T) {
			g := &Gateway{keys: service.NewKeys(nil, &multiChannelCatalogCPA{failure: failure})}
			response := &http.Response{Header: http.Header{}, Request: httptest.NewRequest(http.MethodGet, "/v1/models", nil), Body: io.NopCloser(strings.NewReader(`{"object":"list","extension":"keep","data":[{"id":"gpt-6-luna","thinking":{"levels":["high"]}},{"id":"gpt-5.6-luna"},{"id":" ANTI-ALIAS "},{"id":"claude-alias"},{"id":"gpt-6-sol","owned_by":"openai"},{"id":"claude-real"}]}`))}
			err := g.filterModelList(response)
			if failure != "" {
				if err == nil {
					t.Fatal("filter ignored unavailable channel/aliases")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var parsed struct {
				Extension string           `json:"extension"`
				Data      []map[string]any `json:"data"`
			}
			if err := json.NewDecoder(response.Body).Decode(&parsed); err != nil {
				t.Fatal(err)
			}
			want := []string{"gpt-6-luna", "gpt-6-sol", "claude-real", "gemini-real"}
			if parsed.Extension != "keep" || len(parsed.Data) != len(want) {
				t.Fatalf("catalog=%+v", parsed)
			}
			for i, id := range want {
				if parsed.Data[i]["id"] != id {
					t.Fatalf("item %d=%+v", i, parsed.Data[i])
				}
			}
			if parsed.Data[0]["thinking"] == nil || parsed.Data[1]["owned_by"] != "openai" {
				t.Fatal("original metadata changed")
			}
		})
	}
}
