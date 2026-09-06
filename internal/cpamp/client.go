// Package cpamp contains the small HTTP client used by the portal to talk to
// a CPA-Manager-Plus Full/Manager Server instance.
//
// The client deliberately keeps the CPAMP administrator key private. It never
// includes request bodies, bearer tokens, or API key values in returned errors.
package cpamp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTimeout  = 10 * time.Second
	maxResponseSize = 16 << 20
)

const (
	pathHealth       = "/health"
	pathStatus       = "/status"
	pathAPIKeys      = "/v0/management/api-keys"
	pathAuthFiles    = "/v0/management/auth-files"
	pathAliases      = "/v0/management/api-key-aliases"
	pathAnalytics    = "/v0/management/monitoring/analytics"
	pathQuotaQuery   = "/v0/management/quota-snapshots/query"
	pathModelCatalog = "/v1/models"
)

// Config controls a Client. BaseURL should point at the Manager Server, for
// example http://cpamp:18317. AdminKey is the raw CPAMP administrator key;
// "Bearer " is accepted as a convenience and normalized.
type Config struct {
	BaseURL     string
	AdminKey    string
	ModelAPIKey string
	HTTPClient  *http.Client
	Timeout     time.Duration
}

// Client talks to a CPAMP Manager Server. A Client is safe for concurrent use.
type Client struct {
	baseURL     *url.URL
	adminHeader string
	modelHeader string
	httpClient  *http.Client
	timeout     time.Duration
}

// New creates a client using the default HTTP timeout.
func New(baseURL, adminKey string) (*Client, error) {
	return NewWithConfig(Config{BaseURL: baseURL, AdminKey: adminKey})
}

// NewClient is an explicit constructor alias for callers that prefer that
// naming convention.
func NewClient(baseURL, adminKey string) (*Client, error) {
	return New(baseURL, adminKey)
}

// NewWithConfig creates a client with an optional custom transport and timeout.
func NewWithConfig(cfg Config) (*Client, error) {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		return nil, errors.New("cpamp base URL is required")
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("cpamp base URL must be an absolute HTTP(S) URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("cpamp base URL must use HTTP or HTTPS")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("cpamp base URL must not contain credentials, query, or fragment")
	}
	admin := normalizeBearer(cfg.AdminKey)
	if admin == "" {
		return nil, errors.New("cpamp admin key is required")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}

	modelKey := strings.TrimSpace(cfg.ModelAPIKey)
	if modelKey == "" {
		// CPAMP's model-list proxy preserves the caller's Authorization header.
		// Using the admin key as a fallback keeps the method useful when the CPA
		// instance intentionally accepts that key as an API key, while callers
		// with separate CPA API credentials can set ModelAPIKey or use the
		// WithAPIKey variant.
		modelKey = strings.TrimSpace(cfg.AdminKey)
	}
	return &Client{
		baseURL:     u,
		adminHeader: admin,
		modelHeader: normalizeBearer(modelKey),
		httpClient:  httpClient,
		timeout:     timeout,
	}, nil
}

// API is the narrow CPAMP surface consumed by the portal. Client implements
// this interface; keeping it here lets higher-level services use a fake in
// tests without importing HTTP details.
type API interface {
	Health(context.Context) (HealthResponse, error)
	Status(context.Context) (StatusResponse, error)
	ListAPIKeys(context.Context) ([]string, error)
	ReplaceAPIKeys(context.Context, []string) error
	AddAPIKey(context.Context, string) error
	RemoveAPIKey(context.Context, string) error
	ListAliases(context.Context) ([]APIKeyAlias, error)
	ReplaceAliases(context.Context, []APIKeyAlias, []string, bool) ([]APIKeyAlias, error)
	DeleteAlias(context.Context, string) error
	Analytics(context.Context, AnalyticsRequest) (AnalyticsResponse, error)
	ListModels(context.Context) (ModelListResponse, error)
}

// HTTPError describes a non-2xx response without retaining the response body.
// The body is intentionally discarded because CPAMP/CPA error bodies can echo
// credentials or configuration values.
type HTTPError struct {
	Operation  string
	StatusCode int
}

func (e *HTTPError) Error() string {
	if e == nil {
		return "cpamp request failed"
	}
	return fmt.Sprintf("cpamp %s failed with HTTP %d", e.Operation, e.StatusCode)
}

// RequestError is a transport-level error whose text is intentionally safe.
// Unwrap preserves errors.Is/errors.As behavior without exposing the URL (and
// therefore without exposing a key sent in a query parameter).
type RequestError struct {
	Operation string
	Err       error
}

func (e *RequestError) Error() string {
	if e == nil {
		return "cpamp request failed"
	}
	return fmt.Sprintf("cpamp %s request failed", e.Operation)
}

