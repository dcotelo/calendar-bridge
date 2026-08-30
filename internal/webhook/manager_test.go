package webhook

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeWatcher records Watch/Stop calls and hands out channels with a
// configurable expiry offset.
type fakeWatcher struct {
	mu           sync.Mutex
	watchCalls   int
	stopCalls    int
	nextID       int
	expiryOffset time.Duration
}

func (f *fakeWatcher) Watch(ctx context.Context, account, calendarID, callbackURL, token string, ttl time.Duration) (Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watchCalls++
	f.nextID++
	return Channel{
		ID:         id(f.nextID),
		ResourceID: "res",
		Account:    account,
		CalendarID: calendarID,
		Expiry:     time.Now().Add(f.expiryOffset),
	}, nil
}

func (f *fakeWatcher) Stop(ctx context.Context, ch Channel) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalls++
	return nil
}

func (f *fakeWatcher) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.watchCalls, f.stopCalls
}

func id(n int) string { return "chan-" + time.Duration(n).String() }

func TestManager_RegistersInitialChannels(t *testing.T) {
	// Long expiry: no renewal should happen during the short test.
	fw := &fakeWatcher{expiryOffset: time.Hour}
	m := NewManager(fw, "https://x/webhook", "tok", time.Hour, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	targets := []Target{{Account: "a", CalendarID: "primary"}, {Account: "b", CalendarID: "primary"}}

	done := make(chan struct{})
	go func() { _ = m.Run(ctx, targets); close(done) }()

	// Wait for initial registration, bounded so a slow CI runner doesn't flake.
	var watches int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if watches, _ = fw.counts(); watches == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if watches != 2 {
		t.Errorf("initial watch calls = %d, want 2 (one per target)", watches)
	}

	cancel()
	<-done

	// On shutdown, both channels should be stopped.
	_, stops := fw.counts()
	if stops != 2 {
		t.Errorf("stop calls on shutdown = %d, want 2", stops)
	}
}

func TestManager_RenewsExpiringChannels(t *testing.T) {
	// Channels expire almost immediately, forcing renewal on the first tick.
	// renewSkew is clamped to a 1-minute minimum, and the ticker fires at
	// renewSkew, so we can't observe renewal within a fast unit test via the
	// timer. Instead we exercise renewExpiring directly, which is the unit
	// under test for the renewal decision.
	fw := &fakeWatcher{expiryOffset: -time.Second} // already expired
	m := NewManager(fw, "https://x/webhook", "tok", time.Hour, testLogger())
	ctx := context.Background()
	targets := []Target{{Account: "a", CalendarID: "primary"}}

	// First register.
	if err := m.register(ctx, targets[0]); err != nil {
		t.Fatalf("register error = %v", err)
	}
	watches, stops := fw.counts()
	if watches != 1 || stops != 0 {
		t.Fatalf("after register: watches=%d stops=%d, want 1/0", watches, stops)
	}

	// Now renew: since the channel is already expired, it should be stopped
	// and re-registered.
	m.renewExpiring(ctx, targets)
	watches, stops = fw.counts()
	if watches != 2 {
		t.Errorf("watch calls after renew = %d, want 2 (re-registered)", watches)
	}
	if stops != 1 {
		t.Errorf("stop calls after renew = %d, want 1 (old channel stopped)", stops)
	}
}
