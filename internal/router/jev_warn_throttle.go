package router

import (
	"sync"
	"time"
)

// jevWarnInterval is how often one fallback reason may log a warning again.
const jevWarnInterval = time.Minute

// jevWarnThrottle keeps a missing key or a Jev outage from logging a warning on
// every request. The first occurrence of each distinct reason logs at once; after
// that a reason logs at most once per jevWarnInterval, carrying the number of
// occurrences suppressed since its last line. Reasons are the fixed classes the
// classifier reports (never a response body or the key), so the map stays small.
// The zero value is usable; tests inject now.
type jevWarnThrottle struct {
	mu    sync.Mutex
	now   func() time.Time
	state map[string]*jevWarnState
}

type jevWarnState struct {
	lastLogged time.Time
	suppressed int
}

// allow reports whether reason should log now and, if so, how many occurrences
// were suppressed since its previous line.
func (t *jevWarnThrottle) allow(reason string) (bool, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if t.now != nil {
		now = t.now()
	}
	st, seen := t.state[reason]
	if !seen {
		if t.state == nil {
			t.state = make(map[string]*jevWarnState)
		}
		t.state[reason] = &jevWarnState{lastLogged: now}
		return true, 0
	}
	if now.Sub(st.lastLogged) < jevWarnInterval {
		st.suppressed++
		return false, 0
	}
	suppressed := st.suppressed
	st.suppressed = 0
	st.lastLogged = now
	return true, suppressed
}