func (e *RequestError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// HealthResponse is CPAMP's lightweight health response.
type HealthResponse struct {
	OK      bool   `json:"ok"`
	Service string `json:"service"`
}

// StatusResponse is the stable, useful subset of CPAMP's /status response.
// Database is intentionally left as raw JSON because its shape varies by
// CPAMP version and is not needed by the portal.
type StatusResponse struct {
	Service       string              `json:"service"`
	DBPath        string              `json:"dbPath"`
	Events        int64               `json:"events"`
	DeadLetters   int64               `json:"deadLetters"`
	Collector     CollectorStatus     `json:"collector"`
	DataMigration DataMigrationStatus `json:"dataMigration"`
	Database      json.RawMessage     `json:"database,omitempty"`
}

// CollectorStatus mirrors CPAMP's collector status fields.
type CollectorStatus struct {
	Collector      string `json:"collector"`
	Upstream       string `json:"upstream"`
	Mode           string `json:"mode"`
	Transport      string `json:"transport"`
	Queue          string `json:"queue"`
	LastConsumedAt int64  `json:"lastConsumedAt"`
	LastInsertedAt int64  `json:"lastInsertedAt"`
	TotalInserted  int64  `json:"totalInserted"`
	TotalSkipped   int64  `json:"totalSkipped"`
	DeadLetters    int64  `json:"deadLetters"`
	LastError      string `json:"lastError,omitempty"`
}

// DataMigrationStatus mirrors the migration progress fields exposed by CPAMP.
type DataMigrationStatus struct {
	Name          string `json:"name"`
	Status        string `json:"status"`
	LastEventID   int64  `json:"lastEventId"`
	TargetEventID int64  `json:"targetEventId"`
	ProcessedRows int64  `json:"processedRows"`
	ChangedRows   int64  `json:"changedRows"`
	AppliedRows   int64  `json:"appliedRows"`
	StartedAtMS   int64  `json:"startedAtMs,omitempty"`
	UpdatedAtMS   int64  `json:"updatedAtMs"`
	FinishedAtMS  int64  `json:"finishedAtMs,omitempty"`
}

// APIKeyAlias is the CPAMP alias record. APIKeyHash is the lowercase SHA-256
// hex digest of the complete CPA API key, never the complete key itself.
type APIKeyAlias struct {
	APIKeyHash  string `json:"apiKeyHash"`
	Alias       string `json:"alias"`
	UpdatedAtMS int64  `json:"updatedAtMs,omitempty"`
}

// APIKeysResponse is exported for callers that need to wrap test fixtures or
// inspect a raw response. ListAPIKeys returns the slice directly.
type APIKeysResponse struct {
	APIKeys []string `json:"api-keys"`
}

// AliasesResponse is the wire envelope returned by CPAMP.
type AliasesResponse struct {
	Items []APIKeyAlias `json:"items"`
}

// ModelListResponse is the OpenAI-compatible model catalog envelope.
type ModelListResponse struct {
	Object string  `json:"object,omitempty"`
	Data   []Model `json:"data"`
}

// Model contains the fields used by OpenAI-compatible model catalogs. CPAMP
// may add fields; unknown fields are ignored by design for forward compatibility.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object,omitempty"`
	Created int64  `json:"created,omitempty"`
	OwnedBy string `json:"owned_by,omitempty"`
}

// AuthFile is the sanitized credential identity needed to query CPAMP quota
// snapshots. It deliberately excludes auth-file contents and provider tokens.
type AuthFile struct {
	Name            string
	Provider        string
	AuthIndex       string
	AccountSnapshot string
	AccountID       string
	ProjectID       string
	Disabled        bool
}

// QuotaAccountTarget is CPAMP's credential identity envelope. Callers should
// only populate fields returned by the auth-file metadata endpoint.
type QuotaAccountTarget struct {
	AccountSnapshot       string `json:"account_snapshot,omitempty"`
	AuthFileSnapshot      string `json:"auth_file_snapshot,omitempty"`
	AuthProviderSnapshot  string `json:"auth_provider_snapshot,omitempty"`
	AuthProjectIDSnapshot string `json:"auth_project_id_snapshot,omitempty"`
	AuthIndex             string `json:"auth_index,omitempty"`
	Source                string `json:"source,omitempty"`
}

type QuotaQueryAccount struct {
	RowKey   string             `json:"row_key"`
	Provider string             `json:"provider"`
	Account  QuotaAccountTarget `json:"account"`
}

type QuotaSnapshotQueryRequest struct {
	Accounts        []QuotaQueryAccount `json:"accounts"`
	NowMS           int64               `json:"now_ms,omitempty"`
	IncludeInactive bool                `json:"include_inactive,omitempty"`
}

type QuotaSnapshotWindow struct {
	ProviderWindowID string   `json:"provider_window_id"`
	WindowKind       string   `json:"window_kind"`
	ModelScopeKind   string   `json:"model_scope_kind"`
	ObservedAtMS     int64    `json:"observed_at_ms"`
	CycleEndMS       *int64   `json:"cycle_end_ms,omitempty"`
	UsedPercent      *float64 `json:"used_percent,omitempty"`
	RemainingPercent *float64 `json:"remaining_percent,omitempty"`
	PlanType         string   `json:"plan_type,omitempty"`
	Stale            bool     `json:"stale"`
	Availability     string   `json:"availability,omitempty"`
}

type QuotaSnapshotItem struct {
	RowKey   string                `json:"row_key"`
	Provider string                `json:"provider"`
	Windows  []QuotaSnapshotWindow `json:"windows"`
}

type QuotaSnapshotQueryResponse struct {
	GeneratedAtMS int64               `json:"generated_at_ms"`
	Items         []QuotaSnapshotItem `json:"items"`
}

// AnalyticsRequest selects a CPAMP monitoring analytics query. FromMS and
// ToMS are Unix milliseconds and must be positive with FromMS < ToMS.
type AnalyticsRequest struct {
	FromMS           int64            `json:"from_ms"`
	ToMS             int64            `json:"to_ms"`
	NowMS            int64            `json:"now_ms,omitempty"`
	TimeZone         string           `json:"time_zone,omitempty"`
	SearchQuery      string           `json:"search_query,omitempty"`
	SearchAPIKeyHash string           `json:"search_api_key_hash,omitempty"`
	Filters          AnalyticsFilters `json:"filters,omitempty"`
	Include          AnalyticsInclude `json:"include,omitempty"`
}

