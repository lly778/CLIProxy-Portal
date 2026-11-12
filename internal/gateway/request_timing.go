package gateway

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/store"
)

type requestMeasurement struct {
	mu                 sync.Mutex
	row                store.GatewayRequestTiming
	modelResponseStart bool
}

func (m *requestMeasurement) modelResponseStarted(started bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.modelResponseStart = true
	if started && m.row.ResponseStartedMS == nil {
		m.row.ResponseStartedMS = elapsedMS(m.row.StartedAt, time.Now())
	}
}

func (g *Gateway) startMeasurement(r *http.Request, start time.Time, source string) *requestMeasurement {
	key := apiKey(r)
	if key == "" {
		return nil
	}
	hash := cpamp.HashAPIKey(key)
	owner, err := g.store.KeyByHash(r.Context(), hash)
	if err != nil || owner.UserID == "" {
		return nil
	}
	id, err := newCaptureID()
	if err != nil {
		return nil
	}
	return &requestMeasurement{row: store.GatewayRequestTiming{ID: id, UserID: owner.UserID, APIKeyHash: hash, StartedAt: start, StartSource: source}}
}

func elapsedMS(start, at time.Time) *int64 {
	v := at.Sub(start).Milliseconds()
	if v < 0 {
		v = 0
	}
	return &v
}

func (m *requestMeasurement) requestRead() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.row.RequestReadMS == nil {
		m.row.RequestReadMS = elapsedMS(m.row.StartedAt, time.Now())
	}
}

func (m *requestMeasurement) upstreamResponse(resp *http.Response) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.row.UpstreamHeadersMS = elapsedMS(m.row.StartedAt, time.Now())
	m.row.CPARequestID = cpaRequestID(resp.Header.Get("X-CPA-TRACE-ID"))
	if m.row.FailureReason != "idle_timeout" {
		m.row.StatusCode = resp.StatusCode
	}
}

func (m *requestMeasurement) failure(reason string, status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.row.FailureReason == "idle_timeout" {
		return
	}
	if reason != "idle_timeout" && m.row.RequestReadMS == nil {
		reason = "upload_interrupted"
	}
	m.row.FailureReason, m.row.StatusCode = reason, status
}

// completion runs after the actual server flush for instrumented connections.
// It schedules metadata persistence without adding it to the measured duration.
func (g *Gateway) completeMeasurement(m *requestMeasurement, ended, firstWrite time.Time, completed bool) {
	m.mu.Lock()
	row := m.row
	modelResponseStart := m.modelResponseStart
	m.mu.Unlock()
	row.EndedAt = ended
	row.TotalMS = *elapsedMS(row.StartedAt, ended)
	row.Completed = completed
	if row.FailureReason != "" {
		row.Completed = false
	}
	if modelResponseStart && row.ResponseStartedMS == nil {
		row.ResponseStartedMS = elapsedMS(row.StartedAt, ended)
	} else if !modelResponseStart && !firstWrite.IsZero() {
		row.ResponseStartedMS = elapsedMS(row.StartedAt, firstWrite)
	}
	go func() {
		defer g.captureTasks.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := g.store.SaveGatewayRequestTiming(ctx, row); err != nil {
			g.logger.Warn("gateway request timing save failed", "error", err)
		}
	}()
}

type measuredRequestBody struct {
	io.ReadCloser
	measurement *requestMeasurement
	remaining   int64
}

func (b *measuredRequestBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.remaining > 0 {
		b.remaining -= int64(n)
		if b.remaining == 0 {
			b.measurement.requestRead()
		}
	}
	if err == io.EOF {
		b.measurement.requestRead()
	}
	return n, err
}
