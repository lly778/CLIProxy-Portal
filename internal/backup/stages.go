package backup

import "time"

// Hooks only observe work; they never replace any validation step.
type stageHooks struct {
	begin  func(string)
	finish func(string, time.Duration)
}

func (h *stageHooks) start(name string) func() {
	if h == nil {
		return func() {}
	}
	if h.begin != nil {
		h.begin(name)
	}
	started := time.Now()
	finished := false
	return func() {
		if !finished {
			finished = true
			if h.finish != nil {
				h.finish(name, time.Since(started))
			}
		}
	}
}