// AnalyticsFilters contains the filters needed by the portal. Additional
// dimensions are included for administrator dashboards.
type AnalyticsFilters struct {
	Models           []string `json:"models,omitempty"`
	Providers        []string `json:"providers,omitempty"`
	Accounts         []string `json:"accounts,omitempty"`
	CredentialIDs    []string `json:"credential_ids,omitempty"`
	AuthFiles        []string `json:"auth_files,omitempty"`
	AuthIndices      []string `json:"auth_indices,omitempty"`
	APIKeyHashes     []string `json:"api_key_hashes,omitempty"`
	SourceHashes     []string `json:"source_hashes,omitempty"`
	ProjectIDs       []string `json:"project_ids,omitempty"`
	RequestTypes     []string `json:"request_types,omitempty"`
	HeaderErrorKinds []string `json:"header_error_kinds,omitempty"`
	HeaderErrorCodes []string `json:"header_error_codes,omitempty"`
	HeaderQuotaPlans []string `json:"header_quota_plans,omitempty"`
	HeaderTraceIDs   []string `json:"header_trace_ids,omitempty"`
	IncludeFailed    *bool    `json:"include_failed,omitempty"`
	FailedOnly       bool     `json:"failed_only,omitempty"`
	MinLatencyMS     int64    `json:"min_latency_ms,omitempty"`
	CacheStatus      string   `json:"cache_status,omitempty"`
}

// AnalyticsInclude controls which aggregates CPAMP computes.
type AnalyticsInclude struct {
	Summary            bool        `json:"summary,omitempty"`
	SummaryProfile     string      `json:"summary_profile,omitempty"`
	SummaryPercentiles bool        `json:"summary_percentiles,omitempty"`
	SummaryComparison  bool        `json:"summary_comparison,omitempty"`
	Timeline           bool        `json:"timeline,omitempty"`
	HourlyDistribution bool        `json:"hourly_distribution,omitempty"`
	ModelShare         bool        `json:"model_share,omitempty"`
	ChannelShare       bool        `json:"channel_share,omitempty"`
	ModelStats         bool        `json:"model_stats,omitempty"`
	FailureSources     bool        `json:"failure_sources,omitempty"`
	AccountStats       bool        `json:"account_stats,omitempty"`
	CredentialStats    bool        `json:"credential_stats,omitempty"`
	CredentialTimeline bool        `json:"credential_timeline,omitempty"`
	APIKeyTimeline     bool        `json:"api_key_timeline,omitempty"`
	APIKeyStats        bool        `json:"api_key_stats,omitempty"`
	FilterOptions      bool        `json:"filter_options,omitempty"`
	FilterSelectors    bool        `json:"filter_selectors,omitempty"`
	Heatmap            bool        `json:"heatmap,omitempty"`
	AnomalyPoints      bool        `json:"anomaly_points,omitempty"`
	TaskBuckets        bool        `json:"task_buckets,omitempty"`
	RecentFailures     int         `json:"recent_failures,omitempty"`
	EventsPage         *EventsPage `json:"events_page,omitempty"`
	Granularity        string      `json:"granularity,omitempty"`
}

// EventsPage asks CPAMP for sanitized per-request metadata. It never exposes
// request or response content.
type EventsPage struct {
	Limit    int    `json:"limit,omitempty"`
	BeforeMS *int64 `json:"before_ms,omitempty"`
	BeforeID *int64 `json:"before_id,omitempty"`
}

// AnalyticsResponse is the stable subset consumed by the portal. CPAMP's
// response can contain additional aggregates that are intentionally ignored.
type AnalyticsResponse struct {
	GeneratedAtMS  int64                      `json:"generated_at_ms"`
	Granularity    string                     `json:"granularity"`
	Summary        *UsageSummary              `json:"summary,omitempty"`
	Timeline       []UsageTimelinePoint       `json:"timeline,omitempty"`
	ModelStats     []ModelUsageStat           `json:"model_stats,omitempty"`
	ModelShare     []ModelShareRow            `json:"model_share,omitempty"`
	APIKeyStats    []APIKeyUsageStat          `json:"api_key_stats,omitempty"`
	APIKeyTimeline []APIKeyUsageTimelinePoint `json:"api_key_timeline,omitempty"`
	RecentFailures []RecentFailure            `json:"recent_failures,omitempty"`
	Events         *EventsResponse            `json:"events,omitempty"`
}

// UsageSummary contains request and token totals.
type UsageSummary struct {
	TotalCalls          int64    `json:"total_calls"`
	SuccessCalls        int64    `json:"success_calls"`
	FailureCalls        int64    `json:"failure_calls"`
	SuccessRate         float64  `json:"success_rate"`
	InputTokens         int64    `json:"input_tokens"`
	OutputTokens        int64    `json:"output_tokens"`
	CachedTokens        int64    `json:"cached_tokens"`
	CacheReadTokens     int64    `json:"cache_read_tokens"`
	CacheCreationTokens int64    `json:"cache_creation_tokens"`
	ReasoningTokens     int64    `json:"reasoning_tokens"`
	TotalTokens         int64    `json:"total_tokens"`
	TotalCost           float64  `json:"total_cost"`
	AverageLatencyMS    *float64 `json:"average_latency_ms"`
	P95LatencyMS        *float64 `json:"p95_latency_ms"`
}

// UsageTimelinePoint is one CPAMP timeline bucket.
type UsageTimelinePoint struct {
	BucketMS     int64    `json:"bucket_ms"`
	Label        string   `json:"label"`
	Calls        int64    `json:"calls"`
	Tokens       int64    `json:"tokens"`
	Success      int64    `json:"success"`
	Failure      int64    `json:"failure"`
	InputTokens  int64    `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
	TotalTokens  int64    `json:"total_tokens"`
	Cost         float64  `json:"cost"`
	AvgLatencyMS *float64 `json:"average_latency_ms"`
	SuccessRate  float64  `json:"success_rate"`
	FailureRate  float64  `json:"failure_rate"`
}

// ModelUsageStat is an aggregate by model.
type ModelUsageStat struct {
	Model        string  `json:"model"`
	Calls        int64   `json:"calls"`
	SuccessCalls int64   `json:"success_calls"`
	FailureCalls int64   `json:"failure_calls"`
	SuccessRate  float64 `json:"success_rate"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	TotalTokens  int64   `json:"total_tokens"`
	Cost         float64 `json:"cost"`
}

