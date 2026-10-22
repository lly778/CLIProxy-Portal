package cpamp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

const (
	pathAPICall    = "/v0/management/api-call"
	pathResetQuota = "/v0/management/reset-quota"
	codexUsageURL  = "https://chatgpt.com/backend-api/wham/usage"
)

var ErrNoQuotaWindows = errors.New("cpamp returned no supported Codex quota windows")

// FetchCodexQuota asks CPA for one credential's current Codex quota through
// CPAMP's api-call proxy. It deliberately bypasses CPAMP's persisted
// quota snapshots so lifecycle boundary normalization cannot replace the
// provider's current reset_at value. Both manual and automatic refreshes also
// clear stale routing cooldowns when the live provider response confirms that
// this enabled credential has usable quota again.
func (c *Client) FetchCodexQuota(ctx context.Context, file AuthFile, observedAt time.Time) ([]CodexQuotaWindow, error) {
	authIndex := strings.TrimSpace(file.AuthIndex)
	if authIndex == "" {
		return nil, errors.New("cpamp Codex auth file is missing auth index")
	}
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	headers := map[string]string{
		"Authorization": "Bearer $TOKEN$",
		"Content-Type":  "application/json",
		"User-Agent":    "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal",
	}
	if accountID := strings.TrimSpace(file.AccountID); accountID != "" {
		headers["Chatgpt-Account-Id"] = accountID
	}
	requestBody, err := json.Marshal(map[string]any{
		"authIndex": authIndex,
		"method":    http.MethodGet,
		"url":       codexUsageURL,
		"header":    headers,
	})
	if err != nil {
		return nil, errors.New("cpamp quota refresh request encoding failed")
	}
	body, err := c.do(ctx, http.MethodPost, pathAPICall, requestBody, c.adminHeader)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		StatusCode      int             `json:"status_code"`
		StatusCodeCamel int             `json:"statusCode"`
		Body            json.RawMessage `json:"body"`
	}
	if err := decodeJSON(pathAPICall, body, &envelope); err != nil {
		return nil, err
	}
	statusCode := envelope.StatusCode
	if statusCode == 0 {
		statusCode = envelope.StatusCodeCamel
	}
	if statusCode < 200 || statusCode >= 300 {
		return nil, &HTTPError{Operation: pathAPICall + " upstream", StatusCode: statusCode}
	}
	payload, err := decodeNestedObject(envelope.Body)
	if err != nil {
		return nil, errors.New("cpamp quota refresh returned an invalid response")
	}
	windows := buildMainCodexWindows(payload, observedAt)
	if len(windows) == 0 {
		return nil, ErrNoQuotaWindows
	}
	if !file.Disabled && strings.EqualFold(strings.TrimSpace(file.Provider), "codex") &&
		(file.Unavailable || len(file.Cooldowns) > 0 || strings.EqualFold(strings.TrimSpace(file.Status), "error")) &&
		codexQuotaAllowsRouting(payload) {
		if err := c.resetCodexQuotaCooldown(ctx, authIndex); err != nil {
			// Do not report a successful refresh while routing remains blocked.
			return nil, fmt.Errorf("cpamp quota refreshed but cooldown reset failed: %w", err)
		}
	}
	return windows, nil
}

// codexQuotaAllowsRouting requires explicit live availability. Percentages
// alone, cached reset times, and missing provider flags never lift a cooldown.
func codexQuotaAllowsRouting(payload map[string]any) bool {
	rateLimit := objectValue(payload, "rate_limit", "rateLimit")
	allowed, hasAllowed := rateLimit["allowed"].(bool)
	reached, hasReached := rateLimit["limit_reached"].(bool)
	if !hasReached {
		reached, hasReached = rateLimit["limitReached"].(bool)
	}
	if !hasAllowed || !allowed || !hasReached || reached {
		return false
	}
	windowCount := 0
	for _, keys := range [][]string{{"primary_window", "primaryWindow"}, {"secondary_window", "secondaryWindow"}} {
		window := objectValue(rateLimit, keys...)
		if window == nil {
			continue
		}
		used, ok := numberValue(window, "used_percent", "usedPercent")
		if !ok || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used >= 100 {
			return false
		}
		windowCount++
	}
	// A credential-wide reset also clears model cooldowns. Keep them intact
	// when the provider reports an exhausted additional model quota.
	for _, key := range []string{"additional_rate_limits", "additionalRateLimits"} {
		raw, exists := payload[key]
		if !exists || raw == nil {
			continue
		}
		limits, ok := raw.([]any)
		if !ok {
			return false
		}
		for _, item := range limits {
			entry, ok := item.(map[string]any)
			if !ok || !codexQuotaAllowsRouting(entry) {
				return false
			}
		}
	}
	return windowCount > 0
}

