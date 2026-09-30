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
	mu  sync.Mutex
	row store.GatewayRequestTiming
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
}

// completion runs after the actual server flush for instrumented connections.
// It schedules metadata persistence without adding it to the measured duration.
func (g *Gateway) completeMeasurement(m *requestMeasurement, ended, firstWrite time.Time, completed bool) {
	m.mu.Lock()
	row := m.row
	m.mu.Unlock()
	row.EndedAt = ended
	row.TotalMS = *elapsedMS(row.StartedAt, ended)
	row.Completed = completed
	if !firstWrite.IsZero() {
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
