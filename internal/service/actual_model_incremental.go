package service

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/security"
)

const (
	actualModelOverlap       = 5 * time.Minute
	actualModelReconcile     = time.Hour
	actualModelDeltaPageSize = 100
	actualModelMaxRows       = 50000
	actualModelMaxTotalRows  = 100000
	actualModelMaxScopes     = 16
)

// Retain only fields needed to expire rows or correct model totals.
// Request bodies, error details, credentials and latency metadata are absent.
type actualModelEvent struct {
	atMS                 int64
	model                string
	input, output, total int64
	failed               bool
}

type actualModelSnapshot struct {
	fromMS, toMS  int64
	fullAt        time.Time
	updatedAt     time.Time
	rows          map[string]actualModelEvent
	byModel       map[string]cpamp.ModelUsageStat
	calls, tokens int64
}

func newActualModelSnapshot(req cpamp.AnalyticsRequest, now time.Time) *actualModelSnapshot {
	return &actualModelSnapshot{fromMS: req.FromMS, toMS: req.ToMS, fullAt: now, updatedAt: now,
		rows: make(map[string]actualModelEvent), byModel: make(map[string]cpamp.ModelUsageStat)}
}

func compactActualModelEvent(event cpamp.EventRow) actualModelEvent {
	model := strings.TrimSpace(event.ResolvedModel)
	if model == "" {
		model = strings.TrimSpace(event.ResponseModel)
	}
	if model == "" {
		model = "未记录实际模型"
	}
	return actualModelEvent{atMS: event.TimestampMS, model: model, input: event.InputTokens,
		output: event.OutputTokens, total: event.TotalTokens, failed: event.Failed}
}

func (s *actualModelSnapshot) adjust(event actualModelEvent, sign int64) {
	stat := s.byModel[event.model]
	stat.Model = event.model
	stat.Calls += sign
	if event.failed {
		stat.FailureCalls += sign
	} else {
		stat.SuccessCalls += sign
	}
	stat.InputTokens += sign * event.input
	stat.OutputTokens += sign * event.output
	stat.TotalTokens += sign * event.total
	if stat.Calls == 0 {
		delete(s.byModel, event.model)
	} else {
		s.byModel[event.model] = stat
	}
	s.calls += sign
	s.tokens += sign * event.total
}

func (s *actualModelSnapshot) validEvent(hash string, event actualModelEvent) bool {
	return hash != "" && len(hash) <= 256 && len(event.model) <= 512 && event.atMS > 0 && event.atMS >= s.fromMS && event.atMS < s.toMS &&
		event.input >= 0 && event.output >= 0 && event.total >= 0
}

func (s *actualModelSnapshot) addFull(event cpamp.EventRow) {
	row := compactActualModelEvent(event)
	s.adjust(row, 1)
	if s.rows == nil {
		return
	}
	hash := strings.TrimSpace(event.EventHash)
	_, duplicate := s.rows[hash]
	if !s.validEvent(hash, row) || duplicate || len(s.rows) >= actualModelMaxRows {
		// Older APIs and large ranges still get complete full statistics, but
		// cannot create an unbounded or ambiguous incremental checkpoint.
		s.rows = nil
		return
	}
	s.rows[hash] = row
}

func (s *actualModelSnapshot) matches(summary *cpamp.UsageSummary) bool {
	return summary != nil && s.calls == summary.TotalCalls && s.tokens == summary.TotalTokens
}

func (s *actualModelSnapshot) models() []cpamp.ModelUsageStat {
	var models []cpamp.ModelUsageStat
	for _, stat := range s.byModel {
		stat.SuccessRate = float64(stat.SuccessCalls) / float64(stat.Calls)
		models = append(models, stat)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Model < models[j].Model })
	return models
}

func (s *actualModelSnapshot) copy() *actualModelSnapshot {
	next := *s
	next.rows = make(map[string]actualModelEvent, len(s.rows))
	for hash, row := range s.rows {
		next.rows[hash] = row
	}
	next.byModel = make(map[string]cpamp.ModelUsageStat, len(s.byModel))
	for model, stat := range s.byModel {
		next.byModel[model] = stat
	}
	return &next
}

func (k *Keys) actualModelScopeKey(req cpamp.AnalyticsRequest) string {
	if req.FromMS <= 0 || req.ToMS <= req.FromMS {
		return ""
	}
	span := analyticsRangeKey(k.Now(), time.UnixMilli(req.FromMS), time.UnixMilli(req.ToMS))
	// Every filter and search scope affects the result. Hash identifiers so
	// account labels and key filters are not retained in the map key.
	granularity := req.Include.Granularity
	req.FromMS, req.ToMS, req.NowMS, req.Include = 0, 0, 0, cpamp.AnalyticsInclude{}
	raw, err := json.Marshal(struct {
		Range, Granularity string
		Request            cpamp.AnalyticsRequest
	}{span, granularity, req})
	if err != nil {
		return ""
	}
	return security.SHA256(string(raw))
}

