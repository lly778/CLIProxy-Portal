package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/security"
	"cliproxy-portal/internal/store"
)

type Keys struct {
	Store           *store.Store
	CPAMP           cpamp.API
	Now             func() time.Time
	CacheTTL        time.Duration
	cacheMu         sync.Mutex
	cache           map[string]analyticsCache
	quota           quotaPoolCache
	refreshMu       sync.Mutex
	refresh         QuotaRefreshStatus
	refreshedQuota  map[string][]cpamp.QuotaSnapshotWindow
	reconcileMu     sync.Mutex
	lastSeenSyncAt  time.Time
	RefreshCooldown time.Duration
	RefreshTimeout  time.Duration
}

type analyticsCache struct {
	value     cpamp.AnalyticsResponse
	expiresAt time.Time
}

type quotaPoolCache struct {
	value     UpstreamQuotaPool
	expiresAt time.Time
}

// UpstreamQuotaPool is a privacy-preserving summary of CPAMP's persisted
// provider quota snapshots. Percentages are account-weighted because provider
// plans do not expose a common absolute token capacity.
type UpstreamQuotaPool struct {
	Provider       string
	TotalAccounts  int
	UsableAccounts int
	Groups         []UpstreamQuotaGroup
	UnknownCount   int
}

type UpstreamQuotaGroup struct {
	PlanType          string
	Period            string
	KnownAccounts     int
	AvailableAccounts int
	RemainingPercent  int
	NextResetAt       time.Time
	ObservedAt        time.Time
	Estimated         bool
}

// UpstreamAccount is the sanitized administrator-facing identity and state of
// one Codex credential. It never contains provider tokens or auth-file data.
type UpstreamAccount struct {
	ID        string
	Name      string
	Account   string
	Provider  string
	AuthIndex string
	Disabled  bool
}

// UpstreamAccountQuota contains one credential's current provider quota
// windows. It is keyed by the same opaque account ID used for status changes.
type UpstreamAccountQuota struct {
	AccountID string
	Windows   []UpstreamAccountQuotaWindow
}

type UpstreamAccountQuotaWindow struct {
	Period           string
	PlanType         string
	RemainingPercent int
	ResetAt          time.Time
	ObservedAt       time.Time
}

// OAuthModelSetting is one global OAuth model routing switch. A model disabled
// by a wildcard rule is read-only here because removing that rule would affect
// other models as well.
type OAuthModelSetting struct {
	ID           string
	DisplayName  string
	Enabled      bool
	WildcardRule string
}

var (
	ErrQuotaRefreshRunning  = errors.New("额度正在刷新")
	ErrQuotaRefreshCooldown = errors.New("额度刚刚刷新过，请稍后再试")
)

type QuotaRefreshStatus struct {
	Running        bool
	LastStartedAt  time.Time
	LastFinishedAt time.Time
	NextAllowedAt  time.Time
	Succeeded      int
	Failed         int
	Message        string
}

func NewKeys(st *store.Store, api cpamp.API) *Keys {
	return &Keys{
		Store: st, CPAMP: api, Now: func() time.Time { return time.Now().UTC() },
		CacheTTL: 2 * time.Minute, RefreshCooldown: time.Minute, RefreshTimeout: 3 * time.Minute,
		cache:          make(map[string]analyticsCache),
		refreshedQuota: make(map[string][]cpamp.QuotaSnapshotWindow),
	}
}

// StartQuotaRefresh starts one shared background refresh for every approved
// portal user. The lock and cooldown are global to this portal process so
// concurrent clicks cannot fan out into duplicate provider requests.
func (k *Keys) StartQuotaRefresh() (QuotaRefreshStatus, error) {
	typed, ok := k.CPAMP.(interface {
		ListAuthFiles(context.Context) ([]cpamp.AuthFile, error)
		RefreshCodexQuotaSnapshot(context.Context, cpamp.AuthFile, time.Time) ([]cpamp.QuotaSnapshotWindow, error)
	})
	if !ok {
		return QuotaRefreshStatus{}, errors.New("CPAMP 客户端不支持手动刷新额度")
	}
	now := k.Now()
	k.refreshMu.Lock()
	if k.refresh.Running {
		status := k.refresh
		k.refreshMu.Unlock()
		return status, ErrQuotaRefreshRunning
	}
	if !k.refresh.NextAllowedAt.IsZero() && now.Before(k.refresh.NextAllowedAt) {
		status := k.refresh
		k.refreshMu.Unlock()
		return status, ErrQuotaRefreshCooldown
	}
	k.refresh = QuotaRefreshStatus{
		Running:       true,
		LastStartedAt: now,
		NextAllowedAt: now.Add(k.RefreshCooldown),
		Message:       "正在从上游读取额度，完成后会自动更新",
	}
	status := k.refresh
	k.refreshMu.Unlock()

	timeout := k.RefreshTimeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	go k.runQuotaRefresh(typed, timeout)
	return status, nil
}

func (k *Keys) QuotaRefreshStatus() QuotaRefreshStatus {
	k.refreshMu.Lock()
	defer k.refreshMu.Unlock()
	return k.refresh
}

func (k *Keys) runQuotaRefresh(client interface {
	ListAuthFiles(context.Context) ([]cpamp.AuthFile, error)
	RefreshCodexQuotaSnapshot(context.Context, cpamp.AuthFile, time.Time) ([]cpamp.QuotaSnapshotWindow, error)
}, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	files, err := client.ListAuthFiles(ctx)
	if err != nil {
		k.finishQuotaRefresh(0, 0, "无法读取上游账号，请稍后再试")
		return
	}
	eligible := make([]cpamp.AuthFile, 0, len(files))
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		if file.Disabled || !strings.EqualFold(strings.TrimSpace(file.Provider), "codex") || strings.TrimSpace(file.Name) == "" {
			continue
		}
		identity := strings.TrimSpace(file.Name) + "\x00" + strings.TrimSpace(file.AuthIndex)
		if _, exists := seen[identity]; exists {
			continue
		}
		seen[identity] = struct{}{}
		eligible = append(eligible, file)
		if len(eligible) == 200 {
			break
		}
	}
	if len(eligible) == 0 {
		k.finishQuotaRefresh(0, 0, "没有可刷新的 Codex 账号")
		return
	}
	succeeded := 0
	for _, file := range eligible {
		if ctx.Err() != nil {
			break
		}
		windows, err := client.RefreshCodexQuotaSnapshot(ctx, file, k.Now())
		if err == nil {
			k.putRefreshedQuota(strings.TrimSpace(file.Name)+"\x00"+strings.TrimSpace(file.AuthIndex), windows)
			succeeded++
		}
	}
	failed := len(eligible) - succeeded
	if succeeded > 0 {
		k.invalidateQuota()
	}
	message := fmt.Sprintf("已刷新 %d 个账号", succeeded)
	if failed > 0 {
		message = fmt.Sprintf("已刷新 %d 个账号，%d 个失败", succeeded, failed)
	}
	if succeeded == 0 {
		message = "额度刷新失败，请稍后再试"
	}
	k.finishQuotaRefresh(succeeded, failed, message)
}

