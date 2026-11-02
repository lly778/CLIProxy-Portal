package cpamp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cliproxy-portal/internal/domain"
	"gopkg.in/yaml.v3"
)

// ConfiguredModelReasoning contains only public model metadata. Provider keys
// are used on the server and never returned or included in errors.
type ConfiguredModelReasoning struct {
	ID     string
	Levels []string
}

// ListConfiguredModelReasoning reads the model-list metadata of configured
// OpenAI-compatible providers. The caller caches the sanitized aggregate.
func (c *Client) ListConfiguredModelReasoning(ctx context.Context, data []byte) []ConfiguredModelReasoning {
	type provider struct {
		BaseURL string `yaml:"base-url"`
		Prefix  string `yaml:"prefix"`
		Keys    []struct {
			Key string `yaml:"api-key"`
		} `yaml:"api-key-entries"`
		Models []struct {
			Name  string `yaml:"name"`
			Alias string `yaml:"alias"`
		} `yaml:"models"`
	}
	var config struct {
		Providers []provider `yaml:"openai-compatibility"`
	}
	if yaml.Unmarshal(data, &config) != nil {
		return nil
	}
	client := *c.httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	result := []ConfiguredModelReasoning{}
	for index, provider := range config.Providers {
		if index >= 16 || ctx.Err() != nil {
			break
		}
		address, err := url.Parse(strings.TrimSpace(provider.BaseURL))
		if err != nil || address.Host == "" || address.User != nil || address.RawQuery != "" || address.Fragment != "" || (address.Scheme != "https" && address.Scheme != "http") {
			continue
		}
		address.Path = strings.TrimRight(address.Path, "/") + "/models"
		readCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		request, err := http.NewRequestWithContext(readCtx, http.MethodGet, address.String(), nil)
		if err != nil {
			cancel()
			continue
		}
		if len(provider.Keys) > 0 && strings.TrimSpace(provider.Keys[0].Key) != "" {
			// Do not use c.do: its credential belongs to CPAMP, not this provider.
			request.Header.Set("Authorization", normalizeBearer(provider.Keys[0].Key))
		}
		response, err := client.Do(request)
		if err != nil {
			cancel()
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 2<<20+1))
		response.Body.Close()
		cancel()
		if readErr != nil || response.StatusCode != http.StatusOK || len(body) > 2<<20 {
			continue
		}
		var envelope struct {
			Data []struct {
				ID     string `json:"id"`
				Effort struct {
					Levels []string `json:"supported_levels"`
				} `json:"effort"`
				Thinking struct {
					Levels []string `json:"levels"`
				} `json:"thinking"`
				Levels []string `json:"supported_reasoning_levels"`
			} `json:"data"`
		}
		if json.Unmarshal(body, &envelope) != nil {
			continue
		}
		for _, model := range envelope.Data {
			if !domain.ValidClientModelID(model.ID) {
				continue
			}
			declared := map[string]bool{}
			for _, level := range append(append(model.Effort.Levels, model.Thinking.Levels...), model.Levels...) {
				declared[level] = true
			}
			levels := []string{}
			for _, level := range domain.ClientReasoningLevels() {
				if declared[level] {
					levels = append(levels, level)
				}
			}
			if len(levels) == 0 {
				continue
			}
			ids := []string{}
			if len(provider.Models) == 0 {
				ids = append(ids, model.ID)
			}
			for _, configured := range provider.Models {
				if strings.EqualFold(configured.Name, model.ID) {
					ids = append(ids, configured.Name)
					if configured.Alias != "" {
						ids = append(ids, configured.Alias)
					}
				}
			}
			for _, id := range ids {
				if domain.ValidClientModelID(id) {
					result = append(result, ConfiguredModelReasoning{ID: id, Levels: append([]string{}, levels...)})
				}
				if provider.Prefix != "" && domain.ValidClientModelID(provider.Prefix+"/"+id) {
					result = append(result, ConfiguredModelReasoning{ID: provider.Prefix + "/" + id, Levels: append([]string{}, levels...)})
				}
			}
		}
	}
	return result
}