// ModelShareRow is a compact model share row.
type ModelShareRow struct {
	Model  string  `json:"model"`
	Calls  int64   `json:"calls"`
	Tokens int64   `json:"tokens"`
	Cost   float64 `json:"cost"`
}

// APIKeyUsageStat is a sanitized aggregate for one client API key hash.
type APIKeyUsageStat struct {
	ID              string  `json:"id"`
	APIKeyHash      string  `json:"api_key_hash"`
	Calls           int64   `json:"calls"`
	SuccessCalls    int64   `json:"success_calls"`
	FailureCalls    int64   `json:"failure_calls"`
	SuccessRate     float64 `json:"success_rate"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	TotalTokens     int64   `json:"total_tokens"`
	Cost            float64 `json:"cost"`
	LastSeenMS      int64   `json:"last_seen_ms"`
	AccountSnapshot string  `json:"account_snapshot,omitempty"`
}

// APIKeyUsageTimelinePoint is a per-key timeline bucket.
type APIKeyUsageTimelinePoint struct {
	APIKeyHash   string  `json:"api_key_hash"`
	BucketMS     int64   `json:"bucket_ms"`
	BucketLabel  string  `json:"bucket_label"`
	Calls        int64   `json:"calls"`
	Tokens       int64   `json:"tokens"`
	Success      int64   `json:"success"`
	Failure      int64   `json:"failure"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	TotalTokens  int64   `json:"total_tokens"`
	Cost         float64 `json:"cost"`
}

// RecentFailure is already sanitized by CPAMP; request and response bodies
// are intentionally not represented here.
type RecentFailure struct {
	TimestampMS    int64  `json:"timestamp_ms"`
	Model          string `json:"model"`
	APIKeyHash     string `json:"api_key_hash"`
	Endpoint       string `json:"endpoint"`
	DurationMS     *int64 `json:"duration_ms"`
	FailStatusCode *int64 `json:"fail_status_code,omitempty"`
	FailSummary    string `json:"fail_summary,omitempty"`
}

// EventsResponse contains sanitized request metadata returned by CPAMP.
type EventsResponse struct {
	Items        []EventRow `json:"items"`
	NextBeforeMS int64      `json:"next_before_ms"`
	NextBeforeID int64      `json:"next_before_id"`
	HasMore      bool       `json:"has_more"`
	TotalCount   int64      `json:"total_count"`
}

// EventRow contains no prompt, completion, or raw provider error body.
type EventRow struct {
	RequestID           string `json:"request_id,omitempty"`
	EventHash           string `json:"event_hash"`
	TimestampMS         int64  `json:"timestamp_ms"`
	Model               string `json:"model"`
	Endpoint            string `json:"endpoint"`
	Method              string `json:"method"`
	Path                string `json:"path"`
	AuthIndex           string `json:"auth_index"`
	Source              string `json:"source"`
	SourceHash          string `json:"source_hash"`
	APIKeyHash          string `json:"api_key_hash"`
	ReasoningEffort     string `json:"reasoning_effort,omitempty"`
	InputTokens         int64  `json:"input_tokens"`
	OutputTokens        int64  `json:"output_tokens"`
	CachedTokens        int64  `json:"cached_tokens"`
	CacheReadTokens     int64  `json:"cache_read_tokens"`
	CacheCreationTokens int64  `json:"cache_creation_tokens"`
	ReasoningTokens     int64  `json:"reasoning_tokens"`
	TotalTokens         int64  `json:"total_tokens"`
	LatencyMS           *int64 `json:"latency_ms"`
	Failed              bool   `json:"failed"`
	FailStatusCode      *int64 `json:"fail_status_code,omitempty"`
	FailSummary         string `json:"fail_summary,omitempty"`
}

// HashAPIKey returns the lowercase SHA-256 hex digest CPAMP uses for aliases
// and monitoring filters. The input is hashed exactly as supplied.
func HashAPIKey(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:])
}

// APIKeyHash is an alias for HashAPIKey for callers that prefer noun-like
// naming.
func APIKeyHash(apiKey string) string { return HashAPIKey(apiKey) }

// Health queries the unauthenticated CPAMP health endpoint.
func (c *Client) Health(ctx context.Context) (HealthResponse, error) {
	body, err := c.do(ctx, http.MethodGet, pathHealth, nil, c.adminHeader)
	if err != nil {
		return HealthResponse{}, err
	}
	var result HealthResponse
	if err := decodeJSON(pathHealth, body, &result); err != nil {
		return HealthResponse{}, err
	}
	if !result.OK {
		return HealthResponse{}, errors.New("cpamp health check returned ok=false")
	}
	return result, nil
}

// Status queries CPAMP's authenticated system status endpoint.
func (c *Client) Status(ctx context.Context) (StatusResponse, error) {
	body, err := c.do(ctx, http.MethodGet, pathStatus, nil, c.adminHeader)
	if err != nil {
		return StatusResponse{}, err
	}
	var result StatusResponse
	if err := decodeJSON(pathStatus, body, &result); err != nil {
		return StatusResponse{}, err
	}
	return result, nil
}

// GetHealth is a naming-compatible alias for Health.
func (c *Client) GetHealth(ctx context.Context) (HealthResponse, error) {
	return c.Health(ctx)
}

// GetStatus is a naming-compatible alias for Status.
func (c *Client) GetStatus(ctx context.Context) (StatusResponse, error) {
	return c.Status(ctx)
}

