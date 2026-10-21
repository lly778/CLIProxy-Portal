package resourcecontrol

import (
	"context"
	"math"
	"sync"
	"time"
)

type Event struct {
	At         time.Time `json:"at"`
	User       string    `json:"user"`
	Reason     string    `json:"reason"`
	Observed   bool      `json:"observed"`
	StatusCode int       `json:"status_code,omitempty"`
}
type Snapshot struct {
	Policy     Policy  `json:"policy"`
	Metrics    Metrics `json:"metrics"`
	Active     int     `json:"active"`
	Processing int     `json:"processing"`
	Rejected   uint64  `json:"rejected"`
	Observed   uint64  `json:"observed"`
	Paused     string  `json:"paused"`
	Fresh      bool    `json:"fresh"`
}

type Controller struct {
	mu                                  sync.Mutex
	policy                              Policy
	metrics                             Metrics
	users                               map[string]int
	active, processing                  int
	rejected, observed                  uint64
	events                              chan Event
	auditWake                           chan struct{}
	pendingEvents                       []Event
	logMu                               sync.Mutex
	highSince, recoverSince, timeSample time.Time
	paused                              string
	flushMu                             sync.Mutex
	eventSink                           func(context.Context, []Event) error
	history                             historyBuffer
	historyFlushMu                      sync.Mutex
	historySink                         func(context.Context, []HistoryPoint) error
	cgroupPath                          string
	hostMetricsPath                     string
}

func NewController(p Policy, cgroupPath ...string) *Controller {
	path := DefaultCgroupPath
	if len(cgroupPath) > 0 && cgroupPath[0] != "" {
		path = cgroupPath[0]
	}
	return &Controller{policy: p, users: map[string]int{}, events: make(chan Event, 256), auditWake: make(chan struct{}, 1), cgroupPath: path}
}
func (c *Controller) SetPolicy(p Policy) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.policy = p
	c.highSince = time.Time{}
	c.recoverSince = time.Time{}
	c.paused = ""
}
func (c *Controller) Policy() Policy { c.mu.Lock(); defer c.mu.Unlock(); return c.policy }

func memoryRecoveryThreshold(limit int64) float64 {
	budget := float64(limit)
	return math.Max(budget-math.Max(32, budget*0.05), budget/2)
}