func (k *Keys) putRefreshedQuota(rowKey string, windows []cpamp.QuotaSnapshotWindow) {
	k.refreshMu.Lock()
	defer k.refreshMu.Unlock()
	if k.refreshedQuota == nil {
		k.refreshedQuota = make(map[string][]cpamp.QuotaSnapshotWindow)
	}
	k.refreshedQuota[rowKey] = append([]cpamp.QuotaSnapshotWindow(nil), windows...)
}

func (k *Keys) mergeRefreshedQuota(items []cpamp.QuotaSnapshotItem, now time.Time) []cpamp.QuotaSnapshotItem {
	k.refreshMu.Lock()
	defer k.refreshMu.Unlock()
	if len(k.refreshedQuota) == 0 {
		return items
	}
	byRowKey := make(map[string]int, len(items))
	for index := range items {
		byRowKey[items[index].RowKey] = index
	}
	for rowKey, windows := range k.refreshedQuota {
		active := make([]cpamp.QuotaSnapshotWindow, 0, len(windows))
		for _, window := range windows {
			if window.CycleEndMS == nil || *window.CycleEndMS > now.UnixMilli() {
				active = append(active, window)
			}
		}
		if len(active) == 0 {
			delete(k.refreshedQuota, rowKey)
			continue
		}
		if index, ok := byRowKey[rowKey]; ok {
			items[index].Windows = append(items[index].Windows, active...)
			continue
		}
		items = append(items, cpamp.QuotaSnapshotItem{RowKey: rowKey, Provider: "codex", Windows: active})
	}
	return items
}

func (k *Keys) finishQuotaRefresh(succeeded, failed int, message string) {
	k.refreshMu.Lock()
	defer k.refreshMu.Unlock()
	k.refresh.Running = false
	k.refresh.LastFinishedAt = k.Now()
	k.refresh.Succeeded = succeeded
	k.refresh.Failed = failed
	k.refresh.Message = message
}

func (k *Keys) invalidateQuota() {
	k.cacheMu.Lock()
	defer k.cacheMu.Unlock()
	k.quota = quotaPoolCache{}
}

func (k *Keys) Issue(ctx context.Context, user domain.User) (string, domain.APIKey, error) {
	if user.Status != domain.StatusApproved {
		return "", domain.APIKey{}, errors.New("账号尚未批准或已停用")
	}
	now := k.Now()
	_, oldErr := k.Store.ActiveKey(ctx, user.ID)
	if oldErr == nil {
		return "", domain.APIKey{}, errors.New("当前已有有效 API Key")
	}
	if !store.IsNotFound(oldErr) {
		return "", domain.APIKey{}, oldErr
	}
	secret, err := security.NewAPIKey()
	if err != nil {
		return "", domain.APIKey{}, err
	}
	hash := cpamp.HashAPIKey(secret)
	id, err := security.NewID("key_")
	if err != nil {
		return "", domain.APIKey{}, err
	}
	alias := security.Alias(user.Name, user.Phone, user.ID)
	next := domain.APIKey{ID: id, UserID: user.ID, Hash: hash, LastFour: security.LastFour(secret), Alias: alias, Status: "active", IssuedAt: now}
	if err = k.CPAMP.AddAPIKey(ctx, secret); err != nil {
		return "", domain.APIKey{}, err
	}
	if err = k.upsertAlias(ctx, hash, alias); err != nil {
		_ = k.CPAMP.RemoveAPIKey(ctx, secret)
		return "", domain.APIKey{}, fmt.Errorf("写入 Key 别名失败: %w", err)
	}
	if err = k.Store.SwapActiveKey(ctx, "", next); err != nil {
		_ = k.CPAMP.RemoveAPIKey(ctx, secret)
		_ = k.CPAMP.DeleteAlias(ctx, hash)
		return "", domain.APIKey{}, fmt.Errorf("保存 Key 状态失败: %w", err)
	}
	_ = k.Store.DeleteModelTestResults(ctx, user.ID)
	return secret, next, nil
}

func (k *Keys) Revoke(ctx context.Context, user domain.User) (bool, error) {
	active, err := k.Store.ActiveKey(ctx, user.ID)
	if err != nil {
		if store.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	raw, err := k.rawKeyByHash(ctx, active.Hash)
	if err != nil {
		if errors.Is(err, errKeyMissing) {
			_ = k.Store.SetKeyStatus(ctx, active.Hash, "external_revoked")
			_ = k.CPAMP.DeleteAlias(ctx, active.Hash)
			_ = k.Store.DeleteModelTestResults(ctx, user.ID)
			return false, nil
		}
		return true, err
	}
	if err = k.CPAMP.RemoveAPIKey(ctx, raw); err != nil {
		return true, err
	}
	if err = k.CPAMP.DeleteAlias(ctx, active.Hash); err != nil {
		return true, err
	}
	if err := k.Store.SetKeyStatus(ctx, active.Hash, "revoked"); err != nil {
		return false, err
	}
	_ = k.Store.DeleteModelTestResults(ctx, user.ID)
	return false, nil
}

var errKeyMissing = errors.New("key no longer exists in CPA")

// ErrKeyExternallyRevoked indicates that a portal-managed key no longer
// exists in CPA. Callers may use it to show a safe, actionable message without
// exposing the complete key or an upstream response body.
var ErrKeyExternallyRevoked = errors.New("API Key 已被 CPA 或 CPAMP 外部撤销")

func (k *Keys) rawKeyByHash(ctx context.Context, hash string) (string, error) {
	keys, err := k.CPAMP.ListAPIKeys(ctx)
	if err != nil {
		return "", err
	}
	for _, raw := range keys {
		if cpamp.HashAPIKey(raw) == hash {
			return raw, nil
		}
	}
	return "", errKeyMissing
}

// TestKey verifies a user's active key by using it to read CPA's model list.
// Reading /v1/models does not submit a model inference request.
func (k *Keys) TestKey(ctx context.Context, userID string) ([]cpamp.Model, error) {
	active, err := k.Store.ActiveKey(ctx, userID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, errors.New("当前没有可测试的 API Key")
		}
		return nil, err
	}
	raw, err := k.rawKeyByHash(ctx, active.Hash)
	if err != nil {
		if errors.Is(err, errKeyMissing) {
			_ = k.Store.SetKeyStatus(ctx, active.Hash, "external_revoked")
			_ = k.CPAMP.DeleteAlias(ctx, active.Hash)
			_ = k.Store.DeleteModelTestResults(ctx, userID)
			return nil, ErrKeyExternallyRevoked
		}
		return nil, err
	}
	typed, ok := k.CPAMP.(interface {
		ListModelsWithAPIKey(context.Context, string) (cpamp.ModelListResponse, error)
	})
	if !ok {
		return nil, errors.New("CPAMP 客户端不支持 Key 测试")
	}
	result, err := typed.ListModelsWithAPIKey(ctx, raw)
	if err != nil {
		return nil, errors.New("Key 已存在，但 CPA 模型接口测试失败")
	}
	return result.Data, nil
}