// ListAPIKeys reads the current CPA client API key list through CPAMP's proxy.
func (c *Client) ListAPIKeys(ctx context.Context) ([]string, error) {
	body, err := c.do(ctx, http.MethodGet, pathAPIKeys, nil, c.adminHeader)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		APIKeys json.RawMessage `json:"api-keys"`
	}
	if err := decodeJSON(pathAPIKeys, body, &envelope); err != nil {
		return nil, err
	}
	if len(envelope.APIKeys) == 0 || bytes.Equal(bytes.TrimSpace(envelope.APIKeys), []byte("null")) {
		return []string{}, nil
	}
	var keys []string
	if err := json.Unmarshal(envelope.APIKeys, &keys); err != nil {
		return nil, fmt.Errorf("cpamp %s returned an invalid API key list", pathAPIKeys)
	}
	for _, key := range keys {
		if strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("cpamp %s returned an empty API key", pathAPIKeys)
		}
	}
	return append([]string(nil), keys...), nil
}

// GetAPIKeys is a naming-compatible alias for ListAPIKeys.
func (c *Client) GetAPIKeys(ctx context.Context) ([]string, error) {
	return c.ListAPIKeys(ctx)
}

// ListAuthFiles reads credential metadata through CPAMP's CPA management
// proxy. Unknown fields, including any future secret-bearing fields, are
// ignored and never retained in the returned value.
func (c *Client) ListAuthFiles(ctx context.Context) ([]AuthFile, error) {
	body, err := c.do(ctx, http.MethodGet, pathAuthFiles, nil, c.adminHeader)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Files []map[string]json.RawMessage `json:"files"`
	}
	if err := decodeJSON(pathAuthFiles, body, &envelope); err != nil {
		return nil, err
	}
	files := make([]AuthFile, 0, len(envelope.Files))
	for _, raw := range envelope.Files {
		name := rawText(raw, "physicalName", "physical_name", "name")
		provider := strings.ToLower(rawText(raw, "provider", "type"))
		if name == "" || provider == "" {
			continue
		}
		accountID := rawCodexAccountID(raw)
		files = append(files, AuthFile{
			Name:            name,
			Provider:        provider,
			AuthIndex:       rawText(raw, "authIndex", "auth_index"),
			AccountSnapshot: rawText(raw, "accountSnapshot", "account_snapshot", "account", "email"),
			AccountID:       accountID,
			ProjectID:       rawText(raw, "projectId", "project_id"),
			Disabled:        rawBool(raw, "disabled"),
		})
	}
	return files, nil
}

// rawCodexAccountID mirrors CPA-Manager-Plus' Codex identity resolution. Some
// auth-file rows expose chatgpt_account_id directly, while others only carry it
// as a claim in id_token. Only the non-secret account ID is retained.
func rawCodexAccountID(record map[string]json.RawMessage) string {
	keys := []string{"chatgpt_account_id", "chatgptAccountId", "account_id", "accountId"}
	if value := rawText(record, keys...); value != "" {
		return value
	}
	for _, parent := range []string{"metadata", "attributes"} {
		if value := rawNestedText(record, parent, keys...); value != "" {
			return value
		}
	}
	for _, raw := range codexIDTokenCandidates(record) {
		if value := accountIDFromIDToken(raw, keys); value != "" {
			return value
		}
	}
	return ""
}

func codexIDTokenCandidates(record map[string]json.RawMessage) []json.RawMessage {
	candidates := make([]json.RawMessage, 0, 6)
	for _, key := range []string{"id_token", "idToken"} {
		if raw := record[key]; len(raw) > 0 {
			candidates = append(candidates, raw)
		}
	}
	for _, parent := range []string{"metadata", "attributes"} {
		var nested map[string]json.RawMessage
		if raw := record[parent]; len(raw) == 0 || json.Unmarshal(raw, &nested) != nil {
			continue
		}
		for _, key := range []string{"id_token", "idToken"} {
			if raw := nested[key]; len(raw) > 0 {
				candidates = append(candidates, raw)
			}
		}
	}
	return candidates
}

