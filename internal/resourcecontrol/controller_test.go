package resourcecontrol

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuditWriterDrainsEveryEventDuringRetryBurst(t *testing.T) {
	c := NewController(DefaultPolicy())
	var count atomic.Int64
	c.SetEventSink(func(_ context.Context, events []Event) error { count.Add(int64(len(events))); return nil })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	writerDone := make(chan struct{})
	go func() { defer close(writerDone); c.runAudit(ctx) }()
	produced := make(chan struct{})
	go func() {
		defer close(produced)
		for i := 0; i < 1000; i++ {
			c.Record("same", "global_concurrency")
		}
	}()
	select {
	case <-produced:
	case <-time.After(3 * time.Second):
		t.Fatal("event queue stalled")
	}
	c.FlushEvents(t.Context())
	if count.Load() != 1000 {
		t.Fatal("events merged or lost", count.Load())
	}
	cancel()
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("writer did not stop")
	}
}

func TestZeroIndependentlyDisablesEachLimit(t *testing.T) {
	p := DefaultPolicy()
	p.Mode = "enforce"
	p.Global = 0
	p.PerUser = 0
	p.BodyMiB = 0
	p.CPUPercent = 0
	p.MemoryMiB = 0
	if e := p.Validate(); e != nil {
		t.Fatal(e)
	}
	c := NewController(p)
	var leases []*Lease
	for i := 0; i < 140; i++ {
		l, r := c.AcquireRequest("same", 1<<40)
		if l == nil {
			t.Fatal("zero still imposed a limit", r)
		}
		leases = append(leases, l)
	}
	for _, l := range leases {
		l.Finish()
	}
	p.PerUser = 1
	c.SetPolicy(p)
	a, _ := c.Acquire("same")
	if b, r := c.Acquire("same"); b != nil || r != "user_concurrency" {
		t.Fatal("user cap disabled by global zero", r)
	}
	a.Finish()
	p.PerUser = 0
	p.Global = 1
	c.SetPolicy(p)
	a, _ = c.Acquire("same")
	if b, r := c.Acquire("other"); b != nil || r != "global_concurrency" {
		t.Fatal("global cap disabled by user zero", r)
	}
	a.Finish()
	p.Global = 0
	p.MemoryMiB = 500
	c.SetPolicy(p)
	m := goodMetrics(time.Now())
	m.CPUPercent = 100
	c.UpdateMetrics(m, time.Now())
	if l, r := c.Acquire("a"); l != nil || r != "memory_pressure" {
		t.Fatal("memory cap disabled by CPU zero", r)
	}
	p.MemoryMiB = 0
	p.CPUPercent = 90
	c.SetPolicy(p)
	base := time.Now().Add(time.Second)
	for _, sec := range []int{0, 8, 16} {
		m.At = base.Add(time.Duration(sec) * time.Second)
		m.AvailableMiB = 0
		c.UpdateMetrics(m, m.At)
	}
	if c.paused != "cpu_pressure" {
		t.Fatal("CPU cap disabled by memory zero")
	}
}

func recordedEvents(t *testing.T, c *Controller) []Event {
	t.Helper()
	var events []Event
	c.SetEventSink(func(_ context.Context, batch []Event) error { events = append(events, batch...); return nil })
	c.FlushEvents(t.Context())
	return events
}
func TestResourceAuditPreservesEveryTimestampAndRetriesWithoutDuplicates(t *testing.T) {
	c := NewController(DefaultPolicy())
	for i := 0; i < 150; i++ {
		c.Record("same", "global_concurrency")
	}
	fail := true
	var saved []Event
	c.SetEventSink(func(_ context.Context, events []Event) error {
		if fail {
			return errors.New("retry")
		}
		saved = append(saved, events...)
		return nil
	})
	c.FlushEvents(t.Context())
	if len(c.pendingEvents) == 0 {
		t.Fatal("failed batch discarded")
	}
	fail = false
	c.FlushEvents(t.Context())
	if len(saved) != 150 {
		t.Fatal("events merged, lost or duplicated", len(saved))
	}
	for i, e := range saved {
		if e.At.IsZero() || (i > 0 && e.At.Before(saved[i-1].At)) {
			t.Fatal("original timestamp lost")
		}
	}
	if len(c.events) != 0 || len(c.pendingEvents) != 0 || cap(c.events) != 256 {
		t.Fatal("unbounded or undrained log queue")
	}
}

func TestAdmissionIsAtomicAndExistingRequestKeepsItsPolicy(t *testing.T) {
	p := DefaultPolicy()
	p.Mode = "enforce"
	c := NewController(p)
	c.UpdateMetrics(goodMetrics(time.Now()), time.Now())
	var group sync.WaitGroup
	var leases []*Lease
	var mu sync.Mutex
	for i := 0; i < 40; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			l, _ := c.Acquire("same")
			if l != nil {
				mu.Lock()
				leases = append(leases, l)
				mu.Unlock()
			}
		}()
	}
	group.Wait()
	if len(leases) != 1 {
		t.Fatal("racing requests bypassed user cap", len(leases))
	}
	p.Mode = "observe"
	c.SetPolicy(p)
	leases[0].Record("request_too_large")
	events := recordedEvents(t, c)
	if events[len(events)-1].Observed {
		t.Fatal("active request relabeled after policy change")
	}
	leases[0].Finish()
	p.Mode = "enforce"
	c.SetPolicy(p)
	if l, r := c.AcquireRequest("a", 11*MiB); l != nil || r != "request_too_large" {
		t.Fatal("known body bypassed admission size check", r)
	}
}

