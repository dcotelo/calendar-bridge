package sync

import (
	"context"
	"log/slog"
	"time"

	calendar "google.golang.org/api/calendar/v3"
)

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

func (c *retryingClient) InsertEvent(ctx context.Context, calendarID string, ev *calendar.Event) (*calendar.Event, error) {
	var out *calendar.Event
	err := retry(ctx, c.policy, c.onRetry("InsertEvent"), func() error {
		var err error
		out, err = c.inner.InsertEvent(ctx, calendarID, ev)
		return err
	})
	return out, err
}

func (c *retryingClient) UpdateEvent(ctx context.Context, calendarID, eventID string, ev *calendar.Event) (*calendar.Event, error) {
	var out *calendar.Event
	err := retry(ctx, c.policy, c.onRetry("UpdateEvent"), func() error {
		var err error
		out, err = c.inner.UpdateEvent(ctx, calendarID, eventID, ev)
		return err
	})
	return out, err
}

func (c *retryingClient) DeleteEvent(ctx context.Context, calendarID, eventID string) error {
	return retry(ctx, c.policy, c.onRetry("DeleteEvent"), func() error {
		return c.inner.DeleteEvent(ctx, calendarID, eventID)
	})
}
