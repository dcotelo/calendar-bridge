package sync

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	calendar "google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
)

func fastPolicy(maxAttempts int) RetryPolicy {
	return RetryPolicy{
		MaxAttempts: maxAttempts,
		BaseBackoff: time.Millisecond, // keep tests fast
		MaxBackoff:  2 * time.Millisecond,
	}
}

func TestIsTransient(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"429", &googleapi.Error{Code: http.StatusTooManyRequests}, true},
		{"500", &googleapi.Error{Code: http.StatusInternalServerError}, true},
		{"503", &googleapi.Error{Code: http.StatusServiceUnavailable}, true},
		{"401 not retried", &googleapi.Error{Code: http.StatusUnauthorized}, false},
		{"403 not retried", &googleapi.Error{Code: http.StatusForbidden}, false},
		{"404 not retried", &googleapi.Error{Code: http.StatusNotFound}, false},
		{"400 not retried", &googleapi.Error{Code: http.StatusBadRequest}, false},
		{"plain error not retried", errors.New("boom"), false},
		{"timeout net error retried", timeoutErr{}, true},
		{"temporary non-timeout DNS error retried", &net.DNSError{Err: "server misbehaving", IsTemporary: true}, true},
		{"non-temporary DNS error not retried", &net.DNSError{Err: "no such host", IsTemporary: false}, false},
		{"wrapped 429", errors.Join(errors.New("ctx"), &googleapi.Error{Code: 429}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransient(tc.err); got != tc.want {
				t.Errorf("isTransient(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// timeoutErr implements net.Error with Timeout() == true.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestRetry_SucceedsAfterTransientErrors(t *testing.T) {
	calls := 0
	err := retry(context.Background(), fastPolicy(4), nil, func() error {
		calls++
		if calls < 3 {
			return &googleapi.Error{Code: http.StatusServiceUnavailable}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retry() error = %v, want nil", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (two failures then success)", calls)
	}
}

func TestRetry_FailFastOnNonTransient(t *testing.T) {
	calls := 0
	wantErr := &googleapi.Error{Code: http.StatusUnauthorized}
	err := retry(context.Background(), fastPolicy(4), nil, func() error {
		calls++
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("retry() error = %v, want the 401", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (non-transient must not be retried)", calls)
	}
}

func TestRetry_ExhaustsAttempts(t *testing.T) {
	calls := 0
	err := retry(context.Background(), fastPolicy(3), nil, func() error {
		calls++
		return &googleapi.Error{Code: http.StatusTooManyRequests}
	})
	if err == nil {
		t.Fatal("retry() error = nil, want the last transient error")
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (MaxAttempts)", calls)
	}
}

func TestRetry_StopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := retry(ctx, RetryPolicy{MaxAttempts: 5, BaseBackoff: time.Hour, MaxBackoff: time.Hour}, nil, func() error {
		calls++
		cancel() // cancel during the first attempt so the backoff wait aborts
		return &googleapi.Error{Code: http.StatusServiceUnavailable}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("retry() error = %v, want context.Canceled joined in", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (cancellation must stop further attempts)", calls)
	}
}

func TestRetry_ZeroAttemptsTreatedAsOne(t *testing.T) {
	calls := 0
	_ = retry(context.Background(), RetryPolicy{MaxAttempts: 0}, nil, func() error {
		calls++
		return errors.New("x")
	})
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (MaxAttempts<1 clamps to 1)", calls)
	}
}

func TestBackoffFor_ClampsToMax(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 100, BaseBackoff: time.Second, MaxBackoff: 5 * time.Second}
	for attempt := 0; attempt < 50; attempt++ {
		got := p.backoffFor(attempt)
		if got < 0 || got >= 5*time.Second {
			t.Fatalf("backoffFor(%d) = %v, want within [0, 5s)", attempt, got)
		}
	}
}

// TestRetryingClient_RetriesListEvents verifies the decorator actually wires
// retry into a CalendarClient method.
func TestRetryingClient_RetriesListEvents(t *testing.T) {
	inner := newFakeCalendarClient()
	flaky := &flakyClient{inner: inner, failFirst: 2, code: http.StatusServiceUnavailable}
	c := NewRetryingClient(flaky, fastPolicy(4), newTestLogger(), "test")

	_, err := c.ListEvents(context.Background(), "primary", time.Now(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("ListEvents() error = %v, want nil after retries", err)
	}
	if flaky.listCalls != 3 {
		t.Errorf("underlying ListEvents calls = %d, want 3", flaky.listCalls)
	}
}

// flakyClient wraps a CalendarClient and makes ListEvents fail with a
// transient error the first failFirst times before delegating. Other methods
// pass straight through.
type flakyClient struct {
	inner     CalendarClient
	failFirst int
	code      int
	listCalls int
}

func (f *flakyClient) ListEvents(ctx context.Context, calendarID string, timeMin, timeMax time.Time) ([]*calendar.Event, error) {
	f.listCalls++
	if f.listCalls <= f.failFirst {
		return nil, &googleapi.Error{Code: f.code}
	}
	return f.inner.ListEvents(ctx, calendarID, timeMin, timeMax)
}

func (f *flakyClient) FindBlockBySource(ctx context.Context, calendarID, srcAccount, srcEventID string) (*calendar.Event, error) {
	return f.inner.FindBlockBySource(ctx, calendarID, srcAccount, srcEventID)
}

func (f *flakyClient) GetEvent(ctx context.Context, calendarID, eventID string) (*calendar.Event, error) {
	return f.inner.GetEvent(ctx, calendarID, eventID)
}

// getFlakyClient makes GetEvent fail transiently the first failFirst times.
type getFlakyClient struct {
	inner    CalendarClient
	failN    int
	code     int
	getCalls int
}

func (g *getFlakyClient) ListEvents(ctx context.Context, calendarID string, timeMin, timeMax time.Time) ([]*calendar.Event, error) {
	return g.inner.ListEvents(ctx, calendarID, timeMin, timeMax)
}
func (g *getFlakyClient) FindBlockBySource(ctx context.Context, calendarID, srcAccount, srcEventID string) (*calendar.Event, error) {
	return g.inner.FindBlockBySource(ctx, calendarID, srcAccount, srcEventID)
}
func (g *getFlakyClient) InsertEvent(ctx context.Context, calendarID string, ev *calendar.Event) (*calendar.Event, error) {
	return g.inner.InsertEvent(ctx, calendarID, ev)
}
func (g *getFlakyClient) UpdateEvent(ctx context.Context, calendarID, eventID string, ev *calendar.Event, ifMatchETag string) (*calendar.Event, error) {
	return g.inner.UpdateEvent(ctx, calendarID, eventID, ev, ifMatchETag)
}
func (g *getFlakyClient) DeleteEvent(ctx context.Context, calendarID, eventID, ifMatchETag string) error {
	return g.inner.DeleteEvent(ctx, calendarID, eventID, ifMatchETag)
}
func (g *getFlakyClient) GetEvent(ctx context.Context, calendarID, eventID string) (*calendar.Event, error) {
	g.getCalls++
	if g.getCalls <= g.failN {
		return nil, &googleapi.Error{Code: g.code}
	}
	return g.inner.GetEvent(ctx, calendarID, eventID)
}

func TestRetryingClient_RetriesGetEvent(t *testing.T) {
	inner := newFakeCalendarClient()
	flaky := &getFlakyClient{inner: inner, failN: 2, code: http.StatusServiceUnavailable}
	c := NewRetryingClient(flaky, fastPolicy(4), newTestLogger(), "test")
	if _, err := c.GetEvent(context.Background(), "primary", "id"); err != nil {
		t.Fatalf("GetEvent error = %v, want nil after retries", err)
	}
	if flaky.getCalls != 3 {
		t.Errorf("GetEvent calls = %d, want 3 (2 transient failures then success)", flaky.getCalls)
	}
}

func TestRetryingClient_GetEventExhaustsAttempts(t *testing.T) {
	inner := newFakeCalendarClient()
	flaky := &getFlakyClient{inner: inner, failN: 99, code: http.StatusTooManyRequests}
	c := NewRetryingClient(flaky, fastPolicy(3), newTestLogger(), "test")
	if _, err := c.GetEvent(context.Background(), "primary", "id"); err == nil {
		t.Fatal("GetEvent error = nil, want error after exhausting attempts")
	}
	if flaky.getCalls != 3 {
		t.Errorf("GetEvent calls = %d, want 3 (MaxAttempts)", flaky.getCalls)
	}
}

func (f *flakyClient) InsertEvent(ctx context.Context, calendarID string, ev *calendar.Event) (*calendar.Event, error) {
	return f.inner.InsertEvent(ctx, calendarID, ev)
}

func (f *flakyClient) UpdateEvent(ctx context.Context, calendarID, eventID string, ev *calendar.Event, ifMatchETag string) (*calendar.Event, error) {
	return f.inner.UpdateEvent(ctx, calendarID, eventID, ev, ifMatchETag)
}

func (f *flakyClient) DeleteEvent(ctx context.Context, calendarID, eventID, ifMatchETag string) error {
	return f.inner.DeleteEvent(ctx, calendarID, eventID, ifMatchETag)
}

// insertReconcileClient lets tests control InsertEvent's and
// FindBlockBySource's behavior independently, to exercise
// retryingClient.InsertEvent's ambiguous-insert reconciliation.
type insertReconcileClient struct {
	inner CalendarClient

	insertCalls int
	// insertErrs[i], if non-nil, is returned instead of delegating on the
	// (i+1)th InsertEvent call; once the slice is exhausted, calls delegate
	// to inner.
	insertErrs []error

	findCalls int
	// When findOverride is set, FindBlockBySource returns findResult/findErr
	// directly instead of delegating.
	findOverride bool
	findResult   *calendar.Event
	findErr      error
}

func (c *insertReconcileClient) ListEvents(ctx context.Context, calendarID string, timeMin, timeMax time.Time) ([]*calendar.Event, error) {
	return c.inner.ListEvents(ctx, calendarID, timeMin, timeMax)
}
func (c *insertReconcileClient) FindBlockBySource(ctx context.Context, calendarID, srcAccount, srcEventID string) (*calendar.Event, error) {
	c.findCalls++
	if c.findOverride {
		return c.findResult, c.findErr
	}
	return c.inner.FindBlockBySource(ctx, calendarID, srcAccount, srcEventID)
}
func (c *insertReconcileClient) GetEvent(ctx context.Context, calendarID, eventID string) (*calendar.Event, error) {
	return c.inner.GetEvent(ctx, calendarID, eventID)
}
func (c *insertReconcileClient) InsertEvent(ctx context.Context, calendarID string, ev *calendar.Event) (*calendar.Event, error) {
	idx := c.insertCalls
	c.insertCalls++
	if idx < len(c.insertErrs) && c.insertErrs[idx] != nil {
		return nil, c.insertErrs[idx]
	}
	return c.inner.InsertEvent(ctx, calendarID, ev)
}
func (c *insertReconcileClient) UpdateEvent(ctx context.Context, calendarID, eventID string, ev *calendar.Event, ifMatchETag string) (*calendar.Event, error) {
	return c.inner.UpdateEvent(ctx, calendarID, eventID, ev, ifMatchETag)
}
func (c *insertReconcileClient) DeleteEvent(ctx context.Context, calendarID, eventID, ifMatchETag string) error {
	return c.inner.DeleteEvent(ctx, calendarID, eventID, ifMatchETag)
}

func TestRetryingClient_InsertEvent_ReconcilesAmbiguousResult(t *testing.T) {
	block := ownedEvent("blk-1", "etag-1")

	cases := []struct {
		name          string
		findOverride  bool
		findResult    *calendar.Event
		findErr       error
		wantErr       bool
		wantAmbiguous bool
		wantInsert    int
	}{
		{
			name:         "confirmed persisted: reconciles, does not insert again",
			findOverride: true,
			findResult:   block,
			wantInsert:   1,
		},
		{
			name:         "confirmed absent: safe to retry the insert",
			findOverride: true,
			findResult:   nil,
			wantInsert:   2, // first (ambiguous) attempt + the retry
		},
		{
			name:          "lookup itself fails: stop rather than risk a duplicate",
			findOverride:  true,
			findErr:       timeoutErr{},
			wantErr:       true,
			wantAmbiguous: true,
			wantInsert:    1, // must NOT attempt a second insert while unconfirmed
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := &insertReconcileClient{
				inner:        newFakeCalendarClient(),
				insertErrs:   []error{timeoutErr{}}, // first attempt is ambiguous
				findOverride: tc.findOverride,
				findResult:   tc.findResult,
				findErr:      tc.findErr,
			}
			c := NewRetryingClient(inner, fastPolicy(3), newTestLogger(), "test")
			_, err := c.InsertEvent(context.Background(), "primary", ownedEvent("blk-1", ""))

			if tc.wantErr && err == nil {
				t.Fatal("InsertEvent error = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("InsertEvent error = %v, want nil", err)
			}
			var ambiguous *ambiguousInsertError
			if tc.wantAmbiguous && !errors.As(err, &ambiguous) {
				t.Errorf("InsertEvent error = %v, want *ambiguousInsertError", err)
			}
			if inner.insertCalls != tc.wantInsert {
				t.Errorf("InsertEvent calls = %d, want %d", inner.insertCalls, tc.wantInsert)
			}
		})
	}
}

// A delete whose response was lost still deleted the event. The retry then
// finds nothing there, and reporting that as a failure would make every such
// pass look broken.
func TestRetryingClient_DeleteRetryTreatsAlreadyGoneAsSuccess(t *testing.T) {
	calls := 0
	inner := &scriptedClient{
		deleteFn: func() error {
			calls++
			if calls == 1 {
				return &googleapi.Error{Code: http.StatusInternalServerError}
			}
			return &googleapi.Error{Code: http.StatusNotFound}
		},
	}
	c := NewRetryingClient(inner, RetryPolicy{MaxAttempts: 3, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond},
		newTestLogger(), "test")

	if err := c.DeleteEvent(context.Background(), "primary", "blk-1", ""); err != nil {
		t.Fatalf("DeleteEvent = %v, want nil: the first attempt's 5xx may still have deleted the event", err)
	}
	if calls != 2 {
		t.Errorf("made %d delete calls, want 2", calls)
	}
}

// On the FIRST attempt, "not found" means the caller asked to delete something
// that was never there — a real error worth surfacing.
func TestRetryingClient_DeleteFirstAttemptNotFoundStillErrors(t *testing.T) {
	inner := &scriptedClient{
		deleteFn: func() error { return &googleapi.Error{Code: http.StatusNotFound} },
	}
	c := NewRetryingClient(inner, RetryPolicy{MaxAttempts: 3, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond},
		newTestLogger(), "test")

	err := c.DeleteEvent(context.Background(), "primary", "blk-1", "")
	var apiErr *googleapi.Error
	if !errors.As(err, &apiErr) || apiErr.Code != http.StatusNotFound {
		t.Fatalf("DeleteEvent = %v, want the 404 surfaced on a first attempt", err)
	}
}

// scriptedClient is a CalendarClient whose delete behaviour is supplied per
// test; every other method is unused.
type scriptedClient struct {
	deleteFn func() error
}

func (s *scriptedClient) ListEvents(context.Context, string, time.Time, time.Time) ([]*calendar.Event, error) {
	return nil, nil
}
func (s *scriptedClient) FindBlockBySource(context.Context, string, string, string) (*calendar.Event, error) {
	return nil, nil
}
func (s *scriptedClient) GetEvent(context.Context, string, string) (*calendar.Event, error) {
	return nil, nil
}
func (s *scriptedClient) InsertEvent(context.Context, string, *calendar.Event) (*calendar.Event, error) {
	return nil, nil
}
func (s *scriptedClient) UpdateEvent(context.Context, string, string, *calendar.Event) (*calendar.Event, error) {
	return nil, nil
}
func (s *scriptedClient) DeleteEvent(context.Context, string, string, string) error {
	return s.deleteFn()
}