// ModelTestResult reports only non-sensitive information about one real,
// minimal model request. Models is returned so the UI can keep its selector
// visible after the request.
type ModelTestResult struct {
	Model   string
	Latency time.Duration
}

// TestModel sends one minimal, non-streaming OpenAI-compatible chat request
// through CPA. It intentionally tests only the submitted model and does not
// reload the complete model catalog.
func (k *Keys) TestModel(ctx context.Context, userID, model, cpaBaseURL string) (ModelTestResult, error) {
	model = strings.TrimSpace(model)
	result := ModelTestResult{Model: model}
	if model == "" || len(model) > 256 || strings.ContainsAny(model, "\r\n\x00") {
		return result, errors.New("请选择有效的模型")
	}
	active, err := k.Store.ActiveKey(ctx, userID)
	if err != nil {
		return result, errors.New("当前没有可测试的 API Key")
	}
	raw, err := k.rawKeyByHash(ctx, active.Hash)
	if err != nil {
		if errors.Is(err, errKeyMissing) {
			_ = k.Store.SetKeyStatus(ctx, active.Hash, "external_revoked")
			_ = k.CPAMP.DeleteAlias(ctx, active.Hash)
			_ = k.Store.DeleteModelTestResults(ctx, userID)
			return result, ErrKeyExternallyRevoked
		}
		return result, errors.New("暂时无法读取 Key 状态")
	}
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(cpaBaseURL), "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return result, errors.New("门户配置的 CPA API 地址无效")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/v1/chat/completions"
	base.RawQuery, base.Fragment = "", ""
	requestBody := map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "Reply with OK."}},
		"stream":   false,
	}
	if strings.HasPrefix(strings.ToLower(model), "gpt-") {
		requestBody["max_completion_tokens"] = 16
	} else {
		requestBody["max_tokens"] = 1
	}
	payload, err := json.Marshal(requestBody)
	if err != nil {
		return result, errors.New("无法创建模型测试请求")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String(), bytes.NewReader(payload))
	if err != nil {
		return result, errors.New("无法创建模型测试请求")
	}
	req.Header.Set("Authorization", "Bearer "+raw)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{
		Timeout: 45 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	started := time.Now()
	resp, err := client.Do(req)
	result.Latency = time.Since(started)
	if err != nil {
		return result, errors.New("无法连接 CPA 模型接口")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return result, errors.New("模型请求未通过鉴权")
		case http.StatusNotFound:
			return result, errors.New("CPA 未找到该模型或模型接口")
		case http.StatusTooManyRequests:
			return result, errors.New("模型已连接，但当前额度不足或触发限流")
		default:
			if resp.StatusCode >= 500 {
				return result, errors.New("模型上游暂时不可用")
			}
			return result, fmt.Errorf("模型测试失败（HTTP %d）", resp.StatusCode)
		}
	}
	return result, nil
}

func (k *Keys) upsertAlias(ctx context.Context, hash, alias string) error {
	items, err := k.CPAMP.ListAliases(ctx)
	if err != nil {
		return err
	}
	rawKeys, err := k.CPAMP.ListAPIKeys(ctx)
	if err != nil {
		return err
	}
	activeHashes := make([]string, 0, len(rawKeys))
	activeSet := make(map[string]struct{}, len(rawKeys))
	for _, raw := range rawKeys {
		activeHash := cpamp.HashAPIKey(raw)
		activeHashes = append(activeHashes, activeHash)
		activeSet[activeHash] = struct{}{}
	}
	found := false
	allowOrphanCleanup := false
	filtered := make([]cpamp.APIKeyAlias, 0, len(items)+1)
	for i := range items {
		itemHash := strings.ToLower(strings.TrimSpace(items[i].APIKeyHash))
		if strings.EqualFold(itemHash, hash) {
			items[i].Alias = alias
			items[i].APIKeyHash = hash
			filtered = append(filtered, items[i])
			found = true
			continue
		}
		if strings.EqualFold(strings.TrimSpace(items[i].Alias), strings.TrimSpace(alias)) {
			if _, active := activeSet[itemHash]; active {
				return errors.New("该 Key 别名已被另一个有效 Key 使用")
			}
			// CPAMP keeps alias rows independently from CPA keys. Exclude a
			// stale row from this upsert and explicitly authorize CPAMP's
			// orphan-cleanup guard to replace it with the new key hash.
			allowOrphanCleanup = true
			continue
		}
		filtered = append(filtered, items[i])
	}
	if !found {
		filtered = append(filtered, cpamp.APIKeyAlias{APIKeyHash: hash, Alias: alias})
	}
	_, err = k.CPAMP.ReplaceAliases(ctx, filtered, activeHashes, allowOrphanCleanup)
	return err
}

func (k *Keys) Usage(ctx context.Context, userID string, from, to time.Time, events int) (cpamp.AnalyticsResponse, error) {
	cacheKey := fmt.Sprintf("user:%s:%d:%d:%d", userID, from.Unix()/60, to.Unix()/60, events)
	if value, ok := k.cached(cacheKey); ok {
		return value, nil
	}
	hashes, err := k.hashesForUser(ctx, userID)
	if err != nil {
		return cpamp.AnalyticsResponse{}, err
	}
	if len(hashes) == 0 {
		return cpamp.AnalyticsResponse{Summary: &cpamp.UsageSummary{}}, nil
	}
	req := cpamp.AnalyticsRequest{FromMS: from.UnixMilli(), ToMS: to.UnixMilli(), NowMS: k.Now().UnixMilli(), TimeZone: "Asia/Shanghai", Filters: cpamp.AnalyticsFilters{APIKeyHashes: hashes}, Include: cpamp.AnalyticsInclude{Summary: true, Timeline: true, ModelStats: true, Granularity: usageGranularity(from, to)}}
	if events > 0 {
		req.Include.EventsPage = &cpamp.EventsPage{Limit: events}
	}
	value, err := k.CPAMP.Analytics(ctx, req)
	if err == nil {
		k.putCache(cacheKey, value)
	}
	return value, err
}