func accountIDFromIDToken(raw json.RawMessage, keys []string) string {
	var claims map[string]json.RawMessage
	if len(raw) > 0 && raw[0] == '{' && json.Unmarshal(raw, &claims) == nil {
		return rawText(claims, keys...)
	}
	var token string
	if json.Unmarshal(raw, &token) != nil {
		return ""
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 64<<10 {
		return ""
	}
	if strings.HasPrefix(token, "{") && json.Unmarshal([]byte(token), &claims) == nil {
		return rawText(claims, keys...)
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(payload) > 64<<10 || json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return rawText(claims, keys...)
}

func rawNestedText(record map[string]json.RawMessage, parent string, keys ...string) string {
	value, ok := record[parent]
	if !ok || len(value) == 0 {
		return ""
	}
	var nested map[string]json.RawMessage
	if json.Unmarshal(value, &nested) != nil {
		return ""
	}
	return rawText(nested, keys...)
}

// QueryQuotaSnapshots reads CPAMP's persisted, allowlisted provider quota
// observations. It does not query provider tokens or auth-file contents.
func (c *Client) QueryQuotaSnapshots(ctx context.Context, req QuotaSnapshotQueryRequest) (QuotaSnapshotQueryResponse, error) {
	if len(req.Accounts) == 0 {
		return QuotaSnapshotQueryResponse{}, errors.New("cpamp quota query requires accounts")
	}
	if len(req.Accounts) > 200 {
		return QuotaSnapshotQueryResponse{}, errors.New("cpamp quota query supports at most 200 accounts")
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return QuotaSnapshotQueryResponse{}, fmt.Errorf("cpamp %s request encoding failed", pathQuotaQuery)
	}
	body, err := c.do(ctx, http.MethodPost, pathQuotaQuery, payload, c.adminHeader)
	if err != nil {
		return QuotaSnapshotQueryResponse{}, err
	}
	var result QuotaSnapshotQueryResponse
	if err := decodeJSON(pathQuotaQuery, body, &result); err != nil {
		return QuotaSnapshotQueryResponse{}, err
	}
	if result.Items == nil {
		result.Items = []QuotaSnapshotItem{}
	}
	return result, nil
}

func rawText(record map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		value, ok := record[key]
		if !ok || len(value) == 0 {
			continue
		}
		var text string
		if json.Unmarshal(value, &text) == nil {
			if text = strings.TrimSpace(text); text != "" {
				return text
			}
		}
		var number json.Number
		if json.Unmarshal(value, &number) == nil {
			if text = strings.TrimSpace(number.String()); text != "" {
				return text
			}
		}
	}
	return ""
}

func rawBool(record map[string]json.RawMessage, key string) bool {
	value, ok := record[key]
	if !ok {
		return false
	}
	var result bool
	return json.Unmarshal(value, &result) == nil && result
}

// ReplaceAPIKeys replaces the entire CPA API key list. Callers should use
// ListAPIKeys and merge their desired change before calling this method so
// keys managed manually in CPAMP are preserved.
func (c *Client) ReplaceAPIKeys(ctx context.Context, keys []string) error {
	validated := make([]string, len(keys))
	for i, key := range keys {
		if strings.TrimSpace(key) == "" {
			return errors.New("cpamp API key must not be empty")
		}
		validated[i] = key
	}
	if validated == nil {
		validated = []string{}
	}
	payload, err := json.Marshal(validated)
	if err != nil {
		return fmt.Errorf("cpamp %s request encoding failed", pathAPIKeys)
	}
	_, err = c.do(ctx, http.MethodPut, pathAPIKeys, payload, c.adminHeader)
	return err
}

// PutAPIKeys is a naming-compatible alias for ReplaceAPIKeys.
func (c *Client) PutAPIKeys(ctx context.Context, keys []string) error {
	return c.ReplaceAPIKeys(ctx, keys)
}

// AddAPIKey performs a read-merge-replace append and verifies that CPA
// accepted the resulting list. Existing identical keys are treated as success.
func (c *Client) AddAPIKey(ctx context.Context, apiKey string) error {
	if strings.TrimSpace(apiKey) == "" {
		return errors.New("cpamp API key must not be empty")
	}
	keys, err := c.ListAPIKeys(ctx)
	if err != nil {
		return err
	}
	for _, current := range keys {
		if current == apiKey {
			return nil
		}
	}
	keys = append(keys, apiKey)
	if err := c.ReplaceAPIKeys(ctx, keys); err != nil {
		return err
	}
	confirmed, err := c.ListAPIKeys(ctx)
	if err != nil {
		return err
	}
	for _, current := range confirmed {
		if current == apiKey {
			return nil
		}
	}
	return errors.New("cpamp API key update was not confirmed")
}

// RemoveAPIKey performs an idempotent read-merge-replace removal and verifies
// that CPA no longer reports the key. It succeeds when the key is already gone.
func (c *Client) RemoveAPIKey(ctx context.Context, apiKey string) error {
	if strings.TrimSpace(apiKey) == "" {
		return errors.New("cpamp API key must not be empty")
	}
	keys, err := c.ListAPIKeys(ctx)
	if err != nil {
		return err
	}
	filtered := make([]string, 0, len(keys))
	found := false
	for _, current := range keys {
		if current == apiKey {
			found = true
			continue
		}
		filtered = append(filtered, current)
	}
	if !found {
		return nil
	}
	if err := c.ReplaceAPIKeys(ctx, filtered); err != nil {
		return err
	}
	confirmed, err := c.ListAPIKeys(ctx)
	if err != nil {
		return err
	}
	for _, current := range confirmed {
		if current == apiKey {
			return errors.New("cpamp API key removal was not confirmed")
		}
	}
	return nil
}

// DeleteAPIKey asks the CPA management endpoint to remove all matching values.
// RemoveAPIKey is preferable for portal lifecycle operations because it
// verifies the result and preserves a read-merge-replace workflow.
func (c *Client) DeleteAPIKey(ctx context.Context, apiKey string) error {
	if strings.TrimSpace(apiKey) == "" {
		return errors.New("cpamp API key must not be empty")
	}
	query := url.Values{}
	query.Set("value", apiKey)
	_, err := c.do(ctx, http.MethodDelete, pathAPIKeys+"?"+query.Encode(), nil, c.adminHeader)
	return err
}

// DeleteAPIKeyAt asks CPA to remove a key by its current index.
func (c *Client) DeleteAPIKeyAt(ctx context.Context, index int) error {
	if index < 0 {
		return errors.New("cpamp API key index must not be negative")
	}
	query := url.Values{}
	query.Set("index", strconv.Itoa(index))
	_, err := c.do(ctx, http.MethodDelete, pathAPIKeys+"?"+query.Encode(), nil, c.adminHeader)
	return err
}

// ListAliases returns CPAMP's SHA-256 API-key alias records.
func (c *Client) ListAliases(ctx context.Context) ([]APIKeyAlias, error) {
	body, err := c.do(ctx, http.MethodGet, pathAliases, nil, c.adminHeader)
	if err != nil {
		return nil, err
	}
	var envelope AliasesResponse
	if err := decodeJSON(pathAliases, body, &envelope); err != nil {
		return nil, err
	}
	if envelope.Items == nil {
		envelope.Items = []APIKeyAlias{}
	}
	for i := range envelope.Items {
		if err := validateHash(envelope.Items[i].APIKeyHash); err != nil {
			return nil, fmt.Errorf("cpamp %s returned an invalid alias hash", pathAliases)
		}
	}
	return append([]APIKeyAlias(nil), envelope.Items...), nil
}

// GetAPIKeyAliases is a naming-compatible alias for ListAliases.
func (c *Client) GetAPIKeyAliases(ctx context.Context) ([]APIKeyAlias, error) {
	return c.ListAliases(ctx)
}

// ReplaceAliases replaces CPAMP's alias list. activeHashes may be supplied
// when orphan cleanup is requested; hashes are normalized to lowercase.
func (c *Client) ReplaceAliases(ctx context.Context, aliases []APIKeyAlias, activeHashes []string, allowOrphanCleanup bool) ([]APIKeyAlias, error) {
	items := make([]APIKeyAlias, len(aliases))
	seen := make(map[string]struct{}, len(aliases))
	for i, item := range aliases {
		hash, err := normalizeHash(item.APIKeyHash)
		if err != nil {
			return nil, errors.New("cpamp alias API key hash must be a SHA-256 hex digest")
		}
		alias := strings.TrimSpace(item.Alias)
		if alias == "" {
			return nil, errors.New("cpamp alias must not be empty")
		}
		if len(alias) > 256 {
			return nil, errors.New("cpamp alias is too long")
		}
		if _, exists := seen[hash]; exists {
			return nil, errors.New("cpamp alias API key hashes must be unique")
		}
		seen[hash] = struct{}{}
		items[i] = item
		items[i].APIKeyHash = hash
		items[i].Alias = alias
	}
	active := make([]string, len(activeHashes))
	for i, hash := range activeHashes {
		normalized, err := normalizeHash(hash)
		if err != nil {
			return nil, errors.New("cpamp active API key hash must be a SHA-256 hex digest")
		}
		active[i] = normalized
	}
	if items == nil {
		items = []APIKeyAlias{}
	}
	payload := struct {
		Items                   []APIKeyAlias `json:"items"`
		ActiveAPIKeyHashes      []string      `json:"activeApiKeyHashes,omitempty"`
		AllowOrphanAliasCleanup bool          `json:"allowOrphanAliasCleanup,omitempty"`
	}{
		Items:                   items,
		ActiveAPIKeyHashes:      active,
		AllowOrphanAliasCleanup: allowOrphanCleanup,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("cpamp %s request encoding failed", pathAliases)
	}
	body, err := c.do(ctx, http.MethodPut, pathAliases, encoded, c.adminHeader)
	if err != nil {
		return nil, err
	}
	var result AliasesResponse
	if err := decodeJSON(pathAliases, body, &result); err != nil {
		return nil, err
	}
	if result.Items == nil {
		result.Items = []APIKeyAlias{}
	}
	return result.Items, nil
}

// PutAPIKeyAliases replaces aliases without orphan-cleanup options. Use
// ReplaceAliases when the manager's active-hash cleanup controls are needed.
func (c *Client) PutAPIKeyAliases(ctx context.Context, aliases []APIKeyAlias) ([]APIKeyAlias, error) {
	return c.ReplaceAliases(ctx, aliases, nil, false)
}

// UpsertAliasForKey reads, merges, and saves one alias using the complete API
// key only in memory. The complete key is never sent to CPAMP's alias API.
func (c *Client) UpsertAliasForKey(ctx context.Context, apiKey, alias string) ([]APIKeyAlias, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("cpamp API key must not be empty")
	}
	return c.UpsertAlias(ctx, HashAPIKey(apiKey), alias)
}