func (c *Client) resetCodexQuotaCooldown(ctx context.Context, authIndex string) error {
	requestBody, err := json.Marshal(map[string]string{"auth_index": authIndex})
	if err != nil {
		return errors.New("cpamp cooldown reset request encoding failed")
	}
	body, err := c.do(ctx, http.MethodPost, pathResetQuota, requestBody, c.adminHeader)
	if err != nil {
		return err
	}
	var result struct {
		Status    string `json:"status"`
		AuthIndex string `json:"auth_index"`
	}
	if err := decodeJSON(pathResetQuota, body, &result); err != nil {
		return err
	}
	if result.Status != "ok" || result.AuthIndex != authIndex {
		return errors.New("cpamp cooldown reset was not confirmed for the requested credential")
	}
	return nil
}

func decodeNestedObject(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, errors.New("empty body")
	}
	var object map[string]any
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(text), &object); err != nil {
			return nil, err
		}
	} else if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, errors.New("body is not an object")
	}
	return object, nil
}

func buildMainCodexWindows(payload map[string]any, observedAt time.Time) []CodexQuotaWindow {
	plan := textValue(payload, "plan_type", "planType")
	rateLimit := objectValue(payload, "rate_limit", "rateLimit")
	if rateLimit == nil {
		return nil
	}
	type candidate struct {
		name string
		raw  map[string]any
	}
	candidates := []candidate{
		{name: "primary", raw: objectValue(rateLimit, "primary_window", "primaryWindow")},
		{name: "secondary", raw: objectValue(rateLimit, "secondary_window", "secondaryWindow")},
	}
	windows := make([]CodexQuotaWindow, 0, 2)
	for _, item := range candidates {
		if item.raw == nil {
			continue
		}
		duration, hasDuration := numberValue(item.raw, "limit_window_seconds", "limitWindowSeconds")
		id, kind := classifyMainCodexWindow(item.name, duration, hasDuration, plan)
		if id == "" {
			continue
		}
		used, hasUsed := numberValue(item.raw, "used_percent", "usedPercent")
		if !hasUsed || math.IsNaN(used) || math.IsInf(used, 0) {
			continue
		}
		used = math.Max(0, math.Min(100, used))
		remaining := 100 - used
		window := CodexQuotaWindow{
			ProviderWindowID: id,
			WindowKind:       kind,
			ModelScopeKind:   "all",
			ObservedAtMS:     observedAt.UTC().UnixMilli(),
			UsedPercent:      &used,
			RemainingPercent: &remaining,
			PlanType:         plan,
			Availability:     "active",
		}
		if hasDuration && duration > 0 {
			d := int64(math.Round(duration))
			window.DurationSeconds = &d
			if end, ok := quotaResetEnd(item.raw, observedAt); ok {
				window.CycleEndMS = &end
			}
		}
		windows = append(windows, window)
	}
	return windows
}

func classifyMainCodexWindow(name string, duration float64, hasDuration bool, plan string) (string, string) {
	if hasDuration {
		seconds := int64(math.Round(duration))
		switch {
		case seconds == 5*60*60:
			return "five-hour", "five_hour"
		case seconds == 7*24*60*60:
			return "weekly", "weekly"
		case seconds >= 28*24*60*60 && seconds <= 31*24*60*60:
			return "monthly", "monthly"
		default:
			return "", ""
		}
	}
	if name == "primary" {
		return "five-hour", "five_hour"
	}
	if strings.EqualFold(strings.TrimSpace(plan), "team") {
		return "monthly", "monthly"
	}
	return "weekly", "weekly"
}

func quotaResetEnd(raw map[string]any, observedAt time.Time) (int64, bool) {
	resetAt, hasResetAt := numberValue(raw, "reset_at", "resetAt")
	resetAtMS, validResetAt := quotaAbsoluteResetMS(resetAt)
	if hasResetAt && validResetAt && resetAtMS > observedAt.UnixMilli() {
		return resetAtMS, true
	}
	if after, ok := numberValue(raw, "reset_after_seconds", "resetAfterSeconds"); ok && after > 0 {
		return observedAt.Add(time.Duration(after * float64(time.Second))).UnixMilli(), true
	}
	if hasResetAt && validResetAt {
		return resetAtMS, true
	}
	return 0, false
}

func quotaAbsoluteResetMS(value float64) (int64, bool) {
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	if value < 1e12 {
		value *= 1000
	}
	if value > math.MaxInt64 {
		return 0, false
	}
	return int64(math.Floor(value)), true
}

func objectValue(raw map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		if value, ok := raw[key].(map[string]any); ok {
			return value
		}
	}
	return nil
}

func textValue(raw map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := raw[key].(string); ok {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	return ""
}

func numberValue(raw map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		switch value := raw[key].(type) {
		case float64:
			return value, true
		case json.Number:
			number, err := value.Float64()
			return number, err == nil
		case string:
			var number float64
			if _, err := fmt.Sscan(strings.TrimSpace(value), &number); err == nil {
				return number, true
			}
		}
	}
	return 0, false
}
