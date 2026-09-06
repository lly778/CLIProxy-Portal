package cpamp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	pathQuotaWrite = "/v0/management/quota-snapshots"
	codexUsageURL  = "https://chatgpt.com/backend-api/wham/usage"
)

var ErrNoQuotaWindows = errors.New("cpamp returned no supported Codex quota windows")

type QuotaRefreshWindow struct {
	ProviderWindowID    string   `json:"provider_window_id"`
	WindowKind          string   `json:"window_kind"`
	WindowMode          string   `json:"window_mode"`
	ModelScopeKind      string   `json:"model_scope_kind"`
	Source              string   `json:"source"`
	SourceObservationID string   `json:"source_observation_id,omitempty"`
	ObservedAtMS        int64    `json:"observed_at_ms"`
	BoundaryAccuracy    string   `json:"boundary_accuracy"`
	CycleStartMS        *int64   `json:"cycle_start_ms,omitempty"`
	CycleEndMS          *int64   `json:"cycle_end_ms,omitempty"`
	DurationSeconds     *int64   `json:"duration_seconds,omitempty"`
	UsedPercent         *float64 `json:"used_percent,omitempty"`
	RemainingPercent    *float64 `json:"remaining_percent,omitempty"`
	PlanType            string   `json:"plan_type,omitempty"`
}

type quotaRefreshObservation struct {
	Source              string `json:"source"`
	SourceObservationID string `json:"source_observation_id"`
	ObservedAtMS        int64  `json:"observed_at_ms"`
	InventoryScopeKey   string `json:"inventory_scope_key"`
	InventoryMode       string `json:"inventory_mode"`
}

type quotaRefreshEntry struct {
	RowKey      string                  `json:"row_key,omitempty"`
	Provider    string                  `json:"provider"`
	Account     QuotaAccountTarget      `json:"account"`
	Observation quotaRefreshObservation `json:"observation"`
	Windows     []QuotaRefreshWindow    `json:"windows"`
}