// UpsertAlias reads, merges, and saves one hash-based alias.
func (c *Client) UpsertAlias(ctx context.Context, apiKeyHash, alias string) ([]APIKeyAlias, error) {
	hash, err := normalizeHash(apiKeyHash)
	if err != nil {
		return nil, errors.New("cpamp alias API key hash must be a SHA-256 hex digest")
	}
	aliases, err := c.ListAliases(ctx)
	if err != nil {
		return nil, err
	}
	found := false
	for i := range aliases {
		if strings.EqualFold(aliases[i].APIKeyHash, hash) {
			aliases[i].APIKeyHash = hash
			aliases[i].Alias = alias
			found = true
			break
		}
	}
	if !found {
		aliases = append(aliases, APIKeyAlias{APIKeyHash: hash, Alias: alias})
	}
	return c.ReplaceAliases(ctx, aliases, nil, false)
}

// PutAPIKeyAlias upserts one SHA-256 hash alias while preserving other aliases.
func (c *Client) PutAPIKeyAlias(ctx context.Context, apiKeyHash, alias string) ([]APIKeyAlias, error) {
	return c.UpsertAlias(ctx, apiKeyHash, alias)
}

// DeleteAlias removes an alias by its SHA-256 hash.
func (c *Client) DeleteAlias(ctx context.Context, apiKeyHash string) error {
	hash, err := normalizeHash(apiKeyHash)
	if err != nil {
		return errors.New("cpamp alias API key hash must be a SHA-256 hex digest")
	}
	_, err = c.do(ctx, http.MethodDelete, pathAliases+"/"+url.PathEscape(hash), nil, c.adminHeader)
	return err
}

// DeleteAPIKeyAlias is a naming-compatible alias for DeleteAlias.
func (c *Client) DeleteAPIKeyAlias(ctx context.Context, apiKeyHash string) error {
	return c.DeleteAlias(ctx, apiKeyHash)
}

// DeleteAliasForKey removes the alias for a complete API key without sending
// the complete key to CPAMP.
func (c *Client) DeleteAliasForKey(ctx context.Context, apiKey string) error {
	if strings.TrimSpace(apiKey) == "" {
		return errors.New("cpamp API key must not be empty")
	}
	return c.DeleteAlias(ctx, HashAPIKey(apiKey))
}

