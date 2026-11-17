package metrics

import "sync"

// Limiter caps how many distinct values a label may take. Model names and
// app names come from clients; without a cap one misbehaving client can
// create unbounded series and take Prometheus down with it.
type Limiter struct {
	mu    sync.Mutex
	max   int
	seen  map[string]struct{}
	allow map[string]struct{}
}

// Other is the value used once the cap is reached, or for values that are
// not on the allow list.
const Other = "_other"

// NewLimiter keeps up to max distinct values. If allow is not empty, only
// those values are kept and everything else becomes Other.
func NewLimiter(max int, allow []string) *Limiter {
	l := &Limiter{max: max, seen: map[string]struct{}{}}
	if len(allow) > 0 {
		l.allow = map[string]struct{}{}
		for _, a := range allow {
			l.allow[a] = struct{}{}
		}
	}
	return l
}

func (l *Limiter) Value(v string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.allow != nil {
		if _, ok := l.allow[v]; ok {
			return v
		}
		return Other
	}
	if _, ok := l.seen[v]; ok {
		return v
	}
	if len(l.seen) >= l.max {
		return Other
	}
	l.seen[v] = struct{}{}
	return v
}
