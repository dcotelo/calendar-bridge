package main

import (
	stdsync "sync"
	"time"

	"github.com/dcotelo/calendar-bridge/internal/sync"
	"github.com/dcotelo/calendar-bridge/internal/webui"
)

// syncState is the process's memory of what the last sync pass did.
//
// Both the `run` loop and the `ui` server write to it, and the UI status
// endpoint reads from it, so every field is guarded by one mutex. It holds
// counts, timestamps and account names only — never event data.
type syncState struct {
	mu stdsync.Mutex

	started     bool
	lastAttempt time.Time
	lastSuccess time.Time
	lastErr     string
	last        sync.Result
	pushEnabled bool
}

func newSyncState(pushEnabled bool) *syncState {
	return &syncState{pushEnabled: pushEnabled}
}

// markRunning records that the daemon's poll loop is live, as opposed to the
// UI serving on its own with no background sync.
func (s *syncState) markRunning() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = true
}

// record stores the outcome of a pass. err may be non-nil alongside a
// partially-successful res — a pass that synced three of four accounts did real
// work, and the status panel should say so rather than reporting only failure.
func (s *syncState) record(res sync.Result, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !res.Started.IsZero() {
		s.lastAttempt = res.Started
		s.last = res
	} else {
		s.lastAttempt = time.Now()
	}
	if err != nil {
		s.lastErr = err.Error()
		return
	}
	s.lastErr = ""
	s.lastSuccess = s.lastAttempt
}

// status renders the current state for the web UI.
func (s *syncState) status(accounts int) webui.Status {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := webui.Status{
		Running:     s.started,
		AccountsNum: accounts,
		PushEnabled: s.pushEnabled,
		LastError:   s.lastErr,
		Created:     s.last.Created,
		Updated:     s.last.Updated,
		Deleted:     s.last.Deleted,
		Skipped:     s.last.Skipped,
	}
	if !s.lastAttempt.IsZero() {
		st.LastAttempt = s.lastAttempt.UTC().Format(time.RFC3339)
	}
	if !s.lastSuccess.IsZero() {
		st.LastSync = s.lastSuccess.UTC().Format(time.RFC3339)
		st.LastDurationMS = s.last.Duration().Milliseconds()
	}
	// Report per-account health so "which account is broken?" is answerable
	// from the UI instead of only from the logs.
	for _, name := range s.last.HealthyAccounts {
		st.Accounts = append(st.Accounts, webui.AccountStatus{Name: name, Healthy: true})
	}
	for _, name := range s.last.FailedAccounts {
		st.Accounts = append(st.Accounts, webui.AccountStatus{Name: name, Healthy: false})
	}
	return st
}
