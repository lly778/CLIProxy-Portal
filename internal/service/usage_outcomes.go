package service

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/security"
	"cliproxy-portal/internal/store"
)

type outcomeBucket struct {
	at        int64
	user, key string
}
type usageOutcomeSnapshot struct {
	window            store.OutcomeReadWindow
	dueMS, requiredMS int64
	updatedAt         time.Time
	rows              map[string]domain.GatewayFailure
	parts             map[string][]requestDelta
	rejections        map[outcomeBucket]domain.RequestRejection
	totals            map[outcomeBucket]requestDelta
}

func failureGroup(r domain.GatewayFailure) string {
	if r.RequestID != "" {
		return "trace:" + r.APIKeyHash + ":" + r.RequestID
	}
	return "local:" + r.ID
}

func (s *usageOutcomeSnapshot) adjust(d requestDelta, sign int64) {
	key := outcomeBucket{d.at / 60000 * 60000, d.user, d.key}
	x := s.totals[key]
	x.at, x.user, x.key = key.at, key.user, key.key
	x.calls += sign * d.calls
	x.success += sign * d.success
	x.failed += sign * d.failed
	if x.calls == 0 && x.success == 0 && x.failed == 0 {
		delete(s.totals, key)
	} else {
		s.totals[key] = x
	}
}

func (s *usageOutcomeSnapshot) removeGroup(group string) {
	for _, d := range s.parts[group] {
		s.adjust(d, -1)
	}
	delete(s.parts, group)
	delete(s.rows, group)
}

