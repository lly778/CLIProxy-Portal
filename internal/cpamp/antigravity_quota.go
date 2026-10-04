package cpamp

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

// AntigravityQuotaWindow describes a model group's quota, not a Codex period.
type AntigravityQuotaWindow struct {
	ID, Label           string
	GroupID             string
	RemainingPercent    float64
	ResetAt, ObservedAt time.Time
}

// FetchAntigravityQuota uses CPA's credential-injecting proxy. Neither tokens
// nor credential-file contents are downloaded by the portal.
func (c *Client) FetchAntigravityQuota(ctx context.Context, file AuthFile, observedAt time.Time) ([]AntigravityQuotaWindow, error) {
	if strings.TrimSpace(file.AuthIndex) == "" {
		return nil, errors.New("Antigravity 账号缺少身份索引")
	}
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	project := strings.TrimSpace(file.ProjectID)
	if project == "" {
		project = "bamboo-precept-lgxtn"
	}
	data, _ := json.Marshal(map[string]string{"project": project})
	var lastErr error = errors.New("Antigravity 未返回可用额度")
	for _, action := range []string{"retrieveUserQuotaSummary", "fetchAvailableModels"} {
		for _, host := range []string{"daily-cloudcode-pa.googleapis.com", "daily-cloudcode-pa.sandbox.googleapis.com", "cloudcode-pa.googleapis.com"} {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			body, _ := json.Marshal(map[string]any{"authIndex": file.AuthIndex, "method": http.MethodPost,
				"url": "https://" + host + "/v1internal:" + action, "data": string(data),
				"header": map[string]string{"Authorization": "Bearer $TOKEN$", "Content-Type": "application/json", "User-Agent": "antigravity/cli/1.0.13 (aidev_client; os_type=darwin; arch=arm64)"}})
			response, err := c.do(ctx, http.MethodPost, pathAPICall, body, c.adminHeader)
			if err != nil {
				return nil, err
			}
			var envelope struct {
				Status      int             `json:"status_code"`
				CamelStatus int             `json:"statusCode"`
				Body        json.RawMessage `json:"body"`
			}
			if err = decodeJSON(pathAPICall, response, &envelope); err != nil {
				return nil, err
			}
			status := envelope.Status
			if status == 0 {
				status = envelope.CamelStatus
			}
			if status < 200 || status >= 300 {
				lastErr = &HTTPError{Operation: "Antigravity quota", StatusCode: status}
				if status == 401 || status == 429 {
					return nil, lastErr
				}
				continue
			}
			payload, err := decodeNestedObject(envelope.Body)
			if err != nil {
				lastErr = errors.New("Antigravity 额度响应无效")
				continue
			}
			if windows := buildAntigravityQuotaWindows(payload, observedAt); len(windows) > 0 {
				return windows, nil
			}
		}
	}
	return nil, lastErr
}

func antigravityFraction(raw map[string]any) (float64, bool) {
	f, ok := numberValue(raw, "remainingFraction", "remaining_fraction", "remaining")
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return math.Max(0, math.Min(1, f)) * 100, true
}

func antigravityReset(raw map[string]any) time.Time {
	r, _ := time.Parse(time.RFC3339Nano, textValue(raw, "resetTime", "reset_time"))
	return r.UTC()
}

func buildAntigravityQuotaWindows(payload map[string]any, now time.Time) []AntigravityQuotaWindow {
	result := []AntigravityQuotaWindow{}
	seen := map[string]bool{}
	if groups, ok := payload["groups"].([]any); ok {
		for _, value := range groups {
			group, ok := value.(map[string]any)
			if !ok {
				continue
			}
			label := textValue(group, "displayName", "display_name")
			if label == "" {
				continue
			}
			buckets, _ := group["buckets"].([]any)
			for _, value := range buckets {
				bucket, ok := value.(map[string]any)
				if !ok {
					continue
				}
				percent, ok := antigravityFraction(bucket)
				if !ok {
					continue
				}
				bucketLabel := textValue(bucket, "displayName", "display_name", "window")
				id := label + "\x00" + textValue(bucket, "bucketId", "bucket_id") + "\x00" + bucketLabel + "\x00" + textValue(bucket, "window")
				if seen[id] {
					continue
				}
				seen[id] = true
				fullLabel := label
				if bucketLabel != "" && bucketLabel != label {
					fullLabel += " · " + bucketLabel
				}
				result = append(result, AntigravityQuotaWindow{ID: id, GroupID: label, Label: fullLabel, RemainingPercent: percent, ResetAt: antigravityReset(bucket), ObservedAt: now.UTC()})
			}
		}
	}
	if len(result) > 0 {
		sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
		return result
	}
	// Match CPA Manager Plus's shared Claude/Gemini model grouping. Tab-only
	// inventory is excluded; absent fractions remain unknown, never 100%.
	models := objectValue(payload, "models")
	tabs := map[string]bool{}
	for _, key := range []string{"tabModelIds", "tab_model_ids"} {
		if values, ok := payload[key].([]any); ok {
			for _, v := range values {
				if id, ok := v.(string); ok {
					tabs[id] = true
				}
			}
		}
	}
	groups := map[string]AntigravityQuotaWindow{}
	for id, value := range models {
		model, ok := value.(map[string]any)
		if !ok || tabs[id] {
			continue
		}
		search := strings.ToLower(id + " " + textValue(model, "displayName", "display_name"))
		key, label := "", ""
		if strings.Contains(search, "claude") || strings.Contains(search, "gpt") {
			key, label = "claude-gpt", "Claude"
		} else if strings.Contains(search, "gemini") {
			key, label = "gemini", "Gemini"
		} else {
			continue
		}
		quota := objectValue(model, "quotaInfo", "quota_info")
		percent, ok := antigravityFraction(quota)
		if !ok {
			continue
		}
		reset := antigravityReset(quota)
		g, exists := groups[key]
		if !exists || percent < g.RemainingPercent {
			g = AntigravityQuotaWindow{ID: key, GroupID: key, Label: label, RemainingPercent: percent, ResetAt: reset, ObservedAt: now.UTC()}
		} else if percent == g.RemainingPercent {
			// A shared group recovers only when all limiting models recover.
			if reset.IsZero() || g.ResetAt.IsZero() {
				g.ResetAt = time.Time{}
			} else if reset.After(g.ResetAt) {
				g.ResetAt = reset
			}
		}
		groups[key] = g
	}
	for _, key := range []string{"claude-gpt", "gemini"} {
		if g, ok := groups[key]; ok {
			result = append(result, g)
		}
	}
	return result
}
