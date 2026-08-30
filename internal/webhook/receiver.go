// Package webhook implements the Google Calendar push-notification (events.watch)
// receiver side of calendar-bridge's near-real-time sync path.
//
// # How Google Calendar push works
//
// Instead of polling, an app registers a "watch channel" on a calendar via
// events.watch, supplying a public HTTPS callback URL and an opaque channel
// token. When anything on that calendar changes, Google sends an HTTP POST to
// the callback URL. Crucially, the notification carries NO event data — only
// headers identifying the channel and the type of change:
//
//	X-Goog-Channel-ID       the channel UUID we generated at watch time
//	X-Goog-Channel-Token    the opaque token we supplied (used to authenticate)
//	X-Goog-Resource-ID      an opaque ID for the watched resource
//	X-Goog-Resource-State   "sync" (initial handshake) | "exists" | "not_exists"
//	X-Goog-Message-Number   monotonic per-channel counter
//
// So a notification is only a *nudge*: "something changed on this calendar,
// go reconcile it". calendar-bridge responds by running its normal SyncOnce
// pass — the same reconcile it would do on a poll tick — which keeps the push
// path and the poll path sharing one code path and one set of safety
// invariants. Push simply lowers latency from minutes to seconds; it never
// becomes a second, divergent way to mutate calendars.
//
// # Why this is opt-in
//
// Push requires a publicly reachable HTTPS endpoint (Google validates and
// refuses plain HTTP), and channels expire (max ~1 week, often less) so they
// must be renewed on a timer. That is real infrastructure the pure-polling
// deployment doesn't need, so it's gated behind config.Webhook.Enabled.
//
// # Security
//
//   - Every notification's X-Goog-Channel-Token is compared, in constant time,
//     against the configured verification token; mismatches are rejected 403
//     before any work. This is what stops an attacker who discovers the public
//     URL from spamming forced syncs.
//   - Notifications carry no event content, so even a forged-but-authenticated
//     request can at worst trigger a reconcile — it can never inject data.
//   - The receiver never logs the token value.
package webhook

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Notifier is the minimal thing the receiver needs: a way to request that a
// sync happen soon. The run loop supplies one that (debounced) triggers
// SyncOnce.
type Notifier interface {
	// Notify signals that a change was observed and a sync should run soon.
	// It must be non-blocking and safe for concurrent use.
	Notify()
}

// Receiver is an http.Handler that authenticates Google Calendar push
// notifications and forwards them to a Notifier.
type Receiver struct {
	token    string
	notifier Notifier
	logger   *slog.Logger
}

// NewReceiver builds a push-notification receiver. token is the shared
// verification secret configured under webhook.verification_token; notifier
// is invoked (non-blocking) for each authenticated change notification.
func NewReceiver(token string, notifier Notifier, logger *slog.Logger) *Receiver {
	if logger == nil {
		logger = slog.Default()
	}
	return &Receiver{token: token, notifier: notifier, logger: logger}
}

// ServeHTTP handles a single push notification POST from Google.
func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Authenticate via the channel token, constant-time to avoid leaking it
	// through timing. Reject before doing any work.
	got := req.Header.Get("X-Goog-Channel-Token")
	if subtle.ConstantTimeCompare([]byte(got), []byte(r.token)) != 1 {
		r.logger.Warn("rejected webhook with bad channel token",
			"remote", req.RemoteAddr,
			"channel", req.Header.Get("X-Goog-Channel-ID"),
		)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	state := req.Header.Get("X-Goog-Resource-State")
	// The very first message after watch registration is a "sync" handshake
	// that carries no real change — acknowledge it but don't trigger work.
	if state == "sync" {
		w.WriteHeader(http.StatusOK)
		return
	}

	r.logger.Info("received calendar change notification",
		"state", state,
		"channel", req.Header.Get("X-Goog-Channel-ID"),
		"message", req.Header.Get("X-Goog-Message-Number"),
	)
	r.notifier.Notify()
	w.WriteHeader(http.StatusOK)
}

// Debouncer coalesces a burst of Notify() calls into a single downstream
// trigger sent no more than once per interval. It implements Notifier and
// exposes a channel the run loop selects on to kick off a sync.
type Debouncer struct {
	interval time.Duration

	mu    sync.Mutex
	timer *time.Timer
	// gen increments on every Notify; a fire callback only emits if its
	// captured generation still matches, so a callback from an older timer
	// generation (which Reset can leave scheduled) is discarded rather than
	// emitting early and violating the quiet-period contract.
	gen uint64

	// C receives one value per debounced burst. Buffered so a fire never
	// blocks the debounce goroutine even if the consumer is mid-sync.
	C chan struct{}
}

// NewDebouncer returns a Debouncer that emits at most one trigger on C per
// interval-long window of activity.
func NewDebouncer(interval time.Duration) *Debouncer {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &Debouncer{interval: interval, C: make(chan struct{}, 1)}
}

// Notify marks that a change occurred; a single trigger is emitted on C once
// interval elapses with no further Notify resetting it.
func (d *Debouncer) Notify() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gen++
	gen := d.gen
	if d.timer == nil {
		d.timer = time.AfterFunc(d.interval, func() { d.fire(gen) })
		return
	}
	// Coalesce: extend the window. Re-arm with the current generation so a
	// previously-scheduled callback (if Reset raced with it firing) is ignored.
	d.timer.Stop()
	d.timer = time.AfterFunc(d.interval, func() { d.fire(gen) })
}

func (d *Debouncer) fire(gen uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if gen != d.gen {
		return // superseded by a newer Notify; discard this stale callback
	}
	// Hold the lock through the send so a concurrent Notify can't advance the
	// generation between the check and the send and let a stale callback emit.
	// Non-blocking: if a trigger is already queued, one is enough.
	select {
	case d.C <- struct{}{}:
	default:
	}
}
