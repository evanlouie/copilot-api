package httpapi

import (
	"sync"
	"time"
)

type failureLogEntry struct {
	window time.Time
}

type failureLogSampler struct {
	mu     sync.Mutex
	window time.Duration
	items  map[string]failureLogEntry
}

func newFailureLogSampler(window time.Duration) *failureLogSampler {
	return &failureLogSampler{window: window, items: map[string]failureLogEntry{}}
}

// Allow logs at most the first failure in each client window, bounding
// authentication log amplification.
func (s *failureLogSampler) Allow(key string, now time.Time) bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.items) >= 4096 {
		for itemKey, item := range s.items {
			if now.Sub(item.window) >= s.window {
				delete(s.items, itemKey)
			}
		}
		if len(s.items) >= 4096 {
			for itemKey := range s.items {
				delete(s.items, itemKey)
				break
			}
		}
	}
	entry := s.items[key]
	if entry.window.IsZero() || now.Sub(entry.window) >= s.window {
		s.items[key] = failureLogEntry{window: now}
		return true
	}
	return false
}
