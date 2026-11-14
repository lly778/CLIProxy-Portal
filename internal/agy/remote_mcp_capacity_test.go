package agy

import (
	"context"
	"testing"
	"time"
)

func TestRemoteCapacityBoundsAndCancellation(t *testing.T) {
	m := &Manager{}
	var releases []func(int)
	for i := 0; i < 3; i++ {
		_, release, ok := m.reserveRemoteCapacity(t.Context(), &session{})
		if !ok {
			t.Fatal("available slot refused")
		}
		releases = append(releases, release)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, ok := m.reserveRemoteCapacity(ctx, &session{}); ok {
		t.Fatal("cancelled fourth operation acquired a slot")
	}
	for _, release := range releases {
		release(7)
	}
	if m.remoteInFlight != 0 {
		t.Fatal("capacity leaked")
	}
}

func TestRemoteCapacityAndQueuedRevocationRefuseBeforeDispatch(t *testing.T) {
	for _, mode := range []string{"record_budget", "revoked_while_queued"} {
		t.Run(mode, func(t *testing.T) {
			m, s, c, _ := remotePendingFixture(t)
			c.remote.Approved = true
			close(c.remote.approvalReady)
			if mode == "record_budget" {
				s.remoteRetainedBytes = MaxBody
			} else {
				_, release, ok := m.reserveRemoteCapacity(t.Context(), s)
				if !ok {
					t.Fatal("fixture slot unavailable")
				}
				m.startRemoteJobs(s)
				s.mu.Lock()
				s.tools = nil
				s.mu.Unlock()
				release(0)
			}
			m.startRemoteJobs(s)
			select {
			case <-c.Ready:
			case <-time.After(time.Second):
				t.Fatal("refused operation remained unresolved")
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if c.remote.Attempted || c.remote.Outcome != "not_executed" || !toolResultFailed(c.Output) {
				t.Fatal("capacity or revocation dispatched an action")
			}
		})
	}
}
