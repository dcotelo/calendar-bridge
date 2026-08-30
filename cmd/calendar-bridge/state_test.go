package main

import (
	"errors"
	"testing"
	"time"

	"github.com/dcotelo/calendar-bridge/internal/sync"
)

func TestSyncState_FreshStateReportsNothingSyncedYet(t *testing.T) {
	s := newSyncState(false)
	got := s.status(3)

	if got.AccountsNum != 3 {
		t.Errorf("AccountsNum = %d, want 3", got.AccountsNum)
	}
	if got.LastSync != "" || got.LastAttempt != "" {
		t.Errorf("a fresh state reported LastSync=%q LastAttempt=%q, want both empty", got.LastSync, got.LastAttempt)
	}
	if got.Running {
		t.Error("Running should be false until the poll loop starts")
	}
	if got.LastError != "" {
		t.Errorf("LastError = %q, want empty", got.LastError)
	}
}

func TestSyncState_RecordsASuccessfulPass(t *testing.T) {
	s := newSyncState(true)
	s.markRunning()

	start := time.Date(2026, 3, 12, 14, 0, 0, 0, time.UTC)
	s.record(sync.Result{
		Started:         start,
		Finished:        start.Add(1500 * time.Millisecond),
		Created:         2,
		Updated:         1,
		Deleted:         3,
		Skipped:         4,
		HealthyAccounts: []string{"personal", "work-acme"},
	}, nil)

	got := s.status(2)
	if !got.Running {
		t.Error("Running = false after markRunning")
	}
	if !got.PushEnabled {
		t.Error("PushEnabled = false, want true")
	}
	if got.LastSync != "2026-03-12T14:00:00Z" {
		t.Errorf("LastSync = %q", got.LastSync)
	}
	if got.LastAttempt != got.LastSync {
		t.Errorf("LastAttempt = %q, want it to match LastSync after a success", got.LastAttempt)
	}
	if got.LastDurationMS != 1500 {
		t.Errorf("LastDurationMS = %d, want 1500", got.LastDurationMS)
	}
	if got.Created != 2 || got.Updated != 1 || got.Deleted != 3 || got.Skipped != 4 {
		t.Errorf("counts = %d/%d/%d/%d, want 2/1/3/4", got.Created, got.Updated, got.Deleted, got.Skipped)
	}
	if len(got.Accounts) != 2 {
		t.Fatalf("Accounts = %v, want 2 entries", got.Accounts)
	}
	for _, a := range got.Accounts {
		if !a.Healthy {
			t.Errorf("account %s reported unhealthy after a clean pass", a.Name)
		}
	}
}

// A failed pass must not advance LastSync: "last successful sync" is what an
// alert like "no success in 3x poll_interval" keys off.
func TestSyncState_FailedPassKeepsTheLastSuccessTimestamp(t *testing.T) {
	s := newSyncState(false)
	good := time.Date(2026, 3, 12, 14, 0, 0, 0, time.UTC)
	s.record(sync.Result{Started: good, Finished: good.Add(time.Second), HealthyAccounts: []string{"a", "b"}}, nil)

	bad := good.Add(5 * time.Minute)
	s.record(sync.Result{
		Started:         bad,
		Finished:        bad.Add(time.Second),
		HealthyAccounts: []string{"a"},
		FailedAccounts:  []string{"b"},
	}, errors.New("listing events for account b: token expired"))

	got := s.status(2)
	if got.LastSync != "2026-03-12T14:00:00Z" {
		t.Errorf("LastSync = %q, want the earlier successful pass to stand", got.LastSync)
	}
	if got.LastAttempt != "2026-03-12T14:05:00Z" {
		t.Errorf("LastAttempt = %q, want the failed pass's time", got.LastAttempt)
	}
	if got.LastError == "" {
		t.Error("LastError is empty after a failed pass")
	}

	// Per-account health must name the broken account.
	var sawUnhealthy bool
	for _, a := range got.Accounts {
		if a.Name == "b" && !a.Healthy {
			sawUnhealthy = true
		}
	}
	if !sawUnhealthy {
		t.Errorf("Accounts = %v, want account b reported unhealthy", got.Accounts)
	}
}

// A setup failure produces no Result at all; the state must still record the
// error and a timestamp rather than silently showing the previous success.
func TestSyncState_RecordsAnErrorWithNoResult(t *testing.T) {
	s := newSyncState(false)
	s.record(sync.Result{}, errors.New("account personal: not yet authorized"))

	got := s.status(2)
	if got.LastError == "" {
		t.Error("LastError is empty after a setup failure")
	}
	if got.LastAttempt == "" {
		t.Error("LastAttempt is empty after a setup failure")
	}
	if got.LastSync != "" {
		t.Errorf("LastSync = %q, want empty — nothing has ever succeeded", got.LastSync)
	}
}

// A later success must clear the stored error, or the UI would keep showing a
// failure that has since been fixed.
func TestSyncState_SuccessClearsThePreviousError(t *testing.T) {
	s := newSyncState(false)
	t0 := time.Date(2026, 3, 12, 14, 0, 0, 0, time.UTC)
	s.record(sync.Result{Started: t0, Finished: t0}, errors.New("boom"))
	s.record(sync.Result{Started: t0.Add(time.Minute), Finished: t0.Add(time.Minute)}, nil)

	if got := s.status(2).LastError; got != "" {
		t.Errorf("LastError = %q after a subsequent success, want empty", got)
	}
}

func TestSyncState_ConcurrentRecordAndStatusAreSafe(t *testing.T) {
	s := newSyncState(false)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 500 {
			s.record(sync.Result{
				Started:  time.Unix(int64(i), 0),
				Finished: time.Unix(int64(i), 0).Add(time.Second),
				Created:  i,
			}, nil)
		}
	}()
	for range 500 {
		_ = s.status(2)
	}
	<-done
}