// RefreshCodexQuotaSnapshot asks CPA for one credential's current Codex quota
// through CPAMP's read-only api-call proxy, then stores only the returned main
// rate-limit windows as a partial quota observation. It never starts CPAMP's
// credential inspection workflow and therefore cannot run inspection actions.
func (c *Client) RefreshCodexQuotaSnapshot(ctx context.Context, file AuthFile, observedAt time.Time) error {
	authIndex := strings.TrimSpace(file.AuthIndex)
	if authIndex == "" {
		return errors.New("cpamp Codex auth file is missing auth index")
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
	payload, err := c.fetchCodexQuotaPayload(ctx, authIndex, headers)
	if err != nil {
		return err
	}
	observedAtMS := observedAt.UTC().UnixMilli()
	observationID := quotaObservationID(file, observedAtMS)
	windows := buildMainCodexWindows(payload, observedAt, observationID)
	if len(windows) == 0 {
		return ErrNoQuotaWindows
	}
	// Codex occasionally returns only one of the main windows even though a
	// Plus/Pro account normally exposes both 5-hour and weekly quota. Retry a
	// partial response once and merge the result so one transient omission does
	// not silently remove that account from the pool average.
	if shouldRetryMainCodexWindows(payload, windows) {
		if retryPayload, retryErr := c.fetchCodexQuotaPayload(ctx, authIndex, headers); retryErr == nil {
			windows = mergeQuotaRefreshWindows(windows, buildMainCodexWindows(retryPayload, observedAt, observationID))
		}
	}
	target := QuotaAccountTarget{
		AccountSnapshot:       file.AccountSnapshot,
		AuthFileSnapshot:      file.Name,
		AuthProviderSnapshot:  "codex",
		AuthProjectIDSnapshot: file.ProjectID,
		AuthIndex:             authIndex,
		Source:                file.Name,
	}
	entry := quotaRefreshEntry{
		RowKey:   strings.TrimSpace(file.Name) + "\x00" + authIndex,
		Provider: "codex",
		Account:  target,
		Observation: quotaRefreshObservation{
			Source:              "api_query",
			SourceObservationID: observationID,
			ObservedAtMS:        observedAtMS,
			InventoryScopeKey:   "codex:rate-limits",
			InventoryMode:       "partial",
		},
		Windows: windows,
	}
	writeBody, err := json.Marshal(map[string]any{"entries": []quotaRefreshEntry{entry}})
	if err != nil {
		return errors.New("cpamp quota snapshot encoding failed")
	}
	_, err = c.do(ctx, http.MethodPost, pathQuotaWrite, writeBody, c.adminHeader)
	return err
}

func (c *Client) fetchCodexQuotaPayload(ctx context.Context, authIndex string, headers map[string]string) (map[string]any, error) {
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
	return payload, nil
}

func shouldRetryMainCodexWindows(payload map[string]any, windows []QuotaRefreshWindow) bool {
	if len(windows) != 1 {
		return false
	}
	plan := strings.ToLower(strings.TrimSpace(textValue(payload, "plan_type", "planType")))
	return plan != "team" && windows[0].WindowKind != "monthly"
}

func mergeQuotaRefreshWindows(first, second []QuotaRefreshWindow) []QuotaRefreshWindow {
	merged := make(map[string]QuotaRefreshWindow, len(first)+len(second))
	order := make([]string, 0, len(first)+len(second))
	for _, windows := range [][]QuotaRefreshWindow{first, second} {
		for _, window := range windows {
			key := strings.TrimSpace(window.WindowKind)
			if key == "" {
				key = strings.TrimSpace(window.ProviderWindowID)
			}
			if _, exists := merged[key]; !exists {
				order = append(order, key)
			}
			merged[key] = window
		}
	}
	result := make([]QuotaRefreshWindow, 0, len(order))
	for _, key := range order {
		result = append(result, merged[key])
	}
	return result
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

func buildMainCodexWindows(payload map[string]any, observedAt time.Time, observationID string) []QuotaRefreshWindow {
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
	windows := make([]QuotaRefreshWindow, 0, 2)
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
		window := QuotaRefreshWindow{
			ProviderWindowID:    id,
			WindowKind:          kind,
			WindowMode:          "unknown",
			ModelScopeKind:      "all",
			Source:              "api_query",
			SourceObservationID: observationID,
			ObservedAtMS:        observedAt.UTC().UnixMilli(),
			BoundaryAccuracy:    "unknown",
			UsedPercent:         &used,
			RemainingPercent:    &remaining,
			PlanType:            plan,
		}
		if hasDuration && duration > 0 {
			d := int64(math.Round(duration))
			window.DurationSeconds = &d
			if end, accuracy, ok := quotaResetEnd(item.raw, observedAt); ok {
				start := end - d*1000
				if start > 0 {
					window.WindowMode = "fixed"
					window.BoundaryAccuracy = accuracy
					window.CycleStartMS = &start
					window.CycleEndMS = &end
				}
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

func quotaResetEnd(raw map[string]any, observedAt time.Time) (int64, string, bool) {
	if resetAt, ok := numberValue(raw, "reset_at", "resetAt"); ok && resetAt > 0 {
		return int64(math.Floor(resetAt)) * 1000, "exact", true
	}
	if after, ok := numberValue(raw, "reset_after_seconds", "resetAfterSeconds"); ok && after > 0 {
		return observedAt.Add(time.Duration(after * float64(time.Second))).UnixMilli(), "derived", true
	}
	return 0, "unknown", false
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

func quotaObservationID(file AuthFile, observedAtMS int64) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(file.Name) + "\x00" + strings.TrimSpace(file.AuthIndex) + fmt.Sprint(observedAtMS)))
	return "portal-" + hex.EncodeToString(sum[:16])
}