func (k *Keys) GlobalUsage(ctx context.Context, from, to time.Time, events int) (cpamp.AnalyticsResponse, error) {
	cacheKey := fmt.Sprintf("global:%d:%d:%d", from.Unix()/60, to.Unix()/60, events)
	if value, ok := k.cached(cacheKey); ok {
		return value, nil
	}
	req := cpamp.AnalyticsRequest{FromMS: from.UnixMilli(), ToMS: to.UnixMilli(), NowMS: k.Now().UnixMilli(), TimeZone: "Asia/Shanghai", Include: cpamp.AnalyticsInclude{Summary: true, Timeline: true, ModelStats: true, APIKeyStats: true, Granularity: usageGranularity(from, to)}}
	if events > 0 {
		req.Include.EventsPage = &cpamp.EventsPage{Limit: events}
	}
	value, err := k.CPAMP.Analytics(ctx, req)
	if err == nil {
		k.putCache(cacheKey, value)
	}
	return value, err
}

// GlobalRequests returns the latest sanitized model-request metadata across
// every API key. Prompt and response bodies are never requested or exposed.
func (k *Keys) GlobalRequests(ctx context.Context, limit int) (cpamp.EventsResponse, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 100 {
		limit = 100
	}
	now := k.Now()
	from := now.AddDate(-10, 0, 0)
	cacheKey := fmt.Sprintf("global-requests:%d:%d", now.Unix()/60, limit)
	if value, ok := k.cached(cacheKey); ok && value.Events != nil {
		return *value.Events, nil
	}
	req := cpamp.AnalyticsRequest{
		FromMS: from.UnixMilli(), ToMS: now.UnixMilli(), NowMS: now.UnixMilli(), TimeZone: "Asia/Shanghai",
		Include: cpamp.AnalyticsInclude{EventsPage: &cpamp.EventsPage{Limit: limit}},
	}
	value, err := k.CPAMP.Analytics(ctx, req)
	if err != nil {
		return cpamp.EventsResponse{}, err
	}
	if value.Events == nil {
		return cpamp.EventsResponse{Items: []cpamp.EventRow{}}, nil
	}
	sort.SliceStable(value.Events.Items, func(i, j int) bool { return value.Events.Items[i].TimestampMS > value.Events.Items[j].TimestampMS })
	if len(value.Events.Items) > limit {
		value.Events.Items = value.Events.Items[:limit]
	}
	k.putCache(cacheKey, value)
	return *value.Events, nil
}

// APIKeyUsage returns one aggregate row per requested portal key. It is used
// by paginated administrator lists so one CPAMP query can populate every row.
func (k *Keys) APIKeyUsage(ctx context.Context, hashes []string, from, to time.Time) ([]cpamp.APIKeyUsageStat, error) {
	unique := make(map[string]struct{}, len(hashes))
	normalized := make([]string, 0, len(hashes))
	for _, hash := range hashes {
		hash = strings.ToLower(strings.TrimSpace(hash))
		if hash == "" {
			continue
		}
		if _, exists := unique[hash]; exists {
			continue
		}
		unique[hash] = struct{}{}
		normalized = append(normalized, hash)
	}
	if len(normalized) == 0 {
		return []cpamp.APIKeyUsageStat{}, nil
	}
	sort.Strings(normalized)
	cacheKey := fmt.Sprintf("key-stats:%s:%d:%d", security.SHA256(strings.Join(normalized, ",")), from.Unix()/60, to.Unix()/60)
	if value, ok := k.cached(cacheKey); ok {
		return value.APIKeyStats, nil
	}
	req := cpamp.AnalyticsRequest{
		FromMS:   from.UnixMilli(),
		ToMS:     to.UnixMilli(),
		NowMS:    k.Now().UnixMilli(),
		TimeZone: "Asia/Shanghai",
		Filters:  cpamp.AnalyticsFilters{APIKeyHashes: normalized},
		Include:  cpamp.AnalyticsInclude{APIKeyStats: true},
	}
	value, err := k.CPAMP.Analytics(ctx, req)
	if err != nil {
		return nil, err
	}
	k.putCache(cacheKey, value)
	return value.APIKeyStats, nil
}

func usageGranularity(from, to time.Time) string {
	if to.After(from) && to.Sub(from) <= 48*time.Hour {
		return "hour"
	}
	return "day"
}

// UpstreamAccounts lists Codex credentials that administrators may enable or
// disable. The opaque ID is derived from CPAMP's full sanitized identity so a
// submitted action can be matched against a fresh listing.
func (k *Keys) UpstreamAccounts(ctx context.Context) ([]UpstreamAccount, error) {
	typed, ok := k.CPAMP.(interface {
		ListAuthFiles(context.Context) ([]cpamp.AuthFile, error)
	})
	if !ok {
		return nil, errors.New("CPAMP 客户端不支持上游账号管理")
	}
	files, err := typed.ListAuthFiles(ctx)
	if err != nil {
		return nil, err
	}
	accounts := make([]UpstreamAccount, 0, len(files))
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		if !strings.EqualFold(strings.TrimSpace(file.Provider), "codex") || strings.TrimSpace(file.Name) == "" {
			continue
		}
		id := upstreamAccountID(file)
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		label := strings.TrimSpace(file.AccountSnapshot)
		if label == "" {
			label = strings.TrimSpace(file.Name)
		}
		accounts = append(accounts, UpstreamAccount{
			ID: id, Name: strings.TrimSpace(file.Name), Account: label,
			Provider: "Codex", AuthIndex: strings.TrimSpace(file.AuthIndex), Disabled: file.Disabled,
		})
	}
	sort.SliceStable(accounts, func(i, j int) bool {
		if accounts[i].Disabled != accounts[j].Disabled {
			return !accounts[i].Disabled
		}
		return strings.ToLower(accounts[i].Account) < strings.ToLower(accounts[j].Account)
	})
	return accounts, nil
}

