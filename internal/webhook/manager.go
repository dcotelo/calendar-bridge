package webhook

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Channel describes one live watch channel registered against a calendar.
// It is provider-neutral: the fields are what any push-capable backend needs
// to identify and later stop a channel.
type Channel struct {
	// ID is the channel identifier calendar-bridge generated at watch time.
	ID string
	// ResourceID is the provider's opaque handle for the watched resource,
	// required to stop the channel.
	ResourceID string
	// Account and CalendarID identify what this channel watches.
	Account    string
	CalendarID string
	// Expiry is when the provider will stop delivering on this channel unless
	// renewed.
	Expiry time.Time
}

// ChannelWatcher is the capability a push-capable provider exposes: register
// and stop watch channels. Google's events.watch/channels.stop implement this;
// a future Outlook subscription API would implement the same shape.
//
// Watch registers a channel on (account, calendarID) delivering to callbackURL
// authenticated by token, living roughly ttl. It returns the live Channel.
// Stop tears a channel down (best-effort; called on shutdown and before
// re-registering).
type ChannelWatcher interface {
	Watch(ctx context.Context, account, calendarID, callbackURL, token string, ttl time.Duration) (Channel, error)
	Stop(ctx context.Context, ch Channel) error
}

// Manager keeps a set of watch channels alive: it registers one per
// (account, calendar) and renews each before it expires. It is the push
// counterpart to the poll loop's ticker.
type Manager struct {
	watcher     ChannelWatcher
	callbackURL string
	token       string
	ttl         time.Duration
	logger      *slog.Logger

	// renewSkew is how far before Expiry a channel is renewed, so a channel
	// never lapses in the gap between "expired" and "renewed".
	renewSkew time.Duration

	mu       sync.Mutex
	channels map[string]Channel // key: account|calendarID
}

// Target is one calendar to watch.
type Target struct {
	Account    string
	CalendarID string
}

// NewManager builds a watch-channel manager. callbackURL is the public
// receiver URL (PublicURL + "/webhook"); token is the verification secret;
// ttl is the desired channel lifetime.
func NewManager(watcher ChannelWatcher, callbackURL, token string, ttl time.Duration, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	skew := ttl / 10
	if skew < time.Minute {
		skew = time.Minute
	}
	return &Manager{
		watcher:     watcher,
		callbackURL: callbackURL,
		token:       token,
		ttl:         ttl,
		logger:      logger,
		renewSkew:   skew,
		channels:    make(map[string]Channel),
	}
}

func key(account, calendarID string) string { return account + "|" + calendarID }

// Run registers a channel for every target and keeps them renewed until ctx
// is cancelled, at which point it stops all channels best-effort. It blocks
// until ctx is done, so callers typically run it in its own goroutine.
func (m *Manager) Run(ctx context.Context, targets []Target) error {
	// Initial registration.
	for _, t := range targets {
		if err := m.register(ctx, t); err != nil {
			// Log and continue: one calendar failing to register shouldn't
			// stop push for the others. Polling remains the safety net.
			m.logger.Error("failed to register watch channel; relying on poll fallback for this calendar",
				"account", t.Account, "calendar", t.CalendarID, "error", err)
		}
	}

	ticker := time.NewTicker(m.renewSkew)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.stopAll()
			return ctx.Err()
		case <-ticker.C:
			m.renewExpiring(ctx, targets)
		}
	}
}

func (m *Manager) register(ctx context.Context, t Target) error {
	ch, err := m.watcher.Watch(ctx, t.Account, t.CalendarID, m.callbackURL, m.token, m.ttl)
	if err != nil {
		return fmt.Errorf("watch %s/%s: %w", t.Account, t.CalendarID, err)
	}
	m.mu.Lock()
	m.channels[key(t.Account, t.CalendarID)] = ch
	m.mu.Unlock()
	m.logger.Info("registered watch channel",
		"account", t.Account, "calendar", t.CalendarID, "expiry", ch.Expiry)
	return nil
}

func (m *Manager) renewExpiring(ctx context.Context, targets []Target) {
	now := time.Now()
	for _, t := range targets {
		m.mu.Lock()
		ch, ok := m.channels[key(t.Account, t.CalendarID)]
		m.mu.Unlock()

		needsRenew := !ok || now.Add(m.renewSkew).After(ch.Expiry)
		if !needsRenew {
			continue
		}

		// Stop the old channel first (best-effort) so we don't leak channels
		// on the provider side, then register a fresh one.
		if ok {
			if err := m.watcher.Stop(ctx, ch); err != nil {
				m.logger.Warn("failed to stop expiring channel before renew",
					"account", t.Account, "calendar", t.CalendarID, "error", err)
			}
		}
		if err := m.register(ctx, t); err != nil {
			m.logger.Error("failed to renew watch channel; poll fallback covers this calendar",
				"account", t.Account, "calendar", t.CalendarID, "error", err)
		}
	}
}

func (m *Manager) stopAll() {
	m.mu.Lock()
	chans := make([]Channel, 0, len(m.channels))
	for _, ch := range m.channels {
		chans = append(chans, ch)
	}
	m.channels = make(map[string]Channel)
	m.mu.Unlock()

	// Use a short, detached timeout: ctx is already cancelled at shutdown, but
	// we still want to release channels on the provider side.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, ch := range chans {
		if err := m.watcher.Stop(ctx, ch); err != nil {
			m.logger.Warn("failed to stop channel on shutdown",
				"account", ch.Account, "calendar", ch.CalendarID, "error", err)
		}
	}
}
