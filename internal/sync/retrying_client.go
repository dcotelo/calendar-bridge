package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	calendar "google.golang.org/api/calendar/v3"
)

// ambiguousInsertError is returned by retryingClient.InsertEvent when the
// insert's own result was ambiguous (e.g. a network timeout) and the
// reconciliation lookup used to resolve that ambiguity itself failed, so it's
// still unknown whether the earlier insert persisted. It deliberately does
// not unwrap to either wrapped error, so isTransient never classifies it as
// retryable: retrying InsertEvent again here could create a duplicate owned
// block. The engine's next full sync pass re-checks FindBlockBySource before
// any insert, so that's where this resolves instead.
type ambiguousInsertError struct {
	insertErr error
	findErr   error
}

func (e *ambiguousInsertError) Error() string {
	return fmt.Sprintf("insert result is ambiguous (%v) and could not be reconciled (%v); leaving it for the next sync pass rather than risk a duplicate block", e.insertErr, e.findErr)
}

// retryingClient decorates a CalendarClient with transient-error retry and
// exponential backoff. Because it wraps the CalendarClient interface rather
// than any concrete provider, it is provider-agnostic: a future Outlook or
// iCloud client gets the same retry behavior for free.
type retryingClient struct {
	inner   CalendarClient
	policy  RetryPolicy
	logger  *slog.Logger
	account string // for log context; may be empty
}

// NewRetryingClient wraps inner so that transient failures (HTTP 429/5xx,
// network timeouts) are retried per policy. Non-transient errors (auth,
// not-found, bad request) are returned immediately without retry.
func NewRetryingClient(inner CalendarClient, policy RetryPolicy, logger *slog.Logger, account string) CalendarClient {
	if logger == nil {
		logger = slog.Default()
	}
	return &retryingClient{inner: inner, policy: policy, logger: logger, account: account}
}

func (c *retryingClient) onRetry(op string) func(int, error, time.Duration) {
	return func(attempt int, err error, wait time.Duration) {
		c.logger.Warn("retrying transient Calendar API error",
			"account", c.account,
			"op", op,
			"attempt", attempt,
			"backoff", wait,
			"error", err,
		)
	}
}

func (c *retryingClient) ListEvents(ctx context.Context, calendarID string, timeMin, timeMax time.Time) ([]*calendar.Event, error) {
	var out []*calendar.Event
	err := retry(ctx, c.policy, c.onRetry("ListEvents"), func() error {
		var err error
		out, err = c.inner.ListEvents(ctx, calendarID, timeMin, timeMax)
		return err
	})
	return out, err
}

func (c *retryingClient) FindBlockBySource(ctx context.Context, calendarID, srcAccount, srcEventID string) (*calendar.Event, error) {
	var out *calendar.Event
	err := retry(ctx, c.policy, c.onRetry("FindBlockBySource"), func() error {
		var err error
		out, err = c.inner.FindBlockBySource(ctx, calendarID, srcAccount, srcEventID)
		return err
	})
	return out, err
}

func (c *retryingClient) GetEvent(ctx context.Context, calendarID, eventID string) (*calendar.Event, error) {
	var out *calendar.Event
	err := retry(ctx, c.policy, c.onRetry("GetEvent"), func() error {
		var err error
		out, err = c.inner.GetEvent(ctx, calendarID, eventID)
		return err
	})
	return out, err
}

func (c *retryingClient) InsertEvent(ctx context.Context, calendarID string, ev *calendar.Event) (*calendar.Event, error) {
	var own Ownership
	if ev != nil {
		own = ownershipFromGoogle(ev)
	}
	var out *calendar.Event
	err := retry(ctx, c.policy, c.onRetry("InsertEvent"), func() error {
		created, err := c.inner.InsertEvent(ctx, calendarID, ev)
		if err == nil {
			out = created
			return nil
		}
		if !isAmbiguous(err) || own.SourceAccount == "" || own.SourceEventID == "" {
			return err
		}
		// A network timeout is ambiguous: the insert may have persisted on
		// the server even though we saw no response. Reconcile with a
		// lookup before letting the caller retry, so a retried InsertEvent
		// never creates a second busy block for the same source event.
		existing, findErr := c.inner.FindBlockBySource(ctx, calendarID, own.SourceAccount, own.SourceEventID)
		switch {
		case findErr == nil && existing != nil:
			// Confirmed: the earlier insert persisted. Use it, no retry.
			out = existing
			return nil
		case findErr == nil:
			// Confirmed: nothing persisted yet. Safe to retry the insert.
			return err
		default:
			// The reconciliation lookup itself failed, so it's still
			// unknown whether the earlier insert persisted. Don't guess by
			// retrying InsertEvent again — stop with a non-retryable error.
			return &ambiguousInsertError{insertErr: err, findErr: findErr}
		}
	})
	return out, err
}

func (c *retryingClient) UpdateEvent(ctx context.Context, calendarID, eventID string, ev *calendar.Event, ifMatchETag string) (*calendar.Event, error) {
	var out *calendar.Event
	err := retry(ctx, c.policy, c.onRetry("UpdateEvent"), func() error {
		var err error
		out, err = c.inner.UpdateEvent(ctx, calendarID, eventID, ev, ifMatchETag)
		return err
	})
	return out, err
}

func (c *retryingClient) DeleteEvent(ctx context.Context, calendarID, eventID, ifMatchETag string) error {
	return retry(ctx, c.policy, c.onRetry("DeleteEvent"), func() error {
		return c.inner.DeleteEvent(ctx, calendarID, eventID, ifMatchETag)
	})
}