// UpstreamAccountQuotas returns current quota windows per enabled Codex
// credential. It never exposes provider tokens or credential file contents.
func (k *Keys) UpstreamAccountQuotas(ctx context.Context) (map[string]UpstreamAccountQuota, error) {
	typed, ok := k.CPAMP.(interface {
		ListAuthFiles(context.Context) ([]cpamp.AuthFile, error)
		QueryQuotaSnapshots(context.Context, cpamp.QuotaSnapshotQueryRequest) (cpamp.QuotaSnapshotQueryResponse, error)
	})
	if !ok {
		return nil, errors.New("CPAMP 客户端不支持额度快照")
	}
	files, err := typed.ListAuthFiles(ctx)
	if err != nil {
		return nil, err
	}
	queries := make([]cpamp.QuotaQueryAccount, 0, len(files))
	accountIDByRowKey := make(map[string]string, len(files))
	for _, file := range files {
		if !strings.EqualFold(strings.TrimSpace(file.Provider), "codex") || file.Disabled || strings.TrimSpace(file.Name) == "" {
			continue
		}
		rowKey := strings.TrimSpace(file.Name) + "\x00" + strings.TrimSpace(file.AuthIndex)
		if _, exists := accountIDByRowKey[rowKey]; exists {
			continue
		}
		accountIDByRowKey[rowKey] = upstreamAccountID(file)
		queries = append(queries, cpamp.QuotaQueryAccount{
			RowKey: rowKey, Provider: "codex",
			Account: cpamp.QuotaAccountTarget{
				AccountSnapshot:       file.AccountSnapshot,
				AuthFileSnapshot:      file.Name,
				AuthProviderSnapshot:  "codex",
				AuthProjectIDSnapshot: file.ProjectID,
				AuthIndex:             file.AuthIndex,
				Source:                file.Name,
			},
		})
		if len(queries) == 200 {
			break
		}
	}
	result := make(map[string]UpstreamAccountQuota, len(queries))
	if len(queries) == 0 {
		return result, nil
	}
	now := k.Now()
	snapshots, err := typed.QueryQuotaSnapshots(ctx, cpamp.QuotaSnapshotQueryRequest{Accounts: queries, NowMS: now.UnixMilli()})
	if err != nil {
		return nil, err
	}
	snapshots.Items = k.mergeRefreshedQuota(snapshots.Items, now)
	for _, item := range snapshots.Items {
		accountID := accountIDByRowKey[item.RowKey]
		if accountID == "" {
			continue
		}
		selected := currentQuotaWindows(item.Windows, now)
		quota := UpstreamAccountQuota{AccountID: accountID}
		for _, selection := range selected {
			plan := strings.ToUpper(strings.TrimSpace(selection.Window.PlanType))
			if plan == "" {
				plan = "套餐未知"
			}
			quota.Windows = append(quota.Windows, UpstreamAccountQuotaWindow{
				Period: selection.Period, PlanType: plan,
				RemainingPercent: int(math.Round(quotaRemaining(selection.Window))),
				ResetAt:          quotaWindowEffectiveReset(selection.Period, selection.Window, selected, now),
				ObservedAt:       time.UnixMilli(selection.Window.ObservedAtMS).UTC(),
			})
		}
		if len(quota.Windows) > 0 {
			result[accountID] = quota
		}
	}
	return result, nil
}

// SetUpstreamAccountDisabled re-resolves the submitted opaque ID immediately
// before mutating CPAMP, then verifies that the requested state was applied.
func (k *Keys) SetUpstreamAccountDisabled(ctx context.Context, id string, disabled bool) (UpstreamAccount, error) {
	typed, ok := k.CPAMP.(interface {
		ListAuthFiles(context.Context) ([]cpamp.AuthFile, error)
		SetAuthFileDisabled(context.Context, cpamp.AuthFile, bool) error
	})
	if !ok {
		return UpstreamAccount{}, errors.New("CPAMP 客户端不支持上游账号管理")
	}
	files, err := typed.ListAuthFiles(ctx)
	if err != nil {
		return UpstreamAccount{}, err
	}
	var selected cpamp.AuthFile
	found := false
	for _, file := range files {
		if strings.EqualFold(strings.TrimSpace(file.Provider), "codex") && upstreamAccountID(file) == strings.TrimSpace(id) {
			selected, found = file, true
			break
		}
	}
	if !found {
		return UpstreamAccount{}, errors.New("上游账号不存在或已发生变化，请刷新后重试")
	}
	if selected.Disabled != disabled {
		if err := typed.SetAuthFileDisabled(ctx, selected, disabled); err != nil {
			return UpstreamAccount{}, err
		}
		confirmed, err := typed.ListAuthFiles(ctx)
		if err != nil {
			return UpstreamAccount{}, err
		}
		verified := false
		for _, file := range confirmed {
			if upstreamAccountID(file) == strings.TrimSpace(id) {
				if file.Disabled != disabled {
					return UpstreamAccount{}, errors.New("CPAMP 未确认上游账号状态变更")
				}
				selected, verified = file, true
				break
			}
		}
		if !verified {
			return UpstreamAccount{}, errors.New("状态变更后无法确认上游账号")
		}
	}
	k.invalidateQuota()
	k.refreshMu.Lock()
	k.refreshedQuota = make(map[string][]cpamp.QuotaSnapshotWindow)
	k.refreshMu.Unlock()
	label := strings.TrimSpace(selected.AccountSnapshot)
	if label == "" {
		label = strings.TrimSpace(selected.Name)
	}
	return UpstreamAccount{ID: upstreamAccountID(selected), Name: strings.TrimSpace(selected.Name), Account: label, Provider: "Codex", AuthIndex: strings.TrimSpace(selected.AuthIndex), Disabled: disabled}, nil
}

func upstreamAccountID(file cpamp.AuthFile) string {
	return security.SHA256(strings.Join([]string{
		strings.TrimSpace(file.Name), strings.TrimSpace(file.RuntimeID), strings.TrimSpace(file.AuthIndex),
		strings.ToLower(strings.TrimSpace(file.Provider)), strings.TrimSpace(file.AccountID), strings.TrimSpace(file.AccountSnapshot),
	}, "\x00"))
}

// OAuthModelSettings combines CPAMP's static Codex catalog with the global
// OAuth exclusion rules so administrators can see the effective state.
func (k *Keys) OAuthModelSettings(ctx context.Context) ([]OAuthModelSetting, []string, error) {
	typed, ok := k.CPAMP.(interface {
		ListOAuthModelDefinitions(context.Context, string) ([]cpamp.OAuthModelDefinition, error)
		ListOAuthExcludedModels(context.Context, string) ([]string, error)
	})
	if !ok {
		return nil, nil, errors.New("CPAMP 客户端不支持 OAuth 模型管理")
	}
	definitions, err := typed.ListOAuthModelDefinitions(ctx, "codex")
	if err != nil {
		return nil, nil, err
	}
	rules, err := typed.ListOAuthExcludedModels(ctx, "codex")
	if err != nil {
		return nil, nil, err
	}
	models := make([]OAuthModelSetting, 0, len(definitions))
	for _, definition := range definitions {
		matched, wildcard := excludedModelRule(definition.ID, rules)
		models = append(models, OAuthModelSetting{ID: definition.ID, DisplayName: definition.DisplayName, Enabled: matched == "", WildcardRule: wildcard})
	}
	sort.SliceStable(models, func(i, j int) bool { return strings.ToLower(models[i].ID) < strings.ToLower(models[j].ID) })
	wildcards := make([]string, 0)
	for _, rule := range rules {
		if strings.Contains(rule, "*") {
			wildcards = append(wildcards, rule)
		}
	}
	return models, wildcards, nil
}