func goodMetrics(now time.Time) Metrics {
	return Metrics{At: now, HostID: "host-test", CPUCount: 1, TotalMiB: 926, AvailableMiB: 400}
}
func TestAdmissionEnforcesUserGlobalAndPendingWorkAndReleasesOnce(t *testing.T) {
	p := DefaultPolicy()
	p.Mode = "enforce"
	c := NewController(p)
	c.UpdateMetrics(goodMetrics(time.Now()), time.Now())
	a, r := c.Acquire("a")
	if a == nil || r != "" {
		t.Fatal(r)
	}
	if l, r := c.Acquire("a"); l != nil || r != "user_concurrency" {
		t.Fatal("user cap bypassed", r)
	}
	b, _ := c.Acquire("b")
	if b == nil {
		t.Fatal("second user rejected")
	}
	if l, r := c.Acquire("c"); l != nil || r != "global_concurrency" {
		t.Fatal("global cap bypassed", r)
	}
	a.ModelDone()
	a.ModelDone()
	q := c.Snapshot()
	if q.Active != 1 || q.Processing != 2 {
		t.Fatalf("premature capture release: %+v", q)
	}
	a.Finish()
	a.Finish()
	b.Finish()
	if q = c.Snapshot(); q.Active != 0 || q.Processing != 0 || len(c.users) != 0 {
		t.Fatal("leaked lease", q)
	}
	var jobs []*Lease
	for i := 0; i < p.Global+4; i++ {
		l, _ := c.Acquire("a")
		if l == nil {
			t.Fatal("unexpected backlog cap")
		}
		l.ModelDone()
		jobs = append(jobs, l)
	}
	if l, r := c.Acquire("a"); l != nil || r != "capture_backlog" {
		t.Fatal("unbounded capture backlog", r)
	}
	for _, l := range jobs {
		l.Finish()
	}
}
func TestObserveDoesNotRejectAndKeepsBoundedHistory(t *testing.T) {
	c := NewController(DefaultPolicy())
	var jobs []*Lease
	for i := 0; i < 80; i++ {
		l, r := c.Acquire("same-user")
		if l == nil || r != "" {
			t.Fatal("observe rejected")
		}
		jobs = append(jobs, l)
	}
	q := c.Snapshot()
	if q.Observed == 0 || q.Rejected != 0 || uint64(len(recordedEvents(t, c))) != q.Observed {
		t.Fatal("unbounded or inaccurate observation", q)
	}
	for _, l := range jobs {
		l.Finish()
	}
}
func TestPressureRequiresSustainedCPUAndStableRecovery(t *testing.T) {
	c := NewController(DefaultPolicy())
	base := time.Now().Add(-time.Minute)
	for _, sec := range []int{0, 10, 16} {
		at := base.Add(time.Duration(sec) * time.Second)
		m := goodMetrics(at)
		m.CPUPercent = 95
		c.UpdateMetrics(m, at)
	}
	if c.paused != "cpu_pressure" {
		t.Fatal("CPU pressure not detected")
	}
	for _, sec := range []int{17, 22} {
		at := base.Add(time.Duration(sec) * time.Second)
		c.UpdateMetrics(goodMetrics(at), at)
	}
	if c.paused == "" {
		t.Fatal("recovered too early")
	}
	at := base.Add(28 * time.Second)
	c.UpdateMetrics(goodMetrics(at), at)
	if c.paused != "" {
		t.Fatal("did not recover")
	}
	at = base.Add(30 * time.Second)
	m := goodMetrics(at)
	m.AvailableMiB = 50
	c.UpdateMetrics(m, at)
	if c.paused != "memory_pressure" {
		t.Fatal("low memory ignored")
	}
	c.metrics.At = time.Now().Add(-time.Minute)
	p := c.Policy()
	p.Mode = "enforce"
	c.SetPolicy(p)
	if l, r := c.Acquire("a"); l != nil || r != "monitor_unavailable" {
		t.Fatal("stale metrics admitted", r)
	}
}
func TestPolicyValidationRejectsInvalidTotalLimits(t *testing.T) {
	p := DefaultPolicy()
	if p.Validate() != nil {
		t.Fatal("defaults invalid")
	}
	for _, change := range []func(*Policy){func(p *Policy) { p.CPUPercent = math.NaN() }, func(p *Policy) { p.CPUPercent = math.Inf(1) }, func(p *Policy) { p.MemoryMiB = -1 }, func(p *Policy) { p.BodyMiB = -1 }, func(p *Policy) { p.PerUser = p.Global + 1 }} {
		p = DefaultPolicy()
		change(&p)
		if p.Validate() == nil {
			t.Fatal("invalid total policy accepted")
		}
	}
}
func TestNewMemoryBudgetBlocksAdmissionUsingLatestSample(t *testing.T) {
	p := DefaultPolicy()
	c := NewController(p)
	c.UpdateMetrics(goodMetrics(time.Now()), time.Now())
	p.Mode = "enforce"
	p.MemoryMiB = 500
	c.SetPolicy(p)
	if l, r := c.Acquire("a"); l != nil || r != "memory_pressure" {
		t.Fatal("changed budget ignored current load", r)
	}
}