func (k *Keys) incrementUsageOutcomes(ctx context.Context, users []string, global bool, req cpamp.AnalyticsRequest, refresh bool) (*domain.RequestLogData, error) {
	users = append([]string(nil), users...)
	sort.Strings(users)
	raw, _ := json.Marshal(struct {
		Scope  string
		Users  []string
		Global bool
	}{k.actualModelScopeKey(req), users, global})
	key := security.SHA256(string(raw))
	k.outcomeMu.Lock()
	previous := k.usageOutcomes[key]
	k.outcomeMu.Unlock()
	now := k.Now()
	full := refresh || previous == nil
	if previous != nil {
		full = full || now.UnixMilli() >= previous.dueMS || now.Before(previous.updatedAt) || req.FromMS < previous.window.FromMS || req.ToMS < previous.window.ToMS || req.FromMS >= previous.window.ToMS
	}
	var window *store.OutcomeReadWindow
	if !full {
		window = &previous.window
	}
	batch, err := k.Store.ReadUsageOutcomes(ctx, users, global, req.FromMS, req.ToMS, window, actualModelOverlap)
	if err == nil && !full && (batch.FailureCursor < window.FailureCursor || batch.AuditCursor < window.AuditCursor) {
		full = true
		batch, err = k.Store.ReadUsageOutcomes(ctx, users, global, req.FromMS, req.ToMS, nil, actualModelOverlap)
	}
	if err != nil {
		return nil, err
	}
	next := &usageOutcomeSnapshot{window: store.OutcomeReadWindow{FromMS: req.FromMS, ToMS: req.ToMS, ReadAtMS: now.UnixMilli(), FailureCursor: batch.FailureCursor, AuditCursor: batch.AuditCursor}, updatedAt: now,
		rows: map[string]domain.GatewayFailure{}, parts: map[string][]requestDelta{}, rejections: map[outcomeBucket]domain.RequestRejection{}, totals: map[outcomeBucket]requestDelta{}}
	changed := map[string]bool{}
	if full {
		next.dueMS = now.Add(actualModelReconcile).UnixMilli()
	} else {
		next.dueMS, next.requiredMS = previous.dueMS, previous.requiredMS
		for id, r := range previous.rows {
			next.rows[id] = r
		}
		for id, parts := range previous.parts {
			next.parts[id] = parts
		}
		for id, r := range previous.rejections {
			next.rejections[id] = r
		}
		for id, d := range previous.totals {
			next.totals[id] = d
		}
		for id, r := range next.rows {
			if r.EndedMS < req.FromMS || r.StartedMS >= req.ToMS || r.StartedMS >= batch.HeadFromMS {
				next.removeGroup(id)
				continue
			}
			for _, d := range next.parts[id] {
				if d.at < req.FromMS || d.at >= req.ToMS {
					changed[id] = true
				}
			}
			for _, e := range r.Upstream {
				if e.AtMS >= previous.window.ToMS && e.AtMS < req.ToMS {
					changed[id] = true
				}
			}
		}
		first := req.FromMS / 60000 * 60000
		for id, r := range next.rejections {
			if id.at <= first || id.at >= req.ToMS || id.at >= batch.HeadFromMS {
				next.adjust(requestDelta{at: r.AtMS, user: r.UserID, calls: r.Calls, failed: r.Calls}, -1)
				delete(next.rejections, id)
			}
		}
	}
	// Share the earliest full-reconciliation deadline with model usage. Page
	// caching must never defer either checkpoint's hourly verification.
	if model := k.currentActualModelSnapshot(req); model != nil {
		next.dueMS = min(next.dueMS, model.fullAt.Add(actualModelReconcile).UnixMilli())
	}
	for _, r := range batch.Failures {
		id := failureGroup(r)
		if old, ok := next.rows[id]; ok && old.ID != r.ID {
			start, end := min(old.StartedMS, r.StartedMS), max(old.EndedMS, r.EndedMS)
			if old.CheckedMS > r.CheckedMS || old.CheckedMS == r.CheckedMS && old.ID < r.ID {
				r = old
			}
			r.StartedMS, r.EndedMS = start, end
		}
		next.rows[id] = r
		changed[id] = true
	}
	work := domain.RequestLogData{}
	for id, r := range next.rows {
		due := r.RequestID != "" && (r.CheckedMS == 0 || now.UnixMilli()-r.CheckedMS >= int64(time.Hour/time.Millisecond) || len(r.Upstream) == 0 && now.UnixMilli()-r.CheckedMS >= int64(time.Minute/time.Millisecond))
		if full || changed[id] || due {
			work.Failures = append(work.Failures, r)
		}
	}
	if err = k.reconcileRequestFailures(ctx, &work, full); err != nil {
		return nil, err
	}
	for _, r := range work.Failures {
		id := failureGroup(r)
		for _, d := range next.parts[id] {
			next.adjust(d, -1)
		}
		next.rows[id] = r
		parts := requestLogDeltas(&domain.RequestLogData{Failures: []domain.GatewayFailure{r}}, req.FromMS, req.ToMS)
		next.parts[id] = parts
		for _, d := range parts {
			next.adjust(d, 1)
		}
		next.requiredMS = max(next.requiredMS, r.GeneratedMS)
	}
	for _, r := range batch.Rejections {
		id := outcomeBucket{r.AtMS, r.UserID, ""}
		if old, ok := next.rejections[id]; ok {
			next.adjust(requestDelta{at: old.AtMS, user: old.UserID, calls: old.Calls, failed: old.Calls}, -1)
		}
		next.rejections[id] = r
		next.adjust(requestDelta{at: r.AtMS, user: r.UserID, calls: r.Calls, failed: r.Calls}, 1)
	}
	data := &domain.RequestLogData{Corrections: []domain.RequestCorrection{}, RequiredUpstreamMS: next.requiredMS, Note: work.Note, ReconcileAtMS: next.dueMS}
	for _, d := range next.totals {
		data.Corrections = append(data.Corrections, domain.RequestCorrection{AtMS: d.at, UserID: d.user, APIKeyHash: d.key, Calls: d.calls, Success: d.success, Failure: d.failed})
	}
	k.putUsageOutcomeSnapshot(key, next)
	return data, nil
}

func outcomeSnapshotSize(s *usageOutcomeSnapshot) int {
	count := len(s.rows) + len(s.rejections) + len(s.totals)
	for id, r := range s.rows {
		count += len(r.Upstream) + len(s.parts[id])
	}
	return count
}

func (k *Keys) putUsageOutcomeSnapshot(key string, next *usageOutcomeSnapshot) {
	k.outcomeMu.Lock()
	defer k.outcomeMu.Unlock()
	if k.usageOutcomes == nil {
		k.usageOutcomes = map[string]*usageOutcomeSnapshot{}
	}
	if old := k.usageOutcomes[key]; old != nil && (old.window.ToMS > next.window.ToMS || old.updatedAt.After(next.updatedAt)) {
		return
	}
	delete(k.usageOutcomes, key)
	if outcomeSnapshotSize(next) > actualModelMaxRows {
		return
	}
	for {
		count := outcomeSnapshotSize(next)
		oldest := ""
		for id, s := range k.usageOutcomes {
			count += outcomeSnapshotSize(s)
			if oldest == "" || s.updatedAt.Before(k.usageOutcomes[oldest].updatedAt) {
				oldest = id
			}
		}
		if len(k.usageOutcomes) < actualModelMaxScopes && count <= actualModelMaxTotalRows {
			break
		}
		delete(k.usageOutcomes, oldest)
	}
	k.usageOutcomes[key] = next
}
