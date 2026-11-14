package agy

import "encoding/json"

// Captured synchronously by the ACP reader, before a permission request is
// scheduled. Session IDs alone do not identify a live process or prompt.
type nativeEventOrigin struct {
	Process *process
	Session *session
	Epoch   uint64
	Active  bool
}

func nativePromptActive(s *session) bool {
	// A restored waiter can be marked running before its first native prompt.
	// A process with bound ACP callbacks must not treat that state as generation.
	if s.epoch == 0 && s.proc != nil && s.proc.captureOrigin != nil {
		return false
	}
	return s.running && !s.loading && (s.epoch == 0 || s.promptCompletedEpoch != s.epoch)
}

func (m *Manager) captureNativeOrigin(p *process, raw json.RawMessage) nativeEventOrigin {
	o := nativeEventOrigin{Process: p}
	var params struct {
		ID string `json:"sessionId"`
	}
	if json.Unmarshal(raw, &params) != nil || params.ID == "" {
		return o
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.ID != params.ID {
			continue
		}
		s.mu.Lock()
		if s.proc == p {
			o.Session, o.Epoch, o.Active = s, s.epoch, nativePromptActive(s)
		}
		s.mu.Unlock()
		break
	}
	return o
}

// The reader runs this before delivering the RPC reply. Do not wait for the
// prompt goroutine or HTTP consumer to notice that native generation ended.
func (m *Manager) finishNativeOrigin(o nativeEventOrigin) {
	if o.Session == nil || o.Process == nil {
		return
	}
	s := o.Session
	s.mu.Lock()
	if s.proc == o.Process && s.epoch == o.Epoch {
		s.promptCompletedEpoch = o.Epoch
	}
	s.mu.Unlock()
}

// Callers hold s.mu. Empty origins are used only by direct in-process callers;
// actual ACP callbacks must match the process, session object and captured turn.
func (o nativeEventOrigin) matches(s *session) bool {
	return o.Process == nil || o.Active && o.Session == s && o.Process == s.proc && o.Epoch == s.epoch && nativePromptActive(s)
}

const nativeToolIdentityLimit = 1024

// ACP toolCallId identifies a call within a session, not merely a turn. Keep
// bounded opaque identities so an old tool update cannot create a new call.
// Unknown IDs are not deduplicated by name, arguments or returned text.
func rememberNativeTool(s *session, id string) bool {
	if id == "" || len(id) > 256 {
		return true
	}
	key := digest(id)
	if epoch, exists := s.nativeToolEpochs[key]; exists {
		return epoch == s.epoch
	}
	if s.nativeToolEpochs == nil {
		s.nativeToolEpochs = map[string]uint64{}
	}
	if len(s.nativeToolOrder) >= nativeToolIdentityLimit {
		delete(s.nativeToolEpochs, s.nativeToolOrder[0])
		s.nativeToolOrder = s.nativeToolOrder[1:]
	}
	s.nativeToolOrder = append(s.nativeToolOrder, key)
	s.nativeToolEpochs[key] = s.epoch
	return true
}

func nativeToolCurrent(s *session, id string) bool {
	epoch, known := s.nativeToolEpochs[digest(id)]
	return !known || epoch == s.epoch
}