// SetOAuthModelEnabled updates only an exact model rule. Wildcard exclusions
// remain untouched because changing one would alter multiple model switches.
func (k *Keys) SetOAuthModelEnabled(ctx context.Context, modelID string, enabled bool) error {
	typed, ok := k.CPAMP.(interface {
		ListOAuthModelDefinitions(context.Context, string) ([]cpamp.OAuthModelDefinition, error)
		ListOAuthExcludedModels(context.Context, string) ([]string, error)
		SetOAuthExcludedModels(context.Context, string, []string) error
	})
	if !ok {
		return errors.New("CPAMP 客户端不支持 OAuth 模型管理")
	}
	modelID = strings.TrimSpace(modelID)
	definitions, err := typed.ListOAuthModelDefinitions(ctx, "codex")
	if err != nil {
		return err
	}
	canonical := ""
	for _, definition := range definitions {
		if strings.EqualFold(strings.TrimSpace(definition.ID), modelID) {
			canonical = strings.TrimSpace(definition.ID)
			break
		}
	}
	if canonical == "" {
		return errors.New("OAuth 模型不存在或模型定义已变化，请刷新后重试")
	}
	rules, err := typed.ListOAuthExcludedModels(ctx, "codex")
	if err != nil {
		return err
	}
	matched, wildcard := excludedModelRule(canonical, rules)
	if enabled && wildcard != "" {
		return fmt.Errorf("该模型由通配规则 %q 禁用，请在 CPAMP 中调整该规则", wildcard)
	}
	next := make([]string, 0, len(rules)+1)
	for _, rule := range rules {
		if strings.EqualFold(strings.TrimSpace(rule), canonical) {
			continue
		}
		next = append(next, rule)
	}
	if !enabled && matched == "" {
		next = append(next, canonical)
	}
	if (enabled && matched != "") || (!enabled && matched == "") {
		if err := typed.SetOAuthExcludedModels(ctx, "codex", next); err != nil {
			return err
		}
	}
	confirmed, err := typed.ListOAuthExcludedModels(ctx, "codex")
	if err != nil {
		return err
	}
	confirmedRule, _ := excludedModelRule(canonical, confirmed)
	if (enabled && confirmedRule != "") || (!enabled && confirmedRule == "") {
		return errors.New("CPAMP 未确认 OAuth 模型状态变更")
	}
	return nil
}

func excludedModelRule(model string, rules []string) (matched, wildcard string) {
	for _, rule := range rules {
		rule = strings.TrimSpace(rule)
		if rule == "" {
			continue
		}
		if strings.EqualFold(rule, model) {
			matched = rule
			continue
		}
		if strings.Contains(rule, "*") && wildcardModelMatch(rule, model) {
			return rule, rule
		}
	}
	return matched, ""
}