func (k *Keys) currentActualModelSnapshot(req cpamp.AnalyticsRequest) *actualModelSnapshot {
	key := k.actualModelScopeKey(req)
	if key == "" {
		return nil
	}
	k.actualModelMu.Lock()
	snapshot := k.actualModels[key]
	k.actualModelMu.Unlock()
	now := k.Now()
	if snapshot == nil || now.Before(snapshot.fullAt) || now.Sub(snapshot.fullAt) >= actualModelReconcile ||
		req.FromMS < snapshot.fromMS || req.ToMS < snapshot.toMS || req.FromMS >= snapshot.toMS {
		return nil
	}
	return snapshot
}

func (k *Keys) actualModelInitialPageSize(req cpamp.AnalyticsRequest, forceFull bool) int {
	if !forceFull && k.currentActualModelSnapshot(req) != nil {
		return actualModelDeltaPageSize
	}
	return actualModelPageSize
}

func (k *Keys) dropActualModelSnapshot(key string) {
	k.actualModelMu.Lock()
	delete(k.actualModels, key)
	k.actualModelMu.Unlock()
}

func (k *Keys) putActualModelSnapshot(key string, snapshot *actualModelSnapshot) {
	if key == "" || snapshot.rows == nil || len(snapshot.rows) > actualModelMaxRows || int64(len(snapshot.rows)) != snapshot.calls {
		return
	}
	k.actualModelMu.Lock()
	defer k.actualModelMu.Unlock()
	if k.actualModels == nil {
		k.actualModels = make(map[string]*actualModelSnapshot)
	}
	delete(k.actualModels, key)
	for {
		total := len(snapshot.rows)
		oldest := ""
		for candidate, value := range k.actualModels {
			total += len(value.rows)
			if oldest == "" || value.updatedAt.Before(k.actualModels[oldest].updatedAt) {
				oldest = candidate
			}
		}
		if len(k.actualModels) < actualModelMaxScopes && total <= actualModelMaxTotalRows {
			break
		}
		delete(k.actualModels, oldest)
	}
	k.actualModels[key] = snapshot
}

// Re-read a recent overlap and subtract rows leaving the selected range. Late
// imports, deletions or corrections that change totals trigger a full rebuild.
// Model-only corrections outside the overlap are found by periodic full reads.
func (k *Keys) incrementActualModels(ctx context.Context, req cpamp.AnalyticsRequest, value cpamp.AnalyticsResponse, previous *actualModelSnapshot) (*actualModelSnapshot, bool) {
	next := previous.copy()
	next.fromMS, next.toMS, next.updatedAt = req.FromMS, req.ToMS, k.Now()
	cutoff := max(req.FromMS, previous.toMS-actualModelOverlap.Milliseconds())
	for hash, row := range next.rows {
		if row.atMS < req.FromMS || row.atMS >= req.ToMS || row.atMS >= cutoff {
			next.adjust(row, -1)
			delete(next.rows, hash)
		}
	}
	page := value.Events
	seen := make(map[string]bool)
	var beforeMS, beforeID, lastAt int64
	for pages := 0; ; pages++ {
		if page == nil || page.HasMore && len(page.Items) == 0 {
			return nil, false
		}
		oldest := req.ToMS
		for _, event := range page.Items {
			hash, row := strings.TrimSpace(event.EventHash), compactActualModelEvent(event)
			if !next.validEvent(hash, row) || seen[hash] || lastAt != 0 && row.atMS > lastAt {
				return nil, false
			}
			seen[hash], lastAt, oldest = true, row.atMS, row.atMS
			if old, exists := next.rows[hash]; exists {
				next.adjust(old, -1)
			}
			next.rows[hash] = row
			next.adjust(row, 1)
			if len(next.rows) > actualModelMaxRows {
				return nil, false
			}
		}
		if !page.HasMore || oldest < cutoff {
			break
		}
		if pages >= 199 || !nextModelPage(page, beforeMS, beforeID) {
			return nil, false
		}
		beforeMS, beforeID = page.NextBeforeMS, page.NextBeforeID
		delta := req
		delta.FromMS = cutoff
		delta.Include = cpamp.AnalyticsInclude{EventsPage: &cpamp.EventsPage{Limit: actualModelDeltaPageSize, BeforeMS: &beforeMS, BeforeID: &beforeID}}
		response, err := k.CPAMP.Analytics(ctx, delta)
		if err != nil || rawModelHistoryDeleted(response) {
			return nil, false
		}
		page = response.Events
	}
	return next, next.matches(value.Summary)
}