func (c *Controller) Run(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	go c.runAudit(ctx)
	go c.runHistory(ctx)
	sample := newSuiteSampler(c.cgroupPath)
	for {
		m, e := sample()
		if c.hostMetricsPath != "" && m.SuiteUsedMiB != nil && m.TotalMiB > 0 && m.CPUCount > 0 {
			// External CPU does not need the local CPU delta warm-up.
			m, e = applyExternalMetrics(m, c.hostMetricsPath, time.Now())
		}
		if e != nil {
			m.Error = "CPA 套件资源监测暂不可用"
		}
		c.UpdateMetrics(m, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
func (c *Controller) UpdateMetrics(m Metrics, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !m.At.After(c.timeSample) {
		return
	}
	if m.At.Sub(c.timeSample) > 10*time.Second {
		c.highSince = time.Time{}
		c.recoverSince = time.Time{}
	}
	c.metrics = m
	c.timeSample = m.At
	if !m.Fresh(now) {
		c.highSince = time.Time{}
		c.recoverSince = time.Time{}
		return
	}
	historyUsed, historyTotal := m.UsedMiB(), m.TotalMiB
	if m.HostUsedMiB != nil {
		historyUsed, historyTotal = *m.HostUsedMiB, m.HostTotalMiB
	}
	c.history.add(HistoryPoint{
		At: m.At.UTC().Truncate(HistoryInterval), CPUPercent: math.Round(m.CPUPercent*10) / 10,
		UsedMiB: math.Round(historyUsed*10) / 10, TotalMiB: math.Round(historyTotal*10) / 10,
	})
	high := c.policy.CPUPercent > 0 && m.CPUPercent >= c.policy.CPUPercent
	low := c.policy.CPUPercent == 0 || m.CPUPercent <= math.Max(c.policy.CPUPercent-15, c.policy.CPUPercent/2)
	memory := c.policy.MemoryMiB > 0 && m.UsedMiB() >= float64(c.policy.MemoryMiB)
	if memory {
		c.paused = "memory_pressure"
		c.recoverSince = time.Time{}
		return
	}
	if high {
		if c.highSince.IsZero() {
			c.highSince = now
		}
		if now.Sub(c.highSince) >= 15*time.Second {
			c.paused = "cpu_pressure"
		}
		c.recoverSince = time.Time{}
		return
	}
	c.highSince = time.Time{}
	if c.paused != "" {
		if low && (c.policy.MemoryMiB == 0 || m.UsedMiB() <= memoryRecoveryThreshold(c.policy.MemoryMiB)) {
			if c.recoverSince.IsZero() {
				c.recoverSince = now
			}
			if now.Sub(c.recoverSince) >= 10*time.Second {
				c.paused = ""
				c.recoverSince = time.Time{}
			}
		} else {
			c.recoverSince = time.Time{}
		}
	}
}
func (c *Controller) event(user, reason string, observed bool) {
	c.eventWithStatus(user, reason, observed, 0)
}
func (c *Controller) eventWithStatus(user, reason string, observed bool, status int) {
	if observed {
		c.observed++
	} else {
		c.rejected++
	}
	e := Event{At: time.Now().UTC(), User: user, Reason: reason, Observed: observed, StatusCode: status}
	// Queue backpressure never holds the admission/monitor mutex. The bounded
	// channel stores individual events; the writer batches commits, never records.
	c.mu.Unlock()
	c.events <- e
	select {
	case c.auditWake <- struct{}{}:
	default:
	}
	c.mu.Lock()
}
func (c *Controller) SetEventSink(sink func(context.Context, []Event) error) {
	c.logMu.Lock()
	defer c.logMu.Unlock()
	c.eventSink = sink
}
func (c *Controller) FlushEvents(ctx context.Context) {
	c.flushMu.Lock()
	defer c.flushMu.Unlock()
	c.logMu.Lock()
	sink := c.eventSink
	c.logMu.Unlock()
	if sink == nil {
		return
	}
	for ctx.Err() == nil {
		if len(c.pendingEvents) == 0 {
			for len(c.pendingEvents) < 64 {
				select {
				case e := <-c.events:
					c.pendingEvents = append(c.pendingEvents, e)
				default:
					goto drained
				}
			}
		}
	drained:
		if len(c.pendingEvents) == 0 {
			return
		}
		writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := sink(writeCtx, c.pendingEvents)
		cancel()
		if err != nil {
			return
		}
		c.pendingEvents = nil
	}
}
func (c *Controller) runAudit(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.auditWake:
		}
		for {
			c.FlushEvents(ctx)
			c.flushMu.Lock()
			retry := len(c.pendingEvents) > 0
			c.flushMu.Unlock()
			if !retry {
				break
			}
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
}
func (c *Controller) Record(user, reason string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	observe := c.policy.Mode != "enforce"
	c.event(user, reason, observe)
	return !observe
}

type Lease struct {
	c                     *Controller
	user                  string
	Policy                Policy
	onceModel, onceFinish sync.Once
}

// Record uses the admission policy so changing modes does not relabel an
// already admitted streaming request or WebSocket connection.
func (l *Lease) Record(reason string) {
	l.RecordStatus(reason, 0)
}

// RecordStatus preserves the actual HTTP result when the same protection reason
// can reject with different statuses, such as malformed and oversized bodies.
func (l *Lease) RecordStatus(reason string, status int) {
	l.c.mu.Lock()
	defer l.c.mu.Unlock()
	l.c.eventWithStatus(l.user, reason, l.Policy.Mode != "enforce", status)
}

func (l *Lease) ModelDone() {
	l.onceModel.Do(func() {
		l.c.mu.Lock()
		defer l.c.mu.Unlock()
		l.c.active--
		l.c.users[l.user]--
		if l.c.users[l.user] == 0 {
			delete(l.c.users, l.user)
		}
	})
}
func (l *Lease) Finish() {
	l.onceFinish.Do(func() { l.ModelDone(); l.c.mu.Lock(); l.c.processing--; l.c.mu.Unlock() })
}

func (c *Controller) Acquire(user string) (*Lease, string) {
	return c.AcquireRequest(user, -1)
}

func (c *Controller) AcquireRequest(user string, contentLength int64) (*Lease, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	reason := ""
	p := c.policy
	if p.BodyMiB > 0 && contentLength > p.BodyMiB*MiB {
		c.event(user, "request_too_large", p.Mode != "enforce")
		if p.Mode == "enforce" {
			return nil, "request_too_large"
		}
	}
	if p.Global > 0 && c.active >= p.Global {
		reason = "global_concurrency"
	} else if p.PerUser > 0 && c.users[user] >= p.PerUser {
		reason = "user_concurrency"
	} else if p.Global > 0 && c.processing >= p.Global+4 {
		reason = "capture_backlog"
	} else if (p.CPUPercent > 0 || p.MemoryMiB > 0) && !c.metrics.Fresh(time.Now()) {
		reason = "monitor_unavailable"
	} else if p.MemoryMiB > 0 && c.metrics.UsedMiB() >= float64(p.MemoryMiB) {
		reason = "memory_pressure"
	} else if c.paused != "" {
		reason = c.paused
	}
	if reason != "" {
		c.event(user, reason, p.Mode != "enforce")
		if p.Mode == "enforce" {
			return nil, reason
		}
	}
	c.active++
	c.processing++
	c.users[user]++
	return &Lease{c: c, user: user, Policy: p}, ""
}
func (c *Controller) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.policy
	m := c.metrics
	paused := c.paused
	fresh := m.Fresh(time.Now())
	if !fresh && (p.CPUPercent > 0 || p.MemoryMiB > 0) {
		paused = "monitor_unavailable"
	}
	return Snapshot{p, m, c.active, c.processing, c.rejected, c.observed, paused, fresh}
}