// ListModels requests CPAMP's OpenAI-compatible model proxy. Because CPAMP
// forwards the caller Authorization to CPA, ModelAPIKey should normally be a
// CPA client API key, not the CPAMP admin key.
func (c *Client) ListModels(ctx context.Context) (ModelListResponse, error) {
	return c.ListModelsWithAPIKey(ctx, strings.TrimPrefix(c.modelHeader, "Bearer "))
}

// ListModelsWithAPIKey requests the model list with an explicit CPA API key.
func (c *Client) ListModelsWithAPIKey(ctx context.Context, apiKey string) (ModelListResponse, error) {
	if strings.TrimSpace(apiKey) == "" {
		return ModelListResponse{}, errors.New("CPA API key is required for model list")
	}
	body, err := c.do(ctx, http.MethodGet, pathModelCatalog, nil, normalizeBearer(apiKey))
	if err != nil {
		return ModelListResponse{}, err
	}
	var result ModelListResponse
	if err := decodeJSON(pathModelCatalog, body, &result); err != nil {
		return ModelListResponse{}, err
	}
	if result.Data == nil {
		result.Data = []Model{}
	}
	return result, nil
}

// GetModels is a naming-compatible alias for ListModels.
func (c *Client) GetModels(ctx context.Context) (ModelListResponse, error) {
	return c.ListModels(ctx)
}

// Analytics queries CPAMP's aggregate monitoring API. To scope a user's
// dashboard, set req.Filters.APIKeyHashes to the user's SHA-256 key hashes.
func (c *Client) Analytics(ctx context.Context, req AnalyticsRequest) (AnalyticsResponse, error) {
	if req.FromMS <= 0 || req.ToMS <= 0 || req.FromMS >= req.ToMS {
		return AnalyticsResponse{}, errors.New("cpamp analytics requires from_ms < to_ms")
	}
	if req.SearchAPIKeyHash != "" {
		hash, err := normalizeHash(req.SearchAPIKeyHash)
		if err != nil {
			return AnalyticsResponse{}, errors.New("cpamp analytics API key hash must be a SHA-256 hex digest")
		}
		req.SearchAPIKeyHash = hash
	}
	for i, hash := range req.Filters.APIKeyHashes {
		normalized, err := normalizeHash(hash)
		if err != nil {
			return AnalyticsResponse{}, errors.New("cpamp analytics API key hash must be a SHA-256 hex digest")
		}
		req.Filters.APIKeyHashes[i] = normalized
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return AnalyticsResponse{}, fmt.Errorf("cpamp %s request encoding failed", pathAnalytics)
	}
	body, err := c.do(ctx, http.MethodPost, pathAnalytics, payload, c.adminHeader)
	if err != nil {
		return AnalyticsResponse{}, err
	}
	var result AnalyticsResponse
	if err := decodeJSON(pathAnalytics, body, &result); err != nil {
		return AnalyticsResponse{}, err
	}
	return result, nil
}

func (c *Client) do(ctx context.Context, method, requestPath string, body []byte, authorization string) ([]byte, error) {
	if c == nil || c.baseURL == nil || c.httpClient == nil {
		return nil, errors.New("cpamp client is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	operation := safeOperation(requestPath)
	requestURL, err := c.requestURL(requestPath)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, requestURL, reader)
	if err != nil {
		return nil, fmt.Errorf("cpamp request construction failed")
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	callCtx := request.Context()
	if c.timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(callCtx, c.timeout)
		defer cancel()
		request = request.WithContext(callCtx)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, &RequestError{Operation: operation, Err: err}
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, maxResponseSize+1)
	data, readErr := io.ReadAll(limited)
	if readErr != nil {
		return nil, fmt.Errorf("cpamp %s response read failed", operation)
	}
	if len(data) > maxResponseSize {
		return nil, fmt.Errorf("cpamp %s response is too large", operation)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, &HTTPError{Operation: operation, StatusCode: response.StatusCode}
	}
	return data, nil
}

func safeOperation(requestPath string) string {
	if question := strings.IndexByte(requestPath, '?'); question >= 0 {
		return requestPath[:question]
	}
	return requestPath
}

func (c *Client) requestURL(requestPath string) (string, error) {
	if requestPath == "" || requestPath[0] != '/' {
		return "", errors.New("cpamp request path must be absolute")
	}
	copyURL := *c.baseURL
	copyURL.Path = strings.TrimRight(c.baseURL.Path, "/") + requestPath
	copyURL.RawPath = ""
	if question := strings.IndexByte(requestPath, '?'); question >= 0 {
		copyURL.Path = strings.TrimRight(c.baseURL.Path, "/") + requestPath[:question]
		copyURL.RawQuery = requestPath[question+1:]
	} else {
		copyURL.RawQuery = ""
	}
	copyURL.Fragment = ""
	return copyURL.String(), nil
}

func decodeJSON(operation string, data []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("cpamp %s returned invalid JSON", operation)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("cpamp %s returned multiple JSON values", operation)
	}
	return nil
}

func normalizeBearer(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	if len(trimmed) >= len("Bearer ") && strings.EqualFold(trimmed[:len("Bearer ")], "Bearer ") {
		trimmed = strings.TrimSpace(trimmed[len("Bearer "):])
	}
	if trimmed == "" {
		return ""
	}
	return "Bearer " + trimmed
}

func validateHash(value string) error {
	_, err := normalizeHash(value)
	return err
}

func normalizeHash(value string) (string, error) {
	hash := strings.ToLower(strings.TrimSpace(value))
	if len(hash) != sha256.Size*2 {
		return "", errors.New("invalid SHA-256 hash")
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return "", errors.New("invalid SHA-256 hash")
	}
	return hash, nil
}