func wildcardModelMatch(pattern, value string) bool {
	pattern, value = strings.ToLower(pattern), strings.ToLower(value)
	p, v, star, checkpoint := 0, 0, -1, 0
	for v < len(value) {
		if p < len(pattern) && pattern[p] == value[v] {
			p++
			v++
			continue
		}
		if p < len(pattern) && pattern[p] == '*' {
			star, checkpoint = p, v
			p++
			continue
		}
		if star >= 0 {
			p = star + 1
			checkpoint++
			v = checkpoint
			continue
		}
		return false
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// UpstreamQuota returns the shared Codex account-pool quota windows without
// exposing auth-file names, account labels, or provider credentials.
func (k *Keys) UpstreamQuota(ctx context.Context) (UpstreamQuotaPool, error) {
	if value, ok := k.cachedQuota(); ok {
		return value, nil
	}
	typed, ok := k.CPAMP.(interface {
		ListAuthFiles(context.Context) ([]cpamp.AuthFile, error)
		QueryQuotaSnapshots(context.Context, cpamp.QuotaSnapshotQueryRequest) (cpamp.QuotaSnapshotQueryResponse, error)
	})
	if !ok {
		return UpstreamQuotaPool{}, errors.New("CPAMP 客户端不支持额度快照")
	}
	files, err := typed.ListAuthFiles(ctx)
	if err != nil {
		return UpstreamQuotaPool{}, err
	}
	pool := UpstreamQuotaPool{Provider: "Codex"}
	accounts := make([]cpamp.QuotaQueryAccount, 0, len(files))
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		provider := strings.ToLower(strings.TrimSpace(file.Provider))
		if provider != "codex" || file.Disabled || strings.TrimSpace(file.Name) == "" {
			continue
		}
		rowKey := strings.TrimSpace(file.Name) + "\x00" + strings.TrimSpace(file.AuthIndex)
		if _, exists := seen[rowKey]; exists {
			continue
		}
		seen[rowKey] = struct{}{}
		pool.TotalAccounts++
		accounts = append(accounts, cpamp.QuotaQueryAccount{
			RowKey:   rowKey,
			Provider: "codex",
			Account: cpamp.QuotaAccountTarget{
				AccountSnapshot:       file.AccountSnapshot,
				AuthFileSnapshot:      file.Name,
				AuthProviderSnapshot:  "codex",
				AuthProjectIDSnapshot: file.ProjectID,
				AuthIndex:             file.AuthIndex,
				Source:                file.Name,
			},
		})
		if len(accounts) == 200 {
			break
		}
	}
	if len(accounts) == 0 {
		k.putQuota(pool)
		return pool, nil
	}
	now := k.Now()
	result, err := typed.QueryQuotaSnapshots(ctx, cpamp.QuotaSnapshotQueryRequest{Accounts: accounts, NowMS: now.UnixMilli()})
	if err != nil {
		return pool, err
	}
	result.Items = k.mergeRefreshedQuota(result.Items, now)
	type quotaAccumulator struct {
		group            UpstreamQuotaGroup
		remainingTotal   float64
		includedAccounts int
	}
	groups := make(map[string]*quotaAccumulator)
	knownAccounts := make(map[string]struct{}, len(result.Items))
	for _, item := range result.Items {
		windows := currentQuotaWindows(item.Windows, now)
		if len(windows) == 0 {
			continue
		}
		knownAccounts[item.RowKey] = struct{}{}
		accountUsable := true
		for _, selected := range windows {
			if quotaRemaining(selected.Window) <= 0 {
				accountUsable = false
				break
			}
		}
		for _, selected := range windows {
			window, period := selected.Window, selected.Period
			plan := strings.ToLower(strings.TrimSpace(window.PlanType))
			if plan == "" {
				plan = "unknown"
			}
			key := plan + "\x00" + period
			acc := groups[key]
			if acc == nil {
				acc = &quotaAccumulator{group: UpstreamQuotaGroup{PlanType: plan, Period: period}}
				groups[key] = acc
			}
			remaining := quotaRemaining(window)
			acc.group.KnownAccounts++
			if quotaWindowIncludedInAverage(period, windows) {
				acc.remainingTotal += remaining
				acc.includedAccounts++
				if remaining > 0 {
					acc.group.AvailableAccounts++
				}
			}
			if reset := quotaWindowEffectiveReset(period, window, windows, now); !reset.IsZero() {
				if acc.group.NextResetAt.IsZero() || reset.Before(acc.group.NextResetAt) {
					acc.group.NextResetAt = reset
				}
			}
			observed := time.UnixMilli(window.ObservedAtMS).UTC()
			if window.ObservedAtMS > 0 && (acc.group.ObservedAt.IsZero() || observed.After(acc.group.ObservedAt)) {
				acc.group.ObservedAt = observed
			}
		}
		if accountUsable {
			pool.UsableAccounts++
		}
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		acc := groups[key]
		if acc.includedAccounts > 0 {
			acc.group.RemainingPercent = int(math.Round(acc.remainingTotal / float64(acc.includedAccounts)))
		}
		acc.group.Estimated = acc.includedAccounts > 1
		pool.Groups = append(pool.Groups, acc.group)
	}
	pool.UnknownCount = pool.TotalAccounts - len(knownAccounts)
	if pool.UnknownCount < 0 {
		pool.UnknownCount = 0
	}
	k.putQuota(pool)
	return pool, nil
}

func quotaWindowIncludedInAverage(period string, windows []quotaWindowSelection) bool {
	if period != "five_hour" {
		return true
	}
	for _, selected := range windows {
		if (selected.Period == "weekly" || selected.Period == "monthly") && quotaRemaining(selected.Window) <= 0 {
			return false
		}
	}
	return true
}

func quotaWindowEffectiveReset(period string, window cpamp.QuotaSnapshotWindow, windows []quotaWindowSelection, now time.Time) time.Time {
	reset := futureQuotaReset(window, now)
	if period != "five_hour" {
		return reset
	}
	for _, selected := range windows {
		if selected.Period != "weekly" && selected.Period != "monthly" {
			continue
		}
		if quotaRemaining(selected.Window) > 0 {
			continue
		}
		longReset := futureQuotaReset(selected.Window, now)
		if longReset.After(reset) {
			reset = longReset
		}
	}
	return reset
}

func futureQuotaReset(window cpamp.QuotaSnapshotWindow, now time.Time) time.Time {
	if window.CycleEndMS == nil || *window.CycleEndMS <= now.UnixMilli() {
		return time.Time{}
	}
	return time.UnixMilli(*window.CycleEndMS).UTC()
}

type quotaWindowSelection struct {
	Window cpamp.QuotaSnapshotWindow
	Period string
}

func currentQuotaWindows(windows []cpamp.QuotaSnapshotWindow, now time.Time) []quotaWindowSelection {
	selected := make(map[string]cpamp.QuotaSnapshotWindow, 3)
	for _, window := range windows {
		scope := strings.ToLower(strings.TrimSpace(window.ModelScopeKind))
		availability := strings.ToLower(strings.TrimSpace(window.Availability))
		period := quotaWindowPeriod(window)
		if period == "" || (scope != "" && scope != "all") || availability == "inactive" || availability == "pending_absent" {
			continue
		}
		if window.Stale && !freshQuotaWithObsoleteBoundary(window, availability, now) {
			continue
		}
		if window.UsedPercent == nil && window.RemainingPercent == nil {
			continue
		}
		current, ok := selected[period]
		if !ok || window.ObservedAtMS > current.ObservedAtMS ||
			(window.ObservedAtMS == current.ObservedAtMS && current.Stale && !window.Stale) {
			selected[period] = window
		}
	}
	periods := []string{"five_hour", "weekly", "monthly"}
	result := make([]quotaWindowSelection, 0, len(selected))
	for _, period := range periods {
		if window, ok := selected[period]; ok {
			result = append(result, quotaWindowSelection{Window: window, Period: period})
		}
	}
	return result
}

// freshQuotaWithObsoleteBoundary handles a CPAMP lifecycle edge case where a
// newly observed quota value is paired with the previous cycle's already
// expired boundary. The value is still useful, but only while the observation
// is active, newer than that boundary, and younger than one full window.
func freshQuotaWithObsoleteBoundary(window cpamp.QuotaSnapshotWindow, availability string, now time.Time) bool {
	if availability != "active" || window.ObservedAtMS <= 0 || window.CycleEndMS == nil || window.DurationSeconds == nil {
		return false
	}
	durationSeconds := *window.DurationSeconds
	if durationSeconds <= 0 || durationSeconds > int64((31*24*time.Hour)/time.Second) {
		return false
	}
	nowMS := now.UnixMilli()
	if *window.CycleEndMS >= nowMS || window.ObservedAtMS <= *window.CycleEndMS || window.ObservedAtMS > nowMS {
		return false
	}
	maxAgeMS := durationSeconds * 1000
	return nowMS-window.ObservedAtMS < maxAgeMS
}

func quotaWindowPeriod(window cpamp.QuotaSnapshotWindow) string {
	kind := strings.ToLower(strings.TrimSpace(window.WindowKind + " " + window.ProviderWindowID))
	kind = strings.NewReplacer("-", "_", " ", "_").Replace(kind)
	switch {
	case strings.Contains(kind, "five_hour"), strings.Contains(kind, "5h"), strings.Contains(kind, "primary"):
		return "five_hour"
	case strings.Contains(kind, "week"):
		return "weekly"
	case strings.Contains(kind, "month"):
		return "monthly"
	default:
		return ""
	}
}

func quotaRemaining(window cpamp.QuotaSnapshotWindow) float64 {
	value := float64(0)
	if window.RemainingPercent != nil {
		value = *window.RemainingPercent
	} else if window.UsedPercent != nil {
		value = 100 - *window.UsedPercent
	}
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func (k *Keys) cached(key string) (cpamp.AnalyticsResponse, bool) {
	if k.CacheTTL <= 0 {
		return cpamp.AnalyticsResponse{}, false
	}
	k.cacheMu.Lock()
	defer k.cacheMu.Unlock()
	v, ok := k.cache[key]
	if !ok || k.Now().After(v.expiresAt) {
		if ok {
			delete(k.cache, key)
		}
		return cpamp.AnalyticsResponse{}, false
	}
	return v.value, true
}
func (k *Keys) putCache(key string, value cpamp.AnalyticsResponse) {
	if k.CacheTTL <= 0 {
		return
	}
	k.cacheMu.Lock()
	defer k.cacheMu.Unlock()
	if k.cache == nil {
		k.cache = make(map[string]analyticsCache)
	}
	if len(k.cache) > 500 {
		k.cache = make(map[string]analyticsCache)
	}
	k.cache[key] = analyticsCache{value: value, expiresAt: k.Now().Add(k.CacheTTL)}
}

func (k *Keys) cachedQuota() (UpstreamQuotaPool, bool) {
	if k.CacheTTL <= 0 {
		return UpstreamQuotaPool{}, false
	}
	k.cacheMu.Lock()
	defer k.cacheMu.Unlock()
	if k.quota.expiresAt.IsZero() || k.Now().After(k.quota.expiresAt) {
		k.quota = quotaPoolCache{}
		return UpstreamQuotaPool{}, false
	}
	return k.quota.value, true
}

func (k *Keys) putQuota(value UpstreamQuotaPool) {
	if k.CacheTTL <= 0 {
		return
	}
	k.cacheMu.Lock()
	defer k.cacheMu.Unlock()
	k.quota = quotaPoolCache{value: value, expiresAt: k.Now().Add(k.CacheTTL)}
}

func (k *Keys) hashesForUser(ctx context.Context, userID string) ([]string, error) {
	keys, err := k.Store.ListKeysByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(keys))
	for _, v := range keys {
		out = append(out, v.Hash)
	}
	sort.Strings(out)
	return out, nil
}

func (k *Keys) Models(ctx context.Context) ([]cpamp.Model, error) {
	raw, err := k.CPAMP.ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, errors.New("CPA 中没有可用于查询模型的 API Key")
	}
	typed, ok := k.CPAMP.(interface {
		ListModelsWithAPIKey(context.Context, string) (cpamp.ModelListResponse, error)
	})
	if !ok {
		return nil, errors.New("CPAMP 客户端不支持模型查询")
	}
	result, err := typed.ListModelsWithAPIKey(ctx, raw[0])
	if err != nil {
		return nil, err
	}
	return result.Data, nil
}

