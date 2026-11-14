package agy

import "context"

type remoteMCPResponseLimit struct{}

// Bound external response buffers independently of the number of model tool
// requests. Each native session retains at most one input-body budget of new
// remote records; excess capacity is refused explicitly rather than truncated.
func (m *Manager) reserveRemoteCapacity(ctx context.Context, s *session) (int, func(int), bool) {
	for {
		if ctx.Err() != nil {
			return 0, nil, false
		}
		m.mu.Lock()
		s.mu.Lock()
		remaining := MaxBody - s.remoteRetainedBytes
		if remaining < 2*MaxText {
			s.mu.Unlock()
			m.mu.Unlock()
			return 0, nil, false
		}
		if !s.remoteInFlight && m.remoteInFlight < 3 {
			s.remoteInFlight = true
			m.remoteInFlight++
			s.mu.Unlock()
			m.mu.Unlock()
			return remaining, func(used int) {
				m.mu.Lock()
				s.mu.Lock()
				s.remoteRetainedBytes += used
				s.remoteInFlight = false
				m.remoteInFlight--
				if m.remoteCapacityChanged != nil {
					close(m.remoteCapacityChanged)
				}
				m.remoteCapacityChanged = make(chan struct{})
				s.mu.Unlock()
				m.mu.Unlock()
			}, true
		}
		if m.remoteCapacityChanged == nil {
			m.remoteCapacityChanged = make(chan struct{})
		}
		changed := m.remoteCapacityChanged
		s.mu.Unlock()
		m.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return 0, nil, false
		}
	}
}
