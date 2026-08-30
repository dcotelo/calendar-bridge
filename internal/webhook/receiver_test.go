package webhook

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// countingNotifier counts Notify calls.
type countingNotifier struct{ n atomic.Int64 }

func (c *countingNotifier) Notify() { c.n.Add(1) }

// count returns how many notifications have been delivered.
func (c *countingNotifier) count() int64 { return c.n.Load() }

func postWithHeaders(h map[string]string) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/webhook", nil)
	for k, v := range h {
		req.Header.Set(k, v)
	}
	return req
}

func TestReceiver_RejectsBadToken(t *testing.T) {
	notifier := &countingNotifier{}
	rec := NewReceiver("s3cret", notifier, testLogger())

	w := httptest.NewRecorder()
	rec.ServeHTTP(w, postWithHeaders(map[string]string{
		"X-Goog-Channel-Token":  "wrong",
		"X-Goog-Resource-State": "exists",
	}))

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
	if notifier.n.Load() != 0 {
		t.Errorf("Notify called %d times, want 0 on bad token", notifier.n.Load())
	}
}

func TestReceiver_RejectsNonPost(t *testing.T) {
	rec := NewReceiver("s3cret", &countingNotifier{}, testLogger())
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/webhook", nil)
	rec.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestReceiver_SyncHandshakeDoesNotNotify(t *testing.T) {
	notifier := &countingNotifier{}
	rec := NewReceiver("s3cret", notifier, testLogger())

	w := httptest.NewRecorder()
	rec.ServeHTTP(w, postWithHeaders(map[string]string{
		"X-Goog-Channel-Token":  "s3cret",
		"X-Goog-Resource-State": "sync",
	}))

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if notifier.n.Load() != 0 {
		t.Errorf("Notify called %d times, want 0 on sync handshake", notifier.n.Load())
	}
}

func TestReceiver_ValidChangeNotifies(t *testing.T) {
	notifier := &countingNotifier{}
	rec := NewReceiver("s3cret", notifier, testLogger())

	w := httptest.NewRecorder()
	rec.ServeHTTP(w, postWithHeaders(map[string]string{
		"X-Goog-Channel-Token":  "s3cret",
		"X-Goog-Resource-State": "exists",
		"X-Goog-Channel-ID":     "chan-1",
	}))

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if notifier.n.Load() != 1 {
		t.Errorf("Notify called %d times, want 1", notifier.n.Load())
	}
}

func TestDebouncer_CoalescesBurst(t *testing.T) {
	d := NewDebouncer(30 * time.Millisecond)

	// Fire a burst; should collapse to a single trigger.
	for i := 0; i < 10; i++ {
		d.Notify()
		time.Sleep(2 * time.Millisecond)
	}

	select {
	case <-d.C:
		// good: got the coalesced trigger
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected a debounced trigger, got none")
	}

	// No second trigger should be waiting immediately after.
	select {
	case <-d.C:
		t.Error("got a second trigger, burst should have coalesced to one")
	case <-time.After(60 * time.Millisecond):
		// good: quiet
	}
}

func TestDebouncer_TriggersAgainAfterQuiet(t *testing.T) {
	d := NewDebouncer(20 * time.Millisecond)

	d.Notify()
	select {
	case <-d.C:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("first trigger never arrived")
	}

	// A later, separate change should trigger again.
	d.Notify()
	select {
	case <-d.C:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("second, separate trigger never arrived")
	}
}

// Comparing raw bytes with subtle.ConstantTimeCompare returns immediately when
// the lengths differ, leaking the verification token's length through response
// timing. The receiver hashes both sides to a fixed width first; this test
// pins the behavioural half of that (every wrong token is rejected, including
// prefixes and lookalikes of the right length).
func TestReceiver_RejectsTokensOfEveryShape(t *testing.T) {
	const good = "correct-horse-battery-staple"
	for _, bad := range []string{
		"", "c", "correct", good[:len(good)-1], good + "x",
		strings.Repeat("x", len(good)), strings.ToUpper(good),
		" " + good, good + " ",
	} {
		t.Run("token="+bad, func(t *testing.T) {
			n := &countingNotifier{}
			r := NewReceiver(good, n, testLogger())
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/webhook", nil)
			req.Header.Set("X-Goog-Channel-Token", bad)
			req.Header.Set("X-Goog-Resource-State", "exists")
			r.ServeHTTP(w, req)

			if w.Code != http.StatusForbidden {
				t.Errorf("status = %d for token %q, want 403", w.Code, bad)
			}
			if n.count() != 0 {
				t.Errorf("a rejected notification still triggered a sync")
			}
		})
	}
}

// The token must never appear in a log line, even on rejection.
func TestReceiver_NeverLogsTheToken(t *testing.T) {
	const good = "SUPER-SECRET-VERIFICATION-TOKEN"
	var logs bytes.Buffer
	r := NewReceiver(good, &countingNotifier{}, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhook", nil)
	req.Header.Set("X-Goog-Channel-Token", "wrong-but-also-secret")
	r.ServeHTTP(w, req)

	out := logs.String()
	if strings.Contains(out, good) {
		t.Errorf("logs contain the configured verification token:\n%s", out)
	}
	if strings.Contains(out, "wrong-but-also-secret") {
		t.Errorf("logs contain the presented token:\n%s", out)
	}
}