func (k *Keys) Reconcile(ctx context.Context) error {
	k.reconcileMu.Lock()
	defer k.reconcileMu.Unlock()
	current, err := k.CPAMP.ListAPIKeys(ctx)
	if err != nil {
		return err
	}
	set := make(map[string]struct{}, len(current))
	for _, raw := range current {
		set[cpamp.HashAPIKey(raw)] = struct{}{}
	}
	active, err := k.Store.ActiveKeys(ctx)
	if err != nil {
		return err
	}
	activeByHash := make(map[string]domain.APIKey, len(active))
	activeHashes := make([]string, 0, len(active))
	for _, v := range active {
		if _, ok := set[v.Hash]; !ok {
			_ = k.Store.SetKeyStatus(ctx, v.Hash, "external_revoked")
			_ = k.CPAMP.DeleteAlias(ctx, v.Hash)
			_ = k.Store.DeleteModelTestResults(ctx, v.UserID)
			continue
		}
		activeByHash[v.Hash] = v
		activeHashes = append(activeHashes, v.Hash)
	}
	if len(activeHashes) == 0 {
		k.lastSeenSyncAt = k.Now().UTC()
		return nil
	}
	now := k.Now().UTC()
	from := k.lastSeenSyncAt
	if from.IsZero() {
		from = now
		for _, key := range activeByHash {
			if !key.IssuedAt.IsZero() && key.IssuedAt.Before(from) {
				from = key.IssuedAt.UTC()
			}
		}
	} else {
		// Keep a small overlap so delayed CPAMP ingestion cannot leave a gap.
		from = from.Add(-15 * time.Minute)
	}
	if !from.Before(now) || from.UnixMilli() >= now.UnixMilli() {
		from = now.Add(-24 * time.Hour)
	}
	usage, err := k.CPAMP.Analytics(ctx, cpamp.AnalyticsRequest{
		FromMS:   from.UnixMilli(),
		ToMS:     now.UnixMilli(),
		NowMS:    now.UnixMilli(),
		TimeZone: "Asia/Shanghai",
		Filters:  cpamp.AnalyticsFilters{APIKeyHashes: activeHashes},
		Include:  cpamp.AnalyticsInclude{APIKeyStats: true},
	})
	if err != nil {
		return fmt.Errorf("sync API key last used time: %w", err)
	}
	for _, stat := range usage.APIKeyStats {
		key, ok := activeByHash[strings.ToLower(strings.TrimSpace(stat.APIKeyHash))]
		if !ok || stat.LastSeenMS <= 0 {
			continue
		}
		lastSeen := time.UnixMilli(stat.LastSeenMS).UTC()
		if !key.LastSeenAt.IsZero() && !lastSeen.After(key.LastSeenAt) {
			continue
		}
		if err := k.Store.SetKeyLastSeen(ctx, key.Hash, lastSeen); err != nil {
			return fmt.Errorf("save API key last used time: %w", err)
		}
	}
	k.lastSeenSyncAt = now
	return nil
}

func (k *Keys) ProcessJobs(ctx context.Context, retry time.Duration) error {
	jobs, err := k.Store.DueJobs(ctx, 50)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		user, e := k.Store.UserByID(ctx, job.UserID)
		if e != nil {
			_ = k.Store.DeleteJob(ctx, job.ID)
			continue
		}
		pending, e := k.Revoke(ctx, user)
		if e != nil || pending {
			msg := "撤销尚未确认"
			if e != nil {
				msg = e.Error()
			}
			_ = k.Store.RetryJob(ctx, job.ID, k.Now().Add(retry), msg)
			continue
		}
		if job.Action == "delete" {
			anon := "已删除用户-" + security.ShortID(user.ID)
			e = k.Store.AnonymizeUser(ctx, user.ID, anon)
		} else if job.Action == "revoke" {
			e = nil
		} else {
			e = k.Store.SetUserStatus(ctx, user.ID, domain.StatusSuspended, user.SuspensionReason)
		}
		if e == nil {
			_ = k.Store.DeleteJob(ctx, job.ID)
		} else {
			_ = k.Store.RetryJob(ctx, job.ID, k.Now().Add(retry), e.Error())
		}
	}
	return nil
}
